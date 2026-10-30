# Pool mechanic bench (quota-free, 1000 rpm)

Reproduces the ai-router pool measurements without touching any provider quota:
the "upstream" is a local stub, and the pool API is a local stand-in. Launched
2026-10-22 to prove the identity-recycle / wait-for-lease / lockout-probe fixes
(measurement notes stay in the private working tree).

| file | role |
|---|---|
| `stub.py` | HTTP server on `127.0.0.1:9099`: SSE 200s, or 429 + `Retry-After` in burn mode. Knobs are files: `mode.txt` (`ok`/`burn`), `retry_after.txt`, `serve_s.txt`, `chunks.txt`. Also speaks absolute-URI HTTP (it is the lease proxy). |
| `fakepool.py` | warp-router API stand-in on `127.0.0.1:8788`: `/api/v2/pool/acquire|release`, `/api/v1/pool/rotate`, `/ctl`. Knobs: `burn_all.txt` (empty lease batch), `rotate_new_prefix.txt` (does a rotate mint new /48s?), `rotate_s.txt`. |
| `iso-config.yaml` | ai-router stand config: port 9013, pool transport against the fake API, upstream = stub, `wait_for_lease_s: 20`. |
| `bench.py` | open-loop client: `--rpm`, `--seconds`, `--concurrency`, writes per-request CSV. Submissions and completions are counted separately. |

Run (three terminals / background services):

```bash
python stub.py            # 127.0.0.1:9099
python fakepool.py        # 127.0.0.1:8788
LOCALAPPDATA=<scratch>/localappdata AI_ROUTER_SESSION_DB=off AI_ROUTER_NO_AUTOSTART=1 \
  <build>.exe serve -config iso-config.yaml -cli -no-autostart
python bench.py --token <admin_token> --rpm 1000 --seconds 150 --concurrency 400 --out bench.csv
```

`LOCALAPPDATA` MUST point at a scratch dir: `config.BaseDir()` derives `state.db`
and `pool_state*.json` from it, so without the override the stand writes into the
live daemon's files. `-no-autostart` keeps HKCU Run untouched.

`iso-config.yaml` carries a placeholder `admin_token`: the stand listens on
loopback only, so any value works — pass the same string to `bench.py --token`.

Read the result from the stand's own `state.db` (`request_log`), not from the
client: 1000 rows in one minute means the router really served 1000 rpm.
