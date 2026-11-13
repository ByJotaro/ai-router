// Unit tests for the sidecar's pure identity/accounting rules.
// Run: bun test test/identity.test.mjs
import { describe, expect, test } from "bun:test";
import {
  ACCEPTED_CIPHERS, burnGroups, FP_EXTS, classify429, dedupeByIp, hash32, mintSession, normalizeOverride,
  pruneByField, pruneMap, prunePins, recordBurned, recordIps, retryAfterSeconds,
  SESSION_ID_RE, prefix64, seededPerm, sessionKey, specFor, specForRequest, specSignature,
} from "../lib/identity.mjs";

const hdrs = (obj) => new Headers(obj);

describe("fingerprint spec", () => {
  test("is deterministic for the same client seed", () => {
    const a = specFor("ses_abc|deepseek-v4.1-flash-free");
    const b = specFor("ses_abc|deepseek-v4.1-flash-free");
    expect(a).toEqual(b);
    expect(specSignature(a)).toBe(specSignature(b));
  });

  test("is a valid hello: permutation of the accepted extension set", () => {
    for (let i = 0; i < 200; i++) {
      const s = specFor("k" + i);
      expect(["bun", "wreq"]).toContain(s.kind);
      expect(["HTTP1", "HTTP2"]).toContain(s.alpn);
      expect([...s.order].sort((x, y) => x - y)).toEqual([...FP_EXTS].sort((x, y) => x - y));
      expect(new Set(s.order).size).toBe(FP_EXTS.length);
      expect(s.id).toMatch(/^[bw]-[0-9a-z]{7}$/);
    }
  });

  test("spreads clients across kinds, ALPN and extension orders", () => {
    const kinds = new Set();
    const alpns = new Set();
    const orders = new Set();
    for (let i = 0; i < 4096; i++) {
      const s = specFor("spread-" + i);
      kinds.add(s.kind); alpns.add(s.alpn); orders.add(s.order.join(","));
    }
    expect(kinds.size).toBe(2);
    expect(alpns.size).toBe(2);
    expect(orders.size).toBeGreaterThan(4000); // 11! space, no clustering
  });

  test("display ids are labels, not keys: 32-bit, so a rare collision is fine", () => {
    // Sessions are keyed by the exact hello (see sessionKey), so an id
    // collision only merges a log line. With 5000 clients ~3 collisions are
    // expected in a 2^32 space; anything above 1% would mean a broken hash.
    const ids = new Set();
    for (let i = 0; i < 5000; i++) ids.add(specFor("client-" + i).id);
    expect(ids.size).toBeGreaterThan(4950);
  });

  test("session key is the exact hello, so an id collision cannot share a session", () => {
    const a = { kind: "bun", alpn: "HTTP1", order: [...FP_EXTS], id: "b-0000000" };
    const b = { kind: "wreq", alpn: "HTTP2", order: [...FP_EXTS].reverse(), id: "b-0000000" };
    expect(sessionKey("http://127.0.0.1:1", a)).not.toBe(sessionKey("http://127.0.0.1:1", b));
    // same hello on a different egress must NOT share a session either
    expect(sessionKey("http://127.0.0.1:1", a)).not.toBe(sessionKey("http://127.0.0.1:2", a));
    expect(sessionKey("http://127.0.0.1:1", a)).toBe(sessionKey("http://127.0.0.1:1", { ...a }));
  });

  test("hash32 is stable and 32-bit", () => {
    expect(hash32("abc")).toBe(hash32("abc"));
    expect(hash32("abc")).not.toBe(hash32("abd"));
    for (const v of [hash32(""), hash32("x".repeat(1000))]) {
      expect(v).toBeGreaterThanOrEqual(0);
      expect(v).toBeLessThanOrEqual(0xffffffff);
    }
  });

  test("seededPerm never mutates the shared extension list", () => {
    const before = [...FP_EXTS];
    seededPerm(12345);
    expect(FP_EXTS).toEqual(before);
  });
});

describe("pinned override", () => {
  const good = { kind: "wreq", alpn: "HTTP1", order: [...FP_EXTS] };

  test("accepts a complete valid spec", () => {
    expect(normalizeOverride(good)).toEqual(good);
    expect(specForRequest(good, "ignored").id).toBe("ov-wh1");
  });

  test("rejects the legacy raw-string form instead of spreading it", () => {
    // Old builds wrote "c3perm"; {...("c3perm")} produced kind undefined ->
    // a wreq session with alpnProtocols [undefined] on every request.
    expect(normalizeOverride("c3perm")).toBeNull();
    const spec = specForRequest("c3perm", "seed-1");
    expect(spec.kind).toBe(specFor("seed-1").kind);
    expect(spec.id).not.toStartWith("ov-");
  });

  test("rejects partial or malformed specs", () => {
    expect(normalizeOverride({ kind: "wreq", alpn: "HTTP1" })).toBeNull();
    expect(normalizeOverride({ ...good, kind: "firefox" })).toBeNull();
    expect(normalizeOverride({ ...good, alpn: "h1" })).toBeNull();
    expect(normalizeOverride({ ...good, order: [0, 23, 65281] })).toBeNull();
    expect(normalizeOverride({ ...good, order: [...FP_EXTS, 0] })).toBeNull();
    expect(normalizeOverride({ ...good, order: [...FP_EXTS].map((v, i) => (i ? v : 9999)) })).toBeNull();
    expect(normalizeOverride([])).toBeNull();
    expect(normalizeOverride(null)).toBeNull();
  });
});

describe("Retry-After parsing and 429 class", () => {
  const now = 1_700_000_000_000;

  test("numeric, seconds and zero", () => {
    expect(retryAfterSeconds(hdrs({ "retry-after": "3600" }), now)).toBe(3600);
    expect(retryAfterSeconds(hdrs({ "retry-after": "0" }), now)).toBe(0);
    expect(retryAfterSeconds(hdrs({ "retry-after": "-5" }), now)).toBe(0);
  });

  test("HTTP-date is converted to a delta", () => {
    const at = new Date(now + 120_000).toUTCString();
    expect(retryAfterSeconds(hdrs({ "retry-after": at }), now)).toBe(120);
    const past = new Date(now - 60_000).toUTCString();
    expect(retryAfterSeconds(hdrs({ "retry-after": past }), now)).toBe(0);
  });

  test("missing or garbage is 0, not NaN", () => {
    expect(retryAfterSeconds(hdrs({}), now)).toBe(0);
    expect(retryAfterSeconds(hdrs({ "retry-after": "soon" }), now)).toBe(0);
    expect(retryAfterSeconds(null, now)).toBe(0);
  });

  test("short RA is a soft park for the default window", () => {
    const c = classify429(0, now, { parkMs: 600_000 });
    expect(c).toEqual({ until: now + 600_000, hard: false, ra: 0 });
    expect(classify429(60, now).hard).toBe(false);
    expect(classify429(899, now).hard).toBe(false);
  });

  test("long RA is a hard park until that time", () => {
    const c = classify429(3600, now);
    expect(c).toEqual({ until: now + 3_600_000, hard: true, ra: 3600 });
    expect(classify429(900, now).hard).toBe(true);
  });

  test("absurd RA is clamped to 12h", () => {
    expect(classify429(10_000_000, now).until).toBe(now + 12 * 3600_000);
  });
});

describe("egress dedupe", () => {
  const b = (port, ip) => ({ proxy: `http://127.0.0.1:${port}`, ip });

  test("drops the second slot behind one address, keeps the first", () => {
    const { kept, dups } = dedupeByIp([b(1, "a"), b(2, "b"), b(3, "b")]);
    expect(kept.map((x) => x.proxy)).toEqual(["http://127.0.0.1:1", "http://127.0.0.1:2"]);
    expect(dups).toEqual([{ ip: "b", kept: "http://127.0.0.1:2", dropped: "http://127.0.0.1:3" }]);
  });

  test("keeps entries without an IP and preserves order", () => {
    const { kept, dups } = dedupeByIp([b(1, ""), b(2, "a"), b(3, ""), b(4, "a")]);
    expect(kept.map((x) => x.proxy)).toEqual([
      "http://127.0.0.1:1", "http://127.0.0.1:2", "http://127.0.0.1:3",
    ]);
    expect(dups.length).toBe(1);
  });

  test("ignores malformed entries", () => {
    const { kept } = dedupeByIp([null, {}, b(1, "a"), undefined]);
    expect(kept.length).toBe(1);
  });
});

describe("IP registry", () => {
  test("first discovery seeds without counting reuses", () => {
    const hist = {};
    const r1 = recordIps(hist, new Set(), ["a", "b"], 10, { init: false });
    expect(r1).toMatchObject({ reuses: 0, fresh: 2, reused: [] });
    expect([...r1.present]).toEqual(["a", "b"]);
    expect(hist.a).toMatchObject({ seen: 1, burned: 0, reuse: 0 });
  });

  test("an address that leaves and returns is a reuse, a stable one is not", () => {
    const hist = {};
    let present = new Set();
    ({ present } = recordIps(hist, present, ["a", "b"], 1, { init: false }));
    const stable = recordIps(hist, present, ["a", "b"], 2);
    expect(stable.reuses).toBe(0);
    present = stable.present;
    const afterLeave = recordIps(hist, present, ["a"], 3);
    expect(afterLeave.reuses).toBe(0);
    present = afterLeave.present;
    const back = recordIps(hist, present, ["a", "b"], 4);
    expect(back.reuses).toBe(1);
    expect(back.reused).toEqual(["b"]);
    expect(hist.b.reuse).toBe(1);
    expect(hist.b.seen).toBe(3);
  });

  test("an address that stays absent is never counted as returning", () => {
    const hist = {};
    let present = new Set();
    ({ present } = recordIps(hist, present, ["a", "b"], 1, { init: false }));
    for (let t = 2; t < 6; t++) ({ present } = recordIps(hist, present, ["a"], t));
    expect(hist.b.reuse).toBe(0);
  });

  test("burns accumulate per address", () => {
    const hist = {};
    recordBurned(hist, "a", 1);
    recordBurned(hist, "a", 2);
    expect(hist.a.burned).toBe(2);
    expect(recordBurned(hist, "", 3)).toBeNull();
    expect(Object.keys(hist)).toEqual(["a"]);
  });

  test("registry is pruned by last-seen, keeping the newest", () => {
    const hist = {};
    for (let i = 0; i < 10; i++) hist["ip" + i] = { last: i };
    const dropped = pruneByField(hist, 4, "last");
    expect(dropped.length).toBe(6);
    expect(Object.keys(hist).sort()).toEqual(["ip6", "ip7", "ip8", "ip9"]);
  });
});

describe("bounded state", () => {
  test("pins keep the newest conversations", () => {
    const pins = {};
    for (let i = 0; i < 10; i++) pins["c" + i] = { at: i };
    const dropped = prunePins(pins, 3);
    expect(dropped.sort()).toEqual(["c0", "c1", "c2", "c3", "c4", "c5", "c6"]);
    expect(Object.keys(pins).sort()).toEqual(["c7", "c8", "c9"]);
  });

  test("pins under the cap are untouched", () => {
    const pins = { a: { at: 1 } };
    expect(prunePins(pins, 10)).toEqual([]);
    expect(Object.keys(pins)).toEqual(["a"]);
  });

  test("generic map drops oldest insertions", () => {
    const m = new Map([["a", 1], ["b", 2], ["c", 3], ["d", 4]]);
    const dropped = pruneMap(m, 2);
    expect(dropped).toEqual(["a", "b"]);
    expect([...m.keys()]).toEqual(["c", "d"]);
    expect(pruneMap(m, 5)).toEqual([]);
  });
});

describe("cipher list", () => {
  test("is the accepted 17-entry Chrome list", () => {
    const list = ACCEPTED_CIPHERS.split(":");
    expect(list.length).toBe(17);
    expect(list[0]).toBe("TLS_AES_128_GCM_SHA256");
    expect(list).toContain("ECDHE-ECDSA-CHACHA20-POLY1305");
    expect(new Set(list).size).toBe(list.length);
  });
});

describe("minted session ids", () => {
  const T0 = 1_763_000_000_000;

  test("match the shape the CLI emits", () => {
    const id = mintSession("conv-a", T0);
    expect(id).toMatch(SESSION_ID_RE);
    expect(id.length).toBe(30);
  });

  test("the 12 hex chars encode ~(ms*0x1000+counter), so ids descend in time", () => {
    const hexAt = (ms) => parseInt(mintSession("conv-a", ms).slice(4, 16), 16);
    const a = hexAt(T0);
    const b = hexAt(T0 + 1_000);
    expect(b).toBeLessThan(a);                   // descending ids get smaller
    // Six bytes only carry ms >> 12 through a 40-bit window: the low 28 bits of
    // the timestamp survive (the CLI has the same wrap, ~3.1 days).
    const back = BigInt(a) ^ 0xffffffffffffn;    // un-complement
    const counter = Number(back & 0xfffn);
    expect(counter).toBeGreaterThanOrEqual(1);
    expect(counter).toBeLessThanOrEqual(16);
    expect(Number((back >> 12n) & 0xfffffffn)).toBe(T0 % 2 ** 28);
  });

  test("same seed + same ms is stable, different seeds diverge", () => {
    expect(mintSession("conv-a", T0)).toBe(mintSession("conv-a", T0));
    expect(mintSession("conv-a", T0)).not.toBe(mintSession("conv-b", T0));
    expect(mintSession("conv-a", T0)).not.toBe(mintSession("conv-a", T0 + 1));
  });
});

describe("limiter prefix", () => {
  test("the daily budget is keyed on /64, like the zen server", () => {
    expect(prefix64("2a09:bac5:48eb:261e::3cc:66")).toBe("2a09:bac5:48eb:261e");
    expect(prefix64("2a09:bac5:48eb:261f:1:2:3:4")).toBe("2a09:bac5:48eb:261f");
    expect(prefix64("")).toBe("");
  });
});

describe("per-/64 burn registry", () => {
  test("records the upstream's window per prefix as observation, never as a verdict", () => {
    const burns = {};
    const t0 = 1_800_000_000_000;
    burnGroups(burns, "2a09:bac5:48f2:1f5::32:43", 1120, t0);
    const row = burns["2a09:bac5:48f2:1f5"];
    expect(row.until).toBe(t0 + 1120_000);
    expect(row.ra).toBe(1120);
    // a later short window never shortens the one already known
    burnGroups(burns, "2a09:bac5:48f2:1f5::32:44", 60, t0 + 1000);
    expect(burns["2a09:bac5:48f2:1f5"].until).toBe(t0 + 1120_000);
    expect(burns["2a09:bac5:48f2:1f5"].hits).toBe(2);
    // an IPv4 address carries no /64 and must not create a group
    expect(burnGroups(burns, "203.0.113.7", 600, t0)).toBeNull();
  });
});
