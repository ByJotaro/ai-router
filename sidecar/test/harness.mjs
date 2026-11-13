// Test harness: a real fake pool + forward proxies + upstream with a 429
// window, plus a spawned sidecar wired to them. Everything binds ephemeral
// ports, so the suite can run next to the live stack without touching it.
import { mkdtempSync, mkdirSync, readFileSync, rmSync } from "node:fs";
import { join, resolve } from "node:path";

export const SIDECAR_DIR = resolve(import.meta.dir, "..");

function listen(handler) {
  const srv = Bun.serve({ port: 0, idleTimeout: 30, fetch: handler });
  return srv;
}

export function tempDir(tag) {
  const base = process.env.SIDECAR_TEST_DIR || join("D:/Temp/s", "sidecar-tests");
  mkdirSync(base, { recursive: true });
  const dir = mkdtempSync(join(base, tag + "-"));
  return dir;
}

// startWorld() returns handles + controls:
//   world.upstream.hits   [{sid, egress, status}]
//   world.upstream.burn(port) / burnAll(bool)
//   world.pool.setDup(bool)
export function startWorld({ dup = false, retryAfter = "3600", textOnly = [] } = {}) {
  const hits = [];
  // Egresses whose engine instance has no image input: the free pool mixes
  // multimodal and text-only instances behind different exits (measured
  // 2026-11-05), so an image request must hop to another egress, not fail.
  const textOnlyEgresses = new Set(textOnly);
  const burned = new Set();
  // Egresses whose tunnel accepts the request and never answers (a dead WARP
  // worker behind a still-listening port). The sidecar must demote them at the
  // first timeout instead of paying the TTFB deadline on every pick.
  const hangEgresses = new Set();
  // Optional id-specific refusal, used to exercise the sidecar's id probe. The
  // real zen gate does not do this (any well-formed id works); the rule only
  // models a future gate and the omitted-header case.
  const dead = new Set();
  let refuseOnce = false;
  let burnAll = false;
  let refusePathPrefix = "";
  // Every upstream answer waits this long: lets a test start two requests that
  // both observe the same 429 before the first one finishes its probe.
  let lagMs = 0;

  const upstream = listen(async (req) => {
    const egress = Number(req.headers.get("x-fake-egress") || "0");
    const sid = req.headers.get("x-opencode-session") || "";
    const pathname = new URL(req.url).pathname;
    const text = await req.text();
    if (lagMs) await Bun.sleep(lagMs);
    const raw = text; // full request body, inspected for image parts below
    if (hangEgresses.has(egress)) {
      // Record the attempt, then never answer: the client's own TTFB deadline
      // is the only way out, exactly like a dead WARP tunnel.
      hits.push({ sid, egress, status: 0, pathname });
      await new Promise(() => {});
    }
    if (textOnlyEgresses.has(egress) && raw.includes('"image_url"')) {
      hits.push({ sid, egress, status: 400, pathname, images: true });
      return new Response(JSON.stringify({ type: "error", error: { type: "invalid_request_error", message: "image input is not available on this engine" } }), {
        status: 400,
        headers: { "content-type": "application/json" },
      });
    }
    if (refuseOnce && sid) { refuseOnce = false; dead.add(sid); }
    if (burnAll || burned.has(egress) || dead.has(sid) || (refusePathPrefix && pathname.startsWith(refusePathPrefix))) {
      hits.push({ sid, egress, status: 429, pathname, images: raw.includes('"image_url"') });
      return new Response(JSON.stringify({ type: "error", error: { type: "FreeUsageLimitError", message: "fake window" } }), {
        status: 429,
        headers: { "content-type": "application/json", "retry-after": retryAfter },
      });
    }
    hits.push({ sid, egress, status: 200, pathname, images: raw.includes('"image_url"') });
    return new Response('data: {"choices":[{"delta":{"content":"ok"}}]}\n\ndata: [DONE]\n\n', {
      status: 200,
      headers: { "content-type": "text/event-stream" },
    });
  });

  // One proxy per egress slot; each stamps its own slot index. A slot added
  // later (pool growth) gets a fresh index, i.e. an egress whose window nobody
  // has spent - which is exactly what growing the pool buys in production.
  const slots = [];
  const addSlot = () => {
    const idx = slots.length;
    const srv = listen(async (req) => {
      const url = new URL(req.url);
      const headers = new Headers(req.headers);
      headers.delete("host");
      headers.delete("content-length");
      headers.set("x-fake-egress", String(idx));
      const body = req.method === "GET" || req.method === "HEAD" ? undefined : await req.text();
      const res = await fetch(`http://127.0.0.1:${upstream.port}${url.pathname}${url.search}`, { method: req.method, headers, body });
      return new Response(res.body, { status: res.status, headers: res.headers });
    });
    slots.push(srv);
  };
  for (let i = 0; i < 3; i++) addSlot();
  let dupOn = dup;
  const downSlots = new Set();
  const ipOverride = new Map();
  let rotates = 0;
  let retunnels = 0;
  let retunnelStatus = 200;
  let retunnelSame = false;
  const registers = [];
  const rotateCfg = { hang: false, fresh: false };
  let rotateShift = 0;
  // Account book for the mint path: registration is refused past `cap` (the
  // real daemon has maxAccounts=200), which is what forces the prune step.
  const accounts = [];
  const poolSets = [];
  const poolSizes = [];
  let acctCap = 200;
  const seedAccounts = (n) => { for (let i = 0; i < n; i++) accounts.push({ id: `acc-${accounts.length + 1}`, created_at: `2026-08-${String(i + 1).padStart(2, "0")}T00:00:00Z`, active: false }); };
  const pool = listen(async (req) => {
    const path = new URL(req.url).pathname;
    if (path.endsWith("/api/v1/accounts") && req.method === "GET") {
      return Response.json({ accounts });
    }
    if (path.startsWith("/api/v1/accounts/") && req.method === "DELETE") {
      const id = decodeURIComponent(path.split("/").pop());
      const i = accounts.findIndex((a) => a.id === id);
      if (i < 0) return Response.json({ error: "not found" }, { status: 404 });
      accounts.splice(i, 1);
      return Response.json({ deleted: id });
    }
    if (path.endsWith("/api/v1/accounts")) {
      // warp-router's registration job: the sidecar uses it to mint devices
      // whose egress we have never burned.
      let body = {};
      try { body = JSON.parse(await req.text()); } catch {}
      const count = Number(body.count) || 1;
      if (accounts.length + count > acctCap) {
        return new Response(JSON.stringify({ error: { message: `account limit reached (${acctCap})` } }), { status: 400, headers: { "content-type": "application/json" } });
      }
      registers.push(body);
      for (let i = 0; i < count; i++) accounts.push({ id: `new-${accounts.length + 1}`, created_at: new Date().toISOString(), active: false });
      return Response.json({ job_id: `j${registers.length}` }, { status: 202 });
    }
    if (path.startsWith("/api/v1/jobs/")) {
      return Response.json({ id: path.split("/").pop(), state: "done", message: "registered", progress: 100 });
    }
    if (path.endsWith("/api/v1/pool") && req.method === "POST") {
      // PoolSet: enabled=false drops the fleet, enabled=true respawns every
      // slot from the scanner's endpoint cache (the respread lever).
      let body = {};
      try { body = JSON.parse(await req.text()); } catch {}
      poolSets.push(body);
      // PoolSet is the only way to rebuild the tunnels: it must actually drop
      // the fleet on enabled=false, otherwise a caller that waits for "no
      // backends up" (pool restart on an uplink change) would hang for its
      // whole timeout.
      if (body.enabled === false) { downSlots.clear(); for (let i = 0; i < slots.length; i++) downSlots.add(i); }
      if (body.enabled === true) {
        downSlots.clear();
        poolSizes.push(Number(body.size) || 0);
        // A size in PoolSet is a resize: warp-router spawns the missing workers
        // (new handshakes, new egress) or retires the extras.
        const want = Math.max(1, Math.min(64, Number(body.size) || slots.length));
        while (slots.length < want) addSlot();
        while (slots.length > want) { const s = slots.pop(); try { s.stop(true); } catch {} }
      }
      return Response.json({ ok: true });
    }
    if (path.endsWith("/pool/retunnel")) {
      // The egress slot comes from the HANDSHAKE: a retunnel redraws prefixes on
      // the same accounts. This is the first move when the held slots are spent.
      retunnels++;
      try { await req.text(); } catch {}
      if (retunnelStatus !== 200) {
        return new Response(JSON.stringify({ error: "retunnel refused" }), { status: retunnelStatus, headers: { "content-type": "application/json" } });
      }
      if (retunnelSame) return Response.json({ retunneled: true, respawned: slots.length });
      rotateShift++;
      for (let i = 0; i < slots.length; i++) ipOverride.set(i, `2a09:bad2:${i}:${rotateShift}::${i + 1}`);
      return Response.json({ retunneled: true, respawned: slots.length });
    }
    if (path.endsWith("/rotate")) {
      rotates++;
      try { await req.text(); } catch {}
      if (rotateCfg.fresh) {
        rotateShift++;
        for (let i = 0; i < slots.length; i++) ipOverride.set(i, `2a09:bad0:${i}:9${rotateShift}::${i + 1}`);
      }
      if (rotateCfg.hang) {
        // warp-router respawns the whole fleet inside the handler; the caller is
        // dropped by the timeout before an answer exists (live: WriteTimeout 30 s).
        await new Promise(() => {});
      }
      return Response.json({ ok: true, rotated: true });
    }
    const backends = slots.map((s, i) => ({
      state: downSlots.has(i) ? "down" : "up",
      http_addr: `127.0.0.1:${s.port}`,
      public_ip: ipOverride.get(i) || (dupOn && i === 2 ? "2a09:bad0:1:1::2" : `2a09:bad0:${i}:1::${i + 1}`),
    }));
    return Response.json({ backends });
  });

  return {
    upstream: {
      port: upstream.port,
      hits,
      burnAll: (v) => { burnAll = v; },
      burn: (slot) => burned.add(slot),
      unburn: () => burned.clear(),
      refuseFirstOnce: () => { refuseOnce = true; },
      refusePath: (prefix) => { refusePathPrefix = prefix || ""; },
      setLag: (ms) => { lagMs = Number(ms) || 0; },
      setTextOnly: (i, on = true) => { if (on) textOnlyEgresses.add(i); else textOnlyEgresses.delete(i); },
      hang: (i, on = true) => { if (on) hangEgresses.add(i); else hangEgresses.delete(i); },
      isRefused: (sid) => dead.has(sid),
    },
    pool: {
      port: pool.port,
      setDup: (v) => { dupOn = v; },
      setDown: (i, down = true) => { if (down) downSlots.add(i); else downSlots.delete(i); },
      setIp: (i, ip) => { if (ip) ipOverride.set(i, ip); else ipOverride.delete(i); },
      rotates: () => rotates,
      retunnels: () => retunnels,
      setRetunnelStatus: (v) => { retunnelStatus = v; },
      setRetunnelSame: (v) => { retunnelSame = v; },
      registers: () => registers,
      setAccountCap: (n) => { acctCap = n; },
      poolSets: () => poolSets,
      poolSizes: () => poolSizes,
      seedAccounts,
      accounts: () => accounts.slice(),
      setRotate: (c) => { Object.assign(rotateCfg, c); },
    },
    slots: slots.map((s) => s.port),
    stop() {
      upstream.stop(true);
      pool.stop(true);
      for (const s of slots) s.stop(true);
    },
  };
}

export function freePort() {
  const srv = Bun.serve({ port: 0, fetch: () => new Response("") });
  const port = srv.port;
  srv.stop(true);
  return port;
}

export async function waitForSidecar(port, ms = 20_000) {
  const deadline = Date.now() + ms;
  let lastErr = "";
  while (Date.now() < deadline) {
    try {
      const r = await fetch(`http://127.0.0.1:${port}/admin/state`, { signal: AbortSignal.timeout(3000) });
      if (r.ok) return await r.json();
      lastErr = `status ${r.status}`;
    } catch (e) {
      lastErr = String(e);
    }
    await Bun.sleep(200);
  }
  throw new Error(`sidecar not ready on ${port}: ${lastErr}`);
}

export function startSidecar(env) {
  return Bun.spawn([process.execPath, "sidecar2.mjs"], {
    cwd: SIDECAR_DIR,
    env: {
      ...process.env,
      SIDECAR_FRONT_PROXY: "",
      SIDECAR_ALLOW_ROTATE: "0",
      // The wall/rotation tests assert the immediate contract. Holding a
      // zero-candidate request is a separate behaviour with its own test; a
      // 35 s default here would only turn every wall assertion into a timeout.
      SIDECAR_CLIENT_WAIT_MS: "0",
      ...env,
    },
    stdout: "pipe",
    stderr: "pipe",
  });
}

export function readLog(file) {
  try {
    return readFileSync(file, "utf8").trim().split("\n").filter(Boolean).map((l) => JSON.parse(l));
  } catch {
    return [];
  }
}

// chat() proxies one OpenAI-shaped request through the sidecar exactly like
// ai-router does: the conversation is the x-opencode-session header.
// Real client traffic carries a big system prompt (~1 MB measured on the live
// path) and a tool list. Both matter to the sidecar's parking rules: a 429 only
// teaches a budget verdict for an AGENT-SHAPED call (stream:true + tools), and
// only bodies >= REPRESENTATIVE_BYTES count, so a probe-sized body must not
// park an egress. The default here is therefore agent-shaped; opts.small opts
// out of the size, not of the tools.
const PAD = "context ".repeat(1100); // ~9 KB
const TOOLS = [{
  type: "function",
  function: {
    name: "read",
    description: "Read a file from the workspace",
    parameters: { type: "object", properties: { path: { type: "string" } }, required: ["path"] },
  },
}];

export async function chat(port, conv, opts = {}) {
  const headers = { "content-type": "application/json", "x-opencode-session": conv, "x-opencode-session-id": conv };
  if (opts.nopark) headers["x-sidecar-nopark"] = "1";
  if (opts.backend) headers["x-sidecar-backend"] = String(opts.backend);
  const messages = opts.small
    ? [{ role: "user", content: "hi" }]
    : [{ role: "system", content: PAD }, { role: "user", content: "hi" }];
  if (opts.images) messages.push({ role: "user", content: [{ type: "text", text: "look" }, { type: "image_url", image_url: { url: "data:image/png;base64,AA" } }] });
  const body = { model: "deepseek-v4.1-flash-free", stream: true, messages };
  // `tools:false` models a call that is representative in size but not
  // agent-shaped: the sidecar must treat it as a probe (its 429 may not park).
  if (opts.tools !== false) body.tools = TOOLS;
  const res = await fetch(`http://127.0.0.1:${port}${opts.path || "/v1/chat/completions"}`, {
    method: "POST",
    headers,
    body: JSON.stringify(body),
    signal: AbortSignal.timeout(20_000),
  });
  return { status: res.status, headers: res.headers, text: await res.text() };
}

export function cleanup(dir) {
  try { rmSync(dir, { recursive: true, force: true }); } catch {}
}

