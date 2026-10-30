/* ==================================================================
   DEMO MODE / EMPTY&ERROR STATES / COMBOS / NAV
   ================================================================== */
let DEMO = false;

function emptyHint(hint) {
  return '<div class="state"><div class="big" aria-hidden="true">◔</div><div class="title">данных пока нет</div><div class="hint">' + esc(hint || "запустите ядро или совершите первый запрос через шлюз") + '</div>' +
    '<button class="btn primary" data-demo title="Показать примерные данные" aria-label="Показать примерные данные">показать пример (демо)</button></div>';
}
function errHint(e) {
  return '<div class="state"><div class="big" aria-hidden="true">⚠</div><div class="title">Ошибка загрузки</div><div class="hint">' + esc((e && e.message) || e || "неизвестная ошибка") + '</div>' + '<div style="display:flex;gap:8px;justify-content:center;flex-wrap:wrap"><button class="btn primary sm" data-retry title="Повторить загрузку" aria-label="Повторить загрузку">повторить</button><button class="btn ghost sm" data-demo title="Показать примерные данные" aria-label="Показать примерные данные">демо</button></div></div>';
}
/* ---- детерминированный демо-сид: только ?demo=1; live-путь не тронут ---- */
function mulberry32(seed) { let t = seed >>> 0; return function () { t += 0x6D2B79F5; let z = Math.imul(t ^ (t >>> 15), t | 1); z ^= z + Math.imul(z ^ (z >>> 7), z | 61); return ((z ^ (z >>> 14)) >>> 0) / 4294967296; }; }
let __demoSeed = null;
function demoSeedFromURL() { try { const q = new URLSearchParams(location.search); if (q.get("demo") !== "1") return null; const s = q.get("seed"); if (s === null || s === "") return 42; const n = parseInt(s, 10); return Number.isFinite(n) ? (n >>> 0) : 42; } catch (e) { return 42; } }
function demoRand() { return __demoSeed ? __demoSeed() : Math.random(); }
const RND = (a, b) => a + Math.floor(demoRand() * (b - a + 1));
const RI = arr => arr[Math.floor(demoRand() * arr.length)];

/* ---- примерные данные, повторяющие структуру Wails-биндингов ---- */
function buildDemo() {
  const ds = demoSeedFromURL(); __demoSeed = (ds === null) ? null : mulberry32(ds);
  const today = new Date();
  const daily = { days: 30, requests: 0, prompt: 0, completion: 0, cache_read: 0, cache_write: 0, cost_usd: 0, series: Array.from({ length: 30 }, (_, i) => {
    const d = new Date(today); d.setDate(d.getDate() - (29 - i));
    const reqs = RND(30, 420);
    const prompt = reqs * RND(400, 900) * 1000;
    const completion = reqs * RND(150, 450) * 1000;
    const cr = Math.round(prompt * RND(0, 0.35));
    const cw = Math.round(prompt * RND(0, 0.12));
    const reasoning = reqs * RND(30, 200) * 1000;
    const cost = Math.round(prompt * 0.0016 / 1000 + completion * 0.006 / 1000);
    return { day: d.toISOString().slice(0, 10), requests: reqs, prompt, completion, cache_read: cr, cache_write: cw, reasoning, cost_usd: cost || 1 };
  }) };
  daily.request_sum = daily.series.reduce((a,p)=>a+p.requests, 0);
  daily.requests = daily.series.reduce((a, p) => a + p.requests, 0);
  daily.prompt = daily.series.reduce((a, p) => a + p.prompt, 0);
  daily.completion = daily.series.reduce((a, p) => a + p.completion, 0);
  daily.cache_read = daily.series.reduce((a, p) => a + p.cache_read, 0);

  const provSpec = [
    ["OpenAI", "openai", ["gpt-4o", "gpt-4o-mini", "o3-mini"], "premium"],
    ["Anthropic", "anthropic", ["claude-3-5-sonnet", "claude-3-haiku"], "premium"],
    ["Google", "google", ["gemini-2.5-pro", "gemini-2.5-flash"], "standard"],
    ["DeepSeek", "deepseek", ["deepseek-chat", "deepseek-reasoner"], "economy"],
    ["Mistral", "mistral", ["mistral-large", "mistral-small"], "standard"],
    ["Groq", "groq", ["llama-3.3-70b", "llama-3.1-8b"], "economy"],
  ];
  const end = provSpec.map((sp, idx) => {
    const on = demoRand() > 0.22;
    const keys = RND(3, 6);
    const healthy = Math.min(keys, Math.max(0, keys - (on ? RND(0, 1) : RND(2, 4))));
    const brk = on ? (demoRand() < 0.10 ? "open" : demoRand() < 0.12 ? "half_open" : "closed") : "closed";
    return {
      name: sp[0], type: sp[1], base_url: sp[2][0].length ? "https://api." + sp[0].toLowerCase() + ".example.com" : "",
      egress: "pool", egress_mode: "pool", models: sp[2], wildcard: false, fallback: idx === 0 ? [] : ["OpenAI"],
      timeout_s: 60, tier: sp[3], keys, keys_healthy: healthy, keys_backoff: keys - healthy,
      keys_pct: keys ? healthy / keys : -1, backoff_max_s: on ? 0 : RND(15, 120), last_err: on ? "" : "429 rate_limited",
      breaker: brk, breaker_until_s: brk === "closed" ? 0 : RND(20, 300), breaker_failures: brk === "closed" ? 0 : RND(3, 9),
    };
  });
  // Каталожные (не настроенные) провайдеры — как отдаёт бэкенд: type "catalog",
  // status "unconfigured", без ключей/моделей. Имена НЕ дублируют настроенные
  // выше (иначе «Не настроено» показывает те же Mistral/Groq).
  end.push(
    { name: "Together", type: "catalog", base_url: "https://api.together.xyz", egress: "", egress_mode: "off", models: [], wildcard: true, catalog: true, status: "unconfigured", auth_kind: "api-key", auth_hint: "ключ из консоли Together", fallback: [], timeout_s: 0, tier: "", keys: 0, keys_healthy: 0, keys_backoff: 0, keys_pct: -1, backoff_max_s: 0, last_err: "", breaker: "closed", breaker_until_s: 0, breaker_failures: 0 },
    { name: "Cohere", type: "catalog", base_url: "https://api.cohere.com", egress: "", egress_mode: "off", models: [], wildcard: true, catalog: true, status: "unconfigured", auth_kind: "api-key", auth_hint: "ключ из консоли Cohere", fallback: [], timeout_s: 0, tier: "", keys: 0, keys_healthy: 0, keys_backoff: 0, keys_pct: -1, backoff_max_s: 0, last_err: "", breaker: "closed", breaker_until_s: 0, breaker_failures: 0 }
  );
  const breakers = end.filter(e => e.breaker !== "closed").map(e => ({ provider: e.name, state: e.breaker, opened_until_s: e.breaker_until_s, failures: e.breaker_failures, consecutive_failures: e.breaker_failures, last_error: e.last_err || "upstream timeout" }));
  const locks = [{ model: "gpt-4o", locked: true, until_s: 120, failures: 9, last_err: "5xx burst" }, { model: "mistral-large", locked: false, until_s: 0, failures: 2, last_err: "" }];
  const mkKey = (nm, i, mods) => {
    const quotaT = RND(100, 500) * 1e6, quotaU = RND(20, 200) * 1e5, budget = RND(100, 2000) * 100;
    const usedT = Math.round(quotaT * (0.1 + demoRand() * 0.85));
    const usedU = Math.round(quotaU * (0.05 + demoRand() * 0.8));
    return { name: "key-" + nm.toLowerCase() + "-" + (i + 1), key: "sk-demo-" + nm.toLowerCase() + "-" + (100 + i), models: (mods && mods.length ? [RI(mods)] : []), quota_tokens: quotaT, quota_reqs: quotaU, budget_usd: budget, usage_tokens: usedT, usage_reqs: Math.round(usedU), usage_usd: Math.round(budget * 0.3), token_pct: usedT / quotaT, req_pct: 0.4, usd_pct: 0.3 };
  };
  const kbp = {};
  end.forEach((e, idx) => { kbp[e.name] = [mkKey(e.name, idx, e.models)]; });
  kbp["глобальные"] = [{ name: "global-wildcard", key: "sk-demo-global", models: [], quota_tokens: 2e9, quota_reqs: 1e7, budget_usd: 50000, usage_tokens: 1.2e9, usage_reqs: 6e6, usage_usd: 18000, token_pct: 0.6, req_pct: 0.6, usd_pct: 0.36 }];
  const keys = Object.values(kbp).flat();
  const usage = { keys: keys.length, revoked: 1, reqs: daily.request_sum || daily.requests, tokens: daily.prompt + daily.completion, usd: daily.series.reduce((a, p) => a + (p.cost_usd || 0), 0), budget_usd: 250000 };
  const health = end.map(e => ({ name: e.name, type: "provider", endpoint: e.base_url, latency_ms: RND(80, 1400), http: 200, ok: true, last_err: "" }));
  const logPool = end.filter(e => !e.catalog && e.models && e.models.length);
  const errIdx = new Set();
  { const order = Array.from({ length: 40 }, (_, k) => k);
    for (let k = order.length - 1; k > 0; k--) { const j = Math.floor(demoRand() * (k + 1)); const tmp = order[k]; order[k] = order[j]; order[j] = tmp; }
    order.slice(0, 7).forEach(k => errIdx.add(k)); }
  const logs = Array.from({ length: 40 }, (_, i) => {
    const e = RI(logPool.length ? logPool : end); const ok = !errIdx.has(i);
    const ec = ok ? 200 : (demoRand() < 0.75 ? 429 : (demoRand() < 0.5 ? 500 : 502));
    const mdl = RI(e.models && e.models.length ? e.models : logPool[0].models);
    const d = new Date(); d.setMinutes(d.getMinutes() - i * RND(1, 6));
    return { id: 1000 + i, ts: d.toISOString().slice(0, 19).replace("T", " "), model: mdl, requested_model: mdl, provider: e.name, account: "", duration_ms: RND(60, 2000), prompt: RND(80, 3000), completion: RND(30, 1200), cache_read: demoRand() > 0.7 ? RND(100, 900) : 0, cache_write: RND(0, 80), status: ok ? "ok" : "error", http_status: ec, error: ok ? "" : (ec === 429 ? "upstream rate-limited" : "upstream error " + ec), proxy: "", method: "POST", path: "/v1/chat/completions" };
  });
  const plogs = [{ id: 1, ts: new Date().toISOString().slice(0, 19).replace("T", " "), proxy: "ru-1", action: "connect", ok: true, latency_ms: 87 }, { id: 2, ts: "…", proxy: "us-2", action: "rotate", ok: false, latency_ms: 0, error: "dial timeout" }];
  const proxy = { enabled: true, egress_enabled: true, total: 12, alive: 9, dead: 3, connecting: 0, avg_latency_ms: 132, public_ip: "185.16.33.101", connect_rate: "0.94", failures_24h: 4, degraded: false, active_account_id: "pool/acc-1", accounts: Array.from({ length: 12 }, (_, i) => ({ id: "pool/acc-" + (i + 1), virtual_ip: "10.1.0." + (i + 1), country: RI(["RU", "DE", "NL", "US", "TR"]), latency_ms: RND(60, 300), healthy: i < 9, fail_count: i < 9 ? 0 : (i === 9 ? 7 : 2) })), servers: [], warp_pool: { api_url: "http://127.0.0.1:8787", enabled: true, state: "ready", size: 20, up: 18, starting: 2, down: 0, unique_egresses: 15, anycast_egresses: 4, acquire_total: 1240, acquire_ok: 1210, acquire_fail: 30, release_total: 1205, rotate_total: 3, restarts: 5, started_at: Math.floor(Date.now() / 1000) - 7200, uptime_s: 7200, last_acquire_at: Math.floor(Date.now() / 1000) - 2, last_acquire_err: "", last_rotate_at: Math.floor(Date.now() / 1000) - 1800, front_http: "http://127.0.0.1:8118", front_socks: "127.0.0.1:1080", backends: Array.from({ length: 20 }, (_, i) => ({ id: "be" + (i + 1) + "-c3d4", state: i < 18 ? "up" : "starting", public_ip: "2a09:bac" + (i % 5) + ":47a0:170::a6:" + (10 + i), country: "MD", socks_addr: "127.0.0.1:" + (8120 + i), http_addr: "127.0.0.1:" + (8620 + i), up_at: Math.floor(Date.now() / 1000) - 7100 + i * 5, requests: 20 + ((i * 37) % 100), errors: 0, fails: 0 })) } };
  const combos = [{ id: 1, name: "production-cascade", model: "gpt-4o", steps: end.slice(0, 3).map(e => ({ provider: e.name, model: e.models[0] })), created_at: new Date().toISOString(), updated_at: new Date().toISOString() }, { id: 2, name: "economy-fallback", model: "deepseek-chat", steps: end.slice(3, 5).map(e => ({ provider: e.name, model: e.models[0] })), created_at: new Date().toISOString(), updated_at: new Date().toISOString() }];
  const analytics = { daily_trend: daily.series.map(p => ({ date: p.day, prompt: p.prompt, completion: p.completion, cost_usd: p.cost_usd, requests: p.requests })), by_model: end.flatMap(e => (e.models || []).slice(0, 2).map(m => ({ model: m, provider: e.name, requests: RND(50, 900), prompt: RND(1, 40) * 1e6, completion: RND(1, 8) * 1e5, total: RND(1, 45) * 1e6, errors: RND(0, 12), last_used: new Date().toISOString() }))).slice(0, 8), by_provider: end.filter(e => !e.catalog).map(e => ({ provider: e.name, requests: RND(200, 2400), prompt: RND(5, 90) * 1e6, complete: RND(1, 12) * 1e6, total: RND(6, 100) * 1e6, errors: RND(0, 20), cost_usd: RND(0, 400) })) };
  return {
    status: { version: "1.0.0 (demo)", uptime_s: RND(100, 4000), gateway: "http://127.0.0.1:30080", config: "~/.omnigate/router.yaml", hub_url: "", hub_addr: "", admin_key: "demo", hub_online: false, providers: end.length, models: end.flatMap(e => e.models).length, keys: keys.length },
    end, breakers, locks, keys, kbp, usage, daily, health, logs, plogs, proxy, combos, analytics, cfg: {
      server: { host: "0.0.0.0", port: 30080, log_level: "info" }, admin_token: "demo-token",
      resilience: { breaker: { max_failures: 5, recovery_seconds: 60, half_open_successes: 3 }, cooldown: { base_seconds: 15, max_seconds: 300, factor: 2 }, lockout: { max_failures: 8, window_seconds: 60, lockout_seconds: 300 } },
      routing: { strategy: "priority", default_tier: "free", tiers: ["subscription", "api", "cheap", "free"], costs: { enabled: false } },
      egress: { enabled: true, base_url: "http://127.0.0.1:8801", token: "demo", strategy: "round_robin", rotation: { per_request: true, rotate_after: 10 } }, provider_defaults: { key_swap: "round_robin", error_policy: { "401": "retry_token", "429": "retry_proxy", "5xx": "retry", timeout: "retry", network: "retry", rate_limit: "retry" } },
    },
  };
}
function enableDemo() {
  DEMO = true;
  const d = buildDemo();
  S.status = d.status; S.end = d.end; S.breakers = d.breakers; S.locks = d.locks; S.keys = d.keys;
  S.kbp = d.kbp; S.usage = d.usage; S.daily = d.daily; S.health = d.health;
  S.logs = d.logs; S.plogs = d.plogs; S.proxy = d.proxy; S.combos = d.combos; S.cfg = d.cfg; S.analytics = d.analytics; S.tops90 = d.analytics;
  buildSparkMap();
  S.status.hub_online = false;
  const b = $("#demo-toggle"); if (b) { setCls(b, true, "active"); try { b.setAttribute("aria-pressed", "true"); } catch (e) {} }
  toast("Демо-режим: показаны примерные данные", "ok");
  refreshCurrent();
}
function disableDemo() { DEMO = false; const b = $("#demo-toggle"); if (b) { setCls(b, false, "active"); try { b.setAttribute("aria-pressed", "false"); } catch (e) {} } refreshCurrent(); }
function spinRefresh(on) { try { const ic = document.getElementById("refresh-ico"); if (ic) ic.classList.toggle("spin", !!on); const b = document.getElementById("refresh-btn"); if (b) b.setAttribute("aria-busy", String(!!on)); } catch (eS) {} }
function refreshCurrent() { if (document.hidden || S._refreshing) return; if (document.querySelector("#modal") || document.querySelector(".dd.open")) return; if (S.tab === "logs" && logsUserReading()) { setLogsPaused(true); return; } S._refreshing = true; spinRefresh(true); try { Promise.resolve(({ providers: loadProviders, usage: loadUsage, logs: loadLogs, proxies: loadProxies, settings: loadSettings, combos: loadCombos }[S.tab] || loadHome)()).catch(() => {}).finally(() => { S._refreshing = false; spinRefresh(false); }); } catch (e) { S._refreshing = false; spinRefresh(false); } }

const SWAPS = ["priority", "round_robin", "random", "least_used"];
const SWAP_LBL = { priority: "priority (по порядку)", round_robin: "round-robin (по кругу)", random: "random (случайно)", least_used: "least-used (наименее использованный)" };

/* ---- Комбо ---- */
async function loadCombos() {
  const host = $("#combo-list");
  if (DEMO) { renderCombos(host); return; }
  if (!host.innerHTML) host.innerHTML = '<div class="state"><div class="big" aria-hidden="true">↻</div><div class="title">Загрузка…</div></div>';
  const _d = DEMO;
  try { const cb = (await API.ComboList()) || []; if (DEMO !== _d) return; S.combos = cb; renderCombos(host); }
  catch (e) { if (DEMO !== _d) return; host.innerHTML = errHint(e); }
}
function renderCombos(host) {
  const h = host || $("#combo-list");
  try { const cc = $("#combo-count"); if (cc) cc.textContent = "пресетов: " + ((S.combos || []).length); } catch (e) {}
  if (!S.combos || !S.combos.length) { $("#combo-empty").style.display = "flex"; h.innerHTML = ""; return; }
  $("#combo-empty").style.display = "none";
  h.innerHTML = S.combos.map(c => {
    const steps = c.steps || [];
    const swap = (steps[0] && steps[0].params && steps[0].params.swap) || (steps[0] && steps[0].swap) || "";
    const chain = steps.length
      ? steps.map((s, i) =>
          '<span class="chain-step" role="listitem" aria-label="Шаг ' + (i + 1) + ' из ' + steps.length + ': ' + esc(s.provider || "—") + (s.model ? ", " + s.model : "") + '"><span class="cs-num">' + (i + 1) + '</span><span class="cs-txt"><span class="cs-prov" title="' + esc(s.provider || "—") + '">' + esc(s.provider || "—") + '</span>' +
          (s.model ? '<span class="cs-model" title="' + esc(s.model) + '">' + esc(s.model) + '</span>' : '<span class="model-pill wc" title="пустая модель — выбор на стороне роутера">любая модель</span>') + '</span></span>' +
          (i < steps.length - 1 ? '<span class="chain-arr" aria-hidden="true" title="при сбое шага запрос уйдёт дальше">→</span>' : '')).join("")
      : '<span class="model-pill wc">нет шагов</span>';
    const swapLbl = SWAP_LBL[swap] || swap || "как есть";
    return '<div class="card section combo-card" data-cbxopen="' + esc(c.id) + '" role="button" tabindex="0" title="Открыть редактор каскада"><div class="card-head"><h3>' + esc(c.name) + '</h3><span class="chip">' + esc(c.model || "—") + '</span><span class="spacer"></span>' +
      '<button class="btn btn--pill sm" data-cboxtest="' + esc(c.id) + '" title="Живой вызов ping max_tokens=16 реальными ключами, пишется в журнал">▶ Тест</button><button class="btn btn--pill sm danger" data-cbxedit="' + esc(c.id) + '" title="Открыть в редакторе каскада">✎ Редактировать</button><button class="btn btn--pill sm danger" data-cbxdel="' + esc(c.id) + '" title="Удалить пресет (с подтверждением)">Удалить</button></div>' +
      '<div class="combo-chain" role="list" aria-label="Каскад: шаги слева направо, переход при сбое">' + chain + '</div>' +
      '<div class="hint" style="margin-top:10px">шагов: ' + steps.length + ' · при сбое шага N запрос уходит на шаг N+1</div>' +
      (swap ? '<div class="hint" style="margin-top:8px">порядок перебора: <b>' + esc(swapLbl) + '</b> (пока всегда по порядку)</div>' : '') +
      '</div>';
  }).join("");
}
async function comboTest(id, btn) {
  const nid = +id;
  if (!nid) { toast("Пресет не найден — обновите список", "err"); return; }
  const c = (S.combos || []).find(x => +x.id === nid);
  const nm = c ? c.name : ("#" + nid);
  const n = c && c.steps ? c.steps.length : 0;
  if (DEMO) { toast("Комбо «" + nm + "» (демо): живой вызов выключен, показан пример", "ok"); return; }
  if (btn && btn.disabled) return;
  if (!confirm("Живой тест «" + nm + "»: 1 ping (~16 токенов) реальными ключами шагов по порядку. Продолжить?")) return;
  if (btn) { btn.disabled = true; btn.textContent = "▶ Тест…"; }
  const w = modal("Тест комбо", '<div class="state"><div class="big" aria-hidden="true">↻</div><div class="title">Тестируем «' + esc(nm) + '»…</div><div class="hint">живой вызов ping max_tokens=16 реальными ключами · живых шагов: ' + n + ' · таймаут 90с · пишется в журнал</div></div>');
  try {
    const r = await API.ComboTest(nid, [], "");
    w.close();
    const ok = !r.error && (r.status || 200) < 400;
    const stepTxt = r.combo_step ? "шаг-победитель: " + r.combo_step + (String(r.combo_step).indexOf("1:") === 0 ? "" : " (шаг 1 упал, выиграл шаг " + r.combo_step + ")") : "";
    modal("Тест «" + nm + "»", '<div><div class="test-line">' + (ok ? '<span class="badge on">ok</span>' : '<span class="badge err">ошибка</span>') + ' · <span class="chip">' + (r.latency_ms || 0) + ' мс</span></div>' +
      '<div class="test-line"><span class="k">комбо</span><code>' + esc(nm) + '</code></div>' +
      (stepTxt ? '<div class="test-line"><span class="k">failover</span><code>' + esc(stepTxt) + '</code></div>' : '') +
      '<div class="test-line"><span class="k">провайдер</span><code>' + esc(r.provider || "—") + '</code></div>' +
      '<div class="test-line"><span class="k">модель</span><code>' + esc(r.model || "—") + '</code></div>' +
      '<div class="test-line"><span class="k">статус</span><code>' + esc(String(r.status || (ok ? 200 : "—"))) + '</code></div>' +
      '<div class="test-line"><span class="k">журнал</span><code>записано: COMBO /combo/test</code></div>' +
      (ok ? '<div class="test-out">' + esc(r.content || "") + '</div>' : '<div class="test-out err">' + esc((/deadline|timeout/i.test(r.error || "") ? "таймаут 90с: " : "") + (r.error || "")) + '</div>') + '</div>');
    toast(r.error ? "Комбо «" + nm + "»: " + r.error : "Комбо «" + nm + "» ok, " + (r.latency_ms || 0) + " мс", r.error ? "err" : "ok");
  } catch (e) { w.close(); toast("Комбо «" + nm + "»: " + (e && e.message || e), "err"); }
  finally { if (btn) { btn.disabled = false; btn.textContent = "▶ Тест"; } }
}
async function comboDelete(id, btn) {
  const nid = +id;
  if (!nid) { toast("Нет id комбо — обновите список", "err"); return; }
  const c = (S.combos || []).find(x => +x.id === nid);
  const nm = c ? c.name : ("#" + nid);
  const steps = (c && c.steps) || [];
  const provs = steps.map(s => esc(s.provider || "—")).join(", ");
  if (DEMO) { S.combos = (S.combos || []).filter(x => +x.id !== nid); try { renderCombos(); } catch (e) {} toast("Демо: комбо «" + nm + "» убрано (не сохранено)", "ok"); return; }
  if (btn && btn.disabled) return;
  if (btn) btn.disabled = true;
  const snap = c ? JSON.parse(JSON.stringify(c)) : null;
  const body = '<div><div class="test-line"><span class="k">комбо</span><code>' + esc(nm) + '</code></div>' +
    '<div class="test-line"><span class="k">шагов</span><code>' + steps.length + (provs ? " · " + provs : "") + '</code></div>' +
    '<div class="hint">Действие необратимо. В журналах останется старое имя, живые запросы не заденет.</div></div>';
  const dlg = modal("Удалить комбо?", body, { ok: "Удалить", onOk: async ({ close }) => {
    try {
      await API.ComboDelete(nid);
      close();
      toastUndo("Комбо «" + nm + "» удалено", snap);
      loadCombos();
    } catch (e) {
      const msg = friendlyDelErr(e);
      toast("Не удалено: " + msg, "err");
    }
    return false;
  }});
  try { const cx = dlg && dlg.el ? dlg.el.querySelector("#md-cancel") : null; if (cx && cx.focus) cx.focus(); } catch (e) {}
  if (btn) btn.disabled = false;
}
function friendlyDelErr(e) {
  const s = String((e && e.message) || e || "");
  if (/не найден/i.test(s)) return "уже удалено — обновите список";
  if (/openStore|SQLITE|database|disk/i.test(s)) return "хранилище недоступно — повторите позже";
  return s || "повторите позже";
}
function toastUndo(msg, snap) {
  const w = $("#toast-wrap"); if (!w) { toast(msg, "ok"); return; }
  const el = document.createElement("div"); el.className = "toast ok"; el.setAttribute("role", "status");
  const sp = document.createElement("span"); sp.textContent = msg; el.appendChild(sp);
  if (snap) {
    const b = document.createElement("button"); b.className = "btn sm"; b.style.cssText = "margin-left:8px;flex:none";
    b.textContent = "Вернуть";
    b.addEventListener("click", async () => {
      try { await API.ComboSave(snap.name, snap.model || "", snap.steps || []); toast("Комбо «" + snap.name + "» восстановлено", "ok"); loadCombos(); }
      catch (e) { toast("Не восстановлено: имя занято или шаги устарели", "err"); }
      try { el.remove(); } catch (e2) {}
    });
    el.appendChild(b);
  }
  w.appendChild(el);
  setTimeout(() => { el.classList.add("out"); setTimeout(() => { try { el.remove(); } catch (e) {} }, 200); }, 8000);
}

/* ---- Навигация: работает ВСЕГДА, без зависимости от данных ---- */
function show(tab) {
  if (S.tab === "settings" && tab !== "settings") { try { if (typeof cfgDirty !== "undefined" && cfgDirty && !DEMO && Go()) saveCfg({ auto: true }); } catch (eAS) {} }
  S.tab = tab; try { document.querySelector(".content").classList.remove("prov-open"); } catch (eU) {}
  // topbar удалён из DOM (2026-09-28): .hd-шапка верхняя везде, прятать нечего.
  $$(".tab-panel").forEach(p => p.classList.remove("active"));
  const pan = $("#panel-" + tab); if (pan) pan.classList.add("active");
  $$("#tabs .tab").forEach(b => { const on = b.dataset.tab === tab; b.classList.toggle("active", on); if (on) b.setAttribute("aria-current", "page"); else b.removeAttribute("aria-current"); });
  try { if (typeof setFoot === "function") setFoot(); } catch (eF) {}
  try { ({ providers: loadProviders, usage: loadUsage, logs: loadLogs, proxies: loadProxies, settings: loadSettings, combos: loadCombos }[tab] || loadHome)(); } catch (eL) { try { console.error("tab-load:", tab, eL); } catch (e3) {} }
  try { if (typeof renderUniHeaders === "function") renderUniHeaders(); } catch (eH) {}
}

/* ==================================================================
   WIRING / BOOT
   ================================================================== */
function wire() {
  try { let qd = ""; try { qd = new URLSearchParams(location.search).get("demo") || ""; } catch (e) {} if (!Go() && qd !== "1") { toast("Биндинги Wails недоступны (window.go отсутствует). Включите «Демо-данные» или запустите через wails.", "err"); } } catch (e) { }

  $$("#tabs .tab").forEach(b => b.addEventListener("click", () => show(b.dataset.tab)));
  // Делегированный open провайдера: переживает innerHTML-перерисовки сетки, 2 слушателя навсегда.
  const pg = $("#prov-grid");
  if (pg && !pg._deleg) {
    pg._deleg = true;
    pg.addEventListener("click", e => {
      const tc = e.target && e.target.closest ? e.target.closest(".prov-card[data-p],.prov-card[data-name],.prov-cat-item[data-p]") : null;
      if (tc) openProv(tc.getAttribute("data-p") || tc.getAttribute("data-name"));
    });
    pg.addEventListener("keydown", e => {
      if (e.key !== "Enter" && e.key !== " ") return;
      const tc = e.target && e.target.closest ? e.target.closest(".prov-card[data-p],.prov-card[data-name]") : null;
      if (tc) { e.preventDefault(); openProv(tc.getAttribute("data-p") || tc.getAttribute("data-name")); }
    });
  }

  const rb = $("#refresh-btn"); if (rb) rb.addEventListener("click", () => { const fn = { providers: loadProviders, usage: loadUsage, logs: loadLogs, proxies: loadProxies, settings: loadSettings, combos: loadCombos }[S.tab] || loadHome; spinRefresh(true); try { Promise.resolve(fn()).catch(() => {}).finally(() => spinRefresh(false)); } catch (e) { spinRefresh(false); } });

  const top = $("#auto-toggle");
  if (top) top.addEventListener("click", () => { S.auto = !S.auto; const d = $("#auto-dot"); if (d) d.className = "dot " + (S.auto ? "ok pulse" : ""); top.classList.toggle("on", S.auto); try { top.setAttribute("aria-pressed", String(S.auto)); } catch (e) {} const al = $("#auto-lbl"); if (al) al.textContent = S.auto ? "Авто-обновление" : "Пауза"; });

  const dm = $("#demo-toggle");
  if (dm) dm.addEventListener("click", () => { DEMO ? disableDemo() : enableDemo(); });

  // theme-toggle wired once below (THEMES cycle) — dup const tt killed boot (SyntaxError, blank shots).

  $("#combo-new").addEventListener("click", () => comboNew());
  const ce = $("#combo-empty-new"); if (ce) ce.addEventListener("click", () => comboNew());
  const inp = $("#prov-search"); const provSearchDeb = debounce(() => { S.pSearch = inp.value; renderProvGrid(); }, 200); inp.addEventListener("input", () => { try { document.getElementById("prov-x").hidden = !inp.value; } catch (e) {} provSearchDeb(); });
  try { const _cont = document.querySelector(".content"); if (_cont && !_cont._provScrollWired) { _cont._provScrollWired = 1; _cont.addEventListener("scroll", () => { try { if (S.tab === "providers") provScrollKick(); } catch (eS) {} }, { passive: true }); } } catch (eW) {}
  inp.addEventListener("keydown", e => { if (e.key === "Escape" && inp.value) { inp.value = ""; S.pSearch = ""; renderProvGrid(); inp.focus(); } });
  const px = $("#prov-x"); if (px) px.addEventListener("click", () => { inp.value = ""; S.pSearch = ""; renderProvGrid(); inp.focus(); });
  const pa = $("#prov-add"); if (pa) pa.addEventListener("click", addProviderForm);
  $("#prov-sort").addEventListener("change", e => { S.pSort = e.target.value; renderProvGrid(); });
  $$("#prov-filters .btn").forEach(b => b.addEventListener("click", () => { $$("#prov-filters .btn").forEach(x => { setCls(x, false, "active"); x.setAttribute("aria-pressed", "false"); }); setCls(b, true, "active"); b.setAttribute("aria-pressed", "true"); S.pFilter = b.dataset.f; renderProvGrid(); }));
  $("#prov-back").addEventListener("click", () => backProv());

  const segs = [["#usage-seg button", "usageMode"]];
  const segSync = (sel) => { try { $$(sel).forEach(x => x.setAttribute("aria-pressed", String(x.classList.contains("on")))); } catch (e) {} };
  segs.forEach(([sel, key]) => { segSync(sel); $$(sel).forEach(b => b.addEventListener("click", () => {
    $$(sel).forEach(x => setCls(x, false, "on")); setCls(b, true, "on"); segSync(sel);
    S[key] = b.dataset.m;
    renderUsage(); // режимы — один src: переключение без сброса данных
  }))});
  segSync("#usage-seg");
  wireCal();
  const tt = $("#theme-toggle");
  if (tt) tt.addEventListener("click", () => { applyTheme(THEMES[(THEMES.indexOf(S.theme) + 1) % THEMES.length], true); try { syncHomeThemes(); } catch (eST) {} });
  const DKEY = "omnigate.density";
  try {
    if (localStorage.getItem(DKEY) === "a") document.body.setAttribute("data-density", "a");
  } catch (e) {}
  const dt2 = $("#density-toggle");
  if (dt2) dt2.addEventListener("click", () => {
    const on = document.body.getAttribute("data-density") === "a";
    if (on) document.body.removeAttribute("data-density");
    else document.body.setAttribute("data-density", "a");
    try { localStorage.setItem(DKEY, on ? "" : "a"); } catch (e) {}
  });
  if ($("#usage-view-seg")) { $$("#usage-view-seg button").forEach(x => x.setAttribute("aria-pressed", String(x.classList.contains("on")))); $$("#usage-view-seg button").forEach(b => b.addEventListener("click", () => {
    $$("#usage-view-seg button").forEach(x => setCls(x, false, "on"));
    setCls(b, true, "on");
    $$("#usage-view-seg button").forEach(x => x.setAttribute("aria-pressed", String(x === b)));
    S.usageView = b.dataset.v;
    const cal = $("#usage-cal"), box = $("#usage-chart-box"), leg = $("#usage-legend"), seg = $("#usage-seg");
    const isCal = S.usageView === "cal";
    if (cal) cal.style.display = isCal ? "" : "none";
    if (box) box.style.display = isCal ? "none" : "";
    if (leg) leg.style.display = isCal ? "none" : "";
    if (seg) seg.style.display = isCal ? "none" : "";
    if (isCal) { $("#usage-kpis").innerHTML = ""; S.calOff = 0; S.calSel = null; loadUsage(); }
    else loadUsage(); // календарь перезаписал S.daily (31д) — возврат в тренд только через refetch
  })); }

  const lsearch = $("#log-search");
  const syncLogSearchBox = () => { try { if (lsearch && document.activeElement !== lsearch) lsearch.value = S.logQuery || ""; } catch (e) {} };
  const syncLogKindSeg = () => { try { $$("#log-kind-seg button").forEach(x => { setCls(x, x.dataset.k === S.logKind, "on"); x.setAttribute("aria-pressed", String(x.dataset.k === S.logKind)); }); } catch (e) {} }; syncLogKindSeg();
  lsearch.addEventListener("input", debounce(() => { S.logQuery = lsearch.value; syncLogSearchBox(); loadLogs({ force: true }); }, 300));
  lsearch.addEventListener("keydown", e => { if (e.key === "Escape" && lsearch.value) { lsearch.value = ""; S.logQuery = ""; loadLogs({ force: true }); } });
  $$("#log-kind-seg button").forEach(b => b.addEventListener("click", () => {
    S.logKind = b.dataset.k;
    $$("#log-kind-seg button").forEach(x => setCls(x, false, "on"));
    setCls(b, true, "on");
    $$("#log-kind-seg button").forEach(x => x.setAttribute("aria-pressed", String(x === b)));
    // поиск живёт на обеих ветках: request — серверный (LogsByQuery), proxies — клиентский по окну 200 (renderPlogs); query при смене ветки не сбрасываем
    const lk = $("#log-search"); if (lk) { lk.disabled = false; lk.classList.remove("muted"); lk.placeholder = S.logKind === "proxies" ? "прокси, действие, ошибка…" : "модель, провайдер, rid…"; lk.title = S.logKind === "proxies" ? "Фильтр по загруженным событиям egress (окно 200): прокси, действие, ошибка" : "Поиск по модели, провайдеру, rid; ошибка, статус и HTTP-код — только от 3 символов"; }
    // смена источника журнала — новая выборка: сброс фильтра уровня на все чипы
    S.logLv = { info: true, warn: true, error: true };
    $$("#log-lvchips .chip").forEach(x => x.classList.add("on"));
    loadLogs({ force: true });
  }));
  // чипы уровня: мгновенная клиентская фильтрация уже загруженных записей (обе ветки: request и proxies)
  $$("#log-lvchips .chip").forEach(b => b.addEventListener("click", () => {
    const lv = b.getAttribute("data-lv");
    S.logLv[lv] = !S.logLv[lv];
    setCls(b, S.logLv[lv], "on");
    const lt = $("#log-table");
    // на proxy-ветке warn-записей нет: выключение warn — no-op, остальные чипы фильтруют F→L
    if (S.logKind === "proxies") { if (lt) renderPlogs(lt); return; }
    if (lt && lt.querySelector("table")) renderLogs(lt); else loadLogs({ force: true });
  }));
  $("#log-export").addEventListener("click", e => exportLogs(e.target.closest("button") || $("#log-export")));
  $("#log-clear").addEventListener("click", e => clearLogs(e.target.closest("button") || $("#log-clear")));

  document.addEventListener("click", e => {
    if (e.target.closest("#home-tops-all")) { try { show("usage"); } catch (err) {} return; }
    const tpm = e.target.closest("#home-tops .toprow[data-model]"); if (tpm) { const m = (tpm.getAttribute("data-model") || "").trim(); if (m && m !== "—") { try { gotoLogsQuery(m); } catch (err) {} } return; }
    const tpp = e.target.closest("#home-tops .toprow[data-prov]"); if (tpp) { const p = tpp.getAttribute("data-prov"); if (p) { try { S._homeProv = p; show("providers"); if (typeof openProv === "function") openProv(p); } catch (err) {} } return; }
    if (e.target.closest("#pd-test")) { testProvider(); return; }
    const c = e.target.closest("[data-copy]"); if (c) { copyTo(c.getAttribute("data-copy")); return; }
    if (e.target.closest("[data-retry]")) { if (typeof refreshCurrent === "function") refreshCurrent(); return; }
    if (e.target.closest("[data-demo]")) { enableDemo(); return; }
    const ce = e.target.closest("[data-cbxedit]"); if (ce) { if (window.ComboCanvas && typeof window.ComboCanvas.open === "function") window.ComboCanvas.open(ce.getAttribute("data-cbxedit")); return; }
    const co = e.target.closest("[data-cbxopen]"); if (co && !e.target.closest("button") && !e.target.closest("a") && !e.target.closest("input") && !e.target.closest("select")) { if (window.ComboCanvas && typeof window.ComboCanvas.open === "function") window.ComboCanvas.open(co.getAttribute("data-cbxopen")); return; }
    const ct = e.target.closest("[data-cboxtest]"); if (ct) { comboTest(ct.getAttribute("data-cboxtest"), ct); return; }
    const cd = e.target.closest("[data-cbxdel]"); if (cd) { comboDelete(cd.getAttribute("data-cbxdel"), cd); return; }
    if (e.target.closest("[data-goto-logs]")) { try { const gq = e.target.closest("[data-goto-logs]").getAttribute("data-q") || ""; if (gq.trim()) gotoLogsQuery(gq); else { S.logKind = S.logQuery ? "request" : S.logKind; syncLogKindSeg(); show("logs"); } } catch (err) {} return; }
    const pgo = e.target.closest("#provDetail [data-goto]"); if (pgo) { try { show(pgo.getAttribute("data-goto")); } catch (err) {} return; }
  });
  document.addEventListener("keydown", e => {
    if (e.key !== "Enter" && e.key !== " ") return;
    const card = e.target && e.target.closest ? e.target.closest(".combo-card[data-cbxopen]") : null;
    if (card && !e.target.closest("button") && !e.target.closest("a") && !e.target.closest("input") && !e.target.closest("select")) { e.preventDefault(); if (window.ComboCanvas && typeof window.ComboCanvas.open === "function") window.ComboCanvas.open(card.getAttribute("data-cbxopen")); return; }
    const t = e.target && e.target.closest ? e.target.closest("#home-tops .toprow") : null;
    if (t) { e.preventDefault(); t.click(); }
  });
}
function comboNew() {
  // Новый редактор — ComboCanvas (canvas-граф). Легаси-модалка шагов удалена.
  if (window.ComboCanvas && typeof window.ComboCanvas.openNew === "function") { window.ComboCanvas.openNew(); return; }
  toast("Редактор комбо не загрузился", "err");
}

function copyTo(t) { const s = t || ""; const done = ok => toast(ok ? "Скопировано" : "Не скопировано", ok ? "ok" : "err"); try { if (navigator.clipboard && navigator.clipboard.writeText) { navigator.clipboard.writeText(s).then(() => done(true)).catch(() => legacyCopy(s) ? done(true) : done(false)); return; } } catch (e) {} try { done(legacyCopy(s)); } catch (e2) { toast("Копирование недоступно", "err"); } }
function legacyCopy(s) { try { const ta = document.createElement("textarea"); ta.value = s; ta.setAttribute("readonly", ""); ta.style.cssText = "position:fixed;top:0;left:0;opacity:0"; document.body.appendChild(ta); ta.select(); let ok = false; try { ok = document.execCommand("copy"); } catch (e) {} ta.remove(); return !!ok; } catch (e) { return false; } }

function boot() {
  try { wire(); } catch (e) { console.error("wire:", e); }
  let qTab = "", qDemo = false, qTheme = "";
  try {
    const q = new URLSearchParams(location.search);
    qTheme = q.get("theme") || ""; qTab = q.get("tab") || ""; qDemo = q.get("demo") === "1";
  } catch (e) {}
  applyTheme(qTheme || "mono", false);          // тема одна — «БД»; ?theme= принимается, но приводится к ней
  const validTabs = ["home", "providers", "combos", "usage", "logs", "proxies", "settings"];
  if (qDemo && !DEMO) { try { // стенд: демо-данные ДО первой загрузки — иначе stale-запрос затирает рендер ошибкой
    DEMO = true;
    const dd = buildDemo();
    S.status = dd.status; S.end = dd.end; S.breakers = dd.breakers; S.locks = dd.locks; S.keys = dd.keys;
    S.kbp = dd.kbp; S.usage = dd.usage; S.daily = dd.daily; S.health = dd.health;
    S.logs = dd.logs; S.plogs = dd.plogs; S.proxy = dd.proxy; S.combos = dd.combos; S.cfg = dd.cfg; S.analytics = dd.analytics; S.tops90 = dd.analytics;
    buildSparkMap();
    const db = $("#demo-toggle"); if (db) { setCls(db, true, "active"); try { db.setAttribute("aria-pressed", "true"); } catch (e) {} }
  } catch (e) {} }
  show(validTabs.includes(qTab) ? qTab : "home"); // навигация первой; грузит ТОЛЬКО активную вкладку
  try { const qq = new URLSearchParams(location.search); const qp = qq.get("prov"); const qt = qq.get("pdtab"); if (qt) S._pdTab = qt; if (qp && typeof openProv === "function") { const nm = decodeURIComponent(qp); if ((S.end || []).some(e => e.name === nm)) openProv(nm); } if (qq.get("openmdl")) { try { const r = document.querySelector('#provDetail .mdl[data-mrow]'); if (r) r.classList.add("open"); } catch (e) {} } if (qq.get("openkey")) { try { const r = document.querySelector('#provDetail .kv-row .kdrawer'); if (r) r.classList.add("open"); } catch (e) {} } if (qq.get("newcombo") && typeof comboNew === "function") { try { setTimeout(() => comboNew(), 400); } catch (eC) {} } } catch (e) {}
  // Лёгкие вкладки догружаем в idle, чтобы переключение было мгновенным.
  // Тяжёлые (провайдеры/usage/логи) — строго по открытию, иначе старт висит и данные прыгают.
  try {
    const idle = fn => { if ("requestIdleCallback" in window) requestIdleCallback(fn, { timeout: 9000 }); else setTimeout(fn, 5000); };
    idle(() => { if (!DEMO && Go() && !document.hidden) { try { loadCombos(); } catch (e) {} } });
    setTimeout(() => { if (!DEMO && Go() && !document.hidden && !S.cfg) { try { loadSettings(); } catch (e) {} } }, 10000);
  } catch (e) {}
  window.addEventListener("resize", debounce(() => {
    try {
      if (document.hidden) return;  // скрытое окно — канвасы нулевые, не перерисовываем
      if (S.tab === "providers") { drawSparks(); drawBars(); }
      else if (S.tab === "home") { homeChart(); try { homeHeat(); } catch (eHcal) {} }
      else if (S.tab === "usage") drawUsageChart();
      redrawDonut();
    } catch (e) { }
  }, 180));
  (function watchDpr() {
    if (!window.matchMedia) return;
    const mq = window.matchMedia("(resolution: " + (window.devicePixelRatio || 1) + "dppx)");
    mq.addEventListener("change", () => setTimeout(() => {
      try {
        if (document.hidden) return;
        if (S.tab === "providers") { drawSparks(); drawBars(); }
        else if (S.tab === "home") { homeChart(); try { homeHeat(); } catch (eHcal) {} }
        else if (S.tab === "usage") drawUsageChart();
        redrawDonut();
      } catch (e) { }
    }, 120));
  })();
  setInterval(autoRefreshTick, 25000);
}
function autoRefreshTick() {
  if (DEMO || !S.auto || !Go()) return;
  if (document.hidden) return;                        // окно скрыто — данные не дёргаем
  if (S._refreshing) return;                          // предыдущий тик ещё летит — не наслаиваем
  if (document.querySelector("#modal")) return;       // открыта модалка — не перерисовываем под руками
  if (document.querySelector(".dd.open")) return;     // открыт dropdown — не сбрасываем выбор
  if (S.tab === "logs" && logsUserReading()) { try { setLogsPaused(true); } catch (eP) {} return; }  // читает историю — позицию не сбрасываем
  if (S._pxBusy) return;                               // идёт connect/rotate — кнопки не сносим
  try { if (window.contentScrollBusy && S.tab === "providers") return; } catch (eSB) {}  // providers скроллит — тик пропускаем, рендер не срывает позицию
  // автотик не трогает: фокус в инпуте (печатает), грязный конфиг, чтение истории на proxies-табе
  try {
    const ae = document.activeElement;
    if (ae && (ae.tagName === "INPUT" || ae.tagName === "TEXTAREA" || ae.tagName === "SELECT") && ae.closest("#proxy-panel,#settings-editor,#provDetail")) return;
  } catch (eF) {}
  if (typeof cfgDirty !== "undefined" && cfgDirty) return;
  const fn = { home: loadHome, providers: loadProviders, usage: loadUsage, logs: loadLogs, proxies: loadProxies }[S.tab];
  if (!fn) return;
  S._refreshing = true;
  try { Promise.resolve(fn()).catch(() => {}).finally(() => { S._refreshing = false; }); }
  catch (e) { S._refreshing = false; }
}
if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", boot);
else boot();

/* ============ Кастомный выдвижной dropdown (P1): заменяет <select class="input"> ============ */
// Скрытый select остаётся в DOM (value + change-события синхронизируются), так что
// вся существующая логика (.value, addEventListener("change")) продолжает работать.
function enhanceSelects(root) {
  const r = root || document;
  r.querySelectorAll("select.input:not([data-dd])").forEach(sel => {
    if (sel.closest(".dd")) return;
    if (+sel.size > 1) return; // листбокс (выбор модели в тесте) — не dropdown
    sel.dataset.dd = "1";
    const wrap = document.createElement("div");
    wrap.className = "dd down";
    if (sel.disabled) wrap.setAttribute("disabled", "");
    // Копируем inline-стили ширины (например min-width:150px в строках шагов).
    ["minWidth", "maxWidth", "width", "flex"].forEach(p => { if (sel.style[p]) wrap.style[p] = sel.style[p]; });
    const btn = document.createElement("button");
    btn.type = "button"; btn.className = "dd-btn"; btn.setAttribute("aria-haspopup", "listbox"); btn.setAttribute("aria-expanded", "false");
    const val = document.createElement("span"); val.className = "dd-val";
    const ic = document.createElement("span"); ic.className = "dd-ic"; ic.textContent = "▾";
    btn.appendChild(val); btn.appendChild(ic);
    const list = document.createElement("div"); list.className = "dd-list";
    const renderItems = () => {
      list.innerHTML = "";
      [...sel.options].forEach((o, i) => {
        if (o.hidden) return; // поиск (cb-step-q и pt-model-q) фильтрует нативный select — видимый список следует
        const it = document.createElement("button");
        it.type = "button"; it.className = "dd-item" + (i === sel.selectedIndex ? " sel" : "");
        if (o.disabled) { it.disabled = true; it.classList.add("dis"); }
        if (i === sel.selectedIndex && !o.disabled) { const sr = document.createElement("span"); sr.className = "sr-only"; sr.textContent = " (выбрано)"; it.appendChild(document.createTextNode(o.textContent)); it.appendChild(sr); it.title = o.textContent + " (выбрано)"; it.dataset.base = o.textContent || ""; } else { it.textContent = o.textContent; it.title = o.textContent || ""; it.dataset.base = o.textContent || ""; }
        it.addEventListener("click", () => {
          if (o.disabled) return;
          sel.selectedIndex = i;
          syncVal();
          list.querySelectorAll(".dd-item").forEach(x => { x.classList.remove("sel"); const xs = x.querySelector(".sr-only"); if (xs) xs.remove(); if (x.dataset.base !== undefined) x.title = x.dataset.base; });
          it.classList.add("sel"); const srs = document.createElement("span"); srs.className = "sr-only"; srs.textContent = " (выбрано)"; it.appendChild(srs); it.title = (it.dataset.base || o.textContent || "") + " (выбрано)";
          sel.dispatchEvent(new Event("change", { bubbles: true }));
          closeDD();
        });
        list.appendChild(it);
      });
      if (!list.children.length) { const em = document.createElement("div"); em.className = "dd-item dis"; em.textContent = "нет вариантов"; em.title = "нет вариантов"; list.appendChild(em); }
      if (wrap.classList.contains("open")) { try { const selEl = list.querySelector(".dd-item.sel"); if (selEl) { const lr = list.getBoundingClientRect(), er = selEl.getBoundingClientRect(); if (er.top < lr.top || er.bottom > lr.bottom) list.scrollTop += (er.top - lr.top) - Math.max(0, (lr.height - er.height) / 2); } } catch (eSI) {} }
    };
    const syncVal = () => { val.textContent = sel.options[sel.selectedIndex] ? sel.options[sel.selectedIndex].textContent : ""; btn.title = val.textContent || ""; };
    const setOpen = (open) => {
      if (sel.disabled) return;
      if (open) {
        renderItems();
        const visN = list.querySelectorAll(".dd-item:not(.dis)").length;
        if (!visN) { try { toast(sel.getAttribute("aria-label") ? sel.getAttribute("aria-label") + ": нет доступных вариантов" : "Нет доступных вариантов — измените поиск или провайдера", "warn"); } catch (eT) {} return; }
      }
      wrap.classList.toggle("open", open); try { btn.setAttribute("aria-expanded", String(open)); } catch (eAE) {}
      if (open) {
        const rc = wrap.getBoundingClientRect(), space = window.innerHeight - rc.bottom;
        const up = space < 260 && rc.top > 260;
        wrap.classList.toggle("up", up);
        wrap.classList.toggle("down", !up);
        // V13 ОКНО: любой открытый список — fixed к вьюпорту (не absolute в родителе):
        // переживает overflow:auto/hidden предков (.keys-scroll, таблицы, карточки),
        // ширина по контенту (до 560), прижим к краям вьюпорта с запасом 16px.
        list.style.maxHeight = "0px";
        void list.offsetHeight;
        list.style.maxHeight = "min(42vh," + Math.max(120, (up ? rc.top : space) - 8) + "px)";
        {
          const inModal2 = !!sel.closest("#modal");
          const wCap = inModal2 ? 420 : 560;
          const w = Math.min(wCap, Math.max(rc.width, 280));
          const left = Math.min(Math.max(16, rc.left), window.innerWidth - w - 16);
          list.style.left = Math.max(16, left) + "px";
          list.style.width = w + "px";
          list.style.position = "fixed";
          list.style.zIndex = "200";
          list.style.minWidth = "0px";
          list.style.right = "auto";
          if (up) { list.style.top = "auto"; list.style.bottom = (window.innerHeight - rc.top + 1) + "px"; }
          else { list.style.bottom = "auto"; list.style.top = (rc.bottom + 1) + "px"; }
        }
      } else {
        list.style.maxHeight = "0px";
        list.style.position = ""; list.style.zIndex = ""; list.style.left = ""; list.style.width = ""; list.style.top = ""; list.style.bottom = ""; list.style.minWidth = ""; list.style.right = "";
      }
    };
    const closeDD = () => setOpen(false);
    btn.addEventListener("click", (e) => { e.stopPropagation(); setOpen(!wrap.classList.contains("open")); });
    btn.addEventListener("keydown", (e) => { if (e.key === "ArrowDown" || e.key === "Enter" || e.key === " ") { e.preventDefault(); if (!wrap.classList.contains("open")) setOpen(true); const f = list.querySelector(".dd-item.sel") || list.querySelector(".dd-item"); if (f && f.focus) f.focus(); } else if (e.key === "Escape") { closeDD(); } });
    list.addEventListener("keydown", (e) => { if (e.key === "Escape") { e.stopPropagation(); closeDD(); btn.focus(); } });
    // Синхронизация после программной смены options (каскад моделей в комбо и т.п.).
    sel.addEventListener("change", () => { syncVal(); if (wrap.classList.contains("open")) renderItems(); });
    (window._ddWraps = window._ddWraps || []).push(wrap);
    syncVal();
    wrap.appendChild(btn); wrap.appendChild(list);
    sel.style.display = "none";
    sel.parentNode.insertBefore(wrap, sel);
  });
}
// Автоперехват всех будущих select.input (после рендеров/модалок) — с debounce,
// чтобы не сканировать документ на каждой мутации.
let ddT = null;
const ddObs = new MutationObserver(muts => {
  clearTimeout(ddT);
  ddT = setTimeout(() => {
    const roots = [];
    muts.forEach(m => m.addedNodes.forEach(n => { if (n.nodeType === 1) roots.push(n); }));
    if (!roots.length) return;
    if (roots.some(n => (n.matches && n.matches("select.input")) || (n.querySelector && n.querySelector("select.input:not([data-dd])")))) enhanceSelects(document);
  }, 120);
});
ddObs.observe(document.body, { childList: true, subtree: true });
// Один делегированный закрыватель вместо N document-листенеров (гонка close/open при быстром клике).
if (!window._ddCloser) { window._ddCloser = true; document.addEventListener("click", (e) => { try { (window._ddWraps || []).forEach(w => { if (!w.isConnected) return; if (!w.contains(e.target)) { w.classList.remove("open"); const l = w.querySelector(":scope > .dd-list"); if (l) { l.style.maxHeight = "0px"; l.style.position = ""; l.style.zIndex = ""; l.style.left = ""; l.style.width = ""; l.style.top = ""; l.style.bottom = ""; l.style.minWidth = ""; l.style.right = ""; } } }); } catch (eC) {} }); }
enhanceSelects(document);

