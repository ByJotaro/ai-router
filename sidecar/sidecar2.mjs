// opencode-sidecar v2 — Bun-TLS proxy to opencode.ai with REAL per-egress
// accounting.
//
// Why this exists: the free lane's rate limit is per egress IP, and the
// warp-router front proxy (:8118) hides which backend served a request, so a
// burned egress looks exactly like a burned session. This sidecar talks to the
// pool's individual backend proxies (8620+) so every response is attributable
// to one IP, learns each window's request budget, parks exhausted egresses,
// hops to another live egress so clients never see the 429, and asks
// warp-router for fresh identities only on explicit request.
//
// Contract with ai-router: it is configured with base_url http://127.0.0.1:9099
// and transport direct, and forwards the full opencode CLI header set
// (x-opencode-session/-session-id/-request/-client/-project). The sidecar owns
// the identity dimensions that must never be shared fleet-wide: every
// conversation gets its own real ses_ id pinned to one egress, and every
// request gets a fresh request id. A ban then costs one conversation, not the
// pool — a single shared session id across all egresses is one ban key.
import { appendFileSync, readFileSync, renameSync, statSync, writeFileSync } from "node:fs";
import { uptime as osUptime } from "node:os";
import { shouldProactiveRotate, isLongPark, shouldEscalateReuse } from "./lib/rotate_gate.mjs";
// vendored copy: node_modules/wreq-js is owned by Administrators and stays
// writable by Authenticated Users.
import { createSession } from "./vendor/wreq-js/dist/wreq-js.js";
import {
  ACCEPTED_CIPHERS, burnGroups, classify429, dedupeByIp, hash32, mintSession, prefix64, pruneMap, prunePins,
  recordBurned, recordIps, retryAfterSeconds, seededPerm, sessionKey, specForRequest,
} from "./lib/identity.mjs";

const PORT = Number(process.env.SIDECAR_PORT || 9099);
const TARGET = process.env.SIDECAR_TARGET || "https://opencode.ai";
// Empty string = no front fallback (stands use this so a dead local target
// cannot turn into a 60 s hang through WARP).
// SIDECAR_FRONT_DIRECT=1 makes the host's own egress a last-resort lane with
// no proxy. Measured 2026-10-31 22:0xZ: with FRONT_PROXY empty the sidecar
// answered 503 pool_busy while a direct Bun request from the same host got the
// upstream's own 429 - the host's budget is metered separately by the free
// tier (per /128), and the code had no way to reach it at all.
const FRONT_DIRECT = process.env.SIDECAR_FRONT_DIRECT === "1";
const FRONT_PROXY = process.env.SIDECAR_FRONT_PROXY !== undefined
  ? process.env.SIDECAR_FRONT_PROXY
  : (FRONT_DIRECT ? "direct" : "");
// The direct lane is an address like any other: a hard 429 parks it for its
// own window instead of spending an upstream call on every client retry.
let frontUntil = 0;
const POOL_API = process.env.SIDECAR_POOL_API || "http://127.0.0.1:8787";
const WARP_CONFIG = process.env.SIDECAR_WARP_CONFIG || "";
const LOG = process.env.SIDECAR_LOG || "./requests.jsonl";
// Budget learning: a window that closed after N requests teaches the bound.
// soft = min(samples) * SOFT_MARGIN (never below SOFT_FLOOR).
const BUDGET_SERVE_MS = 10 * 60_000; // a budget redraw only while the fleet served recently
const SOFT_MARGIN = 0.85;
const SOFT_FLOOR = 10;
const SAMPLE_CAP = 64;

// A budget bound must describe the TYPICAL window, not the worst prefix ever
// hit. `min` over 64 samples was pinned to 10 by addresses that arrived inside
// an already-burned site: measured 2026-11-04 22:30, samples 10-286 with median
// 173, so softBudget=10 marked EVERY address low after ten requests and no
// budget-aware decision could be built on it. The median ignores the burned
// outliers; the margin keeps the last ~15% of the window as reserve.
function percentile(list, p) {
  const a = [...list].sort((x, y) => x - y);
  if (!a.length) return 0;
  return a[Math.min(a.length - 1, Math.max(0, Math.round((a.length - 1) * p)))];
}
// Bodies below this are probes/smokes, not client traffic: their 429s are not
// evidence about an egress window (measured: same egress, same id, 200 for a
// real body and 429 for a 300-byte one).
const REPRESENTATIVE_BYTES = 8192;
// Forensics: with SIDECAR_DUMP_BODIES=1 every request body is stored gzipped,
// so a 429 can be replayed byte-for-byte against another egress. Ring: only the
// newest DUMP_KEEP files survive; oversized bodies are skipped, not truncated
// (a truncated body is not the body that got the verdict).
const DUMP_BODIES = process.env.SIDECAR_DUMP_BODIES === "1";
const DUMP_DIR = process.env.SIDECAR_DUMP_DIR || "./dumps";
const DUMP_KEEP = Number(process.env.SIDECAR_DUMP_KEEP || 40);
const DUMP_MAX_BYTES = Number(process.env.SIDECAR_DUMP_MAX_BYTES || 4_000_000);
let lastPrune = 0;
async function dumpBody(file, body) {
  try {
    await Bun.write(`${DUMP_DIR}/${file}`, Bun.gzipSync(new TextEncoder().encode(body)));
  } catch {}
  if (Date.now() - lastPrune < 60_000) return;
  lastPrune = Date.now();
  try {
    const files = (await Array.fromAsync(new Bun.Glob("*.json.gz").scan(DUMP_DIR))).sort();
    for (const f of files.slice(0, Math.max(0, files.length - DUMP_KEEP))) {
      try { await Bun.file(`${DUMP_DIR}/${f}`).delete(); } catch {}
    }
  } catch {}
}
// The upstream error body names the reason; without it a 429 row says only
// "parked". Cheap (a 429 body is a small JSON) and decisive for triage.
async function peekBody(resp, n = 300) {
  try { return (await resp.text()).slice(0, n); } catch { return ""; }
}
function rlHeaders(resp) {
  const out = {};
  for (const k of ["retry-after", "x-ratelimit-limit", "x-ratelimit-remaining", "x-ratelimit-reset", "cf-ray"]) {
    const v = resp.headers.get(k);
    if (v) out[k] = String(v).slice(0, 80);
  }
  return out;
}
const REFRESH_MS = 60_000;
const PARK_MS = 10 * 60_000;
// A first hours-class 429 is not proof that a /64 window is gone: measured
// 2026-10-27/28, 20 of 39 /64s that answered 429 served 200 again within 5
// minutes. Park a single hard strike briefly, and only write the egress off for
// the upstream's own window when the same egress strikes twice inside
// STRIKE_WINDOW_MS. One-strike long parks are what emptied the whole pool
// (22:34: 108 client 429s/min while every egress was parked).
const SHORT_PARK_MS = Number(process.env.SIDECAR_SHORT_PARK_MS || 90_000);
const STRIKE_WINDOW_MS = 15 * 60_000;
// With every egress parked, a request must still touch one: parks are our own
// opinion, and a storm of first-strike 90 s parks otherwise locks the whole pool
// for that window (measured 2026-10-27 23:07: 425 client 429s in 78 s, all with
// attempts 0, while the same egresses served 200 two minutes later). One probe
// per egress per gap keeps the amplification bounded.
const LAST_CHANCE_GAP_MS = Number(process.env.SIDECAR_LAST_CHANCE_GAP_MS || 15_000);
// A park written off for the upstream's own window is still only an opinion: it
// is re-tested this slowly so the pool can come back without a restart (measured
// 22:34-22:51 2026-10-27: 17 min of client 429s while windows were reopening).
const HARD_PROBE_AGE_MS = Number(process.env.SIDECAR_HARD_PROBE_AGE_MS || 10 * 60_000);
// The upstream sends two different 429s (measured 2026-10-27 on the zen lane):
// a per-request throttle that names no window at all (no Retry-After: 1240 of
// them in one day) and an exhausted window that names hours (3834, Retry-After
// in the thousands). A throttle means "slow down for a few seconds"; parking it
// for PARK_MS (10 min) is what quietly emptied the pool.
const THROTTLE_PARK_MS = Number(process.env.SIDECAR_THROTTLE_PARK_MS || 5_000);
// One id probe per (egress, lane): a verdict about the egress is valid for a
// while, and parallel 429s on the same egress must not stack probes (measured
// 2026-10-27 13:58: two concurrent requests paid two probes 73 ms apart for the
// same answer).
const PROBE_REUSE_MS = 60_000;
// A 429 that carries a long Retry-After is a CLOSED WINDOW (hours), not a blip:
// park the egress until that time, mark it hard, and only rotate the whole pool
// when every egress is hard-blocked and nothing succeeded for a while.
const LONG_RA_S = 900;
// A restart must not inherit hours-class parks: they are keyed to an egress
// identity that may be gone (warp rotated) or whose window has reopened, and a
// 12h park with zero traffic sent is a restart that never heals. Two signals
// mean "we were not running": the state predates the last boot, or it is older
// than REBOOT_MIN_AGE. A crash-loop keeps the file fresh and keeps its parks.
const REBOOT_MIN_AGE_MS = 10 * 60_000;
// Boot instant, overridable for tests/containers where os.uptime() reports the
// host rather than this process's namespace.
const BOOT_MS = Number(process.env.SIDECAR_BOOT_MS) || Date.now() - osUptime() * 1000;
const ALLOW_ROTATE = process.env.SIDECAR_ALLOW_ROTATE !== "0";
// When every egress is parked, the pool is dead until identities change. Rotate
// on our own, but rarely: a rotation restarts every WARP backend.
const ROTATE_COOLDOWN_MS = Number(process.env.SIDECAR_ROTATE_COOLDOWN_MS ?? 10 * 60_000);
const ROTATE_TIMEOUT_MS = Number(process.env.SIDECAR_ROTATE_TIMEOUT_MS ?? 180_000);
const DEGRADED_IDLE_MS = Number(process.env.SIDECAR_DEGRADED_IDLE_MS ?? 180_000); // no success for this long + most exits parked = rotate
const ROTATE_SOON_MS = Math.min(90_000, ROTATE_COOLDOWN_MS); // retry after a rotate that changed nothing
const ROTATE_IDLE_MS = Number(process.env.SIDECAR_ROTATE_IDLE_MS ?? 60_000); // ...and only when nothing has succeeded recently
const ROTATE_CHECK_MS = Number(process.env.SIDECAR_ROTATE_CHECK_MS ?? 20_000); // how soon to look whether the fleet actually moved
// A zero-candidate request used to leave in milliseconds with a 503, which the
// client reads as a dead provider and can answer by killing the whole agent
// session. A rotation lands in ~20 s, so hold the request while the pool
// rebuilds and answer 503 only when the waiting budget is gone.
const CLIENT_WAIT_MS = Number(process.env.SIDECAR_CLIENT_WAIT_MS ?? 35_000);
const CLIENT_WAIT_POLL_MS = Number(process.env.SIDECAR_CLIENT_WAIT_POLL_MS ?? 400);
// A redraw is judged by SERVICE, not by novelty: measured 2026-10-31, 42
// rotations landed fresh 14-24 / spent 0 while the fleet served nothing, and
// the address drawn at 21:36 (2a09:bac1:6080:8::32:119) answered
// FreeUsageLimitError ra=10402 on its first request. After this long without a
// 200 the redraw is sterile and the only lever left is registering devices.
const REDRAW_STERILE_MS = Number(process.env.SIDECAR_REDRAW_STERILE_MS ?? 120_000);
// Account minting: the egress address is derived from the WARP client address
// that the registration API assigns, and it is FIXED per account. A pool that
// only cycles its accounts therefore re-serves egresses whose per-address
// budget is already spent (measured 2026-10-28: 18 of 19 "fresh" exits were
// already in the burn book and all answered 429). Registering a new device
// mints a new client address, hence a new egress: measured on the same uplink,
// a device registered at 15:21 egressed 2a09:bac1:5500:10::14:2ab - an address
// the book had never seen - while the whole fleet was 429. That is the lever
// that works without a VPN; the VPN only changes the registration edge.
const MINT_ENABLED = process.env.SIDECAR_MINT !== "0";
const MINT_COUNT = Number(process.env.SIDECAR_MINT_COUNT || 0); // 0 = pool size, capped
const MINT_CAP = Number(process.env.SIDECAR_MINT_MAX || 10);
const MINT_DELAY_S = Number(process.env.SIDECAR_MINT_DELAY_S || 6); // Cloudflare rate-limits registrations per IP
const MINT_COOLDOWN_MS = Number(process.env.SIDECAR_MINT_COOLDOWN_MS ?? 10 * 60_000);
// Share of the fleet whose daily window is closed RIGHT NOW (spentShare now reads
// deadlineBlocked, not the historical `burned` flag): above it a redraw cannot
// buy capacity and only new registrations can.
const MINT_SPENT_SHARE = Number(process.env.SIDECAR_MINT_SPENT_SHARE ?? 0.6);
// Path lock: the same fleet serves 200 through one uplink and 429s every
// address - including unseen ones - through another (measured 2026-10-28:
// home path, median retry-after 25 065 s, 172 of 172 sampled 429s on already
// seen addresses AND 16 freshly minted exits all 429; ProtonVPN/AMS served
// 200s on the same pool). When the upstream says "come back in hours" for the
// whole fleet, no rotation/mint/respread can move that decision, so the churn
// only burns accounts: stop.
// A path lock needs an HOURS-class window, not a specific number of hours.
// The threshold was 4 h, and on 2026-11-04 21:49-21:53 the whole fleet
// answered with retry-after 10 286-11 592 s (2.9-3.2 h): below it, so no lock
// fired and the degraded ladder rotated every 90 s through a wall that only
// an uplink change or the daily reset could move. Short throttles (900 s and
// under) are still excluded; any 200 clears the lock, so a false positive
// costs one redraw delay, not a dead fleet.
const PATH_LOCK_RA_S = Number(process.env.SIDECAR_PATH_LOCK_RA_S ?? 3600);
const PATH_LOCK_MIN_EGRESSES = Number(process.env.SIDECAR_PATH_LOCK_MIN_EGRESSES ?? 4);
// The uplink, not the egress, decides whether the fleet is usable: measured
// 2026-10-28, the whole pool 429'd on the home path and served 200s after
// ProtonVPN was switched on - but only after warp-router was RESTARTED with the
// VPN up. Existing WARP tunnels keep the path they were built on, and the pool
// only re-reads endpoints at boot, so a path switch has to be followed by a
// pool restart or it changes nothing. Watch the host's own egress and do it.
const TRACE_URL = process.env.SIDECAR_TRACE_URL || "https://www.cloudflare.com/cdn-cgi/trace";
const TRACE_TIMEOUT_MS = Number(process.env.SIDECAR_TRACE_TIMEOUT_MS || 8000);
const UPLINK_CHECK_MS = Number(process.env.SIDECAR_UPLINK_CHECK_MS ?? 180_000);
const UPLINK_RESTART_MIN_MS = Number(process.env.SIDECAR_UPLINK_RESTART_MIN_MS ?? 5 * 60_000);
// Retunnel is a rolling worker respawn (~2 s per slot): 20 slots need >40 s.
const RETUNNEL_TIMEOUT_MS = Number(process.env.SIDECAR_RETUNNEL_TIMEOUT_MS ?? 180_000);
// Per-attempt TTFB deadline. A dead tunnel accepts the TCP connection and then
// never answers; without this one hung slot stalled a whole request for 342 s
// (measured on a node-backed WARP worker, 2026-11-03). Abort when the response
// HEADERS do not arrive in time; the timer is cleared as soon as the Response
// resolves, so a long body stream is never interrupted.
const ATTEMPT_TTFB_MS = Number(process.env.SIDECAR_ATTEMPT_TTFB_MS || 30_000);
// A tunnel that accepts TCP and then hangs is NOT "proven" because it served a
// 200 recently: egressRank trusts lastOK for PROVEN_MS (6 h), so a dead slot
// stayed top-ranked and every pick paid the whole TTFB deadline. Measured
// 2026-11-06: 200 timeouts in ~20k requests, the same three silent egresses,
// 12% of requests paying 30/60/90 s before a healthy egress answered.
const HUNG_DEMOTE_MS = Number(process.env.SIDECAR_HUNG_DEMOTE_MS ?? 30 * 60_000);
// A first hang parks 60 s; a second one means the tunnel is gone, not slow.
const HUNG_PARK_MS = Number(process.env.SIDECAR_HUNG_PARK_MS ?? 10 * 60_000);
// Wall-clock budget for one client request. Fast 429 sweeps still fit (a
// refusal answers in ~0.4 s); a chain of hung tunnels can no longer hold the
// caller for maxAttempts x TTFB (6 x 30 s = 3 min).
const RETRY_BUDGET_MS = Number(process.env.SIDECAR_RETRY_BUDGET_MS ?? 45_000);
// First contact with a slot that has never served anything: the fleet's p50
// TTFB is 3.2 s, so a 30 s wait only buys a slow death for a freshly drawn
// dead tunnel (every redraw adds such slots). One timeout is enough to learn;
// the normal deadline applies from the second attempt on.
const UNTESTED_TTFB_MS = Number(process.env.SIDECAR_UNTESTED_TTFB_MS ?? 12_000);

function warpToken() {
  if (!WARP_CONFIG) return "";
  try {
    const conf = readFileSync(WARP_CONFIG, "utf8");
    const m = conf.match(/auth_token:\s*(\S+)/);
    return m ? m[1] : "";
  } catch {
    return "";
  }
}

const TOKEN = process.env.SIDECAR_WARP_TOKEN || warpToken();

// ---- per-egress state -----------------------------------------------------
/** proxy URL -> state */
const egress = new Map();
let samples = []; // window sizes at ban time
let softBudget = 0;
let lastRotate = 0;
let backends = [];
let lastDiscover = 0;
let lastDiscoverOk = 0;
let lastPoolRejectLog = 0;
let rotateInFlight = false;
let lastTransportAt = 0;
let lastOKAt = 0;
// A redraw that landed fresh addresses but has not served them yet: cleared by
// the first 200, escalated to a mint when it goes sterile.
let redrawWatch = null;
// A rotation stops and restarts every WARP worker, so it tears any stream still
// in flight. The `lastOKAt` idle gate cannot see a long SSE response (the stamp
// is written when the stream STARTS), so live streams are counted for real and a
// redraw is refused while one is open.
let liveStreams = 0;
let autoRotations = 0; // consecutive all-closed rotations without a success
let blockedOnlyLogged = false; // pool_deadline_only fires once per process, not per request
let groupBlockedLogged = false; // pool_group_burned_only, same: once per process
const STATE_FILE = process.env.SIDECAR_STATE || "./v2_state.json";
// SIDECAR_STATE used to be rewritten into the sibling names with a regex that
// matched only the default filename, so any other value collapsed the ip book,
// the pins, the fp overrides and the uplink into ONE file: loadIps() then read
// the state json as the book and saveIps() overwrote the parks with it.
const STATE_DIR = (STATE_FILE.match(/^(.*)[\\/][^\\/]+$/) || [null, "."])[1];
function statePath(name) { return `${STATE_DIR}/${name}`; }
let ownsPort = false; // set once Bun.serve returns; a loser must not rewrite state
function loadState() {
  let data = null;
  try { data = readJsonWithFallback(STATE_FILE); } catch {}
  if (!data) return;
  // Rows persisted by the pre-lane build are keyed by the bare proxy while
  // stateFor() reads `${proxy}|${lane}`: the old row is never updated again, yet
  // it stays in the map, is re-saved forever, and shows up in /admin/state as a
  // second "active" row for a live port carrying a dead address (measured: 82
  // rows for 31 ports, 20 of them stale twins). Re-key on load, drop the rest.
  let rekeyed = 0;
  let dropped = 0;
  // Legacy (bare-proxy) keys first, lane-keyed second, so a stale twin can never
  // overwrite the row the current build maintains.
  const entries = Object.entries(data.egress || {}).sort((a, b) => Number(a[0].includes("|")) - Number(b[0].includes("|")));
  for (const [key, st] of entries) {
    const proxy = st && st.proxy ? st.proxy : key.split("|")[0];
    const lane = st && st.lane ? st.lane : (key.split("|")[1] || "inference");
    if (!proxy) { dropped++; continue; }
    if (!key.includes("|")) rekeyed++;
    egress.set(`${proxy}|${lane}`, { ...st, proxy, lane });
  }
  if (rekeyed || dropped) {
    logLine({ ts: new Date().toISOString(), event: "state_rekeyed", rows: egress.size, rekeyed, dropped });
  }
  if (data.burn64 && typeof data.burn64 === "object") burn64 = data.burn64;
  if (Array.isArray(data.samples) && data.samples.length) {
    samples = data.samples.slice(-SAMPLE_CAP).filter((n) => Number.isFinite(n) && n >= SOFT_FLOOR);
    if (samples.length) softBudget = Math.max(SOFT_FLOOR, Math.floor(percentile(samples, 0.5) * SOFT_MARGIN));
  }
  const now = Date.now();
  const savedAt = Date.parse(data.ts || "");
  if (!Number.isFinite(savedAt)) return;
  const acrossBoot = savedAt < BOOT_MS - 5000; // written before the last boot
  const longOutage = now - savedAt > REBOOT_MIN_AGE_MS;
  if (!acrossBoot && !longOutage) return; // crash-loop: the parks are still real
  let states = 0;
  let shortened = 0;
  const proxies = new Set();
  for (const st of egress.values()) {
    states++;
    proxies.add(st.proxy);
    if (st.until > now) { st.until = 0; shortened++; }
    st.hard = 0;
  }
  if (shortened) {
    logLine({
      ts: new Date().toISOString(), event: "parks_cleared",
      mode: acrossBoot ? "reboot" : "restart", rows: proxies.size, states, shortened,
      age_s: Math.round((now - savedAt) / 1000),
    });
    saveState();
  }
}
function saveState() {
  // A second instance that loses the bind used to clear parks in memory and
  // persist them from its exit hook (measured 2026-10-27 20:30:00.298
  // `parks_cleared shortened=10` 29 ms BEFORE `bind_failed`, while the live
  // instance kept serving). Only the owner of the port may write shared state.
  if (!ownsPort) return;
  try {
    writeJsonAtomic(STATE_FILE, JSON.stringify({ ts: new Date().toISOString(), egress: Object.fromEntries(egress), samples, burn64 }));
  } catch {}
}

// Two gateways live behind one host: /zen/... (legacy, per-IP daily request
// budget) and /inference/... (the CLI's own gateway). Measured 2026-10-27: the
// same egress answered 429 on /zen and 200 on /inference seconds apart, so a
// verdict on one says nothing about the other. Parks are therefore per lane.
const LANES = ["inference", "zen"];
function laneOf(pathname) {
  // Only the legacy zen gateway is a lane of its own; /inference/... and the
  // plain OpenAI-compatible paths are the CLI's gateway.
  return String(pathname || "").startsWith("/zen") ? "zen" : "inference";
}

function stateFor(proxy, ip, lane = "inference") {
  const key = `${proxy}|${lane}`;
  let s = egress.get(key);
  if (!s) {
    s = { proxy, lane, ip: ip || "", ok: 0, bad: 0, until: 0, lastOK: 0, hard: 0, seen: 0 };
    egress.set(key, s);
  }
  // The row is keyed by (proxy, lane); the address is a property of the CURRENT
  // binding. Keeping the first ip it ever saw is how /admin/state ended up
  // showing a live port with yesterday's address and yesterday's verdict.
  //
  // warp-router restart/retunnel = same ports, NEW identities, so every score
  // that belongs to the ADDRESS must be reset here: a fresh identity that
  // inherits strikes>=2 turns its very first 429 into a park until midnight
  // (measured 2026-10-28 13:23: rotate landed 18 fresh exits, the fleet went
  // dark in four minutes), and rank 0 from the old address's lastOK keeps the
  // picker on it. This lives in stateFor - the single place that learns a new
  // address - because doing it in discover() keyed on `st.ip` stopped working
  // the moment stateFor refreshed the field first.
  if (ip && s.ip !== ip) {
    const first = !s.ip;
    s.ip = ip;
    if (!first) {
      s.ok = 0;
      s.bad = 0;
      s.lastOK = 0;
      s.hard = 0;
      s.until = 0;
      s.strikes = 0;
      s.lastStrike = 0;
      s.parkedAt = 0;
      s.lastChanceAt = 0;
      s.hungAt = 0;
      s.hung = 0;
      s.ttfb = 0;
    }
  }
  return s;
}

// The gateway's daily budget is per /64, not per full address: two slots in one
// prefix spend one window. Parking only the slot that saw the 429 would send
// the next request into the same spent window through the sibling slot.
function park64(st, until, hard) {
  st.until = Math.max(st.until || 0, until);
  if (hard) st.hard = 1;
  // Group parking is a ZEN rule only. The legacy zen handler limits per
  // x-real-ip truncated to four hextets, so one 429 there closes the prefix.
  // On /inference the budget is per address, measured 2026-10-27: ::32:132 in
  // 48f3:1f5 answered FreeUsageLimit ra=1121 at 23:41 while ::32:144, same /64,
  // served 95x200 during 23:52-23:55. Sharing the park there would idle live
  // addresses for hours on a neighbour's 429.
  if (st.lane !== "zen") return 0;
  const p64 = prefix64(st.ip);
  if (!p64 || !String(p64).includes(":")) return 0;
  let shared = 0;
  for (const other of egress.values()) {
    if (other === st || other.lane !== st.lane) continue;
    if (prefix64(other.ip) !== p64) continue;
    other.until = Math.max(other.until || 0, until);
    if (hard) other.hard = 1;
    shared++;
  }
  return shared;
}

function learnSample(n) {
  // A window of a couple of requests is a per-request reject, not a window
  // size: learning it collapses softBudget to the floor for every egress.
  if (!n || n < SOFT_FLOOR) return;
  samples.push(n);
  if (samples.length > SAMPLE_CAP) samples.splice(0, samples.length - SAMPLE_CAP);
  softBudget = Math.max(SOFT_FLOOR, Math.floor(percentile(samples, 0.5) * SOFT_MARGIN));
}

function isLow(s) {
  return softBudget > 0 && s.ok >= softBudget;
}

// A rotate either answers "rotated" or drops the socket inside warp-router's
// 30 s WriteTimeout while respawning the fleet. Both outcomes are decided by the
// same evidence: did the address set change a few seconds later? A landed
// rotation clears the fruitless-rotation ladder; one that changed nothing has to
// retry soon instead of idling the parked fleet for the whole cooldown.
function parkedKeys() {
  const now = Date.now();
  return [...egress.values()].filter((st) => st.until > now).map((st) => `${st.proxy}|${st.lane}`);
}

// ---- account minting: the only lever that refills capacity on a fixed uplink --
let mintInFlight = false;
let lastMintAt = 0;
let mintedTotal = 0;
let lastRespreadAt = 0;
const RESPRED_COOLDOWN_MS = Number(process.env.SIDECAR_RESPRED_COOLDOWN_MS ?? 30 * 60_000);
let lastRetunnel = 0;
let lastRetunnelOk = false; // outcome of the last REAL retunnel attempt (not a cooldown skip)
const RETUNNEL_COOLDOWN_MS = Number(process.env.SIDECAR_RETUNNEL_COOLDOWN_MS ?? 5 * 60_000);
// Capacity is slots x budget: a spent window is per egress address, so the only
// honest way to buy more requests is more addresses. The pool is the slot count;
// warp-router has hundreds of unused accounts, so growing it is free capacity.
const POOL_MAX = Number(process.env.SIDECAR_POOL_MAX ?? 32);
const POOL_STEP = Number(process.env.SIDECAR_POOL_STEP ?? 6);
let lastGrowAt = 0;
let growCeiling = 0;
let poolHighWater = 0; // the fleet never restarts smaller than this // observed size the pool cannot exceed (a shim contract)
const GROW_COOLDOWN_MS = Number(process.env.SIDECAR_GROW_COOLDOWN_MS ?? 10 * 60_000);
let pathLockUntil = 0;
let lastPathLockLog = 0;

// A path lock says "the decision is above the egress, rotation cannot help".
// The lock used to gate only the zen lane, because on /inference a per-address
// verdict must never disable the only lever that moves it. The 2026-11-04
// 20:37-20:49 window disproved the "lever" part for a whole-fleet hours-class
// wall: two retunnels landed 19-20 fresh exits with moved 0.70/0.83 and spent
// 0.63/0.70, a grow plus six registrations bought nothing, and redraw_sterile
// measured 451 s without a 200; the same retunnel after the uplink change
// landed fresh 25 / spent 0.04 and the fleet served within a minute. When the
// decision is above the egress, redrawing only spends accounts, so the lock
// now gates both lanes. It dies on the first 200 (the path proved it is back),
// on an uplink change, or at the daily reset (see budgetResetMs).
function pathLocked() { return Date.now() < pathLockUntil; }

// The daily free-tier window resets at 00:00 UTC, but the upstream's own
// retry-after overshoots it (measured 20:49Z: RA 14936 s -> 00:58Z while the
// wall actually lifts at 00:00Z). A lock that outlived the reset would keep
// every redraw gated for an hour after the addresses became usable again.
function budgetResetMs(now = Date.now()) {
  const d = new Date(now);
  return Date.UTC(d.getUTCFullYear(), d.getUTCMonth(), d.getUTCDate() + 1) + 120_000;
}

// Called when one request exhausted the fleet with an hours-class retry-after.
// `egresses` is how many tried egresses NAMED an hours-class window: counting
// every attempted egress made one hours-class 429 out of six look like a fleet
// verdict. The lock gates every redraw path (see pathLocked).
function notePathLock(ra, egresses, lane = "inference") {
  const until = Math.min(Date.now() + Math.min(ra, 12 * 3600) * 1000, budgetResetMs());
  if (until > pathLockUntil) pathLockUntil = until;
  if (Date.now() - lastPathLockLog > 5 * 60_000) {
    lastPathLockLog = Date.now();
    logLine({
      ts: new Date().toISOString(), event: "path_locked", ra_s: ra, egresses, lane, gates: true,
      until: new Date(pathLockUntil).toISOString(),
      hint: "every tried egress named an hours-class retry-after on this lane: the decision may be above the egress. Rotating/minting cannot help - switch the uplink (VPN/relay) or wait for the window",
    });
  }
}

const UPLINK_FILE = process.env.SIDECAR_UPLINK_STATE || statePath("v2_uplink.json");
let uplink = null;
let lastUplinkRestart = 0;
let restartInFlight = false;

function loadUplink() {
  // A torn write here used to make loadUplink() return null, so an uplink change
  // that happened while the file was unreadable was never acted on (no pool
  // restart on the new path).
  uplink = readJsonWithFallback(UPLINK_FILE);
  return uplink;
}
function saveUplink() {
  try { writeJsonAtomic(UPLINK_FILE, JSON.stringify(uplink)); } catch {}
}

async function probeUplink() {
  try {
    const r = await fetch(TRACE_URL, { signal: AbortSignal.timeout(8000) });
    if (!r.ok) return null;
    const t = await r.text();
    const ip = (t.match(/^ip=(.+)$/m) || [])[1] || "";
    const colo = (t.match(/^colo=(.+)$/m) || [])[1] || "";
    return ip ? { ip: ip.trim(), colo: colo.trim(), at: new Date().toISOString() } : null;
  } catch { return null; }
}

async function poolControl(enabled, size) {
  const resp = await fetch(`${POOL_API}/api/v1/pool`, {
    method: "POST",
    headers: { "content-type": "application/json", ...(TOKEN ? { authorization: `Bearer ${TOKEN}` } : {}) },
    body: JSON.stringify({ enabled, size }),
    signal: AbortSignal.timeout(30_000),
  });
  // A 409 "already enabled" IS the desired end state (a retry after a concurrent
  // start); any other 409 (start in progress, country lock) is a real failure.
  let ok = resp.ok;
  if (!ok && enabled && resp.status === 409) {
    const t = await resp.text().catch(() => "");
    ok = /already enabled/i.test(t);
  }
  return { ok, status: resp.status };
}

async function countUp() {
  try {
    const resp = await fetch(`${POOL_API}/api/v1/pool`, { headers: TOKEN ? { authorization: `Bearer ${TOKEN}` } : {}, signal: AbortSignal.timeout(5000) });
    if (!resp.ok) return -1;
    const d = await resp.json();
    // Behind the merge shim `backends` is the union (direct + node-backed): the
    // node lanes keep answering while the direct fleet is off, so the union can
    // never reach 0 and restartPool would leave the direct half disabled.
    if (typeof d.main_up === "number") return d.main_up;
    return (Array.isArray(d.backends) ? d.backends : []).filter((b) => b && b.state === "up").length;
  } catch { return -1; }
}

// Rebuild every WARP tunnel: the tunnel, not the address list, is what holds a
// path. Stop, wait for the fleet to drop, start on the fresh endpoints, wait for
// it to come up.
async function restartPool(reason) {
  if (!POOL_API || restartInFlight) return { ok: false, error: "busy" };
  restartInFlight = true;
  poolHighWater = Math.max(poolHighWater, backends.length || 0);
  const size = Math.max(poolHighWater, backends.length || 20, 1);
  const beforeIps = new Set(backends.map((b) => b.ip).filter(Boolean));
  try {
    // An uplink change only needs new HANDSHAKES: the colo follows the path and
    // the slot is drawn per handshake, so retunnel is enough and costs nothing.
    const rt = await retunnelPool(reason + ":retunnel");
    if (rt.ok) {
      const t1 = Date.now();
      while (Date.now() - t1 < 90_000) {
        // countUp() hits the daemon; discover() returns the PREVIOUS array when
        // the status GET fails, so `backends.length > 0` alone would report the
        // pre-retunnel fleet as a successful restart.
        const up = await countUp();
        if (up > 0) {
          await discover(true);
          // A retunnel keeps the accounts, so it can only ever hand back the same
          // addresses. Say so: a caller that needed fresh windows (uplink change)
          // must rotate, and "restarted ok" on a spent fleet is what hid the
          // 18:45-18:50 outage for five minutes.
          const reused = beforeIps.size ? backends.filter((b) => beforeIps.has(b.ip)).length : 0;
          if (up < size) logLine({ ts: new Date().toISOString(), event: "pool_restarted_partial", reason: reason + ":retunnel", up, size });
          logLine({ ts: new Date().toISOString(), event: "pool_restarted", reason: reason + ":retunnel", up, reused });
          return { ok: true, up, reused, partial: up < size };
        }
        await Bun.sleep(1000);
      }
      // A stopped pool accepts a retunnel (0 respawned, nil error) and stays
      // stopped: the fleet never comes up and every later restart repeats it.
      const re = await poolControl(true, size).catch((e) => ({ ok: false, status: 0 }));
      logLine({ ts: new Date().toISOString(), event: "pool_restart_failed", reason: reason + ":retunnel", err: "fleet did not come up", reenabled: re.ok });
      return { ok: false, error: "fleet did not come up" };
    }
    const off = await poolControl(false, 0);
    const t0 = Date.now();
    let sawZero = false;
    while (Date.now() - t0 < 25_000) {
      if ((await countUp()) === 0) { sawZero = true; break; }
      await Bun.sleep(500);
    }
    if (!off.ok || !sawZero) {
      // The daemon refused the stop or never went down: a "restart" that only
      // re-enables the same fleet is not a restart, and reporting success here
      // made the ladder treat spent egresses as fresh.
      // BUT the stop may have been accepted without the fleet dropping: leaving
      // it disabled turns a failed restart into a permanently dead direct half.
      if (off.ok) { try { await poolControl(true, size); } catch {} }
      logLine({ ts: new Date().toISOString(), event: "pool_restart_failed", reason, err: off.ok ? "pool never went down (re-enabled)" : `pool disable rejected (${off.status})` });
      return { ok: false, error: off.ok ? "pool never went down" : "pool disable rejected" };
    }
    let on = await poolControl(true, size);
    // One rejected enable used to leave the direct fleet off for good: every
    // later restart takes the retunnel path, which "succeeds" on a stopped pool
    // and never re-enables it.
    for (let attempt = 0; attempt < 3 && !on.ok; attempt++) {
      await Bun.sleep(2000);
      on = await poolControl(true, size).catch((e) => ({ ok: false, status: 0, err: String(e).slice(0, 120) }));
    }
    if (!on.ok) {
      logLine({ ts: new Date().toISOString(), event: "pool_restart_failed", reason, err: `pool enable rejected after retries (${on.status})` });
      return { ok: false, error: "pool enable rejected" };
    }
    lastRotate = Date.now();
    const t1 = Date.now();
    while (Date.now() - t1 < 90_000) {
      const up = await countUp();
      if (up > 0) {
        await discover(true);
        if (up < size) logLine({ ts: new Date().toISOString(), event: "pool_restarted_partial", reason, up, size });
        logLine({ ts: new Date().toISOString(), event: "pool_restarted", reason, up });
        return { ok: true, up, partial: up < size };
      }
      await Bun.sleep(1000);
    }
    logLine({ ts: new Date().toISOString(), event: "pool_restart_failed", reason, err: "fleet did not come up" });
    return { ok: false, error: "fleet did not come up" };
  } catch (e) {
    logLine({ ts: new Date().toISOString(), event: "pool_restart_failed", reason, err: String(e).slice(0, 160) });
    return { ok: false, error: String(e).slice(0, 160) };
  } finally {
    restartInFlight = false;
  }
}

// The fleet must be redrawn on the NEW path, but two cooldowns can refuse the
// moment the change is detected: the uplink-restart guard and the retunnel
// cooldown. Dropping the event there left the fleet on old-path addresses and
// the path lock (measured on the old fleet) gating every other redraw until a
// stray 200 or midnight - measured 2026-11-04 21:30 (WAW -> EWR skipped),
// followed by 25 client 503s in six minutes. The pending redraw is persisted in
// the uplink file, so a process restart between the change and the redraw (the
// v2.74 deploy at 21:31) still executes it on its first tick.
async function redrawAfterUplinkChange() {
  // A redraw that already ran inside the retunnel cooldown moved the fleet on
  // this path a few minutes ago: falling through to rotatePool here would spend
  // 22 fresh accounts for a redraw we just did. The route change alone does not
  // burn the addresses we hold, so do nothing and let the deferred pass retry.
  if (Date.now() - lastRetunnel < RETUNNEL_COOLDOWN_MS) {
    logLine({ ts: new Date().toISOString(), event: "uplink_redraw_recent", reason: "uplink-changed", last_s: Math.round((Date.now() - lastRetunnel) / 1000) });
    return false;
  }
  const rot = await redrawOrRotate("uplink-changed");
  // `assumed` means the daemon dropped the HTTP socket mid-rotation while the
  // rotate itself keeps going (30 s WriteTimeout) - the fleet IS changing. Only
  // a real refusal earns the retunnel fallback, otherwise every uplink change
  // would rotate and then immediately retunnel the respawning fleet.
  if (!rot.ok && !rot.assumed) { await restartPool("uplink-changed"); return true; }
  // Both rotatePool and retunnelPool schedule their own follow-up on the
  // assumed path; a third timer here measured the same redraw twice (one event
  // produced two pool_rotate_landed verdicts 4 ms apart).
  if (rot.assumed) logLine({ ts: new Date().toISOString(), event: "uplink_rotate_assumed", reason: "uplink-changed" });
  return true;
}

async function uplinkTick() {
  const cur = await probeUplink();
  if (!cur) return;
  const prev = loadUplink();
  if (!prev || !prev.ip) { uplink = cur; saveUplink(); logLine({ ts: new Date().toISOString(), event: "uplink_seen", ip: cur.ip, colo: cur.colo }); return; }
  if (prev.ip === cur.ip) {
    if (cur.colo !== prev.colo) { uplink = { ...prev, colo: cur.colo, at: cur.at }; saveUplink(); }
    // A change consumed while a cooldown was running left the fleet on the OLD
    // path; run the redraw as soon as both gates allow, and drop the lock: it
    // described the old path, not the one the retunnel will draw on.
    if (uplink.pendingFor === cur.ip
      && !restartInFlight && Date.now() - lastUplinkRestart >= UPLINK_RESTART_MIN_MS
      && Date.now() - lastRetunnel >= RETUNNEL_COOLDOWN_MS) {
      uplink = { ...uplink, pendingFor: "" };   // clear before the await: a concurrent tick must not repeat it
      saveUplink();
      pathLockUntil = 0;
      lastUplinkRestart = Date.now();
      logLine({ ts: new Date().toISOString(), event: "uplink_redraw_deferred", ip: cur.ip, since_retunnel_s: Math.round((Date.now() - lastRetunnel) / 1000) });
      const done = await redrawAfterUplinkChange();
      if (!done) { uplink = { ...uplink, pendingFor: cur.ip }; saveUplink(); }
    }
    return;
  }
  uplink = cur;
  uplink.pendingFor = "";   // a new path: the previous pending redraw is moot
  saveUplink();
  logLine({ ts: new Date().toISOString(), event: "uplink_changed", from: prev.ip, colo_from: prev.colo, to: cur.ip, colo_to: cur.colo });
  // The lock was measured on the old path; it says nothing about the new one.
  pathLockUntil = 0;
  // Same for the direct lane: its park was earned by the OLD host address.
  // Turning the VPN off changes the host's /128, and keeping the park would
  // tell the fresh window "come back at 00:00" for hours (measured: the park
  // earned on the VPN exit at 22:14 would have outlived the home-IP switch).
  frontUntil = 0;
  // In-memory parks carry no path (the book keeps untilPath, the in-memory
  // stamp does not): a verdict earned on the OLD uplink says nothing about the
  // new one, and keeping it excluded a slot for hours after the switch.
  for (const st of egress.values()) {
    if (!st) continue;
    // `hard`/`strikes` are path-less too: a lane that earned strike 2 on the old
    // uplink would be written off for hours on the first 429 of the new one.
    if (st.until) { st.until = 0; st.parkedAt = 0; }
    st.hard = 0; st.strikes = 0; st.lastStrike = 0;
  }
  try { saveState(); } catch {}
  // Address verdicts are NOT cleared here: each one carries the path it was
  // measured on (`untilPath`), and deadlineBlocked() ignores a verdict from
  // another path. Zeroing them destroyed evidence that is still valid if the
  // uplink comes back, and the earlier measurement (8 of 8 blocked in-pool
  // addresses still answering 429 under the current path) is exactly why the
  // book must not be wiped by a network event.
  if (Date.now() - lastUplinkRestart < UPLINK_RESTART_MIN_MS) {
    uplink.pendingFor = cur.ip;
    saveUplink();
    logLine({ ts: new Date().toISOString(), event: "uplink_restart_recent", last_s: Math.round((Date.now() - lastUplinkRestart) / 1000), pending: cur.ip });
    return;
  }
  lastUplinkRestart = Date.now();
  // The egress address is drawn by the HANDSHAKE, not bound to the device or
  // the account: one account answered 17 handshakes with 17 distinct /128s, so a
  // redraw after an uplink change hands out unseen addresses on the same
  // accounts (measured 2026-10-30 19:10: POST /pool/retunnel moved 22 of 22
  // slots in 23 s and the fleet served again 25 s later). The 18:44 case that
  // looked like "retunnel cannot help" ran the OLD no-op retunnel, which only
  // respawned slots that collided on a /64.
  const done = await redrawAfterUplinkChange();
  uplink.pendingFor = done ? "" : cur.ip;
  saveUplink();
}

// ---- fleet self-healing ladder -------------------------------------------
// Measured 2026-10-29 (23:00-23:46 UTC): the fleet can sit with every handshake
// failing while the router's own zero-up rescan fires once per 5 min, and the
// sidecar had no escalation at all - 115 `pool_empty` answers, each a 2 s hint
// at a wall that only a restart could move. This ladder is the last line of
// defence: a serving fleet (up > 0) resets everything, and every step is
// rate-limited so a broken machine is never hammered.
const HEAL_TICK_MS = Number(process.env.SIDECAR_HEAL_TICK_MS ?? 30_000);
const HEAL_ZERO_UP_MS = Number(process.env.SIDECAR_HEAL_ZERO_UP_MS ?? 120_000);        // up=0 this long -> rebuild the pool
const HEAL_RETRY_MS = Number(process.env.SIDECAR_HEAL_RETRY_MS ?? 15 * 60_000);         // one escalation per 15 min at most
let zeroUpSince = 0;
let daemonDownSince = 0;
let lastHealAt = 0;
let lastWallLogAt = 0;

// The daemon restart rung deliberately does NOT live here: WarpPoolWatcher
// (a scheduled task, every 5 min, warp_pool_watcher.py) already restarts
// warp-router after two dead samples (~10 min) via restart_warp.ps1, at
// most once per 30 min. A second restarter fires on the same condition with a
// different script and the two kill each other's daemon mid-warmup, stretching
// the outage instead of ending it. The sidecar only rebuilds the POOL inside a
// live daemon (fast, no process restart) and records what it sees.

async function healTick() {
  if (!POOL_API) return;
  const now = Date.now();
  const up = await countUp();
  if (up < 0) {
    // An unreachable daemon is the watcher's case (restart_warp.ps1); here it is
    // only evidence, logged once per window so the timeline has it.
    if (!daemonDownSince) daemonDownSince = now;
    if (now - daemonDownSince > HEAL_ZERO_UP_MS && now - lastHealAt > HEAL_RETRY_MS) {
      lastHealAt = now;
      logLine({ ts: new Date().toISOString(), event: "heal_daemon_down", down_s: Math.round((now - daemonDownSince) / 1000), owner: "WarpPoolWatcher" });
    }
    return;
  }
  daemonDownSince = 0;
  if (up > 0) { zeroUpSince = 0; } else {
    if (!zeroUpSince) {
      zeroUpSince = now;
      logLine({ ts: new Date().toISOString(), event: "heal_zero_up_seen", total: backends.length });
    } else {
      const downMs = now - zeroUpSince;
      if (downMs >= HEAL_ZERO_UP_MS && now - lastHealAt > HEAL_RETRY_MS) {
        // A restart already running means the last attempt has not finished
        // judging yet: logging a new escalation here made the ladder look like
        // it fired nine times in 45 s while restartInFlight refused every one.
        if (!restartInFlight) {
          lastHealAt = now;
          logLine({ ts: new Date().toISOString(), event: "heal_pool_restart", down_s: Math.round(downMs / 1000) });
          await restartPool("heal-zero-up").catch(() => {});
        }
      }
    }
  }
  // The wall a client cannot see: every exit inside an upstream window is
  // upstream quota, not a hang. One line per 15 min with the exact resume time
  // is what tells the user "wait until 04:00" instead of "it is broken".
  if (up > 0 && backends.length && now - lastWallLogAt > HEAL_RETRY_MS) {
    const blocked = backends.filter((b) => deadlineBlocked(b.ip, "inference")).length;
    if (blocked >= backends.length) {
      lastWallLogAt = now;
      const resume = nextUsableMs("inference");
      logLine({ ts: new Date().toISOString(), event: "quota_wall", total: backends.length, resume_in_s: Math.round(resume / 1000), resume_at: new Date(now + resume).toISOString() });
    }
  }
}

// Share of these exits whose per-address budget we have already seen burn.
// A fresh-but-burned set means the pool is cycling devices whose egress we
// spent earlier today: rotating again cannot help, only a new registration can.
// Share of the fleet that is parked right now. This is the live signal a "the
// pool is too small" decision needs: the address book only says an address was
// burned at some point, while a parked slot says its window is closed *now*.
function parkedShare(lane = "inference") {
  const now = Date.now();
  const list = backends.filter((b) => b.proxy);
  if (!list.length) return 0;
  let parked = 0;
  // LIVE parks only: this feeds the refill gate, which decides whether to redraw
  // the fleet. Book-blocked addresses are the `spentShare` signal instead - mixing
  // history into this share made a fleet with 20/31 written off always fail the
  // gate, i.e. a full pool off/on on every refill attempt.
  for (const b of list) if (stateFor(b.proxy, b.ip, lane).until > now) parked++;
  return parked / list.length;
}

// Earliest moment any slot can serve again: a park ends at `until`, a written-off
// slot is re-tested after HARD_PROBE_AGE_MS. This is the only honest number to
// hand a client as Retry-After.
function nextUsableMs(lane = "inference") {
  const now = Date.now();
  let best = Infinity;
  for (const b of backends) {
    const st = stateFor(b.proxy, b.ip, lane);
    let at = st.until > now ? st.until : now;
    if (st.hard && st.parkedAt) at = Math.max(at, st.parkedAt + HARD_PROBE_AGE_MS);
    // A slot whose park expired but whose ADDRESS is still in the book cannot
    // serve either: this is the dominant case (blocked 20-31 of 31), and leaving
    // it out made the honest window collapse to `now` → retry-after: 2.
    if (deadlineBlocked(b.ip, lane, now)) at = Math.max(at, now + 60_000);
    if (at < best) best = at;
  }
  return best === Infinity ? 2000 : Math.max(2000, best - now);
}

// "Spent" means a live verdict, not a historical burn: an address that served
// 200s after its last 429 is not spent, and counting it as such redraws the
// whole fleet while it is serving (measured: 18 of 31 = 0.58 against the 0.60
// trigger during a served hour).
function spentShare(ips) {
  if (!ips.length) return 0;
  let spent = 0;
  for (const ip of ips) if (deadlineBlocked(ip)) spent++;
  return spent / ips.length;
}

async function mintAccounts(reason, count) {
  if (!POOL_API || mintInFlight) return { ok: false, error: "mint busy" };
  if (Date.now() - lastMintAt < MINT_COOLDOWN_MS) return { ok: false, error: "mint cooldown" };
  mintInFlight = true;
  lastMintAt = Date.now();
  try {
    let resp = await postRegister(count);
    let body = (await resp.text()).slice(0, 200);
    // The account cap is a hard wall (warp-router maxAccounts=200) and the
    // devices we want to retire are exactly the oldest ones: their egress is
    // what the burn book holds. Without this the lever dies after a few mints.
    if (!resp.ok && /limit/i.test(body)) {
      const freed = await pruneOldAccounts(count);
      logLine({ ts: new Date().toISOString(), event: "accounts_prune", reason, freed, refused: body });
      if (freed > 0) {
        resp = await postRegister(count);
        body = (await resp.text()).slice(0, 200);
      }
    }
    let jobId = "";
    if (resp.ok) {
      mintedTotal += count;
      try { jobId = (JSON.parse(body) || {}).job_id || ""; } catch {}
    }
    logLine({ ts: new Date().toISOString(), event: resp.ok ? "accounts_mint" : "accounts_mint_refused", reason, count, status: resp.status, job: jobId, body, minted_total: mintedTotal });
    return { ok: resp.ok, status: resp.status, job_id: jobId, body };
  } catch (e) {
    logLine({ ts: new Date().toISOString(), event: "accounts_mint_failed", reason, err: String(e).slice(0, 160) });
    return { ok: false, error: String(e).slice(0, 160) };
  } finally {
    mintInFlight = false;
  }
}

function poolHeaders() {
  return { "content-type": "application/json", ...(TOKEN ? { authorization: `Bearer ${TOKEN}` } : {}) };
}

function postRegister(count) {
  return fetch(`${POOL_API}/api/v1/accounts`, {
    method: "POST",
    headers: poolHeaders(),
    body: JSON.stringify({ count, delay_s: MINT_DELAY_S }),
    signal: AbortSignal.timeout(20_000),
  });
}

// Registration is a job (count x delay_s seconds), so a rotate fired on the
// 202 only ever sees the old account set. Wait for the job, then respawn.
async function waitJob(jobId, timeoutMs = 180_000) {
  if (!jobId) return null;
  const end = Date.now() + timeoutMs;
  while (Date.now() < end) {
    try {
      const r = await fetch(`${POOL_API}/api/v1/jobs/${encodeURIComponent(jobId)}`, { headers: poolHeaders(), signal: AbortSignal.timeout(10_000) });
      if (r.ok) {
        const j = await r.json();
        if (j && j.state && j.state !== "running" && j.state !== "pending") return j;
      }
    } catch {}
    await Bun.sleep(3_000);
  }
  return null;
}

// Endpoints decide which colo pool a tunnel draws its egress from: measured
// 2026-10-28, one account, one uplink, three endpoints -> three DIFFERENT
// unseen egresses (4e50:2705::3e3:38 / 4e57:270f::3e4:50 / 4e53:26fa::3e2:f).
// Rotating the same slots over the same endpoints keeps drawing the same
// exhausted set, so the dead-end recovery re-spreads the endpoints (pool
// off/on respawns every slot from the scanner cache) and only then rotates.
// Only fires when nothing is serving: the off/on drops every lease.
async function poolSet(body) {
  return fetch(`${POOL_API}/api/v1/pool`, {
    method: "POST",
    headers: poolHeaders(),
    body: JSON.stringify(body),
    signal: AbortSignal.timeout(60_000),
  });
}

async function respreadPool(reason, size) {
  const off = await poolSet({ enabled: false }).catch((e) => ({ ok: false, status: 0, err: String(e).slice(0, 120) }));
  await Bun.sleep(1_500);
  const on = await poolSet({ enabled: true, size }).catch((e) => ({ ok: false, status: 0, err: String(e).slice(0, 120) }));
  logLine({ ts: new Date().toISOString(), event: "pool_respread", reason, size, off_status: off.status, on_status: on.status });
  return { ok: !!on.ok, status: on.status };
}

// A fresh egress slot costs a HANDSHAKE, not an account: one account answered
// five handshakes with five different /64s (measured 2026-10-28 on an isolated
// stand, same endpoint and uplink), and 10 freshly registered devices bought
// nothing that a respawn had not already given (16 unseen exits, all 429).
// So the first move when the held slots are spent is a retunnel: warp-router
// kills each worker and respawns it on the SAME account, CF draws a new /64,
// and no reserve identity is burned.
// Widen the fleet instead of re-registering devices: warp-router spawns the
// extra workers from the accounts it already holds, each with its own handshake
// and its own budget window. Measured 2026-10-28: 8 slots (8 /64) could not
// carry a heavy session; 8 -> 20 slots produced 17 up / 17 unique /64 within
// 25 s and 200s resumed without touching a single account.
let growInFlight = false;
async function growPool(reason) {
  if (!POOL_API) return { ok: false, error: "no pool" };
  // Growth is a full stop+start of the direct fleet. Landing it while a
  // rotation/retunnel owns the same slots collapses discovery: measured
  // 2026-11-02 19:36, grow during a retunnel reported backends:3 to every
  // request for 36 s. The flag must be held for the whole stop+enable, not just
  // checked on entry, or the retunnel scheduled right behind a grow walks into
  // a stopped pool.
  if (rotateInFlight || growInFlight) return { ok: false, error: "rotate in flight" };
  if (Date.now() - lastGrowAt < GROW_COOLDOWN_MS) return { ok: false, error: "grow cooling" };
  if (growCeiling && backends.length > growCeiling) growCeiling = 0;
  if (growCeiling && backends.length >= growCeiling - 1) return { ok: false, error: "pool at contract max", size: backends.length };
  const cur = backends.length || 0;
  const target = Math.min(POOL_MAX, Math.max(cur + POOL_STEP, cur + 1));
  // Growing is POST {enabled:false} → {enabled:true,size}, i.e. mgr.Stop() of
  // every worker and a fresh full start - a whole-fleet outage, not a rolling
  // restart. Paying that for a single extra slot (POOL_MAX is 32 and the fleet is
  // already ~31) is strictly worse than a rotate, which redraws with stagger.
  if (cur && target - cur < 2) return { ok: false, error: "pool at max", size: cur };
  lastGrowAt = Date.now();
  growInFlight = true;
  try {
  const off = await poolSet({ enabled: false }).catch((e) => ({ ok: false, status: 0, err: String(e).slice(0, 120) }));
  await Bun.sleep(1_500);
  const on = await poolSet({ enabled: true, size: target }).catch((e) => ({ ok: false, status: 0, err: String(e).slice(0, 120) }));
  logLine({ ts: new Date().toISOString(), event: "pool_grow", reason, from: cur, to: target, off_status: off.status, on_status: on.status });
  if (!on.ok) {
    // The stop was already accepted: leaving it off turns a failed grow into a
    // dark direct fleet until a mint (30 min) or healTick (15 min) respreads.
    let re = await poolControl(true, cur || 20).catch(() => ({ ok: false }));
    for (let attempt = 0; attempt < 3 && !re.ok; attempt++) {
      await Bun.sleep(2000);
      re = await poolControl(true, cur || 20).catch(() => ({ ok: false }));
    }
    logLine({ ts: new Date().toISOString(), event: "pool_grow_reenable", reason, ok: re.ok });
    return { ok: false, status: on.status, size: cur };
  }
  lastRespreadAt = Date.now();
  await discover(true).catch(() => {});
  // A start returns as soon as the workers are SPAWNED (state=starting); the
  // first `up` appears seconds later. Reading it once declared a warming fleet
  // dead 19 ms after the enable and set the caller up for a second redraw.
  let up = await countUp();
  const waitStart = Date.now();
  while (up <= 0 && Date.now() - waitStart < 90_000) {
    await Bun.sleep(1000);
    up = await countUp();
  }
  if (up <= 0) {
    logLine({ ts: new Date().toISOString(), event: "pool_grow_dead", reason, size: target, up });
    return { ok: false, error: "fleet did not come up", size: target };
  }
  if (up < target) {
    // Behind the merge shim the direct size is clamped to the contract, so a
    // grow tears down the fleet and buys nothing: say so instead of reporting
    // a growth that did not happen.
    logLine({ ts: new Date().toISOString(), event: "pool_grow_short", reason, size: target, up });
    // The union size we tried to grow beyond, not `up`: behind the shim `up` is
    // the direct half only, so comparing it with the union refused forever.
    growCeiling = Math.max(cur, up);
    return { ok: false, error: "fleet did not grow", size: target, up };
  }
  return { ok: true, size: target, up };
  } finally {
    growInFlight = false;
  }
}

async function retunnelPool(reason) {
  if (!POOL_API) return { ok: false, error: "no pool" };
  if (rotateInFlight || growInFlight) return { ok: false, error: "rotate in flight" };
  if (Date.now() - lastRetunnel < RETUNNEL_COOLDOWN_MS) return { ok: false, error: "retunnel cooling" };
  rotateInFlight = true;
  const before = new Set(backends.map((b) => b.ip).filter(Boolean));
  try {
    const resp = await fetch(`${POOL_API}/api/v1/pool/retunnel`, {
      method: "POST",
      headers: { "content-type": "application/json", ...(TOKEN ? { authorization: `Bearer ${TOKEN}` } : {}) },
      body: "{}",
      signal: AbortSignal.timeout(RETUNNEL_TIMEOUT_MS),
    });
    const body = (await resp.text()).slice(0, 200);
    lastRetunnel = Date.now();
    logLine({ ts: new Date().toISOString(), event: "pool_retunnel", reason, status: resp.status, body });
    // Only a retunnel that the pool accepted redraws the slots; on a refusal the
    // caller falls through to a registration job, which schedules its own
    // follow-up. Measuring a refused retunnel would double-count the ladder.
    lastRetunnelOk = resp.ok;
    if (resp.ok) rotateFollowUp(reason + ":retunnel", before);
    return { ok: resp.ok, status: resp.status, body };
  } catch (e) {
    lastRetunnel = Date.now();
    lastRetunnelOk = false;
    const msg = String(e).slice(0, 160);
    logLine({ ts: new Date().toISOString(), event: "pool_retunnel_inflight", reason, err: msg });
    rotateFollowUp(reason + ":retunnel", before);
    return { ok: false, assumed: true, error: msg };
  } finally {
    rotateInFlight = false;
  }
}

// Retire the oldest devices (their egress is the spent one) so a registration
// has room under the cap. Active accounts are skipped: a backend may be serving.
async function pruneOldAccounts(count) {
  try {
    const r = await fetch(`${POOL_API}/api/v1/accounts`, { headers: poolHeaders(), signal: AbortSignal.timeout(10_000) });
    if (!r.ok) return 0;
    // A node worker holds its identity by a fixed offset in wide_state.json,
    // outside the pool's `active` flag. Deleting that account leaves the worker
    // unable to restart (the slot stays down until a hand edit).
    const wideAccounts = new Set();
    try {
      const wideState = process.env.SIDECAR_WIDE_STATE || "";
      if (wideState) {
        const ws = JSON.parse(readFileSync(wideState, "utf8"));
        if (Array.isArray(ws)) for (const row of ws) if (row && row.acc) wideAccounts.add(String(row.acc));
      }
    } catch (e) {
      // The file exists but cannot be read (a torn write): deleting accounts
      // then would take the node workers' identities. Refuse instead.
      if (e && e.code !== "ENOENT") return 0;
    }
    const list = ((await r.json()).accounts || []).slice();
    list.sort((a, b) => String(a.created_at).localeCompare(String(b.created_at)));
    let freed = 0;
    for (const a of list) {
      if (freed >= count) break;
      if (!a || !a.id || a.active) continue;
      if (wideAccounts.has(String(a.id))) continue;
      const d = await fetch(`${POOL_API}/api/v1/accounts/${encodeURIComponent(a.id)}`, { method: "DELETE", headers: poolHeaders(), signal: AbortSignal.timeout(10_000) });
      if (d.ok) freed++;
    }
    return freed;
  } catch {
    return 0;
  }
}

// Dead-end refill: mint devices (a new device draws an unseen egress), give the
// endpoints a fresh spread (the colo pool is what a tunnel draws from), then
// rotate. Guarded by the mint cooldown and by "nothing has served recently".
async function refillCapacity(reason, forceMint = false) {
  if (!MINT_ENABLED) return { ok: false, error: "mint disabled" };
  if (pathLocked()) return { ok: false, error: "path locked" };
  // The "nothing served recently" gate has to come FIRST: it used to sit after
  // the retunnel block, so every refill tore down and respawned all WARP workers
  // even when a 200 was served a fraction of a second earlier (measured
  // 21:25:12 and 22:28:21: three fleet redraws in 28 s while requests were being
  // served, each replacing 100% of the addresses).
  if (Date.now() - lastOKAt <= ROTATE_IDLE_MS) return { ok: false, error: "still serving" };
  // First move: redraw the slots on the accounts we already hold. Measured
  // 2026-10-28: retunnel moved 19 workers from 16 unique /64 (3 collisions) to
  // 18 (1 collision) and to completely new prefixes (1b29-1b2e) without adding
  // a single account; the mint path had produced 16 unseen exits that all 429'd.
  // forceMint skips the redraw steps: the caller already measured that a redraw
  // bought addresses and no service (`redraw_sterile`).
  if (!forceMint) {
    const rt = await retunnelPool(reason + ":refill");
    // A redraw that lands the same prefixes buys nothing: judge it by the book,
    // not by the HTTP status. Read the fleet AFTER the redraw, otherwise the
    // stale spent set would force a pointless growth.
    if (rt.ok) await discover(true).catch(() => {});
    // A COOLDOWN refusal is not a verdict about the fleet (every refill follows a
    // redraw that just stamped the gate). A daemon refusal or a transport error
    // is a broken lever, not a capacity reading: there a registration job is the
    // only path left, so the grow rung stays out of it.
    const capacityUnknown = rt.ok
      || ((rt.error === "retunnel cooling" || rt.error === "rotate in flight") && lastRetunnelOk);
    // A REFUSED retunnel usually means its own 5-min gate: every refill is
    // scheduled 60 ms after a redraw, and that redraw just stamped lastRetunnel.
    // Requiring rt.ok therefore skipped the grow rung on every pass and paid for
    // a mint instead (measured: pool_retunnel -> pool_rotate_norefresh ->
    // mint_only_spent -> accounts_mint x6 with no pool_grow). Judge the fleet we
    // have now; the cooldown says nothing about its capacity.
    const ips = backends.map((b) => b.ip).filter(Boolean);
    // Two independent readings: the book (addresses burned at some point) and
    // the live parks (windows closed right now). Either says the redraw bought
    // nothing and the fleet needs more addresses.
    const spent = spentShare(ips) >= MINT_SPENT_SHARE || parkedShare() >= MINT_SPENT_SHARE;
    if (!spent) return rt;
    if (capacityUnknown) {
      const g = await growPool(reason + ":refill");
      if (g.ok) return g;
    }
    // Falling through to the registration job is the only rung that still adds
    // addresses: returning the successful-but-sterile retunnel here left a fleet
    // redrawn onto spent prefixes dark until an unrelated trigger.
  }
  if (Date.now() - lastOKAt <= ROTATE_IDLE_MS) return { ok: false, error: "still serving" };
  const count = MINT_COUNT || Math.max(2, Math.min(backends.length || 20, MINT_CAP));
  const r = await mintAccounts(reason, count);
  if (!r.ok) return r;
  const done = await waitJob(r.job_id);
  logLine({ ts: new Date().toISOString(), event: "accounts_mint_done", reason, job: r.job_id, state: done ? done.state : "timeout", msg: done ? String(done.message || "").slice(0, 120) : "" });
  // A failed/timed-out job registered nothing; the 180 s wait may also have
  // produced a 200 or a live stream, and the whole-fleet respread would cut it.
  if (!done || done.state !== "done") return r;
  if (liveStreams > 0 || pathLocked() || Date.now() - lastOKAt <= ROTATE_IDLE_MS) return r;
  if (Date.now() - lastRespreadAt > RESPRED_COOLDOWN_MS) {
    lastRespreadAt = Date.now();
    await respreadPool(reason, backends.length || 20).catch(() => {});
  }
  rotatePool("after-mint").catch(() => {});
  return r;
}

function rotateFollowUp(reason, beforeIps) {
  const parkedBefore = new Set(parkedKeys());
  setTimeout(async () => {
    try {
      await discover(true);
      const after = new Set(backends.map((b) => b.ip).filter(Boolean));
      const freshList = [...after].filter((ip) => !beforeIps.has(ip));
      const fresh = freshList.length;
      if (fresh > 0) {
        // Do NOT clear the ladder here: fresh addresses are not the same thing as
        // serving ones. Any 200 resets it (see the status===200 branch).
        // How many parked rows the new identities actually released: a rotation
        // that mints fresh addresses must unpark them, otherwise the answer to
        // "did the fleet really move?" is still no.
        const parkedAfter = new Set(parkedKeys());
        const cleared = [...parkedBefore].filter((k) => !parkedAfter.has(k)).length;
        const spent = spentShare(freshList);
        const moved = after.size ? fresh / after.size : 0;
        logLine({ ts: new Date().toISOString(), event: "pool_rotate_landed", reason, fresh, moved: +moved.toFixed(2), spent: +spent.toFixed(2), total: after.size, cleared, parked: parkedAfter.size });
        // A fresh address is not a serving one: arm the sterility watch, and
        // clear it on the first 200. Self-inflicted redraws (refill/mint cycles)
        // do not arm it, or a dead ladder would mint in a loop.
        if (!/:refill|after-mint|:sterile/.test(reason)) {
          // Repeated rotations must not postpone the verdict: keep the first
          // clock and merge the addresses.
          if (!redrawWatch) redrawWatch = { at: Date.now(), reason, ips: new Set(freshList) };
          else for (const ip of freshList) redrawWatch.ips.add(ip);
        }
        // "new to the pool" is not "the fleet moved": measured 2026-10-30 18:52:55,
        // a retunnel across an uplink change landed 1 of 22 slots (fresh:1 spent:0)
        // and this branch read it as a success while 21 slots kept the addresses
        // that path had already burned.
        if (moved < REDRAW_MOVED_MIN) {
          logLine({ ts: new Date().toISOString(), event: "pool_redraw_stuck", reason, fresh, moved: +moved.toFixed(2), total: after.size });
          maybeEscalateReuse(after.size - fresh, after.size);
        }
        // "New to the pool" is not "new to us": the egress is fixed per account,
        // so a rotation can hand back exits we already burned today. Registering
        // devices is the only way to mint unseen exits on an unchanged uplink.
        if (spent >= MINT_SPENT_SHARE) {
          logLine({
            ts: new Date().toISOString(), event: "mint_only_spent", reason, fresh, spent: +spent.toFixed(2), minted_total: mintedTotal,
            hint: "rotation returned exits already burned; registering fresh accounts (POST /api/v1/accounts) - a VPN only changes the registration edge",
          });
          refillCapacity(reason).catch(() => {});
        }
      } else {
        // The rewind assumed the 10-min rung: on the 90 s rung it left
        // now-lastRotate at 510 s, i.e. the gate passed at once and the log
        // promised a 90 s retry that never happened.
        lastRotate = Date.now();
        // Nothing new at all: the pool can only re-serve the set we already
        // hold. If that set is spent, rotating again is pointless - this is the
        // measured shape of the 2026-10-28 16:10-16:20 dead window (297 of 297
        // 429s came from already-burned addresses while 7 rotations produced
        // zero new ones and the refill never fired because it was gated on
        // fresh > 0).
        const spentAll = spentShare([...after]);
        logLine({ ts: new Date().toISOString(), event: "pool_rotate_norefresh", reason, total: after.size, spent: +spentAll.toFixed(2), retry_in_s: Math.round(ROTATE_SOON_MS / 1000) });
        if (spentAll >= MINT_SPENT_SHARE) {
          logLine({
            ts: new Date().toISOString(), event: "mint_only_spent", reason, fresh: 0, spent: +spentAll.toFixed(2), total: after.size, minted_total: mintedTotal,
            hint: "rotation returned no new exits and every held exit is burned; registering fresh accounts and respreading endpoints",
          });
          refillCapacity(reason).catch(() => {});
        }
      }
    } catch {}
  }, ROTATE_CHECK_MS).unref?.();
}

// Whole-pool rotation: warp-router restarts every backend on a new WARP
// identity. Only /admin/rotate-fresh (and the all-closed path) call this: a
// rotation is disruptive, so it is never a per-request reaction.
// A redraw re-handshakes every slot on the SAME account and the egress address
// is drawn by the handshake, not fixed to the identity: measured 2026-10-30,
// POST /api/v1/pool/retunnel moved 22 of 22 slots to unseen /128s in 23 s and
// the fleet answered 200 again 25 s later. It therefore replaces a rotation
// whenever it can run, because a rotation SPENDS a fresh identity (22 accounts
// per call) and buys the same thing.
async function redrawOrRotate(reason = "manual") {
  const rt = await retunnelPool(reason);
  // `assumed` means the socket dropped mid-redraw: the fleet IS changing and a
  // follow-up is already scheduled (retunnelPool). Falling through would rotate
  // a second time for the same event and let a daemon-skipped rotation be
  // counted as a completed one.
  if (rt.ok || rt.assumed) return rt;
  // Our OWN cooldown is not a refusal: rotating here would spend a fresh
  // identity for the same event and double-count the ladder (the 2026-10-28
  // lesson: a refused retunnel must not schedule a rotate follow-up).
  if (rt.error === "retunnel cooling" || rt.error === "rotate in flight") return rt;
  return rotatePool(reason);
}

async function rotatePool(reason = "manual") {
  if (!POOL_API) return { ok: false, error: "no pool" };
  if (!ALLOW_ROTATE) return { ok: false, error: "rotate disabled" };
  if (rotateInFlight || growInFlight) return { ok: false, error: "rotate in flight" };
  rotateInFlight = true;
  const before = new Set(backends.map((b) => b.ip).filter(Boolean));
  try {
    const resp = await fetch(`${POOL_API}/api/v1/pool/rotate`, {
      method: "POST",
      headers: { "content-type": "application/json", ...(TOKEN ? { authorization: `Bearer ${TOKEN}` } : {}) },
      body: JSON.stringify({ reason }),
      signal: AbortSignal.timeout(ROTATE_TIMEOUT_MS),
    });
    const body = (await resp.text()).slice(0, 300);
    lastRotate = Date.now();
    logLine({ ts: new Date().toISOString(), event: "pool_rotate", reason, status: resp.status, body });
    // A 200 is not "the fleet moved": the daemon answers 200/respawned:N even for
    // a rotate it skipped, and this path is the one that fires on the live lane.
    // Measure the set we held against the set we got.
    if (resp.ok) rotateFollowUp(reason, before);
    return { ok: resp.ok, status: resp.status, body };
  } catch (e) {
    // The cooldown must advance here too: otherwise every request that finds the
    // pool empty fires another rotate (measured 2026-10-27: four attempts 44 s
    // apart). The socket close is expected while warp-router respawns the fleet:
    // its handler runs inside a 30 s WriteTimeout and the rotation keeps going
    // after the client is dropped (measured 2026-10-28 12:24 - the "failure" was
    // followed by backends coming up on new identities). So wait for the fleet
    // instead of writing the pool off.
    lastRotate = Date.now();
    const msg = String(e).slice(0, 160);
    logLine({ ts: new Date().toISOString(), event: "pool_rotate_inflight", reason, err: msg });
    rotateFollowUp(reason, before);
    return { ok: false, assumed: true, error: msg };
  } finally {
    rotateInFlight = false;
  }
}

async function discover(force = false) {
  // An unreadable book kept writes disabled forever (a sharing violation at
  // startup was permanent amnesia). Retry it on the discovery cadence instead -
  // before the stand-mode guard, so a stand retries too.
  if (!ipLoaded && ipRetryAt && Date.now() >= ipRetryAt) loadIps();
  if (!POOL_API) return backends; // stand mode: no pool, single direct target
  const now = Date.now();
  if (!force && now - lastDiscover < REFRESH_MS && backends.length > 0) return backends;
  lastDiscover = now;
  try {
    const resp = await fetch(`${POOL_API}/api/v1/pool`, {
      headers: TOKEN ? { authorization: `Bearer ${TOKEN}` } : {},
      signal: AbortSignal.timeout(5000),
    });
    if (!resp.ok) {
      const t = Date.now();
      if (t - lastPoolRejectLog > 60000) {
        lastPoolRejectLog = t;
        logLine({ ts: new Date().toISOString(), event: "pool_api_reject", status: resp.status, pool: POOL_API });
      }
      lastDiscover = now - REFRESH_MS + 5000;
      lastDiscoverOk = lastDiscoverOk || 0;
      return backends;
    }
    const data = await resp.json();
    lastDiscoverOk = Date.now();
    const list = Array.isArray(data.backends) ? data.backends : [];
    const mapped = list
      .filter((b) => b && b.state === "up" && b.http_addr)
      .map((b) => ({ proxy: `http://127.0.0.1:${String(b.http_addr).split(":").pop()}`, ip: b.public_ip || "", kind: b.kind === "wide" ? "wide" : "main" }));
    // One IP must never be served by two slots: two ports behind one address are
    // one window, and routing to both halves the pool while looking bigger.
    const { kept, dups } = dedupeByIp(mapped);
    ipDupCount = dups.length;
    for (const d of dups) {
      logLine({ ts: new Date().toISOString(), event: "ip_dup", ip: d.ip, kept: d.kept, dropped: d.dropped });
    }
    backends = kept;
    poolHighWater = Math.max(poolHighWater, kept.length);
    noteIps(backends);
    for (const b of backends) {
      for (const lane of LANES) {
        // stateFor() resets every address-scoped score when the port is handed a
        // new identity: a stale park/budget must not follow the old IPv6. The
        // reset lives there (one place that learns the address), so this loop
        // only materialises the rows.
        stateFor(b.proxy, b.ip, lane);
        // `burn64` below is observation only, never a reason to park: on
        // /inference the budget is per address (measured 2026-10-27: ::32:132
        // answered FreeUsageLimit ra=1121 while ::32:144, same /64 48f3:1f5,
        // served continuous 200s). Only the legacy zen handler documents a per-/64
        // budget, and even there parking the group is a coarse shortcut.
      }
    }
  } catch {
    // warp-router not up yet (typical right after a reboot): retry in 5s
    // instead of idling for the full refresh interval.
    lastDiscover = now - REFRESH_MS + 5000;
  }
  return backends;
}

// Measured 2026-10-29: the pool mixes egresses the free lane actually serves
// with egresses it refuses outright, and the refusal is stable per address
// (25 slots swept with one body: 4 answered 200, 21 answered FreeUsageLimit on
// every request, including a small 90 KB one). Those 21 are not "spent
// windows" - they never served a single request in two days of log. Sorting by
// least-used sprayed traffic into them, so every client request walked six
// dead egresses before landing on a live one, which is exactly the 429 storm
// the monitors saw. Rank by productivity instead: proven first, untested next,
// refused-only last, so a rotation still gets its first probe but the bulk of
// the traffic goes where it is served.
const PROVEN_MS = Number(process.env.SIDECAR_PROVEN_MS || 6 * 3600_000);

function egressRank(s, now) {
  // A transport failure is direct evidence the slot cannot serve RIGHT NOW and
  // outranks any earlier success: without this line a slot that hung a minute
  // ago still ranked 0 for 6 h (PROVEN_MS) and the picker kept choosing it.
  if (s.hungAt && now - s.hungAt < HUNG_DEMOTE_MS) return 2;
  if (s.lastOK && now - s.lastOK < PROVEN_MS) return 0; // served recently
  if (!s.lastOK) return s.bad >= 2 ? 2 : 1;             // untested, then refused-only
  return 3;                                             // proven once, silent since
}

// A /64 is the unit the free lane's window is inherited by: a BRAND-NEW address
// inside an already-burned prefix answered FreeUsageLimitError on its very first
// request (measured 2026-11-11 21:37Z: fresh account 18bef2bc minted at 11:55Z,
// never in a slot, new /128 2a09:bac1:6520::19b:12 -> ra=8 548 s while burn64
// held 3 earlier hits for 2a09:bac1:6520). The prefix is not absolute - a mixed
// /64 does exist (2026-10-27: ::32:132 429, ::32:144 200) - so this is a
// PREFERENCE: a proven address keeps its rank, an unproven one inside a burned
// group goes last instead of being handed a guaranteed refusal.
function groupBurned(ip, now = Date.now()) {
  const p = prefix64(ip);
  if (!p || !p.includes(":")) return false;
  const g = burn64[p];
  return !!(g && g.until > now);
}

// Least-used live egress not already tried for THIS request: a burned window
// costs one upstream call and the client never sees the 429.
function pickExcept(tried, lane = "inference") {
  const now = Date.now();
  const live = backends.filter((b) => !tried.has(b.proxy) && stateFor(b.proxy, b.ip, lane).until <= now);
  if (live.length === 0) return null;
  // An address the upstream already gave an hours-class window is a verdict,
  // not a hunch (887 of 993 such addresses never served another 200): do not
  // spend an upstream call on it while an unblocked address exists.
  const open = live.filter((b) => !deadlineBlocked(b.ip, lane, now));
  const usable = open.length > 0 ? open : live;
  if (open.length === 0 && !blockedOnlyLogged) {
    blockedOnlyLogged = true;
    logLine({ ts: new Date().toISOString(), event: "pool_deadline_only", lane, live: live.length });
  }
  // A burned /64 goes last: addresses inside such a prefix have almost never
  // served (measured 2026-11-11: 386 of 512 prefixes with traffic gave 0 x 200
  // in a day) and a fresh /128 inside one still inherits the group's verdict.
  // A proven address keeps its rank - the prefix is evidence, not a rule.
  const groupSafe = (b) => {
    const s = stateFor(b.proxy, b.ip, lane);
    return !!(s.lastOK && now - s.lastOK < PROVEN_MS) || !groupBurned(b.ip, now);
  };
  const preferred = usable.filter(groupSafe);
  const picked = preferred.length > 0 ? preferred : usable;
  if (preferred.length === 0 && usable.length > 0 && !groupBlockedLogged) {
    groupBlockedLogged = true;
    logLine({ ts: new Date().toISOString(), event: "pool_group_burned_only", lane, live: usable.length });
  }
  // Load spread belongs in the ORDER, never in a filter: `isLow` is `ok >=
  // softBudget` and `ok` only grows on 200s, so filtering removes exactly the
  // egresses that serve while refused ones (ok=0) always stay eligible. Measured
  // 23:08: the fleet's most productive address (ok=30, soft_budget=18) was
  // filtered out and the same request walked five refused egresses before it.
  const pool = picked.slice();
  // Ties (equal tier, equal isLow, equal ok - every freshly drawn egress has
  // ok 0) must not be settled by `backends` order: that listed the direct fleet
  // first, so untested direct slots took every new pin and the node half never
  // served (measured 2026-11-03 01:01Z: a fresh conversation went to 8764 while
  // 20 node slots sat at ok 0). Shuffle; the stable sort keeps tier/isLow/ok.
  for (let i = pool.length - 1; i > 0; i--) {
    const j = Math.floor(Math.random() * (i + 1));
    [pool[i], pool[j]] = [pool[j], pool[i]];
  }
  // Untested and proven addresses are the same tier. Ranked strictly, a pool
  // with 19 proven slots never gave a freshly drawn address its first request,
  // so adding capacity changed nothing (measured 2026-11-03: 20 new node slots
  // discovered, ok=0 for all of them). Within the tier the least-used address
  // goes first, which is what spreads a burst over the whole fleet. Refused-only
  // (2) and silent (3) stay behind: those are verdicts, not freshness.
  const tier = (st) => { const r = egressRank(st, now); return r <= 1 ? 0 : r; };
  pool.sort((a, b) => {
    const sa = stateFor(a.proxy, a.ip, lane);
    const sb = stateFor(b.proxy, b.ip, lane);
    return tier(sa) - tier(sb)
      || Number(isLow(sa)) - Number(isLow(sb))
      || sa.ok - sb.ok
      // Same tier and load: a fast first byte is the tiebreak. The fleet's p50
      // TTFB is 3.2 s, but a few slots answered in 16-65 s and were picked like
      // everyone else, so every such pick looked like a hang. Buckets of 4 s
      // keep the spread from collapsing onto one egress; 3 s is "unknown/new".
      || Math.round((sa.ttfb || 3000) / 4000) - Math.round((sb.ttfb || 3000) / 4000);
  });
  return pool[0];
}

// ---- egress IP uniqueness ------------------------------------------------
// Every backend must be a distinct public IP, and a newly appearing backend
// must not reuse an IP we have already spent. warp-router can hand out the same
// IPv6 twice (measured: 19 backends, 18 IPs) and nothing stops a rotate from
// returning a name we already burned. The registry below enforces what the pool
// does not: duplicate slots are excluded from routing, every IP is remembered
// with its burn count, and a repeat is logged as `ip_reuse` instead of quietly
// burning the same window twice.
const IP_FILE = process.env.SIDECAR_IP_STATE || statePath("v2_ips.json");
const IP_HISTORY_CAP = 4000;
let ipLoaded = false;      // false until a readable book was found: no writes
let ipRetryAt = 0;         // when an unreadable book may be retried
let ipHistory = {};        // ip -> { first, last, seen, burned, port }
// /64 -> { until, at, ra, hits }: the budget is spent per prefix, so this is what
// a slot inherits when warp-router hands it a new address inside a burned group.
let burn64 = {};
let lastPresent = new Set(); // ips present at the previous discover
let ipDupCount = 0;
let ipReuseCount = 0;
let ipClosedReuseCount = 0;
let ipInit = false; // first discovery seeds history without counting reuses
// A redraw that lands on addresses the book already knows is not a redraw. The
// egress is a function of the WARP device: in v2_ips.json 109 of 1726 low-64
// device tags span >= 5 different /64 prefixes (::a6:8 spans 16), so one device
// re-draws inside a finite universe and its burned addresses come back.
// Measured 2026-10-30 18:44-18:52: four redraws produced 46 ip_reuse rows
// (13 of 22 slots at 18:44:17), every landed set carried 8-10 slots that were
// dead on arrival (spent 0.36-0.45, under the 0.60 mint gate), and the fleet
// answered 429/503 for 9.5 min with no escalation. Reuse is the only signal that
// says "this path's device universe is exhausted", so it gets its own ladder:
// rotate (other devices), then grow/mint (more devices).
const REUSE_ESCALATE_N = Number(process.env.SIDECAR_REUSE_ESCALATE_N ?? 4);
const REUSE_ESCALATE_SHARE = Number(process.env.SIDECAR_REUSE_ESCALATE_SHARE ?? 0.25);
const REUSE_ROTATE_COOLDOWN_MS = Number(process.env.SIDECAR_REUSE_ROTATE_COOLDOWN_MS ?? 10 * 60_000);
const REUSE_MINT_WINDOW_MS = Number(process.env.SIDECAR_REUSE_MINT_WINDOW_MS ?? 15 * 60_000);
const REDRAW_MOVED_MIN = Number(process.env.SIDECAR_REDRAW_MOVED_MIN ?? 0.5);
let lastReuseRotate = 0;
let reuseRungAt = 0;

// State files are written from a process the supervisor kills with -Force on
// every restart, so a half-written json is a normal event. Write to a sibling
// and rename: the reader then always sees either the old or the new file.
function writeJsonAtomic(file, text) {
  // The state files carry an explicit DACL that the deploy path (and the
  // operator) re-applies. rename() creates a NEW file, which inherits the
  // directory ACEs and silently drops that protection - measured when
  // v2_pins.json went back to Authenticated-Users-Modify after one save. Write
  // in place instead: truncating the existing file keeps its security
  // descriptor. A torn write leaves invalid JSON, which every reader already
  // treats as "start over" (readJsonWithFallback picks .tmp if it is newer).
  try {
    writeFileSync(file, text);
    writeFileSync(`${file}.tmp`, text); // kept for the fallback reader
    return;
  } catch {}
  const tmp = `${file}.tmp`;
  writeFileSync(tmp, text);
  renameSync(tmp, file);
}

function readJsonWithFallback(file) {
  // A failed rename (Windows sharing violation, AV) leaves a NEWER tmp beside an
  // OLDER main; parsing the main first silently serves the older book. Pick by
  // mtime instead of by name.
  let main = null;
  let tmp = null;
  try { main = { at: statSync(file).mtimeMs, data: JSON.parse(readFileSync(file, "utf8")) || null }; } catch {}
  try { tmp = { at: statSync(`${file}.tmp`).mtimeMs, data: JSON.parse(readFileSync(`${file}.tmp`, "utf8")) || null }; } catch {}
  if (main && tmp) {
    // A torn tmp (a kill between writeFileSync and renameSync) parses to null;
    // picking by age alone would throw away a readable main and the next save
    // would write the empty book over 1800 rows. Validity first, age second.
    if (!main.data) return tmp.data;
    if (!tmp.data) return main.data;
    return tmp.at > main.at ? tmp.data : main.data;
  }
  return (main || tmp || {}).data || null;
}

// A row that carries a live upstream verdict must outlive the cap: deleting it
// makes the book forget a block it was built to remember (and loses `burned`,
// so a returning address is counted fresh instead of a reuse).
function liveDeadlineMs(row, now = Date.now()) {
  if (!row) return 0;
  const byLane = row.untilByLane || {};
  let mx = row.until || 0;
  for (const v of Object.values(byLane)) if (Number.isFinite(v) && v > mx) mx = v;
  return mx > now ? mx : 0;
}

function pruneIps(now = Date.now()) {
  const keys = Object.keys(ipHistory);
  if (keys.length <= IP_HISTORY_CAP) return 0;
  const dead = keys.filter((k) => !liveDeadlineMs(ipHistory[k], now))
    .sort((a, b) => (ipHistory[a].last || 0) - (ipHistory[b].last || 0));
  let drop = keys.length - IP_HISTORY_CAP;
  let n = 0;
  for (const k of dead) { if (n >= drop) break; delete ipHistory[k]; n++; }
  return n;
}

function loadIps() {
  const data = readJsonWithFallback(IP_FILE);
  if (data && typeof data === "object" && Object.keys(data).length) {
    ipHistory = data;
    ipLoaded = true;
    ipRetryAt = 0;
    return;
  }
  // The three cases must be told apart, because the response to each is
  // different and two of them were wrong before:
  //  * no file (fresh state dir / a stand) -> an empty book is CORRECT, writes on;
  //  * unreadable (sharing violation, AV) -> the book exists and must not be
  //    overwritten: writes off, retry in 5 s;
  //  * unparseable/empty -> quarantine the bytes and start a new book.
  let raw = null;
  let readErr = null;
  try { raw = readFileSync(IP_FILE, "utf8"); } catch (e) { readErr = e; }
  if (readErr && readErr.code !== "ENOENT") {
    ipHistory = {};
    ipLoaded = false;
    ipRetryAt = Date.now() + 5000;
    logLine({ ts: new Date().toISOString(), event: "ips_load_failed", file: IP_FILE, code: readErr.code || "", retry_s: 5 });
    return;
  }
  if (!readErr && raw) {
    try { JSON.parse(raw); } catch (e) {
      try { renameSync(IP_FILE, `${IP_FILE}.corrupt-${Date.now()}`); } catch {}
      logLine({ ts: new Date().toISOString(), event: "ips_quarantined", file: IP_FILE, err: String(e).slice(0, 120) });
    }
  }
  ipHistory = {};
  ipLoaded = true; // a new book may be written; a corrupt one is preserved aside
  ipRetryAt = 0;
  logLine({ ts: new Date().toISOString(), event: "ips_fresh_book", file: IP_FILE });
}
function saveIps() {
  if (!ipLoaded) return; // never overwrite a book we failed to read
  if (!ownsPort) return; // a loser of the bind race must not rewrite shared state
  try {
    pruneIps();
    writeJsonAtomic(IP_FILE, JSON.stringify(ipHistory));
  } catch {}
}
function noteIps(list) {
  const ips = list.map((b) => b.ip).filter(Boolean);
  const res = recordIps(ipHistory, lastPresent, ips, Date.now(), { init: ipInit });
  for (const ip of res.reused) logLine({ ts: new Date().toISOString(), event: "ip_reuse", ip });
  // A redraw that re-lands an address the book already knows wastes a slot only
  // if that address is inside a closed window RIGHT NOW. The book keeps ~3 000
  // rows from previous days, and a pool off/on or a heal hands back addresses
  // that merely appear in it (fresh ones), so counting every known-but-absent
  // address escalated into a rotation of perfectly good exits.
  const stillClosed = res.reused.filter((ip) => deadlineBlocked(ip));
  // The metric must count what its name says: every re-landed address, not only
  // the ones that are still inside a closed window (measured: 26 ip_reuse rows
  // in one process while /admin/state reported 1).
  ipReuseCount += res.reused.length;
  ipClosedReuseCount += stillClosed.length;
  lastPresent = res.present;
  ipInit = true;
  saveIps();
  maybeEscalateReuse(stillClosed.length, ips.length);
}

// The book cannot be observed to re-serve an address without a lever being owed
// for it. Rotate first (other devices, rolling, no registrations); if a rotate
// inside the window is still reuse-heavy, rotate is proven useless on this path
// and the rung is grow/mint (more devices = unseen addresses).
function maybeEscalateReuse(reuses, fleet) {
  if (!reuses || !fleet || !ALLOW_ROTATE || rotateInFlight || pathLocked()) return;
  if (!shouldEscalateReuse(reuses, fleet, REUSE_ESCALATE_N, REUSE_ESCALATE_SHARE)) return;
  const now = Date.now();
  if (now - lastReuseRotate < REUSE_ROTATE_COOLDOWN_MS) return;
  lastReuseRotate = now;
  const rung = now - reuseRungAt < REUSE_MINT_WINDOW_MS ? 2 : 1;
  reuseRungAt = now;
  logLine({
    ts: new Date().toISOString(), event: "ip_reuse_escalate", reuses, fleet,
    share: +(reuses / fleet).toFixed(2), rung,
    hint: rung >= 2 ? "rotate cannot move this path: buying addresses" : "redraw landed on known exits: rotating devices",
  });
  if (rung >= 2) growPool("ip-reuse").then((g) => { if (!g.ok) return refillCapacity("ip-reuse"); }).catch(() => {});
  else redrawOrRotate("ip-reuse").catch(() => {});
}
function noteBurned(ip, save = true) {
  recordBurned(ipHistory, ip);
  if (save) saveIps();
}

// Measured 2026-10-29 over two days of log: an address that answers with an
// hours-class Retry-After almost never comes back - of 993 such addresses, 887
// never served a single 200 again and NONE returned after the upstream's own
// deadline. So the upstream window is a real per-address verdict we can route
// around: remember the deadline and stop spending requests on that address
// until it passes. Transient (seconds-scale) 429s are NOT recorded here; they
// recovered 149 times within seconds, so they must stay routable.
// The verdict also carries WHEN it was observed and on WHICH uplink: a 429 and
// a 200 can be handled out of order (a hard-class sid probe awaits a full round
// trip between the fetch and the verdict write), and the same pool answers
// differently after the uplink switches (measured 2026-10-28 22:40: the verdict
// was address+path, and 467 rows kept a home-path deadline into the new path).
function noteDeadline(ip, lane, untilMs, seenAt = Date.now(), save = true) {
  if (!ip || !Number.isFinite(untilMs)) return;
  const row = ipHistory[ip] || (ipHistory[ip] = { first: Date.now(), last: Date.now(), seen: 1, burned: 0, reuse: 0 });
  const ln = lane || "inference";
  const byLane = row.untilByLane || (row.untilByLane = {});
  const at = row.untilAt || (row.untilAt = {});
  if ((at[ln] || 0) > seenAt) return; // a newer observation already decided this lane
  byLane[ln] = Math.max(Number(byLane[ln]) || 0, Math.floor(untilMs));
  at[ln] = Math.floor(seenAt);
  // The path belongs to THIS verdict, not to the address: keeping it on the row
  // let a fresh verdict on a new uplink re-validate an old path's deadline.
  const upPath = row.untilPath || (row.untilPath = {});
  upPath[ln] = (uplink && uplink.ip) || "";
  if (save) saveIps();
}

// A verdict belongs to ONE lane: the same egress answered 429 on /zen and 200 on
// /inference seconds apart (measured 2026-10-27), so an address-wide block would
// blackhole a healthy lane. The old scalar `until` is still read once for
// compatibility, then dropped on the next write.
function deadlineBlocked(ip, lane = "inference", now = Date.now()) {
  if (!ip) return false;
  const row = ipHistory[ip];
  if (!row) return false;
  // A verdict is an (address, path) fact, not an address fact: the same pool
  // answered hours-class 429 on the home uplink and 200s after the uplink
  // switched (measured 2026-10-28 22:40 - and 467 rows carried the old verdict
  // into the new path). Ignore a verdict recorded on a different path.
  const curPath = (uplink && uplink.ip) || "";
  const byLane = row.untilByLane || {};
  // A verdict recorded on another uplink says nothing about this one (measured
  // 2026-10-28 22:40: the same pool answered hours-class 429 on the home path and
  // 200s after the switch). Legacy rows carry `row.path`, seeded rows carry none
  // (unknown path = applies).
  const verdictPath = (row.untilPath || {})[lane] || row.path || "";
  if (verdictPath && curPath && verdictPath !== curPath) return false;
  const until = Math.max(byLane[lane] || 0, lane === "inference" ? row.until || 0 : 0);
  return until > now;
}

// A 200 is the strongest possible evidence against a verdict: forget it.
function clearDeadline(ip, lane, seenAt = Date.now(), save = true) {
  if (!ip) return;
  const row = ipHistory[ip];
  if (!row) return;
  // A hard-class 429 defers its verdict behind an awaited sid probe; a 200 that
  // landed during that round trip must not be undone by the older 429, and a
  // newer 429 must not be wiped by an older 200.
  if (((row.untilAt || {})[lane] || 0) > seenAt) return;
  const byLane = row.untilByLane || {};
  let changed = false;
  if (byLane[lane]) { byLane[lane] = 0; changed = true; }
  if (lane === "inference" && row.until) { row.until = 0; changed = true; }
  // The clear is itself an observation: a 429 that ARRIVED before this 200 but is
  // handled after it must not re-block the address. The stamp therefore goes on
  // every 200, not only on the ones that cleared something (a first write-off
  // leaves byLane at 0, so `changed` stays false and the in-flight 429 would win).
  const at = row.untilAt || (row.untilAt = {});
  at[lane] = Math.max(at[lane] || 0, seenAt);
  if (changed && save) saveIps();
}

// ---- fingerprint: generated per client, stable per client ----------------
// A fingerprint belongs to the client machine, not to the exit: a real user's
// TLS hello stays the same while the IP changes (laptop -> phone hotspot), and
// two different machines never share a hello. So the spec is derived from the
// conversation seed, generated inside the class the gate accepts, and reused for
// the whole conversation (including egress hops).
//
// Measured 2026-10-27, one egress, gated body, live verdicts:
//   bun native                    t13d1714h1_5b57614c22b0_43ade6aba3df   200
//   wreq 11-ext, order A, h1      t13d1711h1_5b57614c22b0_78e6aca7449b   200
//   wreq 11-ext, order B..E       distinct ja3 each                     200 (x4)
//   wreq 11-ext, h2               t13d1711h2_5b57614c22b0_78e6aca7449b   200
//   wreq firefox_151 preset       t13d1717h2_5b57614c22b0_3cbfd9057e0d   429
// => 11! extension orders x 2 ALPN x 2 stacks of distinct ja3, while the
//    cipher list (the checksum the gate keys on) stays the accepted one.
const FP_FILE = process.env.SIDECAR_FP_STATE || statePath("v2_fp.json");
let fpOverride = {};           // proxy -> { kind, alpn, order } pinned by hand
const fpSessions = new Map();   // exact spec signature -> Promise<session>
const fpSeen = new Map();       // specId -> { convs:Set, egresses:Set }
const FP_SESSION_CAP = 400;
const FP_SEEN_CAP = 500;
const FP_CONV_CAP = 500;
let fpErr = "";

function loadFp() {
  fpOverride = readJsonWithFallback(FP_FILE) || {};
  // Old builds stored raw strings here; normalizeOverride rejects them, so drop
  // them (a garbage spec would otherwise be retried on every request).
  for (const [proxy, raw] of Object.entries(fpOverride)) {
    const ok = raw && typeof raw === "object" && !Array.isArray(raw);
    if (!ok) { delete fpOverride[proxy]; logLine({ ts: new Date().toISOString(), event: "fp_override_dropped", proxy }); }
  }
}
function saveFp() {
  try { writeJsonAtomic(FP_FILE, JSON.stringify(fpOverride)); } catch {}
}
function sessionFor(proxy, spec) {
  // Keyed by the EXACT hello, never by spec.id: two specs whose 4-char ids
  // collide must not share a TLS session (that would silently hand one client
  // the other's extension order).
  const key = sessionKey(proxy, spec);
  let prom = fpSessions.get(key);
  if (!prom) {
    prom = createSession({
      browser: "chrome_149",
      proxy,
      emulation: { tlsOptions: { cipherList: ACCEPTED_CIPHERS, alpnProtocols: [spec.alpn], extensionPermutation: spec.order } },
    }).catch((e) => {
      fpSessions.delete(key);
      fpErr = `session ${spec.id}: ${String(e).slice(0, 120)}`;
      throw e;
    });
    fpSessions.set(key, prom);
    pruneMap(fpSessions, FP_SESSION_CAP);
  }
  return prom;
}
function noteFp(spec, convKey, proxy) {
  let row = fpSeen.get(spec.id);
  if (!row) { row = { convs: new Set(), egresses: new Set() }; fpSeen.set(spec.id, row); pruneMap(fpSeen, FP_SEEN_CAP); }
  if (convKey) {
    row.convs.add(convKey);
    if (row.convs.size > FP_CONV_CAP) row.convs = new Set([convKey]);
  }
  if (proxy) row.egresses.add(proxy);
}

// ---- client identity: (conversation -> egress + minted ses_ id) -----------
// The session header is not a credential: the public zen handler (v1.19.4)
// reads it for logs and sticky-provider selection, then deletes it before the
// upstream call, and never checks it against a database (measured: minted ids
// 8/8 -> 200 on the same egress where real ids were 3/3). So the sidecar mints
// one id per conversation; opencode.db is not read. A 429 is a verdict on the
// egress's daily budget and says nothing about the id.
const PINS_FILE = process.env.SIDECAR_PINS || statePath("v2_pins.json");
const PIN_CAP = 2000;
// One pinned egress per conversation serialised the whole fleet: measured
// 2026-11-02, 3 conversations held 7 617 requests each while only 1-2 of 24
// addresses answered at any 5-minute mark. A small fan-out keeps affinity but
// lets concurrent streams of one conversation spend several budgets.
const PIN_FANOUT = Math.max(1, Number(process.env.SIDECAR_PIN_FANOUT || 4));
let pins = {};            // convKey -> [{ sid, proxy, ip, at }, ...]
let pinErr = "";

function loadPins() {
  pins = readJsonWithFallback(PINS_FILE) || {};
}
function savePins() {
  try { writeJsonAtomic(PINS_FILE, JSON.stringify(pins)); pinErr = ""; } catch (e) { pinErr = String(e).slice(0, 160); }
}

// A conversation keeps up to PIN_FANOUT pinned pairs. Old files stored a single
// pair per key, so read both shapes.
function pinList(convKey) {
  const v = convKey ? pins[convKey] : null;
  if (!v) return [];
  return Array.isArray(v) ? v.slice() : [v];
}

// Reuse the conversation's pinned pairs while they are alive; otherwise bind a
// new one on an egress this request has not tried yet.
function bindPair(convKey, tried, forcedPort, lane = "inference") {
  const now = Date.now();
  const parked = (proxy) => stateFor(proxy, "", lane).until > now;
  const alive = (proxy) => backends.some((b) => b.proxy === proxy);
  // A pin buys affinity, not a dead end: an egress that has never served a
  // request (rank 2) or went silent (rank 3) must not keep a conversation
  // stuck on it while a proven one is available.
  // A pin stores the address it was bound to, but a rotation can hand the same
  // port a new address: test the address the port has NOW (measured: 91 of 100
  // live pins carried a stale address, 29 of them bypassing a real block).
  const viable = [];
  for (const p of pinList(convKey)) {
    if (!p || !p.proxy) continue;
    const pinIp = (backends.find((b) => b.proxy === p.proxy) || {}).ip || p.ip || "";
    if (egressRank(stateFor(p.proxy, pinIp, lane), now) > 1) continue;
    if (deadlineBlocked(pinIp, lane, now) || !alive(p.proxy) || tried.has(p.proxy) || parked(p.proxy)) continue;
    // A pin inside a burned /64 is not affinity, it is a queue on a refusal:
    // drop it unless this very address has served recently (the prefix is
    // evidence, not a rule). bindPair then binds a fresh pair instead.
    const pinState = stateFor(p.proxy, pinIp, lane);
    if (groupBurned(pinIp, now) && !(pinState.lastOK && now - pinState.lastOK < PROVEN_MS)) continue;
    viable.push({ p, ip: pinIp });
  }
  if (viable.length) {
    // Proactive fan-out: while the conversation holds fewer pairs than the
    // fan-out and the fleet still has an address this conversation never used,
    // bind one more. A single long-lived pin kept freshly added capacity idle
    // (measured 2026-11-03 02:5x: 20 new node slots, ok=0 for all of them while
    // three conversations kept their old egresses).
    const pinnedProxies = new Set(pinList(convKey).map((p) => p && p.proxy).filter(Boolean));
    if (pinnedProxies.size < PIN_FANOUT
      && backends.some((b) => !tried.has(b.proxy) && !pinnedProxies.has(b.proxy) && !stateFor(b.proxy, b.ip, lane).lastOK)) {
      const spare = pickExcept(new Set([...pinnedProxies, ...tried]), lane);
      if (spare) {
        const extra = { sid: mintSession(convKey || `one-shot|${now}|${Math.random()}`, now), proxy: spare.proxy, ip: spare.ip, at: now };
        const keepExtra = pinList(convKey);
        keepExtra.push(extra);
        while (keepExtra.length > PIN_FANOUT) {
          keepExtra.sort((a, b) => (a.at || 0) - (b.at || 0));
          keepExtra.shift();
        }
        pins[convKey] = keepExtra;
        savePins();
        return extra;
      }
    }
    // Spread one conversation over its pins: least recently used goes first and
    // `at` is stamped right here, so back-to-back requests take different
    // egresses instead of queueing on one budget.
    viable.sort((a, b) => (a.p.at || 0) - (b.p.at || 0));
    const { p, ip } = viable[0];
    if (ip && ip !== p.ip) p.ip = ip;
    p.at = now;
    return p;
  }
  let backend = null;
  if (forcedPort) backend = backends.find((b) => b.proxy.endsWith(":" + forcedPort)) || { proxy: `http://127.0.0.1:${forcedPort}`, ip: "" };
  else backend = pickExcept(tried, lane);
  if (!backend) return null;
  const pair = { sid: mintSession(convKey || `one-shot|${now}|${Math.random()}`, now), proxy: backend.proxy, ip: backend.ip, at: now };
  // Anonymous traffic (no client session header, e.g. a smoke probe) gets a
  // throwaway pair, and a forced debug backend is not a routing decision:
  // neither may persist as this conversation's pin.
  if (convKey && !forcedPort) {
    const keep = pinList(convKey).filter((p) => {
      if (!p || !p.proxy || p.proxy === pair.proxy) return false;
      const ip = (backends.find((b) => b.proxy === p.proxy) || {}).ip || p.ip || "";
      return alive(p.proxy) && !parked(p.proxy) && !deadlineBlocked(ip, lane, Date.now());
    });
    keep.push(pair);
    while (keep.length > PIN_FANOUT) {
      keep.sort((a, b) => (a.at || 0) - (b.at || 0));
      keep.shift();
    }
    pins[convKey] = keep;
    const dropped = prunePins(pins, PIN_CAP);
    if (dropped.length) logLine({ ts: new Date().toISOString(), event: "pin_pruned", n: dropped.length });
    savePins();
  }
  return pair;
}

// Remove/update one pair inside a conversation's fan-out.
function dropPin(convKey, pair) {
  if (!convKey) return;
  const list = pinList(convKey);
  const i = list.findIndex((p) => p && p.proxy === pair.proxy);
  if (i < 0) return;
  list.splice(i, 1);
  if (list.length) pins[convKey] = list; else delete pins[convKey];
  savePins();
}


// Shared verdicts for the id probe: concurrent 429s on one egress wait for a
// single probe instead of stacking their own. `pending` records are the probe
// in flight; a settled 429 is reused for PROBE_REUSE_MS. A 200 verdict is never
// reused as a response (its body belongs to its owner) - a waiter only learns
// that the egress is healthy and leaves it alone.
const probeFlights = new Map(); // `${proxy}|${lane}` -> { at, pending, status, promise }
function pruneProbes(now) {
  for (const [key, rec] of probeFlights) if (!rec.pending && now - rec.at > PROBE_REUSE_MS) probeFlights.delete(key);
  if (probeFlights.size > 256) probeFlights.clear();
}

// ---- request plumbing -----------------------------------------------------
const GOOD_UPSTREAM_UA = process.env.SIDECAR_UPSTREAM_UA || "opencode/1.19.5 ai-sdk/provider-utils/4.0.23 runtime/bun/1.4.0";
const ALT_UPSTREAM_UA = process.env.SIDECAR_ALT_UPSTREAM_UA || "opencode/1.19.4 ai-sdk/provider-utils/4.0.23 runtime/bun/1.4.0";

function forceUpstreamUA(headers, ua = GOOD_UPSTREAM_UA) {
  const val = String(ua || GOOD_UPSTREAM_UA);
  for (const k of Object.keys(headers || {})) {
    if (k.toLowerCase() === "user-agent") delete headers[k];
  }
  headers["user-agent"] = val;
  return val;
}

function forwardableHeaders(input) {
  const headers = {};
  for (const [key, value] of Object.entries(input || {})) {
    const name = key.toLowerCase();
    if (name === "host" || name === "content-length" || name === "connection") continue;
    if (name === "accept-encoding" || name === "transfer-encoding") continue;
    if (value == null) continue;
    headers[key] = String(value);
  }
  return headers;
}

function responseHeaders(input) {
  const headers = new Headers();
  input.forEach((value, key) => {
    const name = key.toLowerCase();
    if (name === "content-encoding" || name === "content-length") return;
    if (name === "transfer-encoding" || name === "connection") return;
    headers.set(key, value);
  });
  return headers;
}

function logLine(rec) {
  try {
    appendFileSync(LOG, JSON.stringify(rec) + "\n");
  } catch {}
}

loadState();
loadIps();
loadFp();
loadPins();
setInterval(saveState, 15000).unref?.();
// Boot race: the Run key starts warp-router and this process in parallel; until
// the pool reports a backend, keep retrying so the first client request after a
// reboot is not answered from an empty pool.
setInterval(() => { if (!backends.length) discover(true); }, 5000).unref?.();
process.on("exit", saveState);

// A dead sidecar must leave a trace: the launcher hides the console, so stderr
// is lost. Anything fatal lands in the request log the monitors already read.
process.on("uncaughtException", (e) => {
  logLine({ ts: new Date().toISOString(), event: "uncaught", err: String(e && e.stack || e).slice(0, 400) });
});
process.on("unhandledRejection", (e) => {
  logLine({ ts: new Date().toISOString(), event: "unhandled_rejection", err: String(e && e.stack || e).slice(0, 400) });
});

const server = (() => { try { return Bun.serve({
  port: PORT,
  // Admin endpoints (rotate/mint/unpark/profile/trace) carry no auth and the
  // callers are local (ai-router, the pool API, the operator): listen on
  // loopback only. SIDECAR_HOST exists for a deliberate remote stand.
  hostname: process.env.SIDECAR_HOST || "127.0.0.1",
  idleTimeout: 255,
  async fetch(req) {
    const started = Date.now();
    const url = new URL(req.url);
    // DNS-rebinding guard: a page in the user's browser can POST to 127.0.0.1
    // with an arbitrary Host header; admin routes mutate routing, so only answer
    // when the Host is this loopback listener.
    if (url.pathname.startsWith("/admin/")) {
      const host = String(req.headers.get("host") || "").toLowerCase();
      if (host && !/^(127\.0\.0\.1|localhost)(:\d+)?$/.test(host)) {
        return new Response(JSON.stringify({ error: "bad_host" }), { status: 403 });
      }
      // The Host guard alone stops DNS rebinding, not a page on this machine:
      // a browser POST to http://127.0.0.1:9099/admin/rotate-fresh sends the
      // loopback Host and would pass. No script caller sends Origin or a
      // cross-site fetch hint, so their presence is enough to refuse.
      const site = String(req.headers.get("sec-fetch-site") || "").toLowerCase();
      if (req.headers.get("origin") || (site && site !== "same-origin" && site !== "none")) {
        return new Response(JSON.stringify({ error: "browser_origin" }), { status: 403 });
      }
    }
    if (url.pathname === "/admin/state") {
      // ?cached=1 reports the last discovery without forcing one: the boot-race
      // test (and any monitor) must see what routing really has, not heal it.
      if (url.searchParams.get("cached") !== "1") await discover(true);
      const now = Date.now();
      const active = new Set(backends.map((b) => b.proxy));
      const ipByProxy = new Map(backends.map((b) => [b.proxy, b.ip]));
      const rows = [...egress.values()].map((s) => ({
        ip: s.ip,
        proxy: s.proxy,
        lane: s.lane || "inference",
        // Active means the port is in the pool AND carries the address the pool
        // reports now: keying on the port alone made a row left over from the
        // pre-lane build (a dead address on a live port) look active and report
        // ITS book verdict as capacity (the "8 rows vs ip_blocked_in_pool: 1").
        active: active.has(s.proxy) && ipByProxy.get(s.proxy) === s.ip,
        ok: s.ok,
        bad: s.bad,
        low: isLow(s),
        hard: s.hard || 0,
        strikes: s.strikes || 0,
        banned_for_s: s.until > now ? Math.round((s.until - now) / 1000) : 0,
        deadline_for_s: (() => {
          // Report what routing really honours: a verdict measured on another
          // uplink is in the book but never blocks, so a live 6000 s number here
          // described a veto that did not exist.
          const lane = s.lane || "inference";
          if (!deadlineBlocked(s.ip, lane, now)) return 0;
          const row = ipHistory[s.ip] || {};
          const byLane = row.untilByLane || {};
          const until = Math.max(byLane[lane] || 0, lane === "inference" ? row.until || 0 : 0);
          return until > now ? Math.round((until - now) / 1000) : 0;
        })(),
        last_ok: s.lastOK ? new Date(s.lastOK).toISOString() : "",
      }));
      const blockedInPool = backends.filter((b) => deadlineBlocked(b.ip, "inference", now)).length;
      const blockedCount = Object.values(ipHistory).filter((r) => liveDeadlineMs(r, now) > 0).length;
      const parkedAll = backends.length > 0 && backends.every((b) => stateFor(b.proxy, b.ip, "inference").until > now || deadlineBlocked(b.ip, "inference", now));
      // Honest picture: a group counts as closed if its /64 registry entry is
      // still live, even when the slot itself was re-created on the same prefix.
      const liveGroups = new Set(backends.map((b) => prefix64(b.ip)).filter(Boolean));
      // Reported, not inferred from: a burned prefix says nothing about the
      // other addresses inside it on /inference.
      const burnedGroups = [...liveGroups].filter((g) => burn64[g] && burn64[g].until > now);
      const budgetResetIn = Object.values(burn64).reduce((mx, r) => Math.max(mx, r && r.until > now ? Math.round((r.until - now) / 1000) : 0), 0);
      return Response.json({
        ok: true,
        version: "v2.81-ttfb-guard",
        exhausted: parkedAll,
        groups_burned: burnedGroups.length,
        groups_live: new Set(backends.filter((b) => {
          const st = stateFor(b.proxy, b.ip, "inference");
          return st.until <= now && !deadlineBlocked(b.ip, "inference", now);
        }).map((b) => prefix64(b.ip)).filter(Boolean)).size,
        budget_reset_in_s: budgetResetIn,
        groups64: new Set(backends.map((b) => prefix64(b.ip)).filter(Boolean)).size,
        last_ok_ago_s: lastOKAt ? Math.round((now - lastOKAt) / 1000) : -1,
        // ?cached=1 answers from the last discovery without touching the pool;
        // without this field a monitor cannot tell a fresh view from a stale one.
        live_streams: liveStreams,
        backends_age_s: lastDiscoverOk ? Math.round((now - lastDiscoverOk) / 1000) : -1,
        next_usable_s: Math.round(nextUsableMs("inference") / 1000),
        // The request path uses a rung-dependent cooldown (90 s while the ladder
        // is still short, 10 min, then 1 h); reporting the 10-min constant here
        // said 600 s for a gate that opens in 90 s.
        next_rotate_in_s: lastRotate ? Math.max(0, Math.round((lastRotate + (autoRotations <= 6 ? ROTATE_SOON_MS : autoRotations <= 12 ? ROTATE_COOLDOWN_MS : 60 * 60_000) - now) / 1000)) : 0,
        auto_rotations: autoRotations,
        path_lock_s: pathLockUntil > now ? Math.round((pathLockUntil - now) / 1000) : 0,
        path_lock_until: pathLockUntil > now ? new Date(pathLockUntil).toISOString() : "",
        uplink: uplink ? { ip: uplink.ip, colo: uplink.colo || "" } : null,
        target: TARGET,
        front_proxy: FRONT_PROXY,
        backends: backends.length,
        pool_api: POOL_API,
        soft_budget: softBudget,
        window_samples: samples,
        sid_mode: "mint",
        pins: Object.keys(pins).length,
        pins_file: PINS_FILE,
        pin_err: pinErr,
        uniq_ips: new Set(backends.map((b) => b.ip).filter(Boolean)).size,
        ip_dups: ipDupCount,
        ip_reuses: ipReuseCount,
        ip_reuses_closed: ipClosedReuseCount,
        ip_history: Object.keys(ipHistory).length,
        ip_burned: Object.values(ipHistory).filter((r) => (r.burned || 0) > 0).length,
        ip_blocked: blockedCount,
        ip_blocked_in_pool: blockedInPool,
        rows,
      });
    }
    if (url.pathname === "/admin/unpark" && req.method === "POST") {
      // Drop learned parks (optionally one lane) without restarting: parks are
      // a local opinion and must never outlive the evidence behind them.
      const lane = url.searchParams.get("lane") || "";
      const now = Date.now();
      let freed = 0;
      for (const st of egress.values()) {
        if (lane && st.lane !== lane) continue;
        if (st.until > now) freed++;
        st.until = 0;
        st.hard = 0;
        st.bad = 0;
        // "Forget this egress" must also forget the strike history, otherwise
        // the very next 429 escalates straight back into a long park.
        st.strikes = 0;
        st.lastStrike = 0;
        st.parkedAt = 0;
        st.lastChanceAt = 0;
      }
      // The persisted deadline has routing authority of its own (pickExcept and
      // bindPair honour it), so an unpark that leaves it in place reports "freed"
      // while the next request still finds zero candidates.
      let unblocked = 0;
      for (const row of Object.values(ipHistory)) {
        const byLane = row.untilByLane || {};
        let touched = false;
        for (const l of lane ? [lane] : ["inference", "zen"]) {
          if (byLane[l]) { byLane[l] = 0; touched = true; }
          if (l === "inference" && row.until) { row.until = 0; touched = true; }
        }
        if (touched) unblocked++;
      }
      if (unblocked) saveIps();
      saveState();
      logLine({ ts: new Date().toISOString(), event: "unpark", lane: lane || "all", freed, unblocked });
      return Response.json({ ok: true, lane: lane || "all", freed, unblocked });
    }
    if (url.pathname === "/admin/rotate-fresh" && req.method === "POST") {
      // Rotate the whole pool until every exit is an address we have never
      // served on. Disruptive (all backends restart), so it is explicit only.
      if (!ALLOW_ROTATE) return Response.json({ ok: false, error: "rotate disabled" }, { status: 409 });
      // Freshness is per ADDRESS on /inference (a new address inside a used
      // prefix has its own budget - measured 2026-10-27), so only the legacy
      // zen prefix view is offered as a stricter option.
      const before = new Set(Object.keys(ipHistory).map(prefix64).filter(Boolean));
      const beforeAddrs = new Set(Object.keys(ipHistory));
      let rounds = 0, fresh = 0, total = 0, reused = [];
      while (rounds < 3) {
        rounds++;
        await rotatePool("rotate-fresh");
        lastDiscover = 0;
        const deadline = Date.now() + 90_000;
        while (Date.now() < deadline) {
          await Bun.sleep(3000);
          await discover(true);
          if ((await countUp()) > 0) break;
        }
        total = await countUp();
        if (total < 0) total = backends.length;
        reused = backends.filter((b) => beforeAddrs.has(b.ip)).map((b) => b.ip);
        const reusedPrefix = backends.filter((b) => before.has(prefix64(b.ip))).map((b) => b.ip);
        fresh = Math.max(0, total - reused.length);
        logLine({ ts: new Date().toISOString(), event: "rotate_fresh", rounds, fresh, total, reused_prefix: reusedPrefix.length });
        if (total > 0 && reused.length === 0) break;
      }
      // Zero unseen exits is not the end of the road. The address a slot draws
      // comes from the handshake, so a retunnel redraws the whole fleet on the
      // accounts already held (measured 2026-10-28: 19 workers 16 -> 18 unique
      // /64 with no registration). Registering a new device is the LAST resort:
      // it minted 16 exits the pool had never held and every one answered 429.
      let minting = false;
      let retunneled = false;
      if (fresh === 0) {
        retunneled = (await retunnelPool("rotate-fresh")).ok;
      }
      if (fresh === 0 && !retunneled && MINT_ENABLED) {
        minting = true;
        const count = MINT_COUNT || Math.max(2, Math.min(backends.length || 20, MINT_CAP));
        mintAccounts("rotate-fresh", count).then(async (r) => {
          if (!r.ok) return;
          const done = await waitJob(r.job_id);
          logLine({ ts: new Date().toISOString(), event: "accounts_mint_done", reason: "rotate-fresh", job: r.job_id, state: done ? done.state : "timeout", msg: done ? String(done.message || "").slice(0, 120) : "" });
          if (Date.now() - lastRespreadAt > RESPRED_COOLDOWN_MS && Date.now() - lastOKAt > ROTATE_IDLE_MS) {
            lastRespreadAt = Date.now();
            await respreadPool("rotate-fresh", backends.length || 20).catch(() => {});
          }
          rotatePool("after-mint").catch(() => {});
        });
      }
      return Response.json({ ok: total > 0 && reused.length === 0, rounds, fresh, total, reused, minting, retunnel: retunneled, history: Object.keys(ipHistory).length });
    }
    if (url.pathname === "/admin/trace") {
      // Which address does the DESTINATION's edge actually see through a given
      // backend? The pool reports public_ip from warp-router's own view; if the
      // site's edge collapses several of our "distinct" exits into one, the
      // per-address budget story is wrong and every exit shares a spent bucket.
      const port = String(url.searchParams.get("backend") || "").split(":").pop();
      const b = backends.find((x) => x.proxy.endsWith(":" + port)) || null;
      if (!b) return Response.json({ ok: false, error: "unknown backend" }, { status: 404 });
      const spec = (() => { try { return specForRequest(null, `trace|${b.proxy}`); } catch { return null; } })();
      try {
        let res;
        const deadline = AbortSignal.timeout(TRACE_TIMEOUT_MS);
        if (!spec || spec.kind === "bun") {
          res = await fetch(TRACE_URL, { method: "GET", proxy: b.proxy, signal: deadline });
        } else {
          const session = await sessionFor(b.proxy, spec);
          res = await session.fetch(TRACE_URL, { method: "GET", signal: deadline });
        }
        const txt = await res.text();
        const kv = {};
        for (const line of txt.split("\n")) {
          const i = line.indexOf("=");
          if (i > 0) kv[line.slice(0, i)] = line.slice(i + 1).trim();
        }
        return Response.json({ ok: res.status === 200, status: res.status, proxy: b.proxy, pool_ip: b.ip, trace_ip: kv.ip || "", colo: kv.colo || "", loc: kv.loc || "", warp: kv.warp || "" });
      } catch (e) {
        // A timeout leaves the cached session promise pending forever and every
        // later trace of this backend would await it again.
        if (spec) {
          const stale = sessionKey(b.proxy, spec);
          if (fpSessions.delete(stale)) logLine({ ts: new Date().toISOString(), event: "trace_session_dropped", proxy: b.proxy });
        }
        return Response.json({ ok: false, proxy: b.proxy, pool_ip: b.ip, error: String(e).slice(0, 200) }, { status: 502 });
      }
    }
    if (url.pathname === "/admin/profile") {
      if (req.method === "POST") {
        let payload = {};
        try { payload = await req.json(); } catch {}
        const port = String(payload.port || payload.proxy || "").split(":").pop();
        const proxy = [...egress.keys()].find((k) => k.endsWith(":" + port)) || (port ? `http://127.0.0.1:${port}` : "");
        if (!proxy) return Response.json({ ok: false, error: "need {port|proxy}" }, { status: 400 });
        for (const k of [...fpSessions.keys()]) if (k.startsWith(proxy + "|")) fpSessions.delete(k);
        if (String(payload.mode || "") === "auto" || String(payload.profile || "") === "auto") {
          delete fpOverride[proxy];
          saveFp();
          return Response.json({ ok: true, proxy, mode: "per-client" });
        }
        const kind = payload.kind === "bun" ? "bun" : "wreq";
        const alpn = payload.alpn === "HTTP2" ? "HTTP2" : "HTTP1";
        const order = Array.isArray(payload.order) && payload.order.length ? payload.order.map(Number) : seededPerm(hash32(proxy) ^ 0x9e3779b9);
        fpOverride[proxy] = { kind, alpn, order };
        saveFp();
        return Response.json({ ok: true, proxy, pinned: fpOverride[proxy] });
      }
      const stats = {};
      for (const [id, row] of fpSeen) stats[id] = { convs: row.convs.size, egresses: row.egresses.size };
      return Response.json({
        ok: true,
        mode: "per-client (fingerprint derived from the conversation seed)",
        space: { extensionOrders: "11! = 39916800", alpn: 2, stacks: 2 },
        observed: Object.keys(stats).length,
        stats,
        pinned: fpOverride,
        sessions: fpSessions.size,
        error: fpErr,
      });
    }
    if (url.pathname.startsWith("/admin/")) {
      return new Response("not found", { status: 404 });
    }
    const targetUrl = TARGET + url.pathname + url.search;
    const rawHeaders = {};
    req.headers.forEach((v, k) => { rawHeaders[k] = v; });
    const headers = forwardableHeaders(rawHeaders);
    headers["accept-encoding"] = "identity";
    headers["host"] = "opencode.ai";
    forceUpstreamUA(headers, GOOD_UPSTREAM_UA);
    let body = undefined;
    if (req.method !== "GET" && req.method !== "HEAD") {
      body = await req.text();
    }
    let model = "";
    // `ms` is measured when the response HEADERS are handed back, so for a
    // non-streaming completion it contains the whole generation and for an SSE
    // call only the first byte. The flag keeps the two populations apart.
    let streamReq = false;
    // The free tier refuses a request that is not an agent call at all with the
    // same 429 FreeUsageLimitError it uses for a spent budget, so the envelope
    // is part of the verdict: measured 2026-10-29 21:5x on ONE address within
    // seconds — real body (805 KB, 71 msgs, 14 tools, stream:true) 200; the same
    // body with stream:false 429; 90 KB synthetic with tools+stream 200; the
    // same without tools 429; 269 B 429. A big body is therefore NOT enough to
    // call a 429 the egress's budget (see `representative` below).
    let hasTools = false;
    if (body) { try { const b = JSON.parse(body); model = b.model || ""; streamReq = b.stream === true; hasTools = Array.isArray(b.tools) && b.tools.length > 0; } catch {} }
    // One predicate for "this call can teach a budget verdict": an unparseable
    // body is NOT agent-shaped, so it can never park an egress.
    const agentShape = streamReq && hasTools;
    const ses = (rawHeaders["x-opencode-session"] || "").slice(0, 20);
    // Real clients mint a fresh request id per request; reusing one across
    // different bodies is an anomaly that links unrelated calls together.
    const reqId = "msg_" + Date.now().toString(36) + Math.random().toString(36).slice(2, 10);
    const convRaw = rawHeaders["x-opencode-session"] || rawHeaders["x-opencode-session-id"] || "";
    const convKey = convRaw ? convRaw + "|" + model : "";
    const dumpFile = DUMP_BODIES && body && body.length <= DUMP_MAX_BYTES
      ? `${Date.now()}-${reqId}-${body.length}.json.gz` : "";
    if (dumpFile) dumpBody(dumpFile, body);

    if (process.env.SIDECAR_DUMP_HEADERS === "1") {
      // A header dump used to carry `authorization` verbatim into a log that any
      // local principal can read. Keep the shape, drop the secret.
      const safeH = {};
      for (const [k, v] of Object.entries(rawHeaders)) {
        const key = String(k).toLowerCase();
        safeH[key] = (/authorization|cookie|token|api-key|password/i.test(key))
          ? `<redacted len=${String(v).length}>` : String(v).slice(0, 120);
      }
      logLine({ ts: new Date().toISOString(), event: "headers", path: url.pathname, bytes: body ? body.length : 0, h: safeH });
    }
    const forced = (rawHeaders["x-sidecar-backend"] || "").trim();
    const noPark = (rawHeaders["x-sidecar-nopark"] || "") === "1";
    const selftest = (rawHeaders["x-sidecar-selftest"] || "") === "1";
    await discover();
    // Degradation, not just death: rotation used to wait for the LAST live egress
    // to fall over. Measured 2026-10-26: it rotated at 07:13 and then spent 13 h
    // on a pool that answered 200 only at a trickle, and 2026-10-28 13:02-13:21
    // the same shape preceded the collapse. Most exits hours-parked and nothing
    // served for three minutes is the same emergency one step earlier.
    if (ALLOW_ROTATE && backends.length) {
      const nowD = Date.now();
      const parkedD = backends.filter((b) => stateFor(b.proxy, b.ip, "inference").until > nowD).length;
      const coolD = autoRotations <= 6 ? ROTATE_SOON_MS : autoRotations <= 12 ? ROTATE_COOLDOWN_MS : 60 * 60_000;
      // `autoRotations` is the ladder of FRUITLESS rotations. It used to be
      // incremented by every request that passed the gate while one rotation was
      // still in flight (measured: 21 increments in 4.3 s, ladder to 1 h backoff
      // after a single rotation), so the guard checks the flight and stamps
      // `lastRotate` before the call, not after the HTTP round trip.
      // A pool that is 70% parked cannot absorb a burst: the remaining third
      // burns in a minute and the client meets the all-429 wall (measured
      // 2026-10-30 17:26-17:36: ten minutes of 429 while this gate and the
      // all-closed one both waited for liveStreams === 0, which a busy agent
      // session never reaches). Rotate while a third still serves: the restart
      // is rolling, so the cost is one slot, not the fleet.
      const parkedShareD = parkedD / backends.length;
      // Only a long park (a spent window) removes an address for good. A 5 s
      // throttle or a 60 s transport park after an uplink blip parks the whole
      // fleet for a minute: counting those as dead would rotate on a hiccup,
      // spending 22 accounts and tearing live streams for nothing.
      const longParkedD = backends.filter((b) => isLongPark(stateFor(b.proxy, b.ip, "inference").until, nowD)).length;
      const liveD = backends.length - longParkedD;
      // Rotate while the last few addresses still serve: a burst needs more
      // than a handful of windows, and a rotation is rolling (one slot at a
      // time, ~2 min), so the reserve only has to cover that.
      const proactiveD = shouldProactiveRotate(backends.length, longParkedD);
      const urgentD = parkedShareD >= 0.6 && nowD - lastOKAt > DEGRADED_IDLE_MS;
      if ((proactiveD || urgentD) && !pathLocked() && !rotateInFlight
        && nowD - lastRotate > coolD) {
        autoRotations++;
        lastRotate = nowD;
        logLine({ ts: new Date().toISOString(), event: "pool_rotate_degraded", parked: parkedD, total: backends.length });
        // Retunnel first: a re-handshake on the SAME account draws a new /64
        // (fresh budget) while a rotate SPENDS a fresh identity and often lands
        // the same spent prefix. The 2026-11-04 incident: 41 rotates in 2 h
        // (820 account draws against a 796-account registry) bought no capacity,
        // and recovery came from the retunnel after the uplink change.
        redrawOrRotate("degraded").catch(() => {});
      }
    }
    // Budget gate: the free window is ~180 requests per address (measured
    // 2026-11-04: 64 burn samples, median 179, p90 205). Waiting for the 429
    // costs the client six failed attempts and a 503; a retunnel is one ~35 s
    // redraw on the accounts we already hold. Fire when half the serving fleet
    // is inside the learned reserve - only while it is actually serving, so a
    // walled path (ok ~0) never churns.
    if (ALLOW_ROTATE && backends.length && softBudget > 0) {
      const nowB = Date.now();
      const servedB = backends
        // The retunnel only redraws the DIRECT slots; wide lanes are redrawn
        // per slot by wide_redraw.py at their own budget. Counting them here
        // would refire this gate every cooldown because their ok never resets.
        .filter((b) => b.kind !== "wide")
        .map((b) => stateFor(b.proxy, b.ip, "inference"))
        .filter((st) => st.ok > 0 && !(st.until > nowB))
        .map((st) => st.ok);
      const enough = servedB.length >= Math.min(6, backends.length);
      const medServed = enough ? percentile(servedB, 0.5) : 0;
      if (medServed >= softBudget && nowB - lastOKAt < BUDGET_SERVE_MS
        && !pathLocked() && !rotateInFlight && nowB - lastRetunnel > RETUNNEL_COOLDOWN_MS) {
        logLine({ ts: new Date().toISOString(), event: "pool_budget_redraw", served: medServed, budget: softBudget, total: backends.length });
        redrawOrRotate("budget").catch(() => {});
      }
    }
    if (!backends.length) {
      // The pool can be empty for a moment (rotation, warp-router restart):
      // one bounded retry, then say what is actually true.
      await Bun.sleep(1500);
      await discover(true);
    }
    const frontUsable = !!FRONT_PROXY && Date.now() >= frontUntil;
    // Declared here: the empty-pool wait below needs the lane, and a later
    // `const lane` left this block in the temporal dead zone (500 on an empty
    // pool, measured by the suite).
    const lane = laneOf(url.pathname);
    if (!backends.length && !frontUsable) {
      // No egress exists right now (the pool is respawning), which is not the
      // same thing as "every window is spent". A rotation used to leave the
      // list empty and the client got a 503 in milliseconds, which OMP reads as
      // a dead provider. Hold an agent-shaped request while the pool rebuilds.
      if (agentShape && streamReq && !selftest && !forced) {
        const emptyStarted = Date.now();
        logLine({ ts: new Date().toISOString(), event: "client_wait_empty", lane, path: url.pathname });
        while (!backends.length && Date.now() - emptyStarted < CLIENT_WAIT_MS && !req.signal?.aborted) {
          await Bun.sleep(Math.max(250, CLIENT_WAIT_POLL_MS * 2));
          await discover(true).catch(() => {});
        }
        logLine({ ts: new Date().toISOString(), event: "client_wait_empty_done", lane, waited_ms: Date.now() - emptyStarted, backends: backends.length, aborted: !!req.signal?.aborted });
        if (req.signal?.aborted) return new Response(null, { status: 499 });
      }
      if (!backends.length) {
        // Measured 2026-10-27 23:39: a pool rotation emptied the list for ~2 min
        // and the old answer - 429 FreeUsageLimitError "all egresses parked" -
        // made the client back off as if the daily quota was gone. 503 + a short
        // Retry-After is the honest signal. With a usable host lane this is
        // exactly its job: fall through instead.
        logLine({ ts: new Date().toISOString(), event: "pool_empty", path: url.pathname, waited_ms: Date.now() - started });
        return new Response(JSON.stringify({ type: "error", error: { type: "pool_empty", message: "no live egress: the pool is restarting" } }),
          { status: 503, headers: { "content-type": "application/json", "retry-after": "2" } });
      }
    }

    // Generated per client: the conversation seed fixes the hello, so a
    // conversation keeps one fingerprint across turns and egress hops while
    // different conversations look like different machines. Anything that
    // cannot be built falls back to Bun's own stack.
    const fpSeed = convKey || "anon-" + Math.random().toString(36).slice(2);
    const fetchWithTtfb = async (fn, ms = ATTEMPT_TTFB_MS) => {
      const ctl = new AbortController();
      const timer = setTimeout(() => { try { ctl.abort(new Error("ttfb timeout")); } catch {} }, ms);
      try { return await fn(ctl.signal); } finally { clearTimeout(timer); }
    };
    const isTtfbAbort = (e) => `${String(e)} ${String(e?.cause || "")} ${String(e?.name || "")}`.includes("ttfb timeout");
    const doFetch = async (proxy, sidOverride, ttfbMs) => {
      // "direct" is the host's own egress: no proxy at all, Bun's own stack.
      const px = proxy === "direct" ? "" : proxy;
      const profile = specForRequest(px ? fpOverride[px] : null, fpSeed);
      usedFp = profile.id;
      noteFp(profile, convKey, px);
      // A probe swaps only the identity, never the transport: same egress,
      // same fingerprint, same body.
      const h = sidOverride === undefined ? headers : {
        ...headers, "x-opencode-session": sidOverride, "x-opencode-session-id": sidOverride,
      };
      if (profile.kind === "bun" || !px) {
        return fetchWithTtfb((signal) => fetch(targetUrl, { method: req.method, headers: h, body, proxy: px || undefined, redirect: "manual", signal }), ttfbMs);
      }
      try {
        const session = await sessionFor(px, profile);
        const res = await fetchWithTtfb((signal) => session.fetch(targetUrl, {
          method: req.method,
          headers: new Headers(h),
          body,
          redirect: "manual",
          signal,
        }), ttfbMs);
        const outHeaders = new Headers();
        try { res.headers.forEach((v, k) => outHeaders.set(k, v)); } catch {}
        return new Response(res.body, { status: res.status, statusText: res.statusText, headers: outHeaders });
      } catch (e) {
        // Do not retry a transport that just blew the TTFB deadline: the same
        // dead egress would hang again for another full deadline.
        if (isTtfbAbort(e)) throw e;
        fpErr = `fetch ${profile.id}: ${String(e).slice(0, 120)}`;
        usedFp = "bun";
        return fetchWithTtfb((signal) => fetch(targetUrl, { method: req.method, headers: h, body, proxy: px, redirect: "manual", signal }));
      }
    };
    const logReq = (status, ip, err, extra) => logLine({
      ts: new Date().toISOString(), status, ms: Date.now() - started, path: url.pathname,
      model, stream: streamReq, ses, sid: usedSid, fp: usedFp, conv: convKey ? hash32(convKey).toString(36) : "", rq: reqId.slice(-12),
      bytes: body ? body.length : 0, ip, ...(dumpFile ? { dump: dumpFile } : {}), ...(err ? { err } : {}), ...(extra || {}),
    });

    const tried = new Set();
    let probedSid = false;
    // A dead egress answers in ~400 ms, so a wider sweep costs little and
    // keeps the client from seeing a 429 while live egresses remain.
    const maxAttempts = forced ? 1 : 6;
    let resp = null;
    const parkedHere = [];
    let usedState = null;
    let usedSid = "";
    let usedFp = "";
    let usedIp = "front";
    let usedAt = 0; // when the response that decided this request arrived
    let lastErr = "";
    let uaAltTried = false;
    // Longest window the upstream itself named during this request: first-strike
    // parks are deliberately short, so the client's Retry-After must carry the
    // upstream's own number instead of our provisional one.
    let lastUpstreamRa = 0;
    let hardRaTries = 0; // tried egresses that named an hours-class window
    // Retry accounting for the client's journal. `upstreamCalls` counts every
    // real provider call this request made (the synthetic selftest 429 is not
    // one); a served answer reports calls-1 refusals and the wall time those
    // refusals + the egress switch cost (attemptStartedAt of the winning call
    // minus the request start). ai-router turns these into request_log columns.
    let attemptStartedAt = 0;
    let upstreamCalls = 0;
    let retryAttempts = 0;
    let retryMs = 0;
    const startedAnswer = () => {
      retryAttempts = Math.max(0, upstreamCalls - 1);
      retryMs = retryAttempts > 0 ? Math.max(0, attemptStartedAt - started) : 0;
    };
    // Synthesized refusals (pool busy / no candidate / front parked) used to
    // leave without the telemetry headers, so ai-router's journal recorded
    // attempts=0 for requests that had burned 1-6 real provider calls (measured
    // 2026-11-04: 287 incident 503 rows, 421 upstream calls invisible). Emit the
    // spent count on failures too; absent means "never reached an egress".
    const failHeaders = (ra) => {
      const h = { "content-type": "application/json", "retry-after": String(ra) };
      if (upstreamCalls > 0) {
        h["x-sidecar-attempts"] = String(upstreamCalls);
        h["x-sidecar-retry-ms"] = String(Math.max(0, Date.now() - started));
      }
      return h;
    };

    // A zero-candidate request used to leave in milliseconds with a 503, which
    // the client reads as a dead provider and can answer by killing the agent
    // session. Hold an agent-shaped request while the fleet rebuilds: a rotation
    // lands in ~20 s, and the first freed egress ends the wait. The 503 is only
    // sent when the waiting budget is gone; the rotation keeps running either
    // way, because this wait does not count as a live stream.
    if (agentShape && streamReq && !selftest && !forced && !frontUsable) {
      const waitStarted = Date.now();
      const waitUntil = waitStarted + CLIENT_WAIT_MS;
      let waitLogged = false;
      while (!pickExcept(tried, lane) && Date.now() < waitUntil && !req.signal?.aborted) {
        if (!waitLogged) {
          waitLogged = true;
          logLine({ ts: new Date().toISOString(), event: "client_wait", lane, backends: backends.length, blocked: backends.filter((b) => stateFor(b.proxy, b.ip, lane).until > Date.now()).length, rotate: rotateInFlight, model, ses });
        }
        await Bun.sleep(CLIENT_WAIT_POLL_MS);
      }
      if (waitLogged) logLine({ ts: new Date().toISOString(), event: "client_wait_done", lane, waited_ms: Date.now() - waitStarted, candidate: !!pickExcept(tried, lane), aborted: !!req.signal?.aborted });
      // The client gave up while we held the request: do not spend a last-chance
      // probe or a front-lane call on a body nobody will read.
      if (req.signal?.aborted) return new Response(null, { status: 499 });
    }

    for (let attempt = 0; attempt < maxAttempts; attempt++) {
      // The count cap alone bounded SIMULTANEOUS attempts, not their cost: with
      // a 30 s TTFB a chain of hung tunnels held one request for 3 min.
      if (attempt > 0 && Date.now() - started > RETRY_BUDGET_MS) {
        logLine({ ts: new Date().toISOString(), event: "retry_budget", ms: Date.now() - started, attempts: upstreamCalls, lane, model });
        break;
      }
      const pair = bindPair(convKey, tried, forced || null, lane);
      if (!pair) break;
      const t = { proxy: pair.proxy, ip: pair.ip };
      usedSid = pair.sid || "";
      if (pair.sid) {
        headers["x-opencode-session"] = pair.sid;
        headers["x-opencode-session-id"] = pair.sid;
      }
      headers["x-opencode-request"] = reqId;
      tried.add(t.proxy);
      const st = stateFor(t.proxy, t.ip, lane);
      let r;
      try {
        const synthetic = selftest && attempt === 0;
        if (!synthetic) upstreamCalls++;
        attemptStartedAt = Date.now();
        r = synthetic
          ? new Response('{"type":"error","error":{"type":"FreeUsageLimitError","message":"selftest 429"}}', { status: 429 })
          : await doFetch(t.proxy, undefined, (!st.ok && !st.bad) ? Math.min(ATTEMPT_TTFB_MS, UNTESTED_TTFB_MS) : ATTEMPT_TTFB_MS);
      } catch (e) {
        // Transport error: a hung/dead tunnel loses its "proven" rank at once
        // (egressRank) and the park grows with the strike count. A flat 60 s
        // park used to put the same silent egress back within a minute, and
        // every request that picked it paid the whole TTFB deadline again.
        st.hungAt = Date.now();
        st.hung = (st.hung || 0) + 1;
        st.until = Date.now() + (st.hung >= 2 ? HUNG_PARK_MS : 60_000);
        st.ttfb = st.ttfb ? Math.round(st.ttfb * 0.7 + ATTEMPT_TTFB_MS * 0.3) : ATTEMPT_TTFB_MS;
        st.bad++;
        lastTransportAt = Date.now();
        lastErr = String(e).slice(0, 200);
        logReq(0, st.ip, lastErr, { proxy: t.proxy, hung: st.hung });
        continue;
      }
      if (r.status === 429 && !noPark && !selftest) {
        // Stamp the observation the moment the answer arrives: this branch can
        // await a full sid probe below, and a verdict written with a later
        // timestamp would overwrite a 200 that landed in that gap.
        const seenAt = Date.now();
        const ra = retryAfterSeconds(r.headers);
        if (ra > 0) lastUpstreamRa = Math.max(lastUpstreamRa, ra);
        // Measured 2026-10-27 on /inference: bodies >= 8 KB answer 200 (602 of
        // 622), tiny probes answer 429 (42 of 55) on the very same egresses and
        // ids. A 429 on a probe therefore says nothing about the egress's real
        // budget, and parking on it is what took the whole fleet down: probes
        // and smokes must not park.
        // Size alone was not enough (2026-10-29): a 90-400 KB body without
        // stream:true or without tools also answers 429 on an egress that serves
        // the real client the same second, and parking on it wrote off healthy
        // addresses. Only an agent-shaped call can teach a budget verdict.
        const representative = (body ? body.length : 0) >= REPRESENTATIVE_BYTES && agentShape;
        if (!representative) {
          const up = await peekBody(r);
          logLine({ ts: new Date().toISOString(), event: "probe_429", status: 429, ra, bytes: body ? body.length : 0, stream: streamReq, tools: hasTools, ip: st.ip, proxy: t.proxy, up });
          logReq(429, st.ip, "probe 429, retrying next egress", { up, rl: rlHeaders(r) });
          continue;
        }
        // A throttled/blacklisted client UA can masquerade as FreeUsageLimit 429.
        // Try the alternate known-good UA once before writing the egress off.
        if (!uaAltTried) {
          uaAltTried = true;
          const uaBefore = headers["user-agent"];
          forceUpstreamUA(headers, ALT_UPSTREAM_UA);
          let rAlt = null;
          try {
            upstreamCalls++;
            attemptStartedAt = Date.now();
            rAlt = await doFetch(t.proxy, undefined, (!st.ok && !st.bad) ? Math.min(ATTEMPT_TTFB_MS, UNTESTED_TTFB_MS) : ATTEMPT_TTFB_MS);
          } catch {}
          if (rAlt && rAlt.status === 200) {
            try { r.body?.cancel(); } catch {}
            resp = rAlt; usedState = st; usedIp = t.ip || st.ip; usedAt = Date.now();
            startedAnswer();
            forceUpstreamUA(headers, GOOD_UPSTREAM_UA);
            logLine({ ts: new Date().toISOString(), event: "ua_alt_ok", proxy: t.proxy, ip: st.ip || "", ua: headers["user-agent"] });
            break;
          }
          if (rAlt && rAlt.status !== 429) {
            try { r.body?.cancel(); } catch {}
            try { rAlt.body?.cancel(); } catch {}
          } else {
            try { rAlt?.body?.cancel(); } catch {}
          }
          forceUpstreamUA(headers, uaBefore || GOOD_UPSTREAM_UA);
          if (rAlt && rAlt.status === 429) {
            logLine({ ts: new Date().toISOString(), event: "ua_alt_429", proxy: t.proxy, ip: st.ip || "", ra, alt_ua: ALT_UPSTREAM_UA });
          }
        }
        // This window is exhausted. Learn its size, park it for the window the
        // upstream named (hours-class = hard), retry the SAME request on
        // another live egress so the client never sees a 429.
        const cls = classify429(ra, Date.now(), { parkMs: PARK_MS, longRaS: LONG_RA_S });
        // Only a CLOSED window teaches its size: a seconds-scale throttle is not
        // evidence about the egress's budget (learnSample keeps the min over 64
        // samples, so one throttle after 12 successes dropped the fleet's bound
        // to floor(12*0.75) = 9).
        if (cls.hard) { hardRaTries++; learnSample(st.ok); }

        // A 429 here is this egress's daily budget (the zen handler limits per
        // /64, Retry-After = seconds to UTC midnight), not a verdict on the id.
        // Still re-probe this same egress once with a freshly minted id before
        // writing a healthy IP off for hours: if that answers 200, something
        // about the id did matter, and the conversation moves to the new id.
        // Parallel 429s on the same egress share that one probe.
        if (cls.hard && pair.sid && !probedSid) {
          probedSid = true;
          const pk = `${t.proxy}|${lane}`;
          const nowProbe = Date.now();
          pruneProbes(nowProbe);
          const seen = probeFlights.get(pk);
          // A settled 200 verdict is not reusable: it said "the id mattered a
          // minute ago", not "this egress cannot be out of budget now". Only an
          // egress-side 429 (or a probe still in flight) is worth sharing.
          if (seen && nowProbe - seen.at < PROBE_REUSE_MS && (seen.pending || seen.status === 429)) {
            const v = seen.pending ? await seen.promise.catch(() => null) : { status: seen.status };
            if (v && v.status === 200) {
              // A fresh id was accepted here: the egress is healthy and the id
              // was the problem. Do not write the egress off, take ours
              // elsewhere (the body of the peer's probe is already consumed).
              logLine({ ts: new Date().toISOString(), event: "sid_probe_shared", verdict: 200, sid: pair.sid, ip: st.ip, proxy: t.proxy, ra });
              const up = await peekBody(r);
              logReq(429, st.ip, "id refused, egress healthy: trying another egress", { up, rl: rlHeaders(r) });
              continue;
            }
            logLine({ ts: new Date().toISOString(), event: "sid_probe_skipped", verdict: 429, sid: pair.sid, ip: st.ip, proxy: t.proxy, ra, age_s: Math.round((nowProbe - seen.at) / 1000) });
          } else {
            const alt = mintSession(`${convKey || "one-shot"}|alt|${Date.now()}`, Date.now());
            let r2 = null;
            const rec = { at: nowProbe, pending: true, status: 0, promise: null };
            rec.promise = (async () => {
              try { upstreamCalls++; attemptStartedAt = Date.now(); r2 = await doFetch(t.proxy, alt); } catch {}
              return { status: r2 ? r2.status : 0 };
            })();
            probeFlights.set(pk, rec);
            const v = await rec.promise;
            rec.pending = false;
            rec.status = v.status;
            rec.at = Date.now();
            // Rare (hard 429 only) and decisive: record what the same egress
            // says to a different id.
            logLine({ ts: new Date().toISOString(), event: "sid_probe", status: v.status, sid: pair.sid, alt, ip: st.ip, proxy: t.proxy, ra });
            if (r2 && r2.status === 200) {
              const pl = pinList(convKey);
              const pi = pl.findIndex((p) => p && p.proxy === pair.proxy);
              if (pi >= 0) { pl[pi] = { ...pl[pi], sid: alt, at: Date.now() }; pins[convKey] = pl; savePins(); }
              logLine({ ts: new Date().toISOString(), event: "sid_rotated", sid: pair.sid, alt, ip: st.ip, proxy: t.proxy, ra });
              try { r.body?.cancel(); } catch {}
              resp = r2; usedState = st; usedIp = t.ip || st.ip; usedAt = Date.now(); usedSid = alt;
              startedAnswer();
              break;
            }
            try { r2?.body?.cancel(); } catch {}
          }
        }

        const nowStrike = Date.now();
        const hoursClass = cls.hard || cls.ra >= LONG_RA_S;
        // The port may have been handed a new identity while this request was in
        // flight (rotation/retunnel rewrites the row). The response belongs to
        // `t.ip`, and writing its strikes/parks into a row that now describes a
        // FRESH address is how a new identity inherits a write-off. Keep the
        // verdict in the book (keyed by the address that answered) and leave the
        // live row alone.
        if (t.ip && st.ip !== t.ip) {
          logLine({ ts: new Date().toISOString(), event: "strike_stale", bound: t.ip, port_ip: st.ip, proxy: t.proxy, ra, lane });
          if (hoursClass) noteDeadline(t.ip, lane, nowStrike + Math.min(ra || 0, 86400) * 1000, seenAt, false);
          noteBurned(t.ip, false);
          saveIps();
          await peekBody(r);
          logReq(429, t.ip, "stale verdict: the port rotated during the request", { ra });
          continue;
        }
        if (!hoursClass) {
          // Seconds-scale throttle: park for its own window (or a few seconds),
          // never for PARK_MS, and do not let it count as evidence against the
          // egress: no strike, no hard flag, so the last-chance probe may use it.
          const throttleMs = cls.ra > 0 ? Math.min(cls.ra, 60) * 1000 : THROTTLE_PARK_MS;
          const until = nowStrike + throttleMs;
          const shared = park64(st, until, false);
          logLine({ ts: new Date().toISOString(), event: "park_throttle", ip: prefix64(st.ip), slots: shared + 1, until_s: Math.round(throttleMs / 1000), ra });
          // No burn: this branch's own contract says a throttle is not evidence,
          // yet `burned` fed spentShare/ip_burned/mint_only_spent (99 events over
          // 20 prefixes).
          st.bad++;
          parkedHere.push(st);
          saveState();
          const upT = await peekBody(r);
          logReq(429, st.ip, "throttled, retrying next egress", { up: upT, rl: rlHeaders(r), ra });
          continue;
        }
        // Measured: of 603 addresses with two hours-class 429s the gap was
        // median 1 s (482 under 10 s) - the "second strike" is the same closed
        // window seen by a burst of concurrent requests, because the slot is
        // only picked while `until <= now`. Escalate only when the strikes were
        // separated by the short-park wait, i.e. the address was re-tested after
        // its own window and STILL refused.
        const separated = !st.lastStrike || nowStrike - st.lastStrike >= SHORT_PARK_MS;
        st.strikes = (st.lastStrike && nowStrike - st.lastStrike < STRIKE_WINDOW_MS)
          ? (separated ? (st.strikes || 0) + 1 : Math.max(1, st.strikes || 0))
          : 1;
        st.lastStrike = nowStrike;
        st.parkedAt = nowStrike;
        burnGroups(burn64, st.ip, ra, nowStrike);
        const longPark = cls.hard && st.strikes >= 2;
        const parkUntil = longPark ? cls.until
          : Math.min(cls.until, nowStrike + (cls.hard ? SHORT_PARK_MS : PARK_MS));
        const shared64 = park64(st, parkUntil, longPark);
        if (cls.hard) {
          logLine({ ts: new Date().toISOString(), event: "park64", ip: prefix64(st.ip), slots: shared64 + 1, until_s: Math.round((parkUntil - Date.now()) / 1000), strikes: st.strikes, ra });
        }
        noteBurned(t.ip || st.ip, false);
        // The book is keyed by ADDRESS, and `st.ip` is mutable: a retunnel
        // rewrites it in place, so a 429 from the old address used to land on the
        // fresh one (and a 200 from the old one cleared the new one's verdict).
        if (longPark) noteDeadline(t.ip || st.ip, lane, nowStrike + Math.min(ra || 0, 86400) * 1000, seenAt, false);
        // Unconditional on purpose: noteBurned above is an in-memory change of its
        // own (a soft throttle records the burn/reuse history), and skipping this
        // write lost it on restart. noteDeadline already passes save=false, so there
        // is no double write here - only the stale-verdict branch had one.
        saveIps();
        if (cls.hard) {
          // long park. A first strike stays soft on purpose: the session rollback
          // may lift it and the last-chance probe may test it.
          st.hard = longPark ? 1 : 0;
          // The egress is out of daily budget: drop the pin so the next attempt
          // binds a fresh pair elsewhere. The minted id itself is retired with
          // the pin; it is not a server-side resource to exhaust.
          if (pinList(convKey).some((p) => p && p.proxy === pair.proxy)) dropPin(convKey, pair);
        }
        st.bad++;
        st.ok = 0;
        parkedHere.push(st);
        saveState();
        const up = await peekBody(r);
        logReq(429, st.ip, "parked, retrying next egress", { up, rl: rlHeaders(r), ra });
        continue;
      }
      if (selftest && r.status === 429) {
        try { r.body?.cancel(); } catch {}
        logReq(429, st.ip, "selftest 429, retrying next egress");
        continue;
      }
      // A text-only engine instance answers 400 "image input is not available"
      // while its neighbours accept images (measured 2026-11-05: a session
      // carrying history images died on one egress while fresh one- and
      // five-image probes read the PNG with 200). The INSTANCE, not the
      // request, is the problem: retry the SAME body on the next egress so
      // vision survives instead of failing the turn. When no egress serves it,
      // `resp` keeps the honest 400 and the gateway strips the images.
      if (r.status === 400 && !selftest && body && body.includes('"image_url"')) {
        const up = await peekBody(r.clone(), 4000);
        if (up.includes("image input is not available")) {
          lastErr = "image input is not available on this engine";
          logLine({ ts: new Date().toISOString(), event: "image_unsupported", ip: st.ip, proxy: t.proxy, model, conv: convKey ? hash32(convKey).toString(36) : "", rq: reqId.slice(-12), attempt: attempt + 1, up });
          logReq(400, st.ip, "engine without image input, retrying next egress", { up });
          resp = new Response(up, { status: 400, headers: { "content-type": "application/json" } });
          usedState = st;
          usedIp = t.ip || st.ip;
          usedAt = Date.now();
          startedAnswer();
          continue;
        }
      }
      resp = r;
      usedState = st;
      usedIp = t.ip || st.ip;
      usedAt = Date.now();
      startedAnswer();
      break;
    }

    if (!resp && (!FRONT_PROXY || Date.now() < frontUntil)) {
      // No egress answered AND the host lane cannot help right now (disabled, or
      // parked by the upstream). With FRONT_DIRECT=1 this guard used to be dead
      // code: every lane in a short park answered 503 without one probe, and
      // only the degraded rotation (180 s + ladder) ever recovered.
      // Every attempt was a 429 and there is no fallback: answer honestly with
      // the upstream's own class so the caller can back off deliberately.
      const nowMs = Date.now();
      const lastRa = backends.reduce((mx, b) => {
        const st = stateFor(b.proxy, b.ip, lane);
        return Math.max(mx, st.until > nowMs ? Math.round((st.until - nowMs) / 1000) : 0);
      }, 0);
      // Every egress is parked, but a first-strike park is only a suspicion.
      // Give the egress whose park ends soonest one real request (bounded per
      // egress), so our own parks can never lock the pool out of service. A long
      // park (second strike, upstream's own window) is never probed.
      let lastChance = null;
      if (!selftest) {
        const gap = nowMs - LAST_CHANCE_GAP_MS;
        // Two passes: the first honours the book, the second (only when the first
        // found nothing) ignores it. The book is the reason the pool can be fully
        // dark, and the probe is the ONLY call that can lift a verdict with a
        // 200; skipping every blocked slot locked 244 requests out in 78 s behind
        // `pool_no_candidate` until a rotation happened.
        const scan = (ignoreBook) => {
          let best = null;
          for (const b of backends) {
            if (tried.has(b.proxy)) continue;
            const st = stateFor(b.proxy, b.ip, lane);
            if ((st.lastChanceAt || 0) > gap) continue;
            if (!ignoreBook && deadlineBlocked(b.ip, lane, nowMs)) continue;
            // A written-off window is re-tested rarely; a provisional one at once.
            // A park restored from disk has no parkedAt: it keeps its verdict.
            const age = nowMs - (st.parkedAt || nowMs);
            if (st.hard && age < HARD_PROBE_AGE_MS) continue;
            if (!best || st.until < best.st.until) best = { b, st };
          }
          return best;
        };
        lastChance = scan(false);
        if (!lastChance) {
          lastChance = scan(true);
          if (lastChance) logLine({ ts: new Date().toISOString(), event: "last_chance_blocked", ip: lastChance.b.ip, proxy: lastChance.b.proxy, lane, blocked: backends.filter((b) => deadlineBlocked(b.ip, lane, nowMs)).length });
        }
      }
      if (lastChance) {
        lastChance.st.lastChanceAt = nowMs;
        usedSid = mintSession(`${convKey || "one-shot"}|lastchance`, Date.now());
        headers["x-opencode-session"] = usedSid;
        headers["x-opencode-session-id"] = usedSid;
        headers["x-opencode-request"] = reqId;
        try {
          attemptStartedAt = Date.now();
          const r2 = await doFetch(lastChance.b.proxy);
          // Stamp the observation here: a 200 that arrives while the body drains
          // must carry an older stamp and veto this write (D5).
          const n2 = Date.now();
          if (r2 && r2.status !== 429) {
            resp = r2; usedState = lastChance.st; usedIp = lastChance.b.ip; usedAt = Date.now();
            startedAnswer();
            logLine({ ts: new Date().toISOString(), event: "last_chance_ok", status: r2.status, ip: lastChance.b.ip, proxy: lastChance.b.proxy, parked_s: lastChance.st.until ? Math.max(0, Math.round((lastChance.st.until - nowMs) / 1000)) : 0 });
          } else {
            const up = r2 ? await peekBody(r2) : "";
            const ra2 = r2 ? retryAfterSeconds(r2.headers) : 0;
            // A last-chance 429 is a verdict only for an agent-shaped call, and
            // only when the caller allows parking: the free tier answers the same
            // 429 to a body without stream/tools (measured 2026-10-29: 90 KB
            // stream:false 429 vs the same body with tools+stream 200 on the same
            // address), and an A/B probe must be able to ask without writing the
            // address off. Striking on either wrote parks into the book that no
            // request had earned.
            if (!noPark && agentShape) {
              lastChance.st.strikes = (lastChance.st.lastStrike && n2 - lastChance.st.lastStrike < STRIKE_WINDOW_MS) ? (lastChance.st.strikes || 0) + 1 : 1;
              lastChance.st.lastStrike = n2;
              if (lastChance.st.strikes >= 2 && ra2 > 0) {
                // A throttle (ra <= 60) must not harden the slot for 10 min.
                const hours = ra2 >= LONG_RA_S;
                park64(lastChance.st, n2 + Math.min(ra2 * 1000, 12 * 3600_000), hours);
                // park64 only ever RAISES until, so clearing `hard` here left an
                // hours-class `until` with hard=0 for hours: the session-429 rollback
                // frees any non-hard park, i.e. a closed window reopened (D3).
                if (hours) lastChance.st.hard = 1;
                lastChance.st.parkedAt = n2;
                // The main loop writes the book on a long park, so `deadlineBlocked`
                // remembers it across restarts; this branch did not, and hours-class
                // last-chance verdicts (ra 5210-5224) stayed in memory only.
                if (hours) noteDeadline(lastChance.b.ip, lane, n2 + Math.min(ra2, 86400) * 1000, n2);
              }
              logLine({ ts: new Date().toISOString(), event: "last_chance_429", ip: lastChance.b.ip, proxy: lastChance.b.proxy, ra: ra2, strikes: lastChance.st.strikes, up });
            } else {
              logLine({ ts: new Date().toISOString(), event: "last_chance_probe_429", ip: lastChance.b.ip, proxy: lastChance.b.proxy, ra: ra2, stream: streamReq, tools: hasTools, nopark: !!noPark, up });
            }
          }
        } catch (e) {
          lastErr = String(e).slice(0, 160);
        }
      }
      const clientRa = Math.max(
        backends.reduce((mx, b) => {
          const st = stateFor(b.proxy, b.ip, lane);
          return Math.max(mx, st.until > nowMs ? Math.round((st.until - nowMs) / 1000) : 0);
        }, 0),
        lastUpstreamRa);
      // The event must mean "an egress was tried and refused": 97% of the rows
      // (19 978 of 20 565) carried `attempts: 0` because it fired for requests
      // that never touched an egress - `pool_no_candidate` says that instead.
      if (!resp && tried.size > 0) {
        logLine({ ts: new Date().toISOString(), event: "all_egress_429", attempts: tried.size, ses, ra: clientRa, lane, path: url.pathname });
      } else if (resp) {
        logLine({ ts: new Date().toISOString(), event: "pool_rescued", attempts: tried.size, ses, ra: clientRa, lane, path: url.pathname });
      }
      if (!resp && lastUpstreamRa >= PATH_LOCK_RA_S && hardRaTries >= PATH_LOCK_MIN_EGRESSES) notePathLock(lastUpstreamRa, hardRaTries, lane);
      // Nothing is live: rotation is the only lever left, and it is free here
      // because no request can succeed anyway. Rare by construction (cooldown),
      // and skipped while transports still work (a 400/500 storm is not this).
      // Three fruitless rotations in a row mean the prefixes come back the same:
      // back off to one attempt an hour instead of restarting WARP every 10 min.
      // A rotation that produced fresh-but-burned addresses must not buy the
      // pool a 10-minute silence: measured 2026-10-28 13:23 - 18 of 19 exits
      // came back new and every one answered 429, the ladder was reset, and the
      // fleet stayed dark until the user restarted both daemons by hand. Retry
      // fast while nothing has served yet, then walk the ladder up.
      const cooldown = autoRotations <= 6 ? ROTATE_SOON_MS : autoRotations <= 12 ? ROTATE_COOLDOWN_MS : 60 * 60_000;
      // Nothing served after trying real egresses: the streams that are live
      // will fail anyway, so waiting for liveStreams === 0 is what turned a
      // 429-second into a ten-minute outage (measured 2026-10-30 17:26-17:36).
      if (!resp && ALLOW_ROTATE && !pathLocked(lane) && !rotateInFlight && (liveStreams === 0 || tried.size > 0)
        && nowMs - lastRotate > cooldown && nowMs - lastOKAt > ROTATE_IDLE_MS) {
        autoRotations++;
        lastRotate = nowMs;
        redrawOrRotate("all-closed").catch(() => {});
      }
      // Nothing was attempted, so "the daily window is gone" is a lie: the pool
      // was mid-rotation, every slot was parked by our own bookkeeping, or the
      // whole fleet is deadline-blocked. Answering 429 here hands the client a
      // Retry-After of the largest park (measured: 7355 s) and the client backs
      // off the model for two hours over a two-second condition - which is
      // exactly how one conversation "sticks" on 429 while others work.
      if (!resp && tried.size === 0) {
        // The fleet may be blocked for HOURS (the row below used to log
        // `blocked:31` while the client was told "retry in 2 s" and hammered the
        // same wall 140 ms later). Tell it when a slot can actually serve, still
        // inside the 60 s the client honours as a short backoff.
        const blocked = backends.filter((b) => deadlineBlocked(b.ip, lane)).length;
        // Nothing could even be TRIED, so no request can succeed no matter what a
        // rotation does: this is the one moment redrawing the fleet is free. The
        // idle gates exist to protect a serving fleet, and there is none. Without
        // this the sidecar answered 87 requests with 503 over 69 s and only then
        // rotated (measured 23:51:04-23:52:13).
        let launched = false;
        const coolNC = autoRotations <= 6 ? ROTATE_SOON_MS : autoRotations <= 12 ? ROTATE_COOLDOWN_MS : 60 * 60_000;
        // ...but "no candidate" is not the same as "nothing serves": a probe
        // (non-agent body) and a conversation whose pin just died both land
        // here while the rest of the fleet is answering 200. Redrawing the
        // fleet then throws working exits away and, measured 2026-10-30 over
        // 2 h, 112 of 155 replacement addresses never served a single 200
        // (72% arrive already spent). Keep the idle gate the all-closed path
        // already has: rotate only when nothing has served recently.
        if (ALLOW_ROTATE && !pathLocked(lane) && !rotateInFlight && Date.now() - lastRotate > coolNC && Date.now() - lastOKAt > ROTATE_IDLE_MS) {
          lastRotate = Date.now();
          autoRotations++; // a fruitless rotation must walk the ladder like any other
          redrawOrRotate("no-candidate").catch(() => {});
          launched = true;
        }
        const raOut = launched ? 20 : Math.min(60, Math.max(2, Math.ceil(nextUsableMs(lane) / 1000)));
        logLine({ ts: new Date().toISOString(), event: "pool_no_candidate", lane, backends: backends.length, blocked, ra_out: raOut, rotate: launched });
        return new Response(JSON.stringify({ type: "error", error: { type: "pool_empty", message: blocked >= backends.length ? "every egress is inside its upstream window" : "no free egress right now: the pool is rotating" } }),
          { status: 503, headers: failHeaders(raOut) });
      }
      if (!resp) {
        // OMP parses the outward Retry-After and, above retry.maxDelayMs
        // (300 s), kills the provider for the whole session: the upstream's own
        // 7 000-45 000 s window therefore used to look like "this conversation
        // is banned forever" while other sessions kept working. A pool that
        // rotates in seconds must never hand a client a longer window than it
        // can honour, and the body must not contain quota phrases ("usage
        // limit" -> QUOTA_EXHAUSTED, 30 min) because that verdict belongs to
        // one address, not to the model.
        const throttled = lastUpstreamRa > 0 && lastUpstreamRa <= 60;
        // A hint of 5 s is a lie when the earliest slot can serve in 10 min: the
        // client hammered the wall 3x/s for 81 s. Report the real recovery time,
        // clamped to the 60 s the client treats as a short backoff.
        const wallRa = lastUpstreamRa >= 60 ? 60 : 2;
        const shortRa = throttled ? lastUpstreamRa : Math.min(60, Math.max(wallRa, Math.ceil(nextUsableMs(lane) / 1000)));
        logLine({ ts: new Date().toISOString(), event: throttled ? "client_429_short" : "client_503_busy", ra_in: lastUpstreamRa, ra_out: shortRa, attempts: tried.size });
        return new Response(JSON.stringify({
          type: "error",
          error: throttled
            ? { type: "throttled", message: "upstream asked to slow down for a moment" }
            : { type: "pool_busy", message: "no egress with free capacity right now, rotating" },
        }), { status: throttled ? 429 : 503, headers: failHeaders(shortRa) });
      }
    }
    if (!resp && Date.now() < frontUntil) {
      // The host's own egress is inside the window the upstream named: do not
      // spend an upstream call per client retry.
      const raOut = Math.min(60, Math.max(2, Math.ceil((frontUntil - Date.now()) / 1000)));
      logLine({ ts: new Date().toISOString(), event: "front_parked_skip", ra_out: raOut });
      return new Response(JSON.stringify({ type: "error", error: { type: "pool_empty", message: "every lane is inside its upstream window" } }),
        { status: 503, headers: failHeaders(raOut) });
    }
    if (!resp) {
      try {
        upstreamCalls++;
        attemptStartedAt = Date.now();
        resp = await doFetch(FRONT_PROXY);
        // The front lane is one more real call; startedAnswer() counts the
        // refusals before it (0 when the pool never attempted anything).
        startedAnswer();
        usedState = null;
        usedIp = "front";
        usedFp = "front";
        if (resp.status === 429) {
          const ra = retryAfterSeconds(resp.headers);
          if (ra >= LONG_RA_S) {
            frontUntil = Math.min(Date.now() + Math.min(ra, 12 * 3600) * 1000, budgetResetMs());
            logLine({ ts: new Date().toISOString(), event: "front_parked", ra_s: ra, until: new Date(frontUntil).toISOString() });
          }
          // Never hand the client the upstream's own quota body: OMP turns a
          // Retry-After above retry.maxDelayMs (300 s) or the phrase "usage
          // limit" into "provider dead for this session", and that verdict
          // belongs to ONE lane, not to the model.
          try { resp.body?.cancel(); } catch {}
          logLine({ ts: new Date().toISOString(), event: "front_429", ra_in: ra, ra_out: Math.min(60, Math.max(2, ra || 60)) });
          return new Response(JSON.stringify({ type: "error", error: { type: "pool_busy", message: "no egress with free capacity right now, rotating" } }),
            { status: 503, headers: failHeaders(Math.min(60, Math.max(2, ra || 60))) });
        }
        if (resp.status === 200) {
          frontUntil = 0;
        }
      } catch (e2) {
        logReq(0, "front", String(e2).slice(0, 200));
        return Response.json({ error: { message: String(e2), type: "sidecar_error" } }, { status: 502 });
      }
    }

    const status = resp.status;
    if (status === 200) {
      // Service, not novelty: any 200 answers "did the redraw buy capacity".
      redrawWatch = null;
      if (usedIp === "front") lastOKAt = Date.now();
    }
    // A 429 that survives 3 different egresses is a verdict about the SESSION
    // (free-tier quota closed), not about those IPs: un-park them so healthy
    // windows are not thrown away for a per-session limit.
    if (status === 429 && parkedHere.length > 1) {
      // Only SOFT parks are rolled back: a hard (hours-class) park is a real
      // closed window, and clearing it would send traffic straight back into it.
      let freed = 0;
      for (const st of parkedHere) {
        if (st.hard) continue;
        st.until = 0;
        st.bad = Math.max(0, st.bad - 1);
        freed++;
      }
      if (freed) saveState();
      logLine({ ts: new Date().toISOString(), event: "session_429", un_parked: freed, hard_kept: parkedHere.filter((x) => x.hard).length, ses });
    }
    if (usedState && !noPark) {
      if (status === 200) {
        usedState.ok++;
        usedState.lastOK = Date.now();
        // A served request also clears the hung mark and refreshes the TTFB
        // estimate the picker uses as its latency tiebreak.
        usedState.hungAt = 0;
        usedState.hung = 0;
        {
          const ttf = attemptStartedAt > 0 ? Math.max(0, (usedAt || Date.now()) - attemptStartedAt) : 0;
          if (ttf > 0) usedState.ttfb = usedState.ttfb ? Math.round(usedState.ttfb * 0.7 + ttf * 0.3) : ttf;
        }
        // A 200 proves the address serves right now: wipe every written-off
        // opinion about it (park, strike ladder, burn-book deadline). Without
        // this a rescued egress kept its multi-hour park (58 of 81 last_chance_ok
        // rows carried parked_s > 0, one with 5 h left) and the picker stayed off
        // an address that had just answered.
        clearDeadline(usedIp, lane, usedAt || Date.now());
        usedState.until = 0;
        usedState.strikes = 0;
        usedState.lastStrike = 0;
        lastOKAt = Date.now();
        if (pathLockUntil) { pathLockUntil = 0; logLine({ ts: new Date().toISOString(), event: "path_unlocked" }); }
        // A 200 resets the fruitless-rotation ladder only when the fleet has not
        // been redrawn for a full cooldown. Measured 2026-11-04: every redraw
        // landed addresses that served a few hundred 200s before dying, so the
        // unconditional reset kept the ladder on the 90 s rung for 90 min (41
        // rotations in 2 h, median gap 112 s), and each rotate tore the fleet
        // (all 279 client 503s of 08:30-09:20Z fell inside two rotate minutes).
        if (Date.now() - lastRotate > ROTATE_COOLDOWN_MS) autoRotations = 0;
        usedState.hard = 0;
        saveState();
      }
    }

    const outHeaders = responseHeaders(resp.headers);
    // Retry telemetry for the caller's journal: present only when this request
    // actually ate a provider error and was retried on another egress.
    if (retryAttempts > 0) {
      outHeaders.set("x-sidecar-attempts", String(retryAttempts));
      outHeaders.set("x-sidecar-retry-ms", String(Math.max(0, retryMs)));
    }
    // 4xx/5xx bodies are small and worth logging; 200 streams pass through
    // untouched so the client keeps real SSE latency.
    if (status !== 200) {
      const text = await resp.text();
      logReq(status, usedIp, text.slice(0, 300));
      return new Response(text, { status, statusText: resp.statusText, headers: outHeaders });
    }
    logReq(status, usedIp, status === 200 ? "" : (lastErr || ""));
    // Pass-through with a lifetime: the counter must drop when the SSE stream
    // ENDS, not when the Response object is built, or a redraw still tears it.
    if (!resp.body) return new Response(null, { status, statusText: resp.statusText, headers: outHeaders });
    const reader = resp.body.getReader();
    liveStreams++;
    let released = false;
    const release = () => { if (!released) { released = true; liveStreams--; } };
    const watched = new ReadableStream({
      async pull(c) {
        try {
          const { done, value } = await reader.read();
          if (done) { release(); c.close(); } else c.enqueue(value);
        } catch (e) { release(); c.error(e); }
      },
      cancel(reason) { release(); return reader.cancel(reason).catch(() => {}); },
    });
    // Backstop: an abrupt client disconnect normally lands in cancel(), but a
    // version that skips it would leave the counter up forever and disable every
    // rotation. release() is idempotent.
    try { req.signal?.addEventListener?.("abort", release, { once: true }); } catch {}
    return new Response(watched, { status, statusText: resp.statusText, headers: outHeaders });
  },
}); } catch (e) {
  // Port taken by an older instance (watchdog + Run key can race): that instance
  // keeps serving, so exiting 1 is correct — but say so loudly.
  logLine({ ts: new Date().toISOString(), event: "bind_failed", port: PORT, err: String(e).slice(0, 300) });
  process.exit(1);
} })();
// Only the process that actually owns the port may write shared state: a loser
// of the watchdog/Run-key race used to clear parks in memory and persist them
// from its exit hook while the real instance kept serving.
ownsPort = true;
console.log(`sidecar v2 listening on ${PORT}; pool=${POOL_API} front=${FRONT_PROXY}`);
setTimeout(() => { uplinkTick().catch(() => {}); }, 1500).unref?.();
setInterval(() => { uplinkTick().catch(() => {}); }, UPLINK_CHECK_MS).unref?.();
// A redraw that bought addresses but no service is the exact shape the mint
// gate could not see (fresh 14-24, spent 0, zero 200s): escalate to devices.
setInterval(() => {
  const w = redrawWatch;
  if (!w) return;
  if (Date.now() - w.at < REDRAW_STERILE_MS) return;
  redrawWatch = null;
  if (Date.now() - lastOKAt <= ROTATE_IDLE_MS) return; // something served meanwhile
  logLine({ ts: new Date().toISOString(), event: "redraw_sterile", reason: w.reason, age_s: Math.round((Date.now() - w.at) / 1000), fresh: w.ips.size, since_ok_s: Math.round((Date.now() - lastOKAt) / 1000) });
  refillCapacity(w.reason + ":sterile", true).catch(() => {});
}, 30_000).unref?.();
setInterval(() => { healTick().catch(() => {}); }, HEAL_TICK_MS).unref?.();
