# opencode sidecar

A small Bun HTTP front that sits between a client (ai-router, the opencode CLI, any
OpenAI-compatible agent) and the opencode free-tier inference endpoints. It exists
because the free tier meters **egress addresses**, not accounts: one conversation
keeps one `(session, egress, fingerprint)` trio for its whole life, a hard `429`
parks that egress for its `Retry-After` window, and the same request hops to another
egress instead of failing. When every egress in the pool is parked the client gets an
honest `503` (not the upstream's quota body), so the agent's session is not poisoned
for hours by one upstream answer.

The egress pool is external: the sidecar expects a [warp-router](../README.md)-style
pool API (or any service exposing the same endpoints) and never opens VPN connections
itself.

## Requirements

- [Bun](https://bun.sh) 1.2+
- an egress pool on `SIDECAR_POOL_API` (default `http://127.0.0.1:8787`), see
  [Pool API](#pool-api)
- `wreq-js` is vendored under `vendor/wreq-js` (MIT, its `LICENSE` is included) —
  the default egress transport pins a browser TLS/HTTP2 fingerprint that the plain
  `fetch` cannot produce

## Run

```powershell
cd sidecar
bun sidecar2.mjs          # listens on 127.0.0.1:9099
```

Point a client at it as an OpenAI-compatible base URL:

```text
http://127.0.0.1:9099/inference/openai/v1      # inference lane (free tier)
http://127.0.0.1:9099/zen/openai/v1            # zen lane (separate budget)
```

`/admin/state` (GET) returns the live pool/park/pin view; `POST /admin/unpark`,
`/admin/rotate-fresh`, GET `/admin/trace`, `/admin/profile` are the operator hooks.

## Configuration

Everything is environment, no config file. Defaults are safe for a local run; a
standing install pins the absolute paths (log, state, dump dir) in its launcher.
The ones that matter first:

| variable | default | meaning |
|---|---|---|
| `SIDECAR_PORT` | `9099` | listen port |
| `SIDECAR_HOST` | `127.0.0.1` | listen address |
| `SIDECAR_TARGET` | `https://opencode.ai` | upstream origin; paths are forwarded as-is |
| `SIDECAR_POOL_API` | `http://127.0.0.1:8787` | egress pool API |
| `SIDECAR_FRONT_DIRECT` | `0` | `1` allows the host's own IP as a last-resort lane (breaks anonymity) |
| `SIDECAR_FRONT_PROXY` | — | explicit front lane proxy; overrides `FRONT_DIRECT` |
| `SIDECAR_LOG` | `./requests.jsonl` | JSONL event/attempt log |
| `SIDECAR_STATE` | `./v2_state.json` | pins/burn book/uplink state (siblings derived from it) |
| `SIDECAR_IP_STATE` / `SIDECAR_PINS` / `SIDECAR_UPLINK_STATE` / `SIDECAR_FP_STATE` | — | override the state-sibling file names (they normally live next to `SIDECAR_STATE`) |
| `SIDECAR_DUMP_BODIES` | `0` | store request bodies (gzip ring) for 429 replay |
| `SIDECAR_DUMP_DIR` | `./dumps` | dump ring directory |
| `SIDECAR_DUMP_KEEP` | `40` | dump ring length |
| `SIDECAR_RETRY_BUDGET_MS` | 60 000 | per-request retry budget |
| `SIDECAR_CLIENT_WAIT_MS` | 180 000 | wait for a free egress before `503` |
| `SIDECAR_WARP_CONFIG` | — | warp-router `config.yaml` to read `auth_token` from (needed for pool rotate/mint) |
| `SIDECAR_WIDE_STATE` | — | optional `wide_state.json`; accounts bound by node workers are protected from recycle |

The full set (rotators, parks, heuristics, probes) is discoverable with
`grep -o 'process\.env\.SIDECAR_[A-Z_]*' sidecar2.mjs | sort -u`; each site carries a
comment with the measurement that set its default.

## Pool API

The sidecar uses a small contract; any service that implements it can stand in:

| request | expected answer |
|---|---|
| `GET /api/v1/backends` | `{"backends":[{"id","proxy","ip","proven"}]}` — egress endpoints |
| `POST /api/v1/lease` | take one egress out of rotation for a while |
| `POST /api/v1/release` | return it early |
| `POST /api/v1/rotate` / `retunnel` | redraw the fleet (optional, guarded by `SIDECAR_ALLOW_ROTATE`) |
| `GET /api/v1/accounts` | account list (optional, used by the mint/recycle guard) |

Egress endpoints are `http://` or `socks5://` proxies; a `429` from upstream parks the
whole `/64` (or the address, when it is IPv4), and the picker prefers unburned groups.

## Tests

```powershell
bun test test/sidecar.test.mjs
```

The suite spawns the sidecar against fake pool/proxies/upstream and proves the rules a
unit test cannot: duplicate egresses are excluded, a conversation keeps one trio, a
hard `429` parks the window and the same request hops to another egress, and an
exhausted pool yields `503` rather than the upstream quota body.

## License

AGPL-3.0 with the rest of the repository (see `../LICENSE`); the vendored `wreq-js`
stays MIT. The free-tier endpoints belong to their provider: this is an experiment in
transport engineering, not a permission to break provider terms.
