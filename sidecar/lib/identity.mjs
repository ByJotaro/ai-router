// Pure identity/accounting helpers for the opencode sidecar.
//
// Everything here is deterministic and side-effect free so it can be unit
// tested without starting servers or touching the network. The sidecar owns
// the wiring (files, timers, fetch); these functions own the rules:
//   - the TLS hello spec per client (and its exact session key)
//   - Retry-After parsing and 429 classification
//   - egress IP dedupe and the repeat/burn registry
//   - bounded-map pruning so long-lived state cannot grow forever

// Chrome-family extension set of the accepted hello, in its natural order.
export const FP_EXTS = [0, 23, 65281, 10, 11, 35, 16, 13, 51, 45, 43];

// Cipher list the gate keyed its checksum on (measured 2026-10-27): every
// accepted hello carried exactly this list; only the extension order, ALPN and
// stack varied.
export const ACCEPTED_CIPHERS = [
  "TLS_AES_128_GCM_SHA256", "TLS_AES_256_GCM_SHA384", "TLS_CHACHA20_POLY1305_SHA256",
  "ECDHE-ECDSA-AES128-GCM-SHA256", "ECDHE-RSA-AES128-GCM-SHA256", "ECDHE-ECDSA-AES256-GCM-SHA384",
  "ECDHE-RSA-AES256-GCM-SHA384", "ECDHE-ECDSA-CHACHA20-POLY1305", "ECDHE-RSA-CHACHA20-POLY1305",
  "ECDHE-ECDSA-AES128-SHA", "ECDHE-RSA-AES128-SHA", "ECDHE-ECDSA-AES256-SHA", "ECDHE-RSA-AES256-SHA",
  "AES128-GCM-SHA256", "AES256-GCM-SHA384", "AES128-SHA", "AES256-SHA",
].join(":");

export function hash32(str) {
  let h = 2166136261 >>> 0;
  for (let i = 0; i < String(str).length; i++) {
    h ^= String(str).charCodeAt(i);
    h = Math.imul(h, 16777619) >>> 0;
  }
  return h >>> 0;
}

export function seededPerm(seed) {
  const a = [...FP_EXTS];
  let x = seed >>> 0 || 1;
  for (let i = a.length - 1; i > 0; i--) {
    x = (Math.imul(x, 1664525) + 1013904223) >>> 0;
    const j = x % (i + 1);
    [a[i], a[j]] = [a[j], a[i]];
  }
  return a;
}

// Exact identity of a hello. Two specs with the same signature are the same
// fingerprint (and may share a TLS session); anything else must not, even when
// their short display ids happen to collide.
export function specSignature(spec) {
  return `${spec.kind}|${spec.alpn}|${(spec.order || []).join(".")}`;
}

// Deterministic spec for one client: same seed -> same hello forever.
export function specFor(seedKey) {
  const seed = hash32(String(seedKey || "anon"));
  const kind = (seed & 1) ? "wreq" : "bun";
  const spec = {
    kind,
    alpn: (seed & 2) ? "HTTP2" : "HTTP1",
    order: seededPerm(seed ^ 0x9e3779b9),
    seed,
  };
  // Short label for logs/stats only. Never a cache key: 4 base36 chars collide.
  spec.id = (kind === "bun" ? "b-" : "w-") + hash32(specSignature(spec)).toString(36).padStart(7, "0");
  return spec;
}

// The zen limiter keys on the first four hextets of the client address
// (`rawIp.split(":")[0..3]`, handler.ts), so two egresses in one /64 share one
// daily budget however different their full addresses look.
export function prefix64(ip) {
  const s = String(ip || "").toLowerCase();
  if (!s.includes(":")) return s;
  return s.split(":").slice(0, 4).join(":");
}

// ---- opencode session ids (client-side only) ------------------------------
// Measured against the public zen handler (v1.19.4,
// packages/console/app/src/routes/zen/util/handler.ts): `x-opencode-session` is
// read for logging and as the sticky-provider key, then deleted before the
// upstream call. The server never looks the id up, so any well-formed id works
// (measured on one egress, seconds apart: real 3/3 200, minted 8/8 200,
// header omitted 0/2 429). The id is therefore minted locally per conversation
// instead of being borrowed from opencode.db.
export const SESSION_ID_RE = /^ses_[0-9a-f]{12}[0-9A-Za-z]{14}$/;
const B62 = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz";

// Same shape the CLI emits (v1.19.4 identifier.ts, descending ids = the ones
// sessions carry): "ses_" + 6 big-endian bytes of ~(ms * 0x1000 + counter),
// counter in 1..16, then 14 base62 chars from the random part.
export function mintSession(seedKey, nowMs = Date.now()) {
  const now = Number(nowMs);
  const seed = hash32(`${seedKey || "anon"}|${now}`);
  const counter = 1 + (seed % 16);
  const value = ~(BigInt(now) * 0x1000n + BigInt(counter));
  let time = "";
  for (let i = 0; i < 6; i++) {
    time += Number((value >> BigInt(40 - 8 * i)) & 0xffn).toString(16).padStart(2, "0");
  }
  let x = seed;
  let rand = "";
  for (let i = 0; i < 14; i++) {
    x = (Math.imul(x, 1664525) + 1013904223) >>> 0;
    rand += B62[x % 62];
  }
  return "ses_" + time + rand;
}

function isPermutationOfExts(order) {
  if (!Array.isArray(order) || order.length !== FP_EXTS.length) return false;
  const want = new Set(FP_EXTS);
  for (const v of order) {
    if (!want.delete(v)) return false; // unknown, duplicate or wrong type
  }
  return want.size === 0;
}

// Hand-pinned fingerprint override, from v2_fp.json. Older builds wrote raw
// strings there ("c3perm"); spreading one into a spec produced a garbage hello
// (kind undefined -> wreq session with alpnProtocols [undefined]), so anything
// that is not a complete valid spec is rejected instead of half-honoured.
export function normalizeOverride(raw) {
  if (!raw || typeof raw !== "object" || Array.isArray(raw)) return null;
  const { kind, alpn, order } = raw;
  if (kind !== "bun" && kind !== "wreq") return null;
  if (alpn !== "HTTP1" && alpn !== "HTTP2") return null;
  if (!isPermutationOfExts(order)) return null;
  return { kind, alpn, order: [...order] };
}

export function specForRequest(override, seedKey) {
  const ov = normalizeOverride(override);
  if (!ov) return specFor(seedKey);
  const spec = { ...ov, seed: hash32(specSignature(ov)) };
  spec.id = "ov-" + (ov.kind === "bun" ? "b" : "w") + (ov.alpn === "HTTP2" ? "h2" : "h1");
  return spec;
}

// TLS session cache key: exact spec, never the short id.
export function sessionKey(proxy, spec) {
  return `${proxy}|${specSignature(spec)}`;
}

export function retryAfterSeconds(headers, nowMs = Date.now()) {
  try {
    const raw = headers && headers.get ? headers.get("retry-after") : null;
    if (raw == null || raw === "") return 0;
    const secs = Number(raw);
    if (Number.isFinite(secs)) return secs > 0 ? Math.floor(secs) : 0;
    const at = Date.parse(raw);
    if (Number.isFinite(at)) return Math.max(0, Math.floor((at - nowMs) / 1000));
  } catch {}
  return 0;
}

export const DEFAULT_PARK_MS = 10 * 60_000;
export const LONG_RA_S = 900;
export const MAX_RA_S = 12 * 3600;

// A 429 with a long Retry-After is a CLOSED WINDOW (hours), not a blip: park
// until then and mark it hard so nothing rolls the park back.
export function classify429(raSeconds, nowMs = Date.now(), opts = {}) {
  const parkMs = opts.parkMs ?? DEFAULT_PARK_MS;
  const longRaS = opts.longRaS ?? LONG_RA_S;
  const maxRaS = opts.maxRaS ?? MAX_RA_S;
  const ra = Number.isFinite(raSeconds) && raSeconds > 0 ? Math.floor(raSeconds) : 0;
  if (ra > 0) return { until: nowMs + Math.min(ra, maxRaS) * 1000, hard: ra >= longRaS, ra };
  return { until: nowMs + parkMs, hard: false, ra: 0 };
}

// One IP must never be served by two slots: two ports behind one address are
// one window, so routing to both halves the pool while looking bigger.
export function dedupeByIp(list) {
  const kept = [];
  const dups = [];
  const byIp = new Map();
  for (const b of list) {
    if (!b || !b.proxy) continue;
    if (!b.ip) { kept.push(b); continue; }
    const first = byIp.get(b.ip);
    if (first) { dups.push({ ip: b.ip, kept: first.proxy, dropped: b.proxy }); continue; }
    byIp.set(b.ip, b);
    kept.push(b);
  }
  return { kept, dups };
}

// Registry of every egress IP we have ever seen. A name that leaves the pool
// and comes back later is a repeat (`reuse`), not a fresh exit; `burned` counts
// how many times that address answered 429.
export function recordIps(history, present, ips, now = Date.now(), opts = {}) {
  const init = opts.init ?? true;
  const seenNow = new Set();
  for (const ip of ips) if (ip) seenNow.add(ip);
  let fresh = 0;
  const reused = [];
  for (const ip of seenNow) {
    const row = history[ip];
    if (row) {
      row.last = now;
      row.seen = (row.seen || 0) + 1;
      if (init && !present.has(ip)) { row.reuse = (row.reuse || 0) + 1; reused.push(ip); }
    } else {
      history[ip] = { first: now, last: now, seen: 1, burned: 0, reuse: 0 };
      fresh++;
    }
  }
  return { present: seenNow, reuses: reused.length, reused, fresh };
}

// Observation registry per /64. NOT a routing verdict: the per-/64 budget is
// documented for the legacy zen handler only, and on /inference the contrary is
// measured (2026-10-27: ::32:132 answered FreeUsageLimit ra=1121 while ::32:144,
// same /64, served continuous 200s). Used for reporting the upstream's own
// window (burned groups, reset ETA), never to park a healthy address.
export function burnGroups(burns, ip, raSeconds, now = Date.now(), maxS = 12 * 3600) {
  const p = prefix64(ip);
  if (!p || !String(p).includes(":")) return null;
  const ra = Number.isFinite(raSeconds) && raSeconds > 0 ? Math.min(Math.floor(raSeconds), maxS) : 0;
  const row = burns[p] || (burns[p] = { until: 0, at: now, ra: 0, hits: 0 });
  row.until = Math.max(row.until || 0, now + ra * 1000);
  row.at = now;
  row.ra = Math.max(row.ra || 0, ra);
  row.hits = (row.hits || 0) + 1;
  return row;
}

export function recordBurned(history, ip, now = Date.now()) {
  if (!ip) return null;
  const row = history[ip] || (history[ip] = { first: now, last: now, seen: 1, burned: 0, reuse: 0 });
  row.burned = (row.burned || 0) + 1;
  return row;
}

// Keep the newest `cap` pins (by `at`) and drop the rest: a long-lived sidecar
// sees every conversation it ever served else.
export function prunePins(pins, cap = 2000) {
  const keys = Object.keys(pins);
  if (keys.length <= cap) return [];
  // A conversation's value is an array of pinned pairs; `value.at` would be
  // Array.prototype.at (a function), so every key compared equal and the cap
  // evicted by insertion order instead of recency.
  const recency = (k) => {
    const v = pins[k];
    if (Array.isArray(v)) return v.reduce((mx, p) => Math.max(mx, (p && p.at) || 0), 0);
    return (v && v.at) || 0;
  };
  keys.sort((a, b) => recency(a) - recency(b));
  const drop = keys.slice(0, keys.length - cap);
  for (const k of drop) delete pins[k];
  return drop;
}

// Generic bounded Map: drop oldest inserted entries (Map keeps insertion order).
export function pruneMap(map, cap) {
  const drop = [];
  if (map.size <= cap) return drop;
  for (const k of map.keys()) {
    if (map.size - drop.length <= cap) break;
    drop.push(k);
  }
  for (const k of drop) map.delete(k);
  return drop;
}

// Generic bounded object by a numeric field (used for the IP registry).
export function pruneByField(obj, cap, field = "last") {
  const keys = Object.keys(obj);
  if (keys.length <= cap) return [];
  keys.sort((a, b) => (obj[a]?.[field] || 0) - (obj[b]?.[field] || 0));
  const drop = keys.slice(0, keys.length - cap);
  for (const k of drop) delete obj[k];
  return drop;
}
