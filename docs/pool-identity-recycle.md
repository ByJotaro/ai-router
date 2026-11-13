# Pool self-heal: identity recycle, wait-for-egress, stale-session selection

Measured 2026-10-22 on the isolated stand (`:9013` + warp `:8789`, 5 egresses)
and live `:9001`. Every rule below is backed by a red test
(`transport_pool_heal_test.go`, `sessionpool_fresh_test.go`,
`lockout_gen_test.go`) or by a log line quoted here.

## 1. Rotation is no longer skipped during a "global burn"

`PoolTransport.healRotate` replaces both the manual "restart warp-router" step
and the old `if globalActive() { skip rotate }` branches. A rotate re-registers
the WARP devices; when it lands on **new /48 prefixes** the upstream windows are
genuinely new.

A rotate does NOT clear the shared-burn latch by itself (changed 2026-10-22
10:40): measured that same morning, two freshly registered accounts came up in
`2a09:bac1:47a0` — a prefix that had been burned for hours. Clearing the latch on
the rotate alone therefore dropped the router out of fail-fast probing on a
claim. The latch now falls only to evidence: a 200 through a prefix that did not
exist before the rotate (`noteIdentityGroup`), or the two-distinct-/48 probation
(`globalOKAt`/`globalOKGroup`). `TestPoolHealRotateKeepsGlobalBurnUntilRenewal`
pins it (red on the old unconditional clear).

* Gap escalation: 4 min → 8 → 16 → 30 min. A heal that serves no pooled 200
  within 90 s widens the gap (`noteHealOutcome(false)`).
* Renewal is **proven**, not assumed: `noteIdentityGroup` bumps `identGen` (and
  clears the shared-burn flag) only after a 200 arrives through a /48 that was
  absent from the pre-rotate snapshot. Five WARP accounts on one endpoint can
  hand back the same prefixes — claiming "new identities" then would be a lie.
  Live proof: `pool: identities renewed — new egress prefix served
  group=2a09:bac5:48f1: generation=1` (10:14:58).

## 2. Wait for one egress instead of an instant 503

`transport.wait_for_lease_s` (default 20 s, env `AI_ROUTER_POOL_WAIT_S`) widens
the acquire spin: when everything is banned/parked the request **waits** for a
slot instead of returning 503 in two milliseconds, because a client retries a
bounded number of times (OMP: 10) while a rotate needs 30-90 s to respawn
workers.

The wait is journaled, not hidden: `Lease.WaitedMs` → `Result.WaitedMS` →
`request_log.waited_ms` → `RequestLogView.waited_ms`, plus a `wait` event in
`proxies_log` and the log line `pool: lease after wait` for waits ≥ 1.5 s.
`waited_ms > 0` is the "served after wait" category: still a success, but not a
healthy-pool success.

## 3. Per-model lockout: generation reset plus a half-open probe

`lockout.Tracker` locks a model after 10 failures in 5 min for 15 min. Under a
1000 rpm storm that threshold is crossed in about a second, so a healed pool
still answered `429 model locked out (repeated failures)` — the "the pool is
fine but the client is dead" shape.

Two independent clears now exist:

* `Router.syncIdentityGen` clears everything when `PoolTransport.IdentityGen`
  moves (proven renewal), ignoring the first sighting.
* The tracker itself probes: while locked, one request per `ProbeInterval`
  (default 5 s) is let through; a success calls `Reset`, a failure re-arms the
  lock from the probe time.
* Capacity verdicts never enter the tracker at all (`Router.recordModelFailure`):
  `ErrTransportUnavailable` (no egress / pool saturated / pool api restarting),
  pool-503 tails (`isPoolExhaustedErr`) and upstream 429/503 are verdicts about
  the POOL, not about the model. The pool already bans the egress for the
  upstream's Retry-After, so a second 15-minute model penalty only converted
  honest 503s into 429s.

Measured on the stand (`fakepool.py` + `stub.py`: real 1000 rpm open-loop, a
burned window flipped on/off, no provider quota touched), 150 s runs, phases
0-40 s healthy / 40-90 s burned / 90-150 s healthy again:

| build | healthy phase | recovered phase |
|---|---|---|
| lockout with no probe | 666/666 | **0**/1000 ok, 994 `model locked out` |
| + 20 s probe | 666/666 | 609/1000 ok, 390 locked |
| + 5 s probe | 666/666 | 720/1000 ok, 277 locked |
| + transport/quota exemption | 666/666 | 510/1000 ok, **0** locked (491 pool-503s) |

The last row looks "worse" in ok-count only because the failed requests are now
honest pool 503s: everything queued while the window was still shut drains its
20 s wait budget at the moment it reopens. The stand's own journal
(`request_log`) is the source of truth — 1000 rows in the minute, 2501/2501 ok
on a clean run — not the client's `sent` counter.

## 4. Session gate 403 = stale harvested `ses_*`, not a pool fault

Live 2026-10-22 08:11-08:17: ten `403 account gate ... can only be used from
within OpenCode` responses with `gate_hits 3..4` swapping egresses. The harvest
held one id 6 minutes old and 39 ids 116-183 h old; `NextFor` hashed egresses
over **all** of them, so most pins carried a stale TUI session. Worse, when every
id is older than `sessionMaxAge` (72 h) the selection was empty and the router
fell back to a **generated** session — the one shape the gate always rejects.

Selection now:

* `aliveLocked` ranks by `max(time_updated, last 200 served)` — proven-live ids
  rank as fresh — and sorts newest first.
* `NextFor` spreads egresses over the freshness window (`sessionFreshSpan`,
  2 h from the newest) instead of the whole set.
* `sessionStaleRescue`: when nothing is inside `sessionMaxAge`, the newest 5
  out-of-window genuine ids are used instead of a generated session. A burned
  in-window id is still preferred first (resurrection probe), preserving
  `TestSessionPoolStickyAndDead`.
* A burn forces a re-harvest (`loadedAt` reset), so a TUI session minted seconds
  ago enters the pool immediately.

## Operations

* `wait_for_lease_s: 0` restores the old fail-fast behaviour (stands/tests).
* `AI_ROUTER_SESSION_DEAD_TTL` — how long a burned session stays burned.
* `AI_ROUTER_SESSION_DB=off` — disable harvesting entirely (tests).
* Account top-up stays a warp-router action: `POST /api/v1/accounts` registers
  a new WARP device (202 + `job_id`); measured 2026-10-22: new accounts can land
  in an **already burned /48**, so it is capacity, not a guaranteed reset.
