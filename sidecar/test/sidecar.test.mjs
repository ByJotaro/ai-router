// Integration: a spawned sidecar against a fake pool/proxies/upstream.
// Proves the rules that a unit test cannot: duplicate egresses are excluded
// from routing, a conversation keeps one (session, egress, fingerprint) trio,
// a hard 429 parks the window and the SAME request hops to another egress,
// and when every egress is closed the client gets an honest 503 (a 429 with a
// long window would kill the provider for the agent's whole session) instead of
// the upstream's quota body.
//
// Run: bun test test/sidecar.test.mjs   (needs the wreq-js deps installed)
import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { join } from "node:path";
import { SESSION_ID_RE } from "../lib/identity.mjs";
import {
  chat, cleanup, freePort, readLog, startSidecar,
  startWorld, tempDir, waitForSidecar,
} from "./harness.mjs";

const T = 60_000;

// The outward wall contract (sidecar v2.59, verified live 2026-11-02). When no
// egress can serve, the client must never receive the upstream's quota body or
// a long window: OMP kills the provider for the whole session on a 429 whose
// Retry-After exceeds retry.maxDelayMs (300 s) or whose body says "usage
// limit". So:
//   - a pool that tried egresses and every one refused -> 503 pool_busy;
//   - a pool with nothing to try (all parked/blocked) -> 503 pool_empty;
//   - only a seconds-scale upstream window (1..60 s) -> 429 throttled.
// Retry-After is clamped to 60 s: >= 60 when the last upstream window was
// itself hours-class, >= 2 otherwise. `long:true` states that the upstream
// window was hours-class, so the ONLY acceptable answer is 503 (pool_busy when
// egresses were tried, pool_empty when nothing could be tried); a 429 there is
// the exact failure this contract exists to prevent and must fail the test.
// `throttled:true` states the opposite - a measured 1..60 s window - and is the
// only case where a 429 may be the answer.
function expectWall(res, { long = false, throttled = false } = {}) {
  expect([429, 503]).toContain(res.status);
  const body = JSON.parse(res.text);
  expect(body.type).toBe("error");
  expect(res.text).not.toContain("FreeUsageLimitError");
  expect(res.text).not.toContain("usage limit");
  const ra = Number(res.headers.get("retry-after"));
  if (res.status === 429) {
    expect(long).toBe(false);
    expect(throttled).toBe(true);
    expect(body.error.type).toBe("throttled");
    expect(ra).toBeGreaterThanOrEqual(1);
    expect(ra).toBeLessThanOrEqual(60);
  } else {
    expect(throttled).toBe(false);
    if (long) {
      // Every egress was tried and refused, and the upstream named an
      // hours-class window: the only honest answer is `pool_busy` with a hint
      // the client can honour (>= 60 s), never the upstream's own window and
      // never `pool_empty` (which claims nothing could be tried).
      expect(body.error.type).toBe("pool_busy");
      expect(ra).toBeGreaterThanOrEqual(60);
      // The refusal spent real upstream calls; the journal must be able to say
      // so. A missing header here is what made 287 incident 503s log
      // attempts=0 over 421 real calls (2026-11-04).
      const att = res.headers.get("x-sidecar-attempts");
      expect(att).not.toBeNull();
      expect(Number(att)).toBeGreaterThanOrEqual(1);
      const rms = res.headers.get("x-sidecar-retry-ms");
      expect(rms).not.toBeNull();
      expect(Number(rms)).toBeGreaterThanOrEqual(0);
    } else {
      expect(["pool_busy", "pool_empty"]).toContain(body.error.type);
      expect(ra).toBeGreaterThanOrEqual(2);
    }
    expect(ra).toBeLessThanOrEqual(60);
  }
}

describe.serial("sidecar egress accounting", () => {
  let dir, world, child, port, logFile;

  beforeAll(async () => {
    dir = tempDir("it");
    world = startWorld({ dup: true, retryAfter: "3600" });
    port = freePort();
    logFile = join(dir, "requests.jsonl");
    child = startSidecar({
      SIDECAR_PORT: String(port),
      SIDECAR_TARGET: `http://127.0.0.1:${world.upstream.port}`,
      SIDECAR_POOL_API: `http://127.0.0.1:${world.pool.port}`,
      SIDECAR_LOG: logFile,
      SIDECAR_STATE: join(dir, "v2_state.json"),
      SIDECAR_PINS: join(dir, "v2_pins.json"),
      SIDECAR_FP_STATE: join(dir, "v2_fp.json"),
      SIDECAR_IP_STATE: join(dir, "v2_ips.json"),
      // A first hours-class 429 parks provisionally; keep it short so the
      // escalation to the upstream window stays reachable inside a test.
      SIDECAR_SHORT_PARK_MS: "400",
      SIDECAR_LAST_CHANCE_GAP_MS: "5",
    });
    await waitForSidecar(port);
  }, T);

  afterAll(async () => {
    try { child.kill(); } catch {}
    world?.stop();
    if (dir) cleanup(dir);
  });

  test("duplicate public IPs are excluded from routing", async () => {
    const st = await (await fetch(`http://127.0.0.1:${port}/admin/state`)).json();
    expect(st.backends).toBe(2);          // 3 slots up, one is a duplicate address
    expect(st.uniq_ips).toBe(2);
    expect(st.ip_dups).toBe(1);
    // rows are per (egress, lane); the pool itself has two usable egresses
    expect(new Set(st.rows.filter((r) => r.active).map((r) => r.proxy)).size).toBe(2);
    expect(st.ip_reuses).toBe(0);
    const dups = readLog(logFile).filter((l) => l.event === "ip_dup");
    expect(dups.length).toBeGreaterThan(0);
    expect(dups[0].dropped).toContain(String(world.slots[2]));
  }, T);

  test("a conversation spreads over its pinned pairs, one sid per egress", async () => {
    // v2.69 replaced the single pin with a bounded fan-out: a conversation keeps
    // affinity per pair, but its concurrent/adjacent turns spend several
    // budgets instead of queueing on one address.
    const conv = "ses_stable_conv_aaaa";
    for (let i = 0; i < 3; i++) expect((await chat(port, conv)).status).toBe(200);
    const lines = readLog(logFile).filter((l) => l.status === 200 && l.conv);
    expect(lines.length).toBeGreaterThanOrEqual(3);
    const last = lines.slice(-3);
    expect(last.every((l) => l.conv === last[0].conv)).toBe(true);
    expect(last.every((l) => l.fp === last[0].fp)).toBe(true);      // one hello per conversation
    expect(last.every((l) => l.sid && SESSION_ID_RE.test(l.sid))).toBe(true);
    const ips = new Set(last.map((l) => l.ip));
    expect(ips.size).toBe(2);                    // two usable egresses, both used
    // A pair is stable: one sid never travels on two egresses.
    const bySid = new Map();
    for (const l of last) {
      if (bySid.has(l.sid)) expect(bySid.get(l.sid)).toBe(l.ip);
      else bySid.set(l.sid, l.ip);
    }
  }, T);

  test("a second instance on a busy port exits and says so", async () => {
    const dupDir = tempDir("dup");
    const dup = startSidecar({
      SIDECAR_PORT: String(port), // taken by the suite's sidecar
      SIDECAR_TARGET: `http://127.0.0.1:${world.upstream.port}`,
      SIDECAR_POOL_API: `http://127.0.0.1:${world.pool.port}`,
      SIDECAR_LOG: logFile, // same log: the event must be visible where ops looks
      SIDECAR_STATE: join(dupDir, "v2_state.json"),
      SIDECAR_PINS: join(dupDir, "v2_pins.json"),
      SIDECAR_FP_STATE: join(dupDir, "v2_fp.json"),
      SIDECAR_IP_STATE: join(dupDir, "v2_ips.json"),
    });
    const code = await dup.exited;
    cleanup(dupDir);
    expect(code).not.toBe(0);
    const ev = readLog(logFile).filter((l) => l.event === "bind_failed").slice(-1)[0];
    expect(ev).toBeTruthy();
    expect(ev.port).toBe(port);
    // the surviving instance still serves
    expect((await chat(port, "ses_after_dup_attempt")).status).toBe(200);
  }, T);

  test("a hard 429 parks that egress and the same request hops instead of failing", async () => {
    const conv = "ses_hop_conv_bbbb";
    const first = await chat(port, conv);
    expect(first.status).toBe(200);
    const before = readLog(logFile).filter((l) => l.conv && l.status === 200).slice(-1)[0];
    const burnedIp = before.ip;
    const burnedSlot = world.slots.findIndex((_, i) => `2a09:bad0:${i}:1::${i + 1}` === burnedIp);
    expect(burnedSlot).toBeGreaterThanOrEqual(0);
    world.upstream.burn(burnedSlot);

    const second = await chat(port, conv);
    expect(second.status).toBe(200);             // client never sees the 429
    // Retry telemetry (journal category "retried"): the burned egress refused,
    // the request hopped and was served - the answer must say how many provider
    // refusals it ate and how much wall time they + the switch cost. A count of
    // 1 or 2 depends on whether the same-egress id probe ran (it refuses too).
    const attempts = Number(second.headers.get("x-sidecar-attempts"));
    expect(attempts).toBeGreaterThanOrEqual(1);
    expect(Number.isInteger(attempts)).toBe(true);
    const retryMS = Number(second.headers.get("x-sidecar-retry-ms"));
    expect(retryMS).toBeGreaterThan(0);
    expect(retryMS).toBeLessThan(60_000);
    const tail = readLog(logFile).slice(-2);
    expect(tail.map((l) => l.status).sort()).toEqual([200, 429]);
    const parked = tail.find((l) => l.status === 429);
    const served = tail.find((l) => l.status === 200);
    expect(parked.fp).toBe(served.fp);           // fingerprint survives the hop
    expect(parked.sid).not.toBe(served.sid);     // the burned id was retired
    expect(parked.conv).toBe(served.conv);
    expect(served.ip).not.toBe(burnedIp);

    const st = await (await fetch(`http://127.0.0.1:${port}/admin/state`)).json();
    const row = st.rows.find((r) => r.ip === burnedIp);
    expect(row.hard).toBe(0);            // a first strike is a suspicion, not a verdict
    expect(row.strikes).toBe(1);
    // One hours-class 429 is not proof the window is spent (measured 2026-10-27/28:
    // 20 of 39 /64s answered 200 again within 5 min of a 429), so a first strike
    // only quarantines the egress briefly instead of writing it off for an hour.
    expect(row.banned_for_s).toBeGreaterThanOrEqual(0);
    expect(row.banned_for_s).toBeLessThanOrEqual(2);
    expect(st.ip_burned).toBeGreaterThanOrEqual(1);

    // A second 429 on the same egress inside the strike window is evidence:
    // only then park it for the upstream's own Retry-After. The provisional park
    // expires by itself, and the burned egress is the least-used one, so the
    // next conversation lands on it again.
    await Bun.sleep(500);
    const third = await chat(port, "ses_hop_conv_escalate");
    expect(third.status).toBe(200);                   // still hops, never fails
    const st2 = await (await fetch(`http://127.0.0.1:${port}/admin/state`)).json();
    const row2 = st2.rows.find((r) => r.ip === burnedIp);
    expect(row2.strikes).toBeGreaterThanOrEqual(2);
    expect(row2.hard).toBe(1);
    expect(row2.banned_for_s).toBeGreaterThan(3500);  // upstream's own Retry-After
    expect(row2.banned_for_s).toBeLessThanOrEqual(3600);
    const park = readLog(logFile).filter((l) => l.event === "park64").slice(-1)[0];
    expect(park.strikes).toBeGreaterThanOrEqual(2);
    world.upstream.unburn();
  }, T);

  test("with every egress closed the client gets an honest 503 + Retry-After", async () => {
    // Start from a clean book: the escalation below must be this test's own
    // evidence, not a hard park leaked by the previous test.
    await fetch(`http://127.0.0.1:${port}/admin/unpark`, { method: "POST" });
    world.upstream.burnAll(true);
    let last;
    // Two requests, not four: the second one re-tests the exits the first one
    // struck, which is the escalation this test measures. It stays on the
    // pool_busy branch (`long`), so the loop must not run on into the
    // everything-is-parked state where the honest answer is pool_empty.
    for (let i = 0; i < 2; i++) {
      last = await chat(port, "ses_allclosed_" + i);
      // The first strike only quarantines the egress for SHORT_PARK_MS (400 ms
      // in this suite). Wait it out: requests inside that window are "not
      // separated" by design, so without the sleep the ladder never reaches the
      // second strike and the assertions below would depend on leaked state.
      await Bun.sleep(450);
    }
    // Never the upstream's quota body or its hours-long window: both would make
    // the agent client kill the provider for the whole session, and the window
    // belongs to one address, not to the model.
    expectWall(last, { long: true });
    const ev = readLog(logFile).filter((l) => l.event === "all_egress_429").slice(-1)[0];
    expect(ev).toBeTruthy();
    // The request closed whatever windows were still live (at most one per
    // attempt), then answered honestly instead of hammering the burned ones.
    expect(ev.attempts).toBeGreaterThanOrEqual(1);
    expect(ev.attempts).toBeLessThanOrEqual(3);
    expect(ev.ra).toBeGreaterThan(0);
    const closed = await (await fetch(`http://127.0.0.1:${port}/admin/state`)).json();
    const active = closed.rows.filter((r) => r.active);
    expect(new Set(active.map((r) => r.proxy)).size).toBe(2);
    const inference = active.filter((r) => r.lane === "inference");
    expect(inference.length).toBe(2);          // the lane that was just burned
    // A pool that is genuinely closed escalates inside these two requests:
    // the first strike quarantines briefly, the second writes the window off for
    // the upstream's own Retry-After.
    expect(inference.every((r) => r.strikes >= 1)).toBe(true);
    expect(inference.some((r) => r.hard === 1)).toBe(true);
    expect(inference.some((r) => r.banned_for_s > 3000)).toBe(true);
    world.upstream.burnAll(false);
  }, T);

  test("parks can never lock a healthy pool out of service", async () => {
    // Measured live 2026-10-27 23:07: 425 client 429s in 78 s, every one with
    // attempts 0, because a burst of 429s had parked all egresses provisionally
    // while the same egresses served 200 two minutes later. Parks are our own
    // opinion, so a fully parked pool must still be probed before answering a wall.
    // Start from a clean pool: earlier tests leave written-off windows behind.
    await fetch(`http://127.0.0.1:${port}/admin/unpark`, { method: "POST" });
    world.upstream.burnAll(true);
    try {
      // One request strikes every egress once: the pool is now fully parked with
      // provisional (short) parks and nothing has been written off yet.
      const last = await chat(port, "ses_lockout_0");
      expectWall(last, { long: true });
      const parkedNow = await (await fetch(`http://127.0.0.1:${port}/admin/state?cached=1`)).json();
      expect(parkedNow.exhausted).toBe(true);

      // The windows open again while every park is still in place.
      world.upstream.burnAll(false);
      const after = await chat(port, "ses_lockout_after");
      expect(after.status).toBe(200);
      const ev = readLog(logFile).filter((l) => l.event === "last_chance_ok" || l.event === "last_chance_429");
      expect(ev.length).toBeGreaterThanOrEqual(1);
    } finally {
      world.upstream.burnAll(false);
      await fetch(`http://127.0.0.1:${port}/admin/unpark`, { method: "POST" });
    }
  }, T);

  test("an idle pool does not invent reuses", async () => {
    const st = await (await fetch(`http://127.0.0.1:${port}/admin/state`)).json();
    expect(st.ip_reuses).toBe(0);
    expect(st.ip_history).toBe(2);
  }, T);

  test("an empty pool answers 503, not a quota 429", async () => {
    // Measured live 2026-10-27 23:39: a pool rotation left discovery with zero
    // backends for ~2 min and the sidecar answered 429 FreeUsageLimitError "all
    // egresses parked" (358 answers, 90/min), which reads as "daily quota gone".
    // No egress at all is a transient outage: 503 with a short Retry-After.
    const n = world.slots.length;
    for (let i = 0; i < n; i++) world.pool.setDown(i, true);
    try {
      await fetch(`http://127.0.0.1:${port}/admin/state`);   // force a pool re-read
      const r = await chat(port, "conv-pool-empty");
      expect(r.status).toBe(503);
      expect(Number(r.headers.get("retry-after"))).toBe(2);
      expect(readLog(logFile).filter((l) => l.event === "pool_empty").length).toBeGreaterThanOrEqual(1);
    } finally {
      for (let i = 0; i < n; i++) world.pool.setDown(i, false);
      await fetch(`http://127.0.0.1:${port}/admin/unpark`, { method: "POST" });
    }
    expect((await chat(port, "conv-pool-back")).status).toBe(200);
  }, T);


  test("a forced debug backend does not become the conversation pin", async () => {
    const before = (await (await fetch(`http://127.0.0.1:${port}/admin/state`)).json()).pins;
    const res = await fetch(`http://127.0.0.1:${port}/v1/chat/completions`, {
      method: "POST",
      headers: {
        "content-type": "application/json",
        "x-opencode-session": "ses_forced_debug_conv",
        "x-opencode-session-id": "ses_forced_debug_conv",
        "x-sidecar-backend": String(world.slots[0]),
      },
      body: JSON.stringify({ model: "deepseek-v4.1-flash-free", messages: [{ role: "user", content: "hi" }] }),
      signal: AbortSignal.timeout(20_000),
    });
    await res.text();
    const after = (await (await fetch(`http://127.0.0.1:${port}/admin/state`)).json()).pins;
    expect(after).toBe(before);
    const pins = JSON.parse(await Bun.file(join(dir, "v2_pins.json")).text());
    expect(Object.keys(pins).some((k) => k.startsWith("ses_forced_debug_conv"))).toBe(false);
  }, T);

  test("rotate-fresh is refused unless explicitly enabled", async () => {
    const res = await fetch(`http://127.0.0.1:${port}/admin/rotate-fresh`, { method: "POST" });
    expect(res.status).toBe(409);
  }, T);
});

describe.serial("restart resilience", () => {
  // The failure this guards: after a load-induced death the state file holds
  // every egress parked for hours-class Retry-After. A reboot may hand out the
  // SAME addresses (warp account reuse), so an inherited park keyed to an old
  // IP would answer 429 for up to 12h with zero traffic sent — a restart that
  // never heals. A restart that spans a boot (or a long outage) must re-probe;
  // a crash-loop must keep its parks, or every loop hammers banned windows.
  async function restartScenario({ ageMs, bootAgoMs, expectStatus, expectMode }) {
    const dir = tempDir("restart");
    const world = startWorld({ retryAfter: "3600" });
    const port = freePort();
    const logFile = join(dir, "requests.jsonl");
    const stateFile = join(dir, "v2_state.json");
    const baseEnv = {
      SIDECAR_PORT: String(port),
      SIDECAR_TARGET: `http://127.0.0.1:${world.upstream.port}`,
      SIDECAR_POOL_API: `http://127.0.0.1:${world.pool.port}`,
      SIDECAR_LOG: logFile,
      SIDECAR_STATE: stateFile,
      SIDECAR_PINS: join(dir, "v2_pins.json"),
      SIDECAR_FP_STATE: join(dir, "v2_fp.json"),
      SIDECAR_IP_STATE: join(dir, "v2_ips.json"),
      SIDECAR_BOOT_MS: String(Date.now() - 4 * 3600_000), // long-lived process
    };
    const first = startSidecar(baseEnv);
    try {
      await waitForSidecar(port);
      expect((await chat(port, "ses_restart_seed_conv")).status).toBe(200);

      first.kill();
      await first.exited;
      await Bun.sleep(250);

      // Simulate the post-crash file: every known egress hard-parked for the
      // upstream's 12h cap, same public IPs as the pool still reports.
      const state = JSON.parse(await Bun.file(stateFile).text());
      const until = Date.now() + 12 * 3600_000;
      for (const row of Object.values(state.egress)) { row.until = until; row.hard = 1; }
      state.ts = new Date(Date.now() - ageMs).toISOString();
      await Bun.write(stateFile, JSON.stringify(state));

      const second = startSidecar({ ...baseEnv, SIDECAR_BOOT_MS: String(Date.now() - bootAgoMs) });
      try {
        await waitForSidecar(port);
        const after = await chat(port, "ses_restart_after_conv");
        expect(after.status).toBe(expectStatus);
        const cleared = readLog(logFile).filter((l) => l.event === "parks_cleared");
        if (expectMode) {
          expect(cleared.length).toBe(1);
          expect(cleared[0].mode).toBe(expectMode);
          expect(cleared[0].rows).toBe(world.slots.length);
        } else {
          expect(cleared.length).toBe(0);
          // The inherited hours-class parks still hold, so nothing could even be
          // tried: 503 pool_empty with a clamped hint, never the 12 h window.
          expectWall(after);
        }
      } finally {
        second.kill();
        await second.exited;
      }
    } finally {
      try { first.kill(); } catch {}
      world.stop();
      cleanup(dir);
    }
  }

  test("a long outage clears inherited hours-class parks", async () => {
    await restartScenario({ ageMs: 600_000, bootAgoMs: 4 * 3600_000, expectStatus: 200, expectMode: "restart" });
  }, 120_000);

  test("a state written before the last boot is cleared even for a quick reboot", async () => {
    // downtime 40s, machine booted 20s ago: only the boot signal can tell.
    await restartScenario({ ageMs: 40_000, bootAgoMs: 20_000, expectStatus: 200, expectMode: "reboot" });
  }, 120_000);

  test("a crash-loop keeps its parks instead of hammering closed windows", async () => {
    await restartScenario({ ageMs: 5_000, bootAgoMs: 4 * 3600_000, expectStatus: 503, expectMode: null });
  }, 120_000);
});

describe.serial("throttle vs exhausted window", () => {
  // The upstream sends two different 429s: a seconds-scale throttle with no
  // Retry-After, and an exhausted window that names hours in Retry-After.
  // Parking the throttle for PARK_MS (10 min) is what emptied the pool.
  let dir, world, child, port, logFile;

  beforeAll(async () => {
    dir = tempDir("throttle");
    world = startWorld({ retryAfter: "" });
    port = freePort();
    logFile = join(dir, "requests.jsonl");
    child = startSidecar({
      SIDECAR_PORT: String(port),
      SIDECAR_TARGET: `http://127.0.0.1:${world.upstream.port}`,
      SIDECAR_POOL_API: `http://127.0.0.1:${world.pool.port}`,
      SIDECAR_LOG: logFile,
      SIDECAR_STATE: join(dir, "v2_state.json"),
      SIDECAR_PINS: join(dir, "v2_pins.json"),
      SIDECAR_FP_STATE: join(dir, "v2_fp.json"),
      SIDECAR_IP_STATE: join(dir, "v2_ips.json"),
      SIDECAR_THROTTLE_PARK_MS: "7000",
      SIDECAR_LAST_CHANCE_GAP_MS: "5",
      // One egress per conversation: this test follows the pin across a 429.
      SIDECAR_PIN_FANOUT: "1",
    });
    await waitForSidecar(port);
  }, T);

  afterAll(async () => {
    try { child.kill(); } catch {}
    world?.stop();
    if (dir) cleanup(dir);
  });

  test("a 429 without Retry-After parks seconds, not the ten-minute default", async () => {
    expect((await chat(port, "conv-throttle-a")).status).toBe(200);
    const served = readLog(logFile).filter((l) => l.conv && l.status === 200).slice(-1)[0];
    const slot = world.slots.findIndex((_, i) => `2a09:bad0:${i}:1::${i + 1}` === served.ip);
    expect(slot).toBeGreaterThanOrEqual(0);
    world.upstream.burn(slot);
    try {
      const second = await chat(port, "conv-throttle-a");   // same conversation: keeps the pin
      expect(second.status).toBe(200);                    // hopped, never failed
      const ev = readLog(logFile).filter((l) => l.event === "park_throttle").slice(-1)[0];
      expect(ev).toBeTruthy();
      expect(ev.until_s).toBeGreaterThanOrEqual(5);
      expect(ev.until_s).toBeLessThanOrEqual(8);
      const st = await (await fetch(`http://127.0.0.1:${port}/admin/state?cached=1`)).json();
      const row = st.rows.find((r) => r.ip === served.ip);
      // A throttle is not evidence against the egress: no strike, no verdict.
      expect(row.strikes).toBe(0);
      expect(row.hard).toBe(0);
      expect(row.banned_for_s).toBeLessThanOrEqual(8);
    } finally {
      world.upstream.unburn();
    }
  }, T);
});

// The other half of the wall contract: a seconds-scale upstream window is NOT a
// dead pool. The client backs off for exactly the value it is given, so a 7 s
// window must reach it as 429 throttled with retry-after 7; answering 503 here
// would read as a pool outage. This is the only case where a 429 may be the
// outward answer, and it keeps that branch of expectWall alive.
describe.serial("a seconds-scale window is a throttle, not a dead pool", () => {
  let dir, world, child, port, logFile;

  beforeAll(async () => {
    dir = tempDir("shortra");
    world = startWorld({ retryAfter: "7" });
    port = freePort();
    logFile = join(dir, "requests.jsonl");
    child = startSidecar({
      SIDECAR_PORT: String(port),
      SIDECAR_TARGET: `http://127.0.0.1:${world.upstream.port}`,
      SIDECAR_POOL_API: `http://127.0.0.1:${world.pool.port}`,
      SIDECAR_LOG: logFile,
      SIDECAR_STATE: join(dir, "v2_state.json"),
      SIDECAR_PINS: join(dir, "v2_pins.json"),
      SIDECAR_FP_STATE: join(dir, "v2_fp.json"),
      SIDECAR_IP_STATE: join(dir, "v2_ips.json"),
    });
    await waitForSidecar(port);
  }, T);

  afterAll(async () => {
    try { child.kill(); } catch {}
    world?.stop();
    if (dir) cleanup(dir);
  });

  test("a 7 s window reaches the client as 429 throttled", async () => {
    world.upstream.burnAll(true);
    const r = await chat(port, "conv-short-window");
    expectWall(r, { throttled: true });
    expect(Number(r.headers.get("retry-after"))).toBe(7);
    const ev = readLog(logFile).filter((l) => l.event === "client_429_short").pop();
    expect(ev).toBeTruthy();
    expect(ev.ra_in).toBe(7);
    expect(ev.ra_out).toBe(7);
    // A throttle is not a verdict against the egress: no strike, no hard flag.
    const st = await (await fetch(`http://127.0.0.1:${port}/admin/state?cached=1`)).json();
    const inf = st.rows.filter((row) => row.lane === "inference");
    expect(inf.length).toBeGreaterThan(0);
    expect(inf.every((row) => row.strikes === 0 && row.hard === 0)).toBe(true);
    world.upstream.burnAll(false);
  }, T);
});

describe.serial("per-lane parks", () => {
  // Measured on the live pool 2026-10-27: one egress answered 429 on /zen and
  // 200 on /inference seconds apart. Parking it for both lanes took the whole
  // fleet down for hours, which is exactly the "every request in this session
  // is 429" outage. A verdict must stay on the lane that produced it.
  let dir, world, child, port, logFile;

  beforeAll(async () => {
    dir = tempDir("lanes");
    world = startWorld({ retryAfter: "3600" });
    port = freePort();
    logFile = join(dir, "requests.jsonl");
    child = startSidecar({
      SIDECAR_PORT: String(port),
      SIDECAR_TARGET: `http://127.0.0.1:${world.upstream.port}`,
      SIDECAR_POOL_API: `http://127.0.0.1:${world.pool.port}`,
      SIDECAR_LOG: logFile,
      SIDECAR_STATE: join(dir, "v2_state.json"),
      SIDECAR_PINS: join(dir, "v2_pins.json"),
      SIDECAR_FP_STATE: join(dir, "v2_fp.json"),
      SIDECAR_IP_STATE: join(dir, "v2_ips.json"),
    });
    await waitForSidecar(port);
  }, T);

  afterAll(async () => {
    try { child.kill(); } catch {}
    world?.stop();
    if (dir) cleanup(dir);
  });

  test("a zen-only 429 does not park the egress for the inference lane", async () => {
    world.upstream.refusePath("/zen");
    const zen = await chat(port, "conv-lane-zen", { path: "/zen/v1/chat/completions" });
    expectWall(zen, { long: true });              // every egress refuses this lane
    const zenHits = world.upstream.hits.filter((h) => h.status === 429 && h.pathname.startsWith("/zen"));
    expect(new Set(zenHits.map((h) => h.egress)).size).toBe(3);   // one probe per egress max

    const before = world.upstream.hits.length;
    const inf = await chat(port, "conv-lane-inf", { path: "/inference/openai/v1/chat/completions" });
    expect(inf.status).toBe(200);
    expect(world.upstream.hits.length).toBe(before + 1);   // first egress served it
    expect(world.upstream.hits.slice(-1)[0].pathname).toBe("/inference/openai/v1/chat/completions");

    const st = await (await fetch(`http://127.0.0.1:${port}/admin/state?cached=1`)).json();
    const lanes = new Set(st.rows.map((r) => r.lane));
    expect(lanes.has("zen")).toBe(true);
    expect(lanes.has("inference")).toBe(true);
    const zenRows = st.rows.filter((r) => r.lane === "zen");
    const infRows = st.rows.filter((r) => r.lane === "inference");
    expect(zenRows.every((r) => r.banned_for_s > 0)).toBe(true);
    expect(infRows.every((r) => r.banned_for_s === 0)).toBe(true);
    world.upstream.refusePath("");
  }, T);

  test("a probe-sized 429 leaves every park untouched", async () => {
    // Small bodies answer 429 while the same egress serves real traffic (live
    // measurement, 42 of 55 vs 602 of 622). Parking on that evidence parked the
    // whole fleet and produced the "every request is 429" outage.
    world.upstream.burnAll(true);
    const r = await chat(port, "conv-probe-small", { small: true });
    expectWall(r, { long: true });
    const st = await (await fetch(`http://127.0.0.1:${port}/admin/state?cached=1`)).json();
    // the zen lane is parked on purpose by the previous test; the inference
    // lane must stay clean because the 429 came from a probe body.
    expect(st.rows.filter((row) => row.lane === "inference" && row.banned_for_s > 0).length).toBe(0);
    const probes = readLog(logFile).filter((l) => l.event === "probe_429");
    expect(probes.length).toBeGreaterThan(0);
    world.upstream.burnAll(false);
  }, T);
});

// `representative` is size AND shape: the same egress answered 429 to a body
// without stream/tools while the real client got 200 (measured 2026-10-29), so
// a full-size body WITHOUT tools is still a probe and may not park. Only the
// pair (>= REPRESENTATIVE_BYTES, stream:true, tools[]) teaches a budget verdict.
describe.serial("agent shape gates the parking verdict", () => {
  let dir, world, child, port, logFile;

  beforeAll(async () => {
    dir = tempDir("shape");
    world = startWorld({ retryAfter: "3600" });
    port = freePort();
    logFile = join(dir, "requests.jsonl");
    child = startSidecar({
      SIDECAR_PORT: String(port),
      SIDECAR_TARGET: `http://127.0.0.1:${world.upstream.port}`,
      SIDECAR_POOL_API: `http://127.0.0.1:${world.pool.port}`,
      SIDECAR_LOG: logFile,
      SIDECAR_STATE: join(dir, "v2_state.json"),
      SIDECAR_PINS: join(dir, "v2_pins.json"),
      SIDECAR_FP_STATE: join(dir, "v2_fp.json"),
      SIDECAR_IP_STATE: join(dir, "v2_ips.json"),
    });
    await waitForSidecar(port);
  }, T);

  afterAll(async () => {
    try { child.kill(); } catch {}
    world?.stop();
    if (dir) cleanup(dir);
  });

  const infRows = async () => {
    const st = await (await fetch(`http://127.0.0.1:${port}/admin/state?cached=1`)).json();
    return st.rows.filter((row) => row.lane === "inference");
  };

  test("a full-size body without tools is a probe: no strike, no park", async () => {
    world.upstream.burnAll(true);
    const r = await chat(port, "conv-shape-bare", { tools: false });
    expectWall(r, { long: true });
    const probe = readLog(logFile)
      .filter((l) => l.event === "probe_429" && l.tools === false && l.bytes >= 8192)
      .pop();
    expect(probe).toBeTruthy();
    expect(probe.stream).toBe(true);
    for (const row of await infRows()) {
      expect(row.strikes).toBe(0);
      expect(row.banned_for_s).toBe(0);
      expect(row.hard).toBe(0);
    }
    world.upstream.burnAll(false);
  }, T);

  test("the same call with tools is agent-shaped: it strikes and parks", async () => {
    world.upstream.burnAll(true);
    const r = await chat(port, "conv-shape-agent");
    expectWall(r, { long: true });
    const rows = await infRows();
    expect(rows.some((row) => row.strikes >= 1 && row.banned_for_s > 0)).toBe(true);
    // An agent-shaped 429 never takes the probe path.
    expect(readLog(logFile).filter((l) => l.event === "probe_429" && l.tools === true && l.bytes >= 8192).length).toBe(0);
    world.upstream.burnAll(false);
  }, T);
});

describe.serial("boot race", () => {
  // The Run key starts warp-router and this process in parallel. If the sidecar
  // wins, it must keep probing instead of answering 429 from an empty pool
  // until the next client request happens to trigger a refresh.
  test("a sidecar that starts before the pool heals without a client request", async () => {
    const dir = tempDir("boot");
    const port = freePort();
    const poolPort = freePort();
    const logFile = join(dir, "requests.jsonl");
    const child = startSidecar({
      SIDECAR_PORT: String(port),
      SIDECAR_TARGET: "http://127.0.0.1:1",
      SIDECAR_POOL_API: `http://127.0.0.1:${poolPort}`, // nothing listening yet
      SIDECAR_LOG: logFile,
      SIDECAR_STATE: join(dir, "v2_state.json"),
      SIDECAR_PINS: join(dir, "v2_pins.json"),
      SIDECAR_FP_STATE: join(dir, "v2_fp.json"),
      SIDECAR_IP_STATE: join(dir, "v2_ips.json"),
    });
    let pool;
    try {
      await waitForSidecar(port);
      const empty = await (await fetch(`http://127.0.0.1:${port}/admin/state`)).json();
      expect(empty.backends).toBe(0);
      pool = Bun.serve({
        port: poolPort,
        idleTimeout: 10,
        fetch: () => Response.json({ backends: [{ state: "up", http_addr: "127.0.0.1:65001", public_ip: "2a09:boot::1" }] }),
      });
      // No client request: the bootstrap loop must discover on its own.
      const deadline = Date.now() + 12_000;
      let backends = 0;
      while (Date.now() < deadline) {
        await Bun.sleep(500);
        // cached: no forced refresh, so only the bootstrap loop can heal this
        const st = await (await fetch(`http://127.0.0.1:${port}/admin/state?cached=1`)).json();
        backends = st.backends;
        if (backends > 0) break;
      }
      expect(backends).toBe(1);
    } finally {
      try { child.kill(); } catch {}
      try { pool?.stop(true); } catch {}
      cleanup(dir);
    }
  }, 60_000);
});

describe.serial("pool exhaustion", () => {
  // A /64 is one budget window: two slots inside one prefix must park together,
  // and a pool with nothing live must rotate itself instead of idling until
  // midnight UTC (measured live 2026-10-27 17:52-18:17: 1970 all_egress_429 in
  // 25 minutes and recovery only after a manual restart).
  let dir, world, child, port, logFile;

  beforeAll(async () => {
    dir = tempDir("g64");
    world = startWorld({ retryAfter: "3600" });
    port = freePort();
    logFile = join(dir, "requests.jsonl");
    child = startSidecar({
      SIDECAR_PORT: String(port),
      SIDECAR_TARGET: `http://127.0.0.1:${world.upstream.port}`,
      SIDECAR_POOL_API: `http://127.0.0.1:${world.pool.port}`,
      SIDECAR_LOG: logFile,
      SIDECAR_STATE: join(dir, "v2_state.json"),
      SIDECAR_PINS: join(dir, "v2_pins.json"),
      SIDECAR_FP_STATE: join(dir, "v2_fp.json"),
      SIDECAR_IP_STATE: join(dir, "v2_ips.json"),
      // The harness disables rotation by default; this pool is fake.
      SIDECAR_ALLOW_ROTATE: "1",
      // No idle grace: the test cannot wait a minute for the real 60 s gate.
      SIDECAR_ROTATE_IDLE_MS: "0",
      // Follow the conversation's pinned egress through its 429 (fan-out would
      // mint a fresh pin and route around the burned one).
      SIDECAR_PIN_FANOUT: "1",
    });
    // Slot 1 sits inside slot 0's /64 with a different address.
    world.pool.setIp(1, "2a09:bad0:0:1::99");
    await waitForSidecar(port);
  }, T);

  afterAll(async () => {
    try { child.kill(); } catch {}
    world?.stop();
    if (dir) cleanup(dir);
  });

  test("an hours-class 429 parks the egress, not its /64 (inference lane)", async () => {
    // Measured 2026-10-27 on the live pool: ::32:132 inside 48f3:1f5 answered
    // FreeUsageLimit ra=1121 at 23:41, while ::32:144 (same /64) served 95x200
    // during 23:52-23:55. The budget is per address here, so a neighbour is not
    // part of this verdict. Group parking stays a zen rule (next test).
    await fetch(`http://127.0.0.1:${port}/admin/unpark`, { method: "POST" });
    // Pin to slot 0 while the other exits are down: pick order is randomised and
    // this test is about the burned egress, not whichever the shuffle takes.
    world.pool.setDown(1, true);
    world.pool.setDown(2, true);
    await fetch(`http://127.0.0.1:${port}/admin/state`);
    expect((await chat(port, "conv-64-park")).status).toBe(200);
    world.pool.setDown(1, false);
    world.pool.setDown(2, false);
    await fetch(`http://127.0.0.1:${port}/admin/state`);
    world.upstream.burn(0);
    try {
      const before = world.upstream.hits.length;
      const r = await chat(port, "conv-64-park");
      expect(r.status).toBe(200);                          // hopped, client never saw the 429
      const hits = world.upstream.hits.slice(before);
      const byEgress = hits.reduce((m, h) => (m[h.egress] = (m[h.egress] || 0) + 1, m), {});
      // Three upstream calls on the burned address before the hop: the request,
      // its alternate-UA attempt (a throttled UA can masquerade as
      // FreeUsageLimit 429) and the fresh-id probe of the SAME egress.
      expect(byEgress[0]).toBe(3);
      const park = readLog(logFile).filter((l) => l.event === "park64").slice(-1)[0];
      expect(park.slots).toBe(1);                           // this slot only
      const st = await (await fetch(`http://127.0.0.1:${port}/admin/state?cached=1`)).json();
      const parkedInf = st.rows.filter((row) => row.lane === "inference" && row.banned_for_s > 0);
      expect(parkedInf.length).toBe(1);
      // The sibling address in the same /64 keeps its own budget: it stays in
      // rotation (serving this very request is the desired outcome).
      const sibling = st.rows.find((row) => row.lane === "inference" && row.ip === "2a09:bad0:0:1::99");
      expect(sibling.banned_for_s).toBe(0);
      expect(st.groups64).toBe(2);                          // slot 0/1 share, slot 2 alone
    } finally {
      world.upstream.unburn();
    }
  }, T);

  test("the same 429 on the zen lane parks its whole /64", async () => {
    // The legacy zen handler limits per x-real-ip truncated to four hextets, so
    // there one 429 does close the prefix; this is the only place where sharing
    // a park is documented.
    await fetch(`http://127.0.0.1:${port}/admin/unpark`, { method: "POST" });
    // Pin the conversation to slot 0 first: pick order is randomised (so a
    // fresh pool spreads over both fleets), and the test needs the burned
    // egress, not whichever one the shuffle happened to take.
    world.pool.setDown(1, true);
    world.pool.setDown(2, true);
    await fetch(`http://127.0.0.1:${port}/admin/state`);      // discover(true): see only slot 0
    expect((await chat(port, "conv-64-zen", { path: "/zen/v1/chat/completions" })).status).toBe(200);
    world.pool.setDown(1, false);
    world.pool.setDown(2, false);
    await fetch(`http://127.0.0.1:${port}/admin/state`);
    world.upstream.burn(0);
    try {
      const r = await chat(port, "conv-64-zen", { path: "/zen/v1/chat/completions" });
      expect(r.status).toBe(200);                           // served by another egress
      const park = readLog(logFile).filter((l) => l.event === "park64").slice(-1)[0];
      expect(park.slots).toBe(2);                           // slot 0 and its /64 sibling
      const st = await (await fetch(`http://127.0.0.1:${port}/admin/state?cached=1`)).json();
      const parkedZen = st.rows.filter((row) => row.lane === "zen" && row.banned_for_s > 0);
      expect(parkedZen.length).toBe(2);
    } finally {
      world.upstream.unburn();
    }
  }, T);

  test("a fully parked pool redraws itself once, then waits", async () => {
    world.upstream.burnAll(true);
    const r1 = await chat(port, "conv-64-dead");
    expectWall(r1, { long: true });
    // The first lever on a dead pool is a retunnel (new handshakes on the
    // accounts we hold); a whole-pool rotate is only the fallback when the pool
    // refuses it. Assert the lever by name: a rotate-only implementation spends
    // identities and must not pass this test.
    const redraws = () => world.pool.retunnels() + world.pool.rotates();
    const deadline = Date.now() + 5000;
    while (redraws() === 0 && Date.now() < deadline) await Bun.sleep(50);
    expect(world.pool.retunnels()).toBe(1);
    expect(world.pool.rotates()).toBe(0);
    const r2 = await chat(port, "conv-64-dead");
    // Whether the redraw has already re-read the fleet decides whether this
    // request can try the new exits (pool_busy) or finds the old ones still
    // parked (pool_empty); both are the honest wall, and the invariant here is
    // that no SECOND redraw happens.
    expectWall(r2);
    await Bun.sleep(300);
    expect(redraws()).toBe(1);                              // cooldown holds
    const st = await (await fetch(`http://127.0.0.1:${port}/admin/state?cached=1`)).json();
    expect(st.exhausted).toBe(true);
    world.upstream.burnAll(false);
  }, T);
});

describe.serial("session attribution", () => {
  // The real gate validates no ids: any well-formed `ses_` passes (measured
  // 2026-10-27: minted 8/8 200, real 3/3 200 on one egress). The probe exists
  // only for the pathological case — an id-specific 429 must not write off a
  // healthy egress for hours — so the fake gate refuses exactly one id.
  let dir, world, child, port, logFile;

  beforeAll(async () => {
    dir = tempDir("sids");
    world = startWorld({ retryAfter: "3600" });
    port = freePort();
    logFile = join(dir, "requests.jsonl");
    child = startSidecar({
      SIDECAR_PORT: String(port),
      SIDECAR_TARGET: `http://127.0.0.1:${world.upstream.port}`,
      SIDECAR_POOL_API: `http://127.0.0.1:${world.pool.port}`,
      SIDECAR_LOG: logFile,
      SIDECAR_STATE: join(dir, "v2_state.json"),
      SIDECAR_PINS: join(dir, "v2_pins.json"),
      SIDECAR_FP_STATE: join(dir, "v2_fp.json"),
      SIDECAR_IP_STATE: join(dir, "v2_ips.json"),
      // The probe keeps the conversation on one egress; fan-out would route the
      // next turn elsewhere and never check the rotated id.
      SIDECAR_PIN_FANOUT: "1",
    });
    await waitForSidecar(port);
  }, T);

  afterAll(async () => {
    try { child.kill(); } catch {}
    world?.stop();
    if (dir) cleanup(dir);
  });

  test("an id-specific 429 rotates the id, not the egress", async () => {
    world.upstream.refuseFirstOnce();
    const r = await chat(port, "conv-dead-sid-1");
    expect(r.status).toBe(200);                       // client never sees the 429
    const hits = world.upstream.hits;
    // Shape of the current build: 429, the same id under the alternate UA
    // (cheap check for a UA gate), then the fresh-id probe on the SAME egress.
    expect(hits.length).toBe(3);
    expect(hits[0].status).toBe(429);
    expect(hits[1].status).toBe(429);
    expect(hits[2].status).toBe(200);
    expect(hits[0].egress).toBe(hits[1].egress);
    expect(hits[2].egress).toBe(hits[0].egress);      // same egress, different id
    expect(hits[1].sid).toBe(hits[0].sid);            // the UA check keeps the id
    expect(hits[2].sid).not.toBe(hits[0].sid);
    expect(hits[0].sid).toMatch(SESSION_ID_RE);       // minted, not borrowed
    expect(hits[2].sid).toMatch(SESSION_ID_RE);
    expect(world.upstream.isRefused(hits[0].sid)).toBe(true);

    const rotated = readLog(logFile).filter((l) => l.event === "sid_rotated");
    expect(rotated.length).toBe(1);
    expect(rotated[0].sid).toBe(hits[0].sid);
    expect(rotated[0].alt).toBe(hits[2].sid);

    const st = await (await fetch(`http://127.0.0.1:${port}/admin/state?cached=1`)).json();
    expect(st.sid_mode).toBe("mint");
    const used = st.rows.find((row) => row.proxy.endsWith(":" + world.slots[hits[0].egress]));
    expect(used.banned_for_s).toBe(0);                // the IP was not written off
    expect(used.hard).toBe(0);
    expect(used.ok).toBeGreaterThanOrEqual(1);

    // The conversation keeps the rotated id: the next turn is one request.
    const before = world.upstream.hits.length;
    expect((await chat(port, "conv-dead-sid-1")).status).toBe(200);
    expect(world.upstream.hits.length).toBe(before + 1);
    expect(world.upstream.hits.slice(-1)[0].sid).toBe(hits[2].sid);
  }, T);

  test("concurrent 429s on one egress share a single id probe", async () => {
    // One live egress with its window spent and nothing to hop to: both
    // requests see the same 429 while the probe is in flight and must not
    // stack a second probe (measured live 2026-10-27 13:58: two probes 73 ms
    // apart for the same answer).
    world.pool.setDown(1);
    world.pool.setDown(2);
    world.upstream.burn(0);
    world.upstream.setLag(250);
    try {
      await fetch(`http://127.0.0.1:${port}/admin/state`);      // force a pool re-read
      const hitsBefore = world.upstream.hits.length;
      const logBefore = readLog(logFile).length;
      const [a, b] = await Promise.all([chat(port, "conv-shared-probe"), chat(port, "conv-shared-probe")]);
      expectWall(a, { long: true });                             // no live egress left
      expectWall(b, { long: true });
      const fresh = world.upstream.hits.slice(hitsBefore).filter((h) => h.egress === 0);
      // Two client requests, their two alternate-UA attempts (each 429 takes
      // that cheap check first) and ONE shared fresh-id probe: five upstream
      // calls, not two-per-request-plus-probe.
      expect(fresh.length).toBe(5);
      const log = readLog(logFile).slice(logBefore);
      expect(log.filter((l) => l.event === "sid_probe").length).toBe(1);
      expect(log.filter((l) => l.event === "sid_probe_skipped").length).toBe(1);
    } finally {
      world.upstream.setLag(0);
      world.upstream.unburn();
      world.pool.setDown(1, false);
      world.pool.setDown(2, false);
      await fetch(`http://127.0.0.1:${port}/admin/unpark`, { method: "POST" });
    }
  }, T);

  test("a closed egress is parked even though ids keep changing", async () => {
    world.upstream.burnAll(true);
    const r = await chat(port, "conv-closed-egress");
    expectWall(r, { long: true });
    const st = await (await fetch(`http://127.0.0.1:${port}/admin/state?cached=1`)).json();
    expect(st.rows.filter((row) => row.banned_for_s > 0).length).toBeGreaterThan(0);
    world.upstream.burnAll(false);
  }, T);
});

describe.serial("rotate drops its socket", () => {
  // warp-router respawns the whole fleet inside the rotate handler and its 30 s
  // WriteTimeout drops the client first (measured live 2026-10-28 12:24: the
  // "failure" was followed by backends coming up on new identities). Treating
  // that as a failed rotation left an already-parked fleet dead for the full
  // cooldown - the "recovery only after a manual restart" outage.
  let dir, world, child, port, logFile;

  beforeAll(async () => {
    dir = tempDir("rotate");
    world = startWorld({ retryAfter: "3600" });
    port = freePort();
    logFile = join(dir, "requests.jsonl");
    child = startSidecar({
      SIDECAR_PORT: String(port),
      SIDECAR_TARGET: `http://127.0.0.1:${world.upstream.port}`,
      SIDECAR_POOL_API: `http://127.0.0.1:${world.pool.port}`,
      SIDECAR_LOG: logFile,
      SIDECAR_STATE: join(dir, "v2_state.json"),
      SIDECAR_PINS: join(dir, "v2_pins.json"),
      SIDECAR_FP_STATE: join(dir, "v2_fp.json"),
      SIDECAR_IP_STATE: join(dir, "v2_ips.json"),
      SIDECAR_ALLOW_ROTATE: "1",
      SIDECAR_ROTATE_IDLE_MS: "0",
      SIDECAR_ROTATE_CHECK_MS: "60",
      SIDECAR_ROTATE_TIMEOUT_MS: "150",
      SIDECAR_ROTATE_COOLDOWN_MS: String(10 * 60_000),
    });
    await waitForSidecar(port);
  }, T);

  afterAll(async () => {
    try { child.kill(); } catch {}
    world?.stop();
    if (dir) cleanup(dir);
  });

  test("a rotation that changed nothing retries in 90 s, not in the whole cooldown", async () => {
    world.upstream.burnAll(true);
    world.pool.setRotate({ hang: true, fresh: false });
    // The cheap lever (retunnel) must be unavailable, otherwise the redraw
    // succeeds and never reaches the rotation whose "nothing changed" verdict
    // this test measures.
    world.pool.setRetunnelStatus(500);
    const r = await chat(port, "conv-rotate-norefresh");
    expectWall(r, { long: true });                               // nothing live, nothing invented
    await Bun.sleep(400);                                        // the follow-up discovery
    const rows = readLog(logFile);
    expect(rows.some((l) => l.event === "pool_rotate_inflight")).toBe(true);
    const nore = rows.filter((l) => l.event === "pool_rotate_norefresh").pop();
    expect(nore).toBeTruthy();
    expect(nore.retry_in_s).toBe(90);
    const st = await (await fetch(`http://127.0.0.1:${port}/admin/state?cached=1`)).json();
    expect(st.next_rotate_in_s).toBeLessThanOrEqual(90);
    expect(st.auto_rotations).toBe(1);                           // counted, not zeroed
    world.upstream.burnAll(false);
  });
});

describe.serial("rotate lands after dropping its socket", () => {
  let dir, world, child, port, logFile;

  beforeAll(async () => {
    dir = tempDir("rotate2");
    world = startWorld({ retryAfter: "3600" });
    port = freePort();
    logFile = join(dir, "requests.jsonl");
    child = startSidecar({
      SIDECAR_PORT: String(port),
      SIDECAR_TARGET: `http://127.0.0.1:${world.upstream.port}`,
      SIDECAR_POOL_API: `http://127.0.0.1:${world.pool.port}`,
      SIDECAR_LOG: logFile,
      SIDECAR_STATE: join(dir, "v2_state.json"),
      SIDECAR_PINS: join(dir, "v2_pins.json"),
      SIDECAR_FP_STATE: join(dir, "v2_fp.json"),
      SIDECAR_IP_STATE: join(dir, "v2_ips.json"),
      SIDECAR_ALLOW_ROTATE: "1",
      SIDECAR_ROTATE_IDLE_MS: "0",
      SIDECAR_ROTATE_CHECK_MS: "60",
      SIDECAR_ROTATE_TIMEOUT_MS: "150",
      SIDECAR_ROTATE_COOLDOWN_MS: String(4_000),
    });
    await waitForSidecar(port);
  }, T);

  afterAll(async () => {
    try { child.kill(); } catch {}
    world?.stop();
    if (dir) cleanup(dir);
  });

  test("a respawned fleet clears the fruitless-rotation ladder", async () => {
    world.upstream.burnAll(true);
    world.pool.setRotate({ hang: true, fresh: true });           // new identities, client dropped
    // A rotate is only attempted when the retunnel is refused: that is the
    // socket-drop path this test measures.
    world.pool.setRetunnelStatus(500);
    const r = await chat(port, "conv-rotate-land");
    expectWall(r, { long: true });
    await Bun.sleep(500);
    const land = readLog(logFile).filter((l) => l.event === "pool_rotate_landed").pop();
    expect(land).toBeTruthy();
    expect(land.fresh).toBeGreaterThan(0);
    expect(land.cleared).toBeGreaterThan(0);                       // parks died with the old IPs
    expect(land.parked).toBe(0);
    // Fresh addresses are not the same thing as serving ones: while nothing has
    // answered 200 the pool keeps retrying on the short ladder, never the full
    // cooldown (live 2026-10-28 13:23: 18 fresh exits, all 429, 10 min dark).
    let st = await (await fetch(`http://127.0.0.1:${port}/admin/state?cached=1`)).json();
    expect(st.auto_rotations).toBeGreaterThan(0);
    expect(st.next_rotate_in_s).toBeLessThanOrEqual(90);
    // A real 200 is what clears it — but only once the fleet has not been
    // redrawn for a full cooldown. Resetting on the trickle a redraw itself
    // creates kept the ladder on the 90 s rung forever (2026-11-04: 41
    // rotations in 2 h, median gap 112 s).
    world.upstream.burnAll(false);
    const ok = await chat(port, "conv-rotate-land");
    expect(ok.status).toBe(200);
    st = await (await fetch(`http://127.0.0.1:${port}/admin/state?cached=1`)).json();
    expect(st.auto_rotations).toBe(1);
    await Bun.sleep(4_200);                                      // SIDECAR_ROTATE_COOLDOWN_MS=4000
    const ok2 = await chat(port, "conv-rotate-land-2");
    expect(ok2.status).toBe(200);
    st = await (await fetch(`http://127.0.0.1:${port}/admin/state?cached=1`)).json();
    expect(st.auto_rotations).toBe(0);
  });
});

describe.serial("degraded pool", () => {
  // Rotation used to wait for the last live egress to fall over: measured
  // 2026-10-26 it rotated at 07:13 and then spent 13 h on a trickling pool.
  let dir, world, child, port, logFile;

  beforeAll(async () => {
    dir = tempDir("degraded");
    world = startWorld({ retryAfter: "3600" });
    port = freePort();
    logFile = join(dir, "requests.jsonl");
    child = startSidecar({
      SIDECAR_PORT: String(port),
      SIDECAR_TARGET: `http://127.0.0.1:${world.upstream.port}`,
      SIDECAR_POOL_API: `http://127.0.0.1:${world.pool.port}`,
      SIDECAR_LOG: logFile,
      SIDECAR_STATE: join(dir, "v2_state.json"),
      SIDECAR_PINS: join(dir, "v2_pins.json"),
      SIDECAR_FP_STATE: join(dir, "v2_fp.json"),
      SIDECAR_IP_STATE: join(dir, "v2_ips.json"),
      SIDECAR_ALLOW_ROTATE: "1",
      SIDECAR_ROTATE_IDLE_MS: "0",
      SIDECAR_DEGRADED_IDLE_MS: "80",
      // The retry must walk the burned pins, not fan out around them.
      SIDECAR_PIN_FANOUT: "1",
      // The forced parks end with an all-closed redraw, which stamps the same
      // lastRotate the gate checks. Zero cooldown keeps the gate observable in
      // one test; production keeps the 60 s that the redraw just spent.
      SIDECAR_ROTATE_COOLDOWN_MS: "0",
      // The redraw itself is a retunnel now: keep its own gate short so the
      // degraded trigger is observable in the same test.
      SIDECAR_RETUNNEL_COOLDOWN_MS: "0",
    });
    await waitForSidecar(port);
  }, T);

  afterAll(async () => {
    try { child.kill(); } catch {}
    world?.stop();
    if (dir) cleanup(dir);
  });

  test("most exits parked and a couple of quiet minutes redraw before total death", async () => {
    world.upstream.burn(0);
    world.upstream.burn(1);                                     // two of three exits are spent
    // Park both spent exits explicitly. The hop after a 429 picks among the
    // remaining egresses (shuffled), so walking 0 -> 1 by retries is a coin
    // flip: half the runs park only one exit and the 60% gate never fires.
    // The client sees 503 (a forced call has maxAttempts 1, so the 429 is
    // parked and the loop ends without a retry); the park is the observation.
    await chat(port, "conv-degraded-park0", { backend: world.slots[0] });
    await chat(port, "conv-degraded-park1", { backend: world.slots[1] });
    const parkedNow = (await (await fetch(`http://127.0.0.1:${port}/admin/state?cached=1`)).json())
      .rows.filter((r) => r.lane === "inference" && r.banned_for_s > 0).length;
    expect(parkedNow).toBeGreaterThanOrEqual(2);
    await Bun.sleep(120);                                       // idle window passes
    // The first move is a retunnel (new /64 on the same accounts), not a
    // rotate: a rotate spends a fresh identity and lands the same spent prefix.
    const before = world.pool.retunnels();
    const r = await chat(port, "conv-degraded-2");
    expect(r.status).toBe(200);                                 // the live exit still serves
    await Bun.sleep(150);
    expect(world.pool.retunnels()).toBeGreaterThan(before);
    const ev = readLog(logFile).find((l) => l.event === "pool_rotate_degraded");
    expect(ev).toBeTruthy();
    expect(ev.total).toBe(3);
    expect(ev.parked).toBeGreaterThanOrEqual(2);
  });
});

describe.serial("strike history is per address", () => {
  // A port that was long-parked and then got a NEW identity used to inherit
  // strikes>=2, so its first 429 parked it until the RA (midnight). Measured
  // 2026-10-28 13:23: rotate landed 18 fresh exits and the fleet died in minutes.
  let dir, world, child, port, logFile;

  beforeAll(async () => {
    dir = tempDir("strikes");
    world = startWorld({ retryAfter: "3600" });
    port = freePort();
    logFile = join(dir, "requests.jsonl");
    child = startSidecar({
      SIDECAR_PORT: String(port),
      SIDECAR_TARGET: `http://127.0.0.1:${world.upstream.port}`,
      SIDECAR_POOL_API: `http://127.0.0.1:${world.pool.port}`,
      SIDECAR_LOG: logFile,
      SIDECAR_STATE: join(dir, "v2_state.json"),
      SIDECAR_PINS: join(dir, "v2_pins.json"),
      SIDECAR_FP_STATE: join(dir, "v2_fp.json"),
      SIDECAR_IP_STATE: join(dir, "v2_ips.json"),
      SIDECAR_SHORT_PARK_MS: "3000",                             // first strike must expire inside the test
    });
    await waitForSidecar(port);
  }, T);

  afterAll(async () => {
    try { child.kill(); } catch {}
    world?.stop();
    if (dir) cleanup(dir);
  });

  const rowFor = async (ip) => {
    const st = await (await fetch(`http://127.0.0.1:${port}/admin/state`)).json();
    return st.rows.find((r) => r.lane === "inference" && r.ip === ip);
  };

  test("a fresh identity starts with a clean strike history", async () => {
    // Only one exit is up, so every attempt lands on the same port. The pool
    // list is cached for REFRESH_MS, so force the re-read: without it the
    // request still routes to the slots the pool just reported down.
    world.pool.setDown(1);
    world.pool.setDown(2);
    await fetch(`http://127.0.0.1:${port}/admin/state`);
    // Two hours-class 429s on the same port inside the strike window: the second
    // one is a long park, which is correct for THAT address.
    world.upstream.burn(0);
    await chat(port, "conv-strike-1");
    await Bun.sleep(3200);                                       // the first-strike park expires
    await chat(port, "conv-strike-2");
    const oldIp = "2a09:bad0:0:1::1";
    const before = await rowFor(oldIp);
    expect(before.hard).toBe(1);                                 // strikes>=2 => long park
    expect(before.banned_for_s).toBeGreaterThan(600);

    // Same port, new address: the verdict must die with the old identity.
    world.pool.setIp(0, "2a09:bad9:1:1::1");
    const fresh = await rowFor("2a09:bad9:1:1::1");
    expect(fresh).toBeTruthy();
    expect(fresh.banned_for_s).toBe(0);
    expect(fresh.strikes).toBe(0);

    // Its first 429 is a first strike: a short park, not the rest of the day.
    await chat(port, "conv-strike-3");
    const after = await rowFor("2a09:bad9:1:1::1");
    expect(after.banned_for_s).toBeGreaterThan(0);
    expect(after.banned_for_s).toBeLessThanOrEqual(150);         // SHORT_PARK_MS, not ~3600
    world.upstream.unburn();
    world.pool.setDown(1, false);
    world.pool.setDown(2, false);
  });
});

// The egress is fixed per WARP account, so a rotation on an unchanged uplink
// can hand back exits whose per-address budget was already spent today. That is
// not a reason to keep rotating: only a new device mints an unseen egress.
// Measured 2026-10-28: with the fleet fully 429, a device registered at 15:21
// egressed 2a09:bac1:5500:10::14:2ab, an address the burn book had never seen.
describe.serial("mint: rotation that only re-serves burned exits registers devices", () => {
  let dir, world, child, port, logFile;

  beforeAll(async () => {
    dir = tempDir("mint");
    world = startWorld({ retryAfter: "3600" });
    port = freePort();
    logFile = join(dir, "requests.jsonl");
    // The exits a rotate would hand back are already inside a closed window:
    // "spent" is a LIVE deadline (untilByLane), not the historical burn flag.
    const until = Date.now() + 3600_000;
    const burned = {};
    for (let i = 0; i < 3; i++) burned[`2a09:bad0:${i}:91::${i + 1}`] = { first: 1, last: 2, seen: 40, burned: 1, reuse: 0, untilByLane: { inference: until } };
    await Bun.write(join(dir, "v2_ips.json"), JSON.stringify(burned));
    child = startSidecar({
      SIDECAR_PORT: String(port),
      SIDECAR_TARGET: `http://127.0.0.1:${world.upstream.port}`,
      SIDECAR_POOL_API: `http://127.0.0.1:${world.pool.port}`,
      SIDECAR_LOG: logFile,
      SIDECAR_STATE: join(dir, "v2_state.json"),
      SIDECAR_PINS: join(dir, "v2_pins.json"),
      SIDECAR_FP_STATE: join(dir, "v2_fp.json"),
      SIDECAR_IP_STATE: join(dir, "v2_ips.json"),
      SIDECAR_ALLOW_ROTATE: "1",
      SIDECAR_ROTATE_IDLE_MS: "0",
      SIDECAR_ROTATE_CHECK_MS: "60",
      SIDECAR_ROTATE_TIMEOUT_MS: "150",
      SIDECAR_ROTATE_COOLDOWN_MS: String(10 * 60_000),
      SIDECAR_MINT_COOLDOWN_MS: "0",
      SIDECAR_MINT_COUNT: "3",
    });
    await waitForSidecar(port);
  }, T);

  afterAll(async () => {
    try { child.kill(); } catch {}
    world?.stop();
    if (dir) cleanup(dir);
  });

  // Capacity is slots x budget. When a redraw lands the same spent prefixes the
// fleet is too small, not mis-tunnelled, so the refill must widen the pool
// (warp-router spawns the extra workers from accounts it already holds) instead
// of burning registrations.
describe.serial("growth: a spent fleet that a redraw cannot save widens the pool", () => {
  let dir, world, child, port, logFile;

  beforeAll(async () => {
    dir = tempDir("grow");
    world = startWorld({ retryAfter: "3600" });
    port = freePort();
    logFile = join(dir, "requests.jsonl");
    // The held fleet is spent RIGHT NOW: spentShare reads the live per-lane
    // deadline, so the seed carries one (the historical burn flag is only an
    // observation and no longer triggers a refill).
    const until = Date.now() + 3600_000;
    const book = {};
    for (let i = 0; i < 3; i++) book[`2a09:bad0:${i}:1::${i + 1}`] = { first: 1, last: 2, seen: 40, burned: 1, reuse: 0, untilByLane: { inference: until } };
    await Bun.write(join(dir, "v2_ips.json"), JSON.stringify(book));
    child = startSidecar({
      SIDECAR_PORT: String(port),
      SIDECAR_TARGET: `http://127.0.0.1:${world.upstream.port}`,
      SIDECAR_POOL_API: `http://127.0.0.1:${world.pool.port}`,
      SIDECAR_LOG: logFile,
      SIDECAR_STATE: join(dir, "v2_state.json"),
      SIDECAR_PINS: join(dir, "v2_pins.json"),
      SIDECAR_FP_STATE: join(dir, "v2_fp.json"),
      SIDECAR_IP_STATE: join(dir, "v2_ips.json"),
      SIDECAR_ALLOW_ROTATE: "1",
      SIDECAR_ROTATE_IDLE_MS: "0",
      SIDECAR_ROTATE_CHECK_MS: "60",
      SIDECAR_ROTATE_TIMEOUT_MS: "150",
      SIDECAR_ROTATE_COOLDOWN_MS: String(10 * 60_000),
      SIDECAR_MINT_COOLDOWN_MS: "0",
      SIDECAR_POOL_STEP: "6",
    });
    await waitForSidecar(port);
  }, T);

  afterAll(async () => {
    try { child.kill(); } catch {}
    world?.stop();
    if (dir) cleanup(dir);
  });

  test("a redraw onto spent prefixes grows the pool instead of minting", async () => {
    world.upstream.burnAll(true);
    world.pool.setRotate({ hang: true, fresh: false });           // the redraw lands the same set
    world.pool.setRetunnelSame(true);
    const r = await chat(port, "conv-grow");
    expectWall(r, { long: true });
    await Bun.sleep(2_800);                                      // follow-up -> retunnel -> grow (1.5 s off/on)
    const rows = readLog(logFile);
    // The ladder's promise: a redraw that cannot save the fleet widens it.
    // Registering devices here burns identities the pool did not need.
    expect(world.pool.registers().length).toBe(0);
    expect(rows.some((l) => l.event === "pool_grow")).toBe(true);
    const grow = rows.filter((l) => l.event === "pool_grow").pop();
    expect(grow.to).toBeGreaterThan(grow.from);                   // widened, not merely re-spread
    expect(world.pool.poolSizes().pop()).toBe(grow.to);
    world.upstream.burnAll(false);
  }, 30_000);
});

// Measured 2026-10-28: minting 10 devices produced 16 exits the pool had
  // never held and every one of them answered 429, while a retunnel (same
  // accounts, new handshakes) moved the fleet to 18 unique /64 with no
  // registrations at all. The egress slot is drawn by the handshake, so the
  // first move on a spent fleet must be a retunnel, not a registration job.
  test("a spent fleet retunnels instead of burning accounts", async () => {
    world.upstream.burnAll(true);
    world.pool.setRotate({ hang: true, fresh: true });           // rotation would only re-serve spent exits
    const r = await chat(port, "conv-mint");
    expectWall(r, { long: true });
    await Bun.sleep(700);
    const rows = readLog(logFile);
    // The retunnel is the first lever and it succeeded, so no refill was due and
    // the whole-pool rotate (which would spend identities) never ran.
    expect(rows.some((l) => l.event === "pool_retunnel" && l.status === 200)).toBe(true);
    expect(world.pool.retunnels()).toBeGreaterThanOrEqual(1);
    expect(world.pool.rotates()).toBe(0);
    expect(world.pool.registers().length).toBe(0);               // no identity burned
    world.upstream.burnAll(false);
  });
});

// The registration cap is a hard wall (warp-router maxAccounts=200): once the
// pool is full, minting has to retire the oldest devices first, otherwise the
// lever dies after a few mints and the fleet is back to cycling spent exits.
describe.serial("mint: registration at the account cap retires the oldest devices", () => {
  let dir, world, child, port, logFile;

  beforeAll(async () => {
    dir = tempDir("mint-cap");
    world = startWorld({ retryAfter: "3600" });
    world.pool.seedAccounts(3);
    world.pool.setAccountCap(4);                                  // room for one mint of 3 only after a prune
    port = freePort();
    logFile = join(dir, "requests.jsonl");
    // The rotate lands these addresses: they must be spent RIGHT NOW, which is
    // what untilByLane encodes (the burn flag alone is only history).
    const until = Date.now() + 3600_000;
    const burned = {};
    for (let i = 0; i < 3; i++) burned[`2a09:bad0:${i}:91::${i + 1}`] = { first: 1, last: 2, seen: 40, burned: 1, reuse: 0, untilByLane: { inference: until } };
    await Bun.write(join(dir, "v2_ips.json"), JSON.stringify(burned));
    child = startSidecar({
      SIDECAR_PORT: String(port),
      SIDECAR_TARGET: `http://127.0.0.1:${world.upstream.port}`,
      SIDECAR_POOL_API: `http://127.0.0.1:${world.pool.port}`,
      SIDECAR_LOG: logFile,
      SIDECAR_STATE: join(dir, "v2_state.json"),
      SIDECAR_PINS: join(dir, "v2_pins.json"),
      SIDECAR_FP_STATE: join(dir, "v2_fp.json"),
      SIDECAR_IP_STATE: join(dir, "v2_ips.json"),
      SIDECAR_ALLOW_ROTATE: "1",
      SIDECAR_ROTATE_IDLE_MS: "0",
      SIDECAR_ROTATE_CHECK_MS: "60",
      SIDECAR_ROTATE_TIMEOUT_MS: "150",
      SIDECAR_ROTATE_COOLDOWN_MS: String(10 * 60_000),
      SIDECAR_MINT_COOLDOWN_MS: "0",
      SIDECAR_MINT_COUNT: "3",
    });
    await waitForSidecar(port);
  }, T);

  afterAll(async () => {
    try { child.kill(); } catch {}
    world?.stop();
    if (dir) cleanup(dir);
  });

  test("the refused registration prunes and then succeeds", async () => {
    world.upstream.burnAll(true);
    world.pool.setRetunnelStatus(500);                            // mint is the fallback, so exercise it
    world.pool.setRotate({ hang: true, fresh: true });
    const r = await chat(port, "conv-mint-cap");
    expectWall(r, { long: true });
    await Bun.sleep(900);
    const rows = readLog(logFile);
    expect(rows.some((l) => l.event === "mint_only_spent")).toBe(true);
    const prune = rows.filter((l) => l.event === "accounts_prune").pop();
    expect(prune).toBeTruthy();
    expect(prune.freed).toBe(3);                                  // the three oldest
    expect(rows.some((l) => l.event === "accounts_mint" && l.status === 202)).toBe(true);
    expect(world.pool.registers().length).toBe(1);
    expect(world.pool.accounts().length).toBe(3);                  // 0 after prune + 3 new
    world.upstream.burnAll(false);
  });
});

// /admin/rotate-fresh is the operator's "give me unseen exits" button. On a
// fixed uplink the pool can run out of unseen accounts; the first move is a
// retunnel (new handshakes redraw the slots on the accounts already held) and a
// registration job is only the fallback when the pool refuses it.
describe.serial("rotate-fresh with no unseen exits retunnels", () => {
  let dir, world, child, port, logFile;

  beforeAll(async () => {
    dir = tempDir("mint-fresh");
    world = startWorld({ retryAfter: "3600" });
    port = freePort();
    logFile = join(dir, "requests.jsonl");
    // Every address the pool can cycle is already known: the strict "unseen"
    // check has nothing to offer, which is exactly the dead-end case.
    const book = {};
    for (let i = 0; i < 3; i++) book[`2a09:bad0:${i}:1::${i + 1}`] = { first: 1, last: 2, seen: 5, burned: 0, reuse: 0 };
    await Bun.write(join(dir, "v2_ips.json"), JSON.stringify(book));
    child = startSidecar({
      SIDECAR_PORT: String(port),
      SIDECAR_TARGET: `http://127.0.0.1:${world.upstream.port}`,
      SIDECAR_POOL_API: `http://127.0.0.1:${world.pool.port}`,
      SIDECAR_LOG: logFile,
      SIDECAR_STATE: join(dir, "v2_state.json"),
      SIDECAR_PINS: join(dir, "v2_pins.json"),
      SIDECAR_FP_STATE: join(dir, "v2_fp.json"),
      SIDECAR_IP_STATE: join(dir, "v2_ips.json"),
      SIDECAR_ALLOW_ROTATE: "1",
      SIDECAR_ROTATE_CHECK_MS: "60",
      SIDECAR_ROTATE_TIMEOUT_MS: "150",
      SIDECAR_MINT_COOLDOWN_MS: "0",
      SIDECAR_MINT_COUNT: "2",
    });
    await waitForSidecar(port);
  }, T);

  afterAll(async () => {
    try { child.kill(); } catch {}
    world?.stop();
    if (dir) cleanup(dir);
  });

  test("a zero-fresh rotate-fresh retunnels instead of registering devices", async () => {
    world.pool.setRotate({ hang: false, fresh: false });          // same identities back
    const r = await fetch(`http://127.0.0.1:${port}/admin/rotate-fresh`, { method: "POST" });
    const body = await r.json();
    expect(body.fresh).toBe(0);
    expect(body.retunnel).toBe(true);
    expect(body.minting).toBe(false);
    await Bun.sleep(500);
    expect(world.pool.retunnels()).toBeGreaterThanOrEqual(1);
    expect(world.pool.registers().length).toBe(0);
    const rows = readLog(logFile);
    expect(rows.some((l) => l.event === "pool_retunnel" && l.status === 200)).toBe(true);
  }, 30_000);

  test("a refused retunnel falls back to registering devices", async () => {
    world.pool.setRetunnelStatus(500);
    const r = await fetch(`http://127.0.0.1:${port}/admin/rotate-fresh`, { method: "POST" });
    const body = await r.json();
    expect(body.retunnel).toBe(false);
    expect(body.minting).toBe(true);
    await Bun.sleep(500);
    const regs = world.pool.registers();
    expect(regs.length).toBe(1);
    expect(regs[0].count).toBe(2);
  }, 30_000);
});

// A rotation that changes nothing is the same dead end as a rotation that only
// re-serves burned exits. Measured 2026-10-28 16:10-16:20Z (VPN off): 297 of 297
// 429s came from already-burned addresses, 7 rotations produced zero new ones,
// and the refill never fired because it was gated on fresh > 0.
describe.serial("mint: a rotation with no new exits still triggers the refill", () => {
  let dir, world, child, port, logFile;

  beforeAll(async () => {
    dir = tempDir("mint-norefresh");
    world = startWorld({ retryAfter: "3600" });
    port = freePort();
    logFile = join(dir, "requests.jsonl");
    // The whole held set is inside a closed window: spentShare reads the live
    // deadline per lane, so the seed must carry one.
    const until = Date.now() + 3600_000;
    const burned = {};
    for (let i = 0; i < 3; i++) burned[`2a09:bad0:${i}:1::${i + 1}`] = { first: 1, last: 2, seen: 90, burned: 1, reuse: 0, untilByLane: { inference: until } };
    await Bun.write(join(dir, "v2_ips.json"), JSON.stringify(burned));
    child = startSidecar({
      SIDECAR_PORT: String(port),
      SIDECAR_TARGET: `http://127.0.0.1:${world.upstream.port}`,
      SIDECAR_POOL_API: `http://127.0.0.1:${world.pool.port}`,
      SIDECAR_LOG: logFile,
      SIDECAR_STATE: join(dir, "v2_state.json"),
      SIDECAR_PINS: join(dir, "v2_pins.json"),
      SIDECAR_FP_STATE: join(dir, "v2_fp.json"),
      SIDECAR_IP_STATE: join(dir, "v2_ips.json"),
      SIDECAR_ALLOW_ROTATE: "1",
      SIDECAR_ROTATE_IDLE_MS: "0",
      SIDECAR_ROTATE_CHECK_MS: "60",
      SIDECAR_ROTATE_TIMEOUT_MS: "150",
      SIDECAR_ROTATE_COOLDOWN_MS: String(10 * 60_000),
      SIDECAR_MINT_COOLDOWN_MS: "0",
      SIDECAR_MINT_COUNT: "4",
    });
    await waitForSidecar(port);
  }, T);

  afterAll(async () => {
    try { child.kill(); } catch {}
    world?.stop();
    if (dir) cleanup(dir);
  });

  test("norefresh + all exits burned registers devices", async () => {
    world.upstream.burnAll(true);
    world.pool.setRetunnelStatus(500);                            // the cheap lever is unavailable
    world.pool.setRotate({ hang: true, fresh: false });           // same identities back
    const r = await chat(port, "conv-mint-norefresh");
    expectWall(r, { long: true });
    await Bun.sleep(600);
    const rows = readLog(logFile);
    const nore = rows.filter((l) => l.event === "pool_rotate_norefresh").pop();
    expect(nore).toBeTruthy();
    expect(nore.spent).toBeGreaterThanOrEqual(0.6);
    expect(rows.some((l) => l.event === "mint_only_spent")).toBe(true);
    const regs = world.pool.registers();
    expect(regs.length).toBe(1);
    expect(regs[0].count).toBe(4);
    world.upstream.burnAll(false);
  });
});

// The same fleet serves 200 through one uplink and 429s everything through
// another: measured 2026-10-28 on the home path - median retry-after 25 065 s,
// 172 of 172 sampled 429s on already-burned exits, and 16 freshly minted exits
// all 429, while ProtonVPN served 200s on the same pool. Rotating, minting and
// respreading cannot move a decision made above the egress, so the sidecar must
// stop churning once the whole fleet answers with an hours-class retry-after.
// Measured again 2026-11-04 20:37-20:49 on /inference: retunnel fresh 19-20 /
// spent 0.63-0.70, grow + 6 registrations, redraw_sterile at 451 s without a
// 200; the same retunnel after the uplink change gave fresh 25 / spent 0.04.
describe.serial("path lock: an hours-class 429 for the whole fleet stops the churn", () => {
  let dir, world, child, port, logFile;

  beforeAll(async () => {
    dir = tempDir("pathlock");
    // 3 h 3 min: the measured class on 2026-11-04 21:49 (10 286-11 592 s). The
    // old 4 h threshold let exactly this wall through, and the ladder rotated
    // every 90 s for it.
    world = startWorld({ retryAfter: "11000" });
    port = freePort();
    logFile = join(dir, "requests.jsonl");
    child = startSidecar({
      SIDECAR_PORT: String(port),
      SIDECAR_TARGET: `http://127.0.0.1:${world.upstream.port}`,
      SIDECAR_POOL_API: `http://127.0.0.1:${world.pool.port}`,
      SIDECAR_LOG: logFile,
      SIDECAR_STATE: join(dir, "v2_state.json"),
      SIDECAR_PINS: join(dir, "v2_pins.json"),
      SIDECAR_FP_STATE: join(dir, "v2_fp.json"),
      SIDECAR_IP_STATE: join(dir, "v2_ips.json"),
      SIDECAR_ALLOW_ROTATE: "1",
      SIDECAR_ROTATE_IDLE_MS: "0",
      SIDECAR_ROTATE_CHECK_MS: "60",
      SIDECAR_ROTATE_TIMEOUT_MS: "150",
      // No cooldown: the churn assertion below must be gated by the PATH LOCK
      // alone, not by "a rotation happened 300 ms ago".
      SIDECAR_ROTATE_COOLDOWN_MS: "0",
      SIDECAR_MINT_COOLDOWN_MS: "0",
      SIDECAR_MINT_COUNT: "2",
      SIDECAR_PATH_LOCK_MIN_EGRESSES: "3",                          // the fake pool has 3 exits; production default is 4 of 20
    });
    await waitForSidecar(port);
  }, T);

  afterAll(async () => {
    try { child.kill(); } catch {}
    world?.stop();
    if (dir) cleanup(dir);
  });

  test("one dead window is logged, then the inference lane's churn is gated too", async () => {
    // The redraw must keep the same addresses: the gated request below needs
    // the fleet still parked, otherwise it takes the all-closed path, which is
    // the serve-protecting escape the lock deliberately leaves open.
    world.pool.setRetunnelSame(true);
    world.upstream.burnAll(true);
    // The wall is the same on both lanes, so the lock must be measured where the
    // traffic is: /inference. Gating only zen left this window redrawing the
    // fleet every 90 s for twelve minutes with nothing to show for it.
    const r1 = await chat(port, "conv-lock-1");
    expectWall(r1, { long: true });
    const all = readLog(logFile);
    const rows = all.filter((l) => l.event === "path_locked");
    expect(rows.length).toBe(1);
    expect(rows[0].ra_s).toBeGreaterThanOrEqual(3600);            // hours-class, not 4 h specifically
    expect(rows[0].lane).toBe("inference");
    expect(rows[0].gates).toBe(true);
    const st = await (await fetch(`http://127.0.0.1:${port}/admin/state?cached=1`)).json();
    expect(st.path_lock_s).toBeGreaterThan(0);
    // With the rotate cooldown at 0 and the egresses still parked, only the
    // lock can stop this request from redrawing: a redraw here would spend
    // accounts for a decision made above the egress.
    const redraws = () => world.pool.retunnels() + world.pool.rotates();
    const before = redraws();
    const regBefore = world.pool.registers().length;
    const again = await chat(port, "conv-lock-2");
    // Nothing could be tried (the rows are parked), so this is the
    // pool_empty shape; the assertion that matters is the churn below.
    expectWall(again);
    await Bun.sleep(300);
    expect(redraws()).toBe(before);                                // the lock stopped the churn
    expect(world.pool.registers().length).toBe(regBefore);
    // A 200 proves the path is back: the lock must die with it. Parks are our
    // own opinion, so drop them first - the lock is about the upstream path.
    await fetch(`http://127.0.0.1:${port}/admin/unpark`, { method: "POST" });
    world.upstream.burnAll(false);
    const ok = await chat(port, "conv-lock-3");
    expect(ok.status).toBe(200);
    const st2 = await (await fetch(`http://127.0.0.1:${port}/admin/state?cached=1`)).json();
    expect(st2.path_lock_s).toBe(0);
  });
});

// Measured 2026-10-28: switching the host to ProtonVPN did not produce a single
// 200 until warp-router was restarted with the VPN up. A WARP tunnel keeps the
// path it was built on and the pool only re-reads endpoints at boot, so an
// uplink change has to be followed by a pool restart. Watch the host's own
// egress (trace) and do exactly that instead of waiting for a human.
describe.serial("uplink change restarts the pool and drops the path lock", () => {
  let dir, world, child, port, logFile, trace, traceIp;

  beforeAll(async () => {
    dir = tempDir("uplink");
    traceIp = "198.51.100.9";
    trace = Bun.serve({
      port: 0,
      fetch(req) {
        if (new URL(req.url).pathname !== "/cdn-cgi/trace") return new Response("nope", { status: 404 });
        return new Response(`fl=test\nip=${traceIp}\ncolo=XXX\nwarp=off\n`);
      },
    });
    world = startWorld({ retryAfter: "25000" });
    port = freePort();
    logFile = join(dir, "requests.jsonl");
    child = startSidecar({
      SIDECAR_PORT: String(port),
      SIDECAR_TARGET: `http://127.0.0.1:${world.upstream.port}`,
      SIDECAR_POOL_API: `http://127.0.0.1:${world.pool.port}`,
      SIDECAR_LOG: logFile,
      SIDECAR_STATE: join(dir, "v2_state.json"),
      SIDECAR_PINS: join(dir, "v2_pins.json"),
      SIDECAR_FP_STATE: join(dir, "v2_fp.json"),
      SIDECAR_IP_STATE: join(dir, "v2_ips.json"),
      SIDECAR_UPLINK_STATE: join(dir, "v2_uplink.json"),
      SIDECAR_TRACE_URL: `http://127.0.0.1:${trace.port}/cdn-cgi/trace`,
      SIDECAR_UPLINK_CHECK_MS: "120",
      SIDECAR_UPLINK_RESTART_MIN_MS: "0",
      SIDECAR_PATH_LOCK_MIN_EGRESSES: "3",
      SIDECAR_ALLOW_ROTATE: "0",
    });
    await waitForSidecar(port);
  }, T);

  afterAll(async () => {
    try { child.kill(); } catch {}
    world?.stop();
    trace?.stop(true);
    if (dir) cleanup(dir);
  });

  test("the new host egress clears the lock and rebuilds the fleet", async () => {
    world.upstream.burnAll(true);
    const r = await chat(port, "conv-uplink-1");
    expectWall(r, { long: true });
    let st = await (await fetch(`http://127.0.0.1:${port}/admin/state?cached=1`)).json();
    // The lock is clamped to the daily reset, so its remaining time is hours
    // except in the last hour before 00:00Z: assert it exists, not its length.
    expect(st.path_lock_s).toBeGreaterThan(0);
    const seenBy = Date.now() + 6_000;                              // the first trace tick lands 1.5 s after boot
    while (!st.uplink?.ip && Date.now() < seenBy) {
      await Bun.sleep(150);
      st = await (await fetch(`http://127.0.0.1:${port}/admin/state?cached=1`)).json();
    }
    expect(st.uplink?.ip).toBe("198.51.100.9");
    traceIp = "203.0.113.7";                                        // the VPN came up
    const deadline = Date.now() + 20_000;
    let redrawn = false;
    while (Date.now() < deadline) {
      const all = readLog(logFile);
      // An uplink change redraws the fleet on the accounts it already holds:
      // the event is pool_retunnel, not a full pool restart (which would drop
      // the daemon's tunnels and re-register devices).
      if (all.some((l) => l.event === "pool_retunnel")) { redrawn = true; break; }
      await Bun.sleep(200);
    }
    expect(redrawn).toBe(true);
    // An uplink change only needs new handshakes: the colo follows the path and
    // the slot is drawn per handshake, so the fleet retunnels on the accounts it
    // already holds instead of dropping and re-registering.
    expect(world.pool.retunnels()).toBeGreaterThanOrEqual(1);
    expect(readLog(logFile).some((l) => l.event === "pool_retunnel")).toBe(true);
    st = await (await fetch(`http://127.0.0.1:${port}/admin/state?cached=1`)).json();
    expect(st.path_lock_s).toBe(0);                                 // the lock described the old path
    expect(st.uplink.ip).toBe("203.0.113.7");
  }, 40_000);
});

// Measured 2026-11-04 21:30: the uplink moved WAW -> EWR while the retunnel
// cooldown was running, uplinkTick consumed the event and returned, and the
// fleet kept old-path addresses - 25 client 503s in six minutes. The skipped
// redraw must be remembered (persisted: the v2.74 deploy restarted the sidecar
// between the change and any retry) and executed when the gates expire.
describe.serial("an uplink change inside the cooldowns is deferred, not dropped", () => {
  let dir, world, child, port, logFile, trace, traceIp;

  beforeAll(async () => {
    dir = tempDir("uplinkdefer");
    traceIp = "198.51.100.11";
    trace = Bun.serve({
      port: 0,
      fetch(req) {
        if (new URL(req.url).pathname !== "/cdn-cgi/trace") return new Response("nope", { status: 404 });
        return new Response(`fl=test\nip=${traceIp}\ncolo=XXX\nwarp=off\n`);
      },
    });
    world = startWorld({ retryAfter: "25000" });
    port = freePort();
    logFile = join(dir, "requests.jsonl");
    child = startSidecar({
      SIDECAR_PORT: String(port),
      SIDECAR_TARGET: `http://127.0.0.1:${world.upstream.port}`,
      SIDECAR_POOL_API: `http://127.0.0.1:${world.pool.port}`,
      SIDECAR_LOG: logFile,
      SIDECAR_STATE: join(dir, "v2_state.json"),
      SIDECAR_PINS: join(dir, "v2_pins.json"),
      SIDECAR_FP_STATE: join(dir, "v2_fp.json"),
      SIDECAR_IP_STATE: join(dir, "v2_ips.json"),
      SIDECAR_UPLINK_STATE: join(dir, "v2_uplink.json"),
      SIDECAR_TRACE_URL: `http://127.0.0.1:${trace.port}/cdn-cgi/trace`,
      SIDECAR_UPLINK_CHECK_MS: "100",
      SIDECAR_UPLINK_RESTART_MIN_MS: "0",
      SIDECAR_RETUNNEL_COOLDOWN_MS: "4000",
      SIDECAR_PATH_LOCK_MIN_EGRESSES: "3",
      SIDECAR_ALLOW_ROTATE: "1",
      SIDECAR_ROTATE_IDLE_MS: "0",
    });
    await waitForSidecar(port);
  }, T);

  afterAll(async () => {
    try { child.kill(); } catch {}
    world?.stop();
    trace?.stop(true);
    if (dir) cleanup(dir);
  });

  test("the skipped change is remembered and redrawn once the cooldown expires", async () => {
    world.upstream.burnAll(true);
    // Wait for the first trace so the next IP is a CHANGE, not the first sighting.
    let st = await (await fetch(`http://127.0.0.1:${port}/admin/state?cached=1`)).json();
    const seenBy = Date.now() + 6_000;
    while (!st.uplink?.ip && Date.now() < seenBy) {
      await Bun.sleep(120);
      st = await (await fetch(`http://127.0.0.1:${port}/admin/state?cached=1`)).json();
    }
    expect(st.uplink?.ip).toBe("198.51.100.11");
    // A first change buys the retunnel and starts its 4 s cooldown.
    traceIp = "203.0.113.21";
    const t1 = Date.now() + 8_000;
    while (world.pool.retunnels() < 1 && Date.now() < t1) await Bun.sleep(100);
    expect(world.pool.retunnels()).toBeGreaterThanOrEqual(1);
    // A second change inside the cooldown: the redraw is skipped and must be
    // remembered in the uplink file (a process restart must not lose it).
    traceIp = "203.0.113.22";
    const t2 = Date.now() + 5_000;
    while (!readLog(logFile).some((l) => l.event === "uplink_redraw_recent") && Date.now() < t2) await Bun.sleep(100);
    expect(readLog(logFile).some((l) => l.event === "uplink_redraw_recent")).toBe(true);
    const persisted = JSON.parse(await Bun.file(join(dir, "v2_uplink.json")).text());
    expect(persisted.pendingFor).toBe("203.0.113.22");
    // The fleet is still on the old path, so a request re-earns the lock; the
    // deferred redraw must drop it (it described the old path) and redraw.
    const wall = await chat(port, "conv-defer-1");
    expectWall(wall, { long: true });
    st = await (await fetch(`http://127.0.0.1:${port}/admin/state?cached=1`)).json();
    expect(st.path_lock_s).toBeGreaterThan(0);
    const t3 = Date.now() + 10_000;
    while (!readLog(logFile).some((l) => l.event === "uplink_redraw_deferred") && Date.now() < t3) await Bun.sleep(150);
    expect(readLog(logFile).some((l) => l.event === "uplink_redraw_deferred")).toBe(true);
    expect(world.pool.retunnels()).toBeGreaterThanOrEqual(2);
    st = await (await fetch(`http://127.0.0.1:${port}/admin/state?cached=1`)).json();
    expect(st.path_lock_s).toBe(0);                                 // the old-path verdict died with the redraw
    const after = JSON.parse(await Bun.file(join(dir, "v2_uplink.json")).text());
    expect(after.pendingFor).toBe("");
    expect(after.ip).toBe("203.0.113.22");
  }, 60_000);
});

// The learned window is ~180 requests per address (measured 2026-11-04: 64 burn
// samples, median 179). The sidecar must redraw the fleet BEFORE the last
// addresses 429, not after: a retunnel is one ~35 s redraw on the same accounts,
// while waiting for the wall costs the client six failed attempts and a 503.
describe.serial("the budget gate redraws before the window runs out", () => {
  let dir, world, child, port, logFile;

  beforeAll(async () => {
    dir = tempDir("budget");
    world = startWorld();
    port = freePort();
    logFile = join(dir, "requests.jsonl");
    // Seed the book with the pair the sidecar will discover (proxy + ip): a row
    // whose address differs is a different identity and resets its counters.
    const rows = (await (await fetch(`http://127.0.0.1:${world.pool.port}/api/v1/pool`)).json()).backends;
    const egress = {};
    for (const b of rows) {
      const p = String(b.http_addr).split(":").pop();
      egress[`http://127.0.0.1:${p}|inference`] = {
        proxy: `http://127.0.0.1:${p}`, lane: "inference", ip: b.public_ip,
        ok: 195, bad: 0, until: 0, lastOK: Date.now(), hard: 0, strikes: 0,
      };
    }
    await Bun.write(join(dir, "v2_state.json"), JSON.stringify({
      ts: new Date().toISOString(), egress, samples: [210], burn64: {},
    }));
    child = startSidecar({
      SIDECAR_PORT: String(port),
      SIDECAR_TARGET: `http://127.0.0.1:${world.upstream.port}`,
      SIDECAR_POOL_API: `http://127.0.0.1:${world.pool.port}`,
      SIDECAR_LOG: logFile,
      SIDECAR_STATE: join(dir, "v2_state.json"),
      SIDECAR_PINS: join(dir, "v2_pins.json"),
      SIDECAR_FP_STATE: join(dir, "v2_fp.json"),
      SIDECAR_IP_STATE: join(dir, "v2_ips.json"),
      SIDECAR_ALLOW_ROTATE: "1",
      SIDECAR_ROTATE_IDLE_MS: "0",
      SIDECAR_RETUNNEL_COOLDOWN_MS: "0",
      SIDECAR_ROTATE_COOLDOWN_MS: "0",
      SIDECAR_MINT_COOLDOWN_MS: "0",
    });
    await waitForSidecar(port);
  }, T);

  afterAll(async () => {
    try { child.kill(); } catch {}
    world?.stop();
    if (dir) cleanup(dir);
  });

  test("the bound is the median window, and a serving fleet redraws at it", async () => {
    const st = await (await fetch(`http://127.0.0.1:${port}/admin/state`)).json();
    expect(st.soft_budget).toBe(178);                      // floor(210 * 0.85), not min(samples)
    const r1 = await chat(port, "conv-budget-1");
    expect(r1.status).toBe(200);                           // stamps lastOKAt
    expect(world.pool.retunnels()).toBe(0);
    const r2 = await chat(port, "conv-budget-2");
    expect(r2.status).toBe(200);                           // the redraw never blocks the answer
    const t = Date.now() + 5_000;
    while (world.pool.retunnels() < 1 && Date.now() < t) await Bun.sleep(100);
    expect(world.pool.retunnels()).toBeGreaterThanOrEqual(1);
    expect(readLog(logFile).some((l) => l.event === "pool_budget_redraw")).toBe(true);
  });
});

describe.serial("a zero-candidate request waits for the pool", () => {
  // The pool is empty between a rotation's first kill and its first 200. The
  // client used to get a 503 in milliseconds there, which the agent reads as a
  // dead provider. A request must instead wait for capacity and be served.
  let dir, world, child, port, logFile;

  beforeAll(async () => {
    dir = tempDir("clientwait");
    world = startWorld();
    port = freePort();
    logFile = join(dir, "requests.jsonl");
    child = startSidecar({
      SIDECAR_PORT: String(port),
      SIDECAR_TARGET: `http://127.0.0.1:${world.upstream.port}`,
      SIDECAR_POOL_API: `http://127.0.0.1:${world.pool.port}`,
      SIDECAR_LOG: logFile,
      SIDECAR_STATE: join(dir, "v2_state.json"),
      SIDECAR_PINS: join(dir, "v2_pins.json"),
      SIDECAR_FP_STATE: join(dir, "v2_fp.json"),
      SIDECAR_IP_STATE: join(dir, "v2_ips.json"),
      // Long enough that the egresses stay parked while the request waits.
      SIDECAR_SHORT_PARK_MS: "60000",
      SIDECAR_LAST_CHANCE_GAP_MS: "5",
      SIDECAR_CLIENT_WAIT_MS: "8000",
      SIDECAR_CLIENT_WAIT_POLL_MS: "50",
    });
    await waitForSidecar(port);
  }, T);

  afterAll(async () => {
    try { child.kill(); } catch {}
    world?.stop();
    if (dir) cleanup(dir);
  });

  test("holds a zero-candidate request, then serves it when capacity returns", async () => {
    // Park every egress: one agent-shaped request strikes the whole pool.
    world.upstream.burnAll(true);
    const wall = await chat(port, "ses_cw_0");
    expectWall(wall, { long: true });

    // Nothing can be tried now. The request must not answer 503 immediately.
    const pending = chat(port, "ses_cw_1");
    await Bun.sleep(500);
    // A rotation/retunnel landing frees the book; the waiting request is served.
    world.upstream.burnAll(false);
    await fetch(`http://127.0.0.1:${port}/admin/unpark`, { method: "POST" });
    const res = await pending;
    expect(res.status).toBe(200);

    const ev = readLog(logFile).filter((l) => l.event === "client_wait");
    expect(ev.length).toBeGreaterThanOrEqual(1);
    const done = readLog(logFile).filter((l) => l.event === "client_wait_done").slice(-1)[0];
    expect(done).toBeTruthy();
    expect(done.waited_ms).toBeGreaterThanOrEqual(300);
    expect(done.candidate).toBe(true);
  }, T);
});

// --- image input -----------------------------------------------------------
// The free pool mixes multimodal and text-only engine instances behind
// different egresses (measured 2026-11-05: one egress read the PNG with 200,
// another answered 400 "image input is not available on this engine"). A 400
// must move the SAME body to the next egress so vision survives; when every
// egress is text-only the honest 400 is passed through, because the gateway
// strips the images and answers text-only rather than failing the turn.
test("every text-only egress is tried with the image intact, then the honest 400 is returned", async () => {
  const dir = tempDir("img400all");
  const world = startWorld({ retryAfter: "3600", textOnly: [0, 1, 2] });
  const port = freePort();
  const logFile = join(dir, "requests.jsonl");
  const side = startSidecar({
    SIDECAR_PORT: String(port),
    SIDECAR_TARGET: `http://127.0.0.1:${world.upstream.port}`,
    SIDECAR_POOL_API: `http://127.0.0.1:${world.pool.port}`,
    SIDECAR_LOG: logFile,
    SIDECAR_STATE: join(dir, "v2_state.json"),
    SIDECAR_PINS: join(dir, "v2_pins.json"),
    SIDECAR_FP_STATE: join(dir, "v2_fp.json"),
    SIDECAR_IP_STATE: join(dir, "v2_ips.json"),
  });
  try {
    await waitForSidecar(port);
    const res = await chat(port, "ses_image_all_conv", { images: true });
    expect(res.status).toBe(400);
    expect(res.text).toContain("image input is not available");
    // No fake 503 and no outward Retry-After: OMP must see the engine's own
    // refusal so the gateway can strip the images and retry once.
    expect(res.headers.get("retry-after")).toBeNull();
    const hits = world.upstream.hits.filter((h) => h.pathname.endsWith("/chat/completions"));
    // The order of tied slots is random, so the invariant is "all three, each
    // once, all carrying the image" - a same-body rotation with no stripping.
    expect(hits.length).toBe(3);
    expect(new Set(hits.map((h) => h.egress)).size).toBe(3);
    for (const h of hits) {
      expect(h.status).toBe(400);
      expect(h.images).toBe(true);
    }
    expect(readLog(logFile).filter((l) => l.event === "image_unsupported").length).toBe(3);
  } finally {
    side.kill();
    world.stop();
    cleanup(dir);
  }
});

test("a multimodal egress serves the same image body after a text-only refusal", async () => {
  const dir = tempDir("img400mix");
  const world = startWorld({ retryAfter: "3600", textOnly: [0, 1] });
  const port = freePort();
  const logFile = join(dir, "requests.jsonl");
  const side = startSidecar({
    SIDECAR_PORT: String(port),
    SIDECAR_TARGET: `http://127.0.0.1:${world.upstream.port}`,
    SIDECAR_POOL_API: `http://127.0.0.1:${world.pool.port}`,
    SIDECAR_LOG: logFile,
    SIDECAR_STATE: join(dir, "v2_state.json"),
    SIDECAR_PINS: join(dir, "v2_pins.json"),
    SIDECAR_FP_STATE: join(dir, "v2_fp.json"),
    SIDECAR_IP_STATE: join(dir, "v2_ips.json"),
  });
  try {
    await waitForSidecar(port);
    const res = await chat(port, "ses_image_mix_conv", { images: true });
    expect(res.status).toBe(200);
    const hits = world.upstream.hits.filter((h) => h.pathname.endsWith("/chat/completions"));
    // Whatever the random slot order, the sidecar never strips: every attempt
    // carries the image, and a refusal always moves to a different egress.
    for (const h of hits) expect(h.images).toBe(true);
    const ok = hits.find((h) => h.status === 200);
    expect(ok).toBeTruthy();
    for (const h of hits.filter((x) => x.status === 400)) expect(h.egress).not.toBe(ok.egress);
  } finally {
    side.kill();
    world.stop();
    cleanup(dir);
  }
});

// A dead WARP tunnel keeps its port listening, so the pool still lists it. The
// sidecar's only evidence is its own TTFB deadline; the failure it must not
// repeat (measured live 2026-11-06) is paying that deadline on EVERY request
// because the hung slot kept its rank-0 "served recently" status for hours.
test("a hung egress is demoted at once: the next request never pays its TTFB", async () => {
  const dir = tempDir("hangdemote");
  const world = startWorld({ retryAfter: "3600" });
  const port = freePort();
  const logFile = join(dir, "requests.jsonl");
  const side = startSidecar({
    SIDECAR_PORT: String(port),
    SIDECAR_TARGET: `http://127.0.0.1:${world.upstream.port}`,
    SIDECAR_POOL_API: `http://127.0.0.1:${world.pool.port}`,
    SIDECAR_LOG: logFile,
    SIDECAR_STATE: join(dir, "v2_state.json"),
    SIDECAR_PINS: join(dir, "v2_pins.json"),
    SIDECAR_FP_STATE: join(dir, "v2_fp.json"),
    SIDECAR_IP_STATE: join(dir, "v2_ips.json"),
    SIDECAR_ATTEMPT_TTFB_MS: "700",
  });
  try {
    await waitForSidecar(port);
    // Make slots 1 and 2 PROVEN (rank 0) first: the picker sorts ties by `ok`
    // ascending, so an unproven slot 0 would otherwise win on freshness alone
    // and this test would pass for the wrong reason.
    for (const i of [1, 2]) await chat(port, `ses_hang_warm_${i}`, { backend: world.slots[i] });
    world.upstream.hang(0);
    // Teach the sidecar that slot 0 is dead. The forced path (x-sidecar-backend)
    // has maxAttempts=1, so this call cannot hide the timeout behind a retry.
    await chat(port, "ses_hang_warm_0", { backend: world.slots[0] });
    const timeouts = readLog(logFile).filter((l) => l.status === 0 && /ttfb timeout/i.test(String(l.err || "")));
    expect(timeouts.length).toBe(1);
    // The journal row now names the egress that timed out (proxy was missing).
    expect(timeouts[0].proxy).toBeTruthy();
    // /admin/unpark is "forget the parks": it clears until/hard/bad/strikes but
    // NOT the hung mark. Without the rank demotion this puts slot 0 back at the
    // front (ok=0, no park), so the very next request pays the whole deadline.
    const un = await fetch(`http://127.0.0.1:${port}/admin/unpark`, { method: "POST" });
    expect(un.status).toBe(200);
    // One normal conversation must skip the hung slot outright: a single 700 ms
    // timeout would already break the wall-time bound below.
    const before = world.upstream.hits.length;
    const t0 = Date.now();
    const r = await chat(port, "ses_hang_after_unpark");
    const ms = Date.now() - t0;
    expect(r.status).toBe(200);
    expect(ms).toBeLessThan(500);
    const after = world.upstream.hits.slice(before);
    expect(after.length).toBe(1);
    expect(after[0].egress).not.toBe(0);
  } finally {
    side.kill();
    world.stop();
    cleanup(dir);
  }
});

test("a request cannot chain hung tunnels for minutes: the retry budget stops the sweep", async () => {
  const dir = tempDir("hangbudget");
  const world = startWorld({ retryAfter: "3600" });
  const port = freePort();
  const logFile = join(dir, "requests.jsonl");
  const side = startSidecar({
    SIDECAR_PORT: String(port),
    SIDECAR_TARGET: `http://127.0.0.1:${world.upstream.port}`,
    SIDECAR_POOL_API: `http://127.0.0.1:${world.pool.port}`,
    SIDECAR_LOG: logFile,
    SIDECAR_STATE: join(dir, "v2_state.json"),
    SIDECAR_PINS: join(dir, "v2_pins.json"),
    SIDECAR_FP_STATE: join(dir, "v2_fp.json"),
    SIDECAR_IP_STATE: join(dir, "v2_ips.json"),
    SIDECAR_ATTEMPT_TTFB_MS: "900",
    SIDECAR_RETRY_BUDGET_MS: "300",
  });
  try {
    await waitForSidecar(port);
    for (const i of [0, 1, 2]) world.upstream.hang(i);
    const t0 = Date.now();
    const res = await chat(port, "ses_budget");
    const ms = Date.now() - t0;
    // The wall contract still applies: the client hears an honest refusal.
    expect([429, 503]).toContain(res.status);
    const ev = readLog(logFile).filter((l) => l.event === "retry_budget");
    expect(ev.length).toBe(1);
    // Budget 300 ms is spent by the first 900 ms attempt, so the sweep stops
    // at one real call. Without the budget the loop would try all three slots
    // (count cap only) and this event would not exist at all.
    expect(ev[0].attempts).toBe(1);
    expect(ms).toBeLessThan(4000);
  } finally {
    side.kill();
    world.stop();
    cleanup(dir);
  }
});

test("first contact with a never-served slot uses the short deadline, the retry the full one", async () => {
  const dir = tempDir("untestedttfb");
  const world = startWorld({ retryAfter: "3600" });
  const port = freePort();
  const logFile = join(dir, "requests.jsonl");
  const side = startSidecar({
    SIDECAR_PORT: String(port),
    SIDECAR_TARGET: `http://127.0.0.1:${world.upstream.port}`,
    SIDECAR_POOL_API: `http://127.0.0.1:${world.pool.port}`,
    SIDECAR_LOG: logFile,
    SIDECAR_STATE: join(dir, "v2_state.json"),
    SIDECAR_PINS: join(dir, "v2_pins.json"),
    SIDECAR_FP_STATE: join(dir, "v2_fp.json"),
    SIDECAR_IP_STATE: join(dir, "v2_ips.json"),
    SIDECAR_ATTEMPT_TTFB_MS: "900",
    SIDECAR_UNTESTED_TTFB_MS: "300",
  });
  try {
    await waitForSidecar(port);
    world.upstream.hang(0);
    const t0 = Date.now();
    await chat(port, "ses_untested_a", { backend: world.slots[0] });
    const firstMs = Date.now() - t0;
    // A slot that never served anything is cut at the short deadline.
    expect(firstMs).toBeLessThan(700);
    const t1 = Date.now();
    await chat(port, "ses_untested_b", { backend: world.slots[0] });
    const secondMs = Date.now() - t1;
    // After the first failure the slot is no longer "untested": full deadline.
    expect(secondMs).toBeGreaterThanOrEqual(800);
    const tt = readLog(logFile).filter((l) => /ttfb timeout/i.test(String(l.err || "")));
    expect(tt.length).toBe(2);
    expect(tt[0].ms).toBeLessThan(700);
    expect(tt[1].ms).toBeGreaterThanOrEqual(800);
  } finally {
    side.kill();
    world.stop();
    cleanup(dir);
  }
});
