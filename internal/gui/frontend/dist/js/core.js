
/* OmniGate — панель ядра. Static JS (без npm). Данные только через
   window.go.gui.App.* (Wails). Provider-центрная ИА: ключи и ендпоинты
   живут в drill-down провайдера; Настройки — только глобальная химия. */

"use strict";

function ga(){ return (window.go && window.go.gui && window.go.gui.App) || null; }
/* rt — запускает биндинг и возвращает promise: и синхронный TypeError
   («биндинг недоступен / недогенерен»), и отклонённый промис ловим в .catch. */
const rt = fn => { try { return Promise.resolve(fn()); } catch (e) { return Promise.reject(e); } };
const API = {
  Status: ()=>rt(()=>ga().Status()),
  Endpoints: ()=>rt(()=>ga().Endpoints()),
  Breakers: ()=>rt(()=>ga().Breakers()),
  Lockouts: ()=>rt(()=>ga().Lockouts()),
  Keys: ()=>rt(()=>ga().Keys()),
  KeysByProvider: ()=>rt(()=>ga().KeysByProvider()),
  Usage: ()=>rt(()=>ga().Usage()),
  DailyUsage: d=>rt(()=>ga().DailyUsage(d)),
  UsageAnalytics: d=>rt(()=>ga().UsageAnalytics(d)),
  UsageAnalyticsLight: d=>rt(()=>ga().UsageAnalyticsLight(d)),
  Risks: ()=>rt(()=>ga().Risks()),
  HealthCheck: ()=>rt(()=>ga().HealthCheck()),
  Test: m=>rt(()=>ga().Test(m)),
  TestProvider: (p,m)=>rt(()=>ga().TestProvider(p,m)),
  TestKey: k=>rt(()=>ga().TestKey(k)),
  SetKeyQuota: (n,a,b,c)=>rt(()=>ga().SetKeyQuota(n,a,b,c)),
  RevokeKey: n=>rt(()=>ga().RevokeKey(n)),
  Logs: l=>rt(()=>ga().Logs(l)),
  LogsByQuery: (q,l)=>rt(()=>ga().LogsByQuery(q,l)),
  LogByID: id=>rt(()=>ga().LogByID(id)),
  ProxiesLogs: l=>rt(()=>ga().ProxiesLogs(l)),
  ExportLogs: (k,f)=>rt(()=>ga().ExportLogs(k,f)),
  ClearLogs: k=>rt(()=>ga().ClearLogs(k)),
  GetConfig: ()=>rt(()=>ga().GetConfig()),
  SaveConfig: c=>rt(()=>ga().SaveConfig(c)),
  ReloadConfig: ()=>rt(()=>ga().ReloadConfig()),
  ComboList: ()=>rt(()=>ga().ComboList()),
  ComboSave: (n,m,s)=>rt(()=>ga().ComboSave(n,m,s)),
  ComboDelete: id=>rt(()=>ga().ComboDelete(id)),
  ComboTest: (...a)=>rt(()=>ga().ComboTest(...a)),
  ProxyStatus: ()=>rt(()=>ga().ProxyStatus()),
  ProxyConnect: s=>rt(()=>ga().ProxyConnect(s)),
  ProxyDisconnect: ()=>rt(()=>ga().ProxyDisconnect()),
  ProxyRotate: s=>rt(()=>ga().ProxyRotate(s)),
  AddProvider: p=>rt(()=>ga().AddProvider(p)),
  AddProviderKeys: (p,v)=>rt(()=>ga().AddProviderKeys(p,v)),
  SyncModels: p=>rt(()=>ga().SyncModels(p)),
  SyncModelsPruned: (p,pr)=>rt(()=>ga().SyncModelsPruned(p,pr)),
  UpdateProvider: (n,p)=>rt(()=>ga().UpdateProvider(n,p)),
  ResetProviderDefaults: n=>rt(()=>ga().ResetProviderDefaults(n)),
  SetKeyDefault: (p,h)=>rt(()=>ga().SetKeyDefault(p,h)),
  SetKeyProxy: (p,h,px)=>rt(()=>ga().SetKeyProxy(p,h,px)),
  PoolKeys: p=>rt(()=>ga().PoolKeys(p)),
  SetKeyEnabled: (p,h,e)=>rt(()=>ga().SetKeyEnabled(p,h,e)),
  Modules: ()=>rt(()=>ga().Modules()),
  ModuleStart: n=>rt(()=>ga().ModuleStart(n)),
  ModuleStop: n=>rt(()=>ga().ModuleStop(n)),
  ModuleAuto: (n,a)=>rt(()=>ga().ModuleAuto(n,a)),
};

const R = { reduced: window.matchMedia("(prefers-reduced-motion: reduce)").matches };
// Переменные темы лежат на :root (база) и уточняются на body[data-theme=mono] — читаем computed-стиль
// body и обновляемся при смене темы (тема одна: «БД»).
let cs = getComputedStyle(document.body);
const CV = n => (cs.getPropertyValue(n) || "").trim();
let E = null;
function refreshVars() {
  cs = getComputedStyle(document.body);
  E = {
    border: CV("--border") || "#232c3b",
    border2: CV("--border-2") || "#2d3849",
    panel: CV("--panel") || "#131315",
    accent: CV("--accent") || "#e5484d",
    accent2: CV("--accent-2") || "#f76b6f",
    violet: CV("--violet") || "#8e8e96",
    ok: CV("--ok") || "#e8e8e8",
    warn: CV("--warn") || "#a8a8ad",
    danger: CV("--danger") || "#e5484d",
    info: CV("--info") || "#c9c9cf",
    text: CV("--text") || "#f5f5f5",
    muted: CV("--muted") || "#9a9aa0",
    faint: CV("--faint") || "#6e6e76",
    mono: CV("--mono") || "Consolas, monospace",
  };
  try { _heatCache = null; } catch (eHC) { try { window._heatCache = null; } catch (e2) {} } // тема сменилась — пересчитать heat-цвета (var живёт в core, не в usage)
}
var _heatCache = null; // heat-кэш usage-календаря: var-hoisted, refreshVars() выше безопасен

const $ = (s, r) => (r || document).querySelector(s);
const $$ = (s, r) => Array.from((r || document).querySelectorAll(s));
const esc = s => String(s == null ? "" : s).replace(/[&<>"']/g, c => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
/* provIcon: имя провайдера -> глиф из карты; фавиконка ТОЛЬКО для известных
   бренд-доменов (nvidia и прочий generic-глобус отсекаем — сразу буква).
   Суффиксы -v1/-zen/-free/-compatible и UUID-хвосты чистятся до токена. */
const _provGlyph = { openai: "O", anthropic: "A", google: "G", deepseek: "D", mistral: "M", groq: "Gq", opencode: "OC", zen: "Z", cloudflare: "CF", nvidia: "N", xai: "X", cohere: "C", perplexity: "P", together: "T", fireworks: "F", gemini: "G", meta: "M", llama: "L", qwen: "Q", kimi: "K", minimax: "MM", novita: "N", lmarena: "LM", baseten: "B", airforce: "AF", gitlawb: "GL", freetheai: "FT", freemodel: "FM", gorouter: "GR", elevenlabs: "11", agentrouter: "AR" };
const _provFaviconOK = { openai: "openai.com", anthropic: "anthropic.com", google: "google.com", deepseek: "deepseek.com", mistral: "mistral.ai", groq: "groq.com", xai: "x.ai", cohere: "cohere.com", perplexity: "perplexity.ai", cloudflare: "cloudflare.com", nvidia: "nvidia.com" };
const maskStrict = m => !m ? "****" : (m.length <= 2 ? "•••" : "•••" + m.slice(-2));
const provTok = nm => String(nm || "").toLowerCase().replace(/^[a-f0-9]{8}-[a-f0-9-]{20,}.*$/, "").replace(/[-_](v\d+|zen|free|compatible|chat|cn)$/g, "").replace(/[-_](v\d+|zen|free|compatible|chat|cn)$/g, "").split(/[-_./\s]/).filter(Boolean);
const provIcon = e => {
  const nm = (e && e.name) || "?";
  const toks = provTok(nm);
  for (const t of toks) { if (_provGlyph[t]) return '<span class="glyph">' + _provGlyph[t] + '</span>'; }
  const joined = toks.join("");
  for (const k in _provGlyph) { if (k.length >= 2 && joined.includes(k)) return '<span class="glyph">' + _provGlyph[k] + '</span>'; }
  for (const t of toks) { for (const k in _provGlyph) { if (t.includes(k) && k.length >= 2) return '<span class="glyph">' + _provGlyph[k] + '</span>'; } }
  const w = toks.find(t => /[a-z0-9]/i.test(t || "")) || "?";
  const letter = esc((w[0] || "?").toUpperCase());
  // Фавиконка только для известных бренд-доменов из карты (иначе generic-глобус).
  let host = "";
  try { host = new URL(e.base_url || "").hostname.toLowerCase(); } catch (err) {}
  const brand = toks.map(t => _provFaviconOK[t]).find(Boolean);
  if (!brand || (host && !host.includes(brand.split(".")[0]))) return '<span class="glyph">' + letter + '</span>';
  const src = "https://www.google.com/s2/favicons?domain=" + encodeURIComponent(brand) + "&sz=64";
  return '<img class="pfi" src="' + src + '" alt="" loading="lazy" data-l="' + letter + '" onerror="this.outerHTML=\'<span class=glyph>\'+this.dataset.l+\'</span>\'">';
};
const iN = n => +(n || 0);
const _nfFmt = new Intl.NumberFormat("ru-RU", { maximumFractionDigits: 0 });
const nf = n => _nfFmt.format(iN(n));
const _collRu = new Intl.Collator("ru");
 const compact = n => { n = iN(n); const f = (v, s) => { const t = (n / v).toFixed(1); return (t.endsWith(".0") ? t.slice(0, -2) : t) + s; }; if (n >= 1e9) return f(1e9, "B"); if (n >= 1e6) return f(1e6, "M"); if (n >= 1e3) return f(1e3, "k"); return "" + n; };
const $c = c => { c = iN(c); const neg = c < 0; c = Math.abs(c); return (neg ? "−" : "") + (c < 10000 ? "$" + (c / 100).toFixed(2) : "$" + compact(Math.round(c / 100))); };
const up = s => { const n = iN(s); return n < 60 ? n + "s" : n < 3600 ? Math.floor(n / 60) + "м" : n < 86400 ? Math.floor(n / 3600) + "ч" : Math.floor(n / 86400) + "д"; };
/* Честный total без двойного счёта кэша: prompt уже включает cached. */
const uncachedTotal = (prompt, completion, cr, cw) => {
  prompt = +prompt || 0; completion = +completion || 0; cr = +cr || 0; cw = +cw || 0;
  return completion + cr + cw + (prompt - Math.min(cr, prompt));
};
const pct = p => (p == null || p < 0 || !isFinite(p)) ? "∞" : (Math.round(p * 100) > 100 ? ">" + Math.round(p * 100) : Math.round(p * 100)) + "%";
const qBar = p => {
  if (p == null) return { label: "—", w: 0, cls: "" };
  if (!isFinite(p) || p < 0) return { label: "∞", w: 0, cls: "" };   // безлимит — пустой трек, не «переполнение»
  const v = Math.round(p * 100);
  return { label: (v > 100 ? ">" + v : v) + "%", w: Math.min(100, Math.max(0, v)), cls: v >= 90 ? "danger" : v >= 70 ? "warn" : "" };
};
 const dt = s => { if (!s) return "—"; const d = new Date(s); if (isNaN(d)) return s; const p = n => String(n).padStart(2, "0"); return p(d.getDate()) + "." + p(d.getMonth() + 1) + " " + p(d.getHours()) + ":" + p(d.getMinutes()); };
 const tm = s => { if (!s) return "—"; const d = new Date(s); if (isNaN(d)) return esc(s); return '<time datetime="' + esc(s) + '">' + esc(dt(s)) + '</time>'; };
const deb = (fn, ms) => { let t; return (...a) => { clearTimeout(t); t = setTimeout(() => fn(...a), ms); }; };
/* ---------------- tiny helpers (were missing → boot crashed with ReferenceError) ---------------- */
const Go = () => ga();                      // доступность Wails-биндингов
const nFmt = nf;                            // number format alias
const moneyC = $c;                          // центы -> $ alias
const debounce = deb;                       // debounced()
function setCls(el, on, cls) { if (el) { if (on) el.classList.add(cls); else el.classList.remove(cls); } }
function toast(msg, kind, ms) { const w = $("#toast-wrap"); if (!w) return; if (w.children.length >= 4) { try { w.firstChild.remove(); } catch (e) {} } const el = document.createElement("div"); el.className = "toast " + (kind || ""); el.setAttribute("role", "status"); el.textContent = msg; el.addEventListener("click", () => { try { el.remove(); } catch (e) {} }); w.appendChild(el); setTimeout(() => { el.classList.add("out"); setTimeout(() => { try { el.remove(); } catch (e) {} }, 200); }, ms || 3700); }
function rawA(hex, a) { const n = parseInt(String(hex).replace("#", ""), 16); if (isNaN(n)) return "rgba(128,128,128," + a + ")"; return "rgba(" + ((n >> 16) & 255) + "," + ((n >> 8) & 255) + "," + (n & 255) + "," + a + ")"; }
 /* удалено: tag() — употреблений 0 */

/* ---------------- state ---------------- */
const S = { tab: "home", theme: "mono", status: null, usage: null, daily: null, end: [], breakers: [], locks: [], keys: [], kbp: null, health: [], logs: [], plogs: [], logKind: "request", logQuery: "", logLv: { info: true, warn: true, error: true }, logSort: null, proxy: null, combos: [], auto: true, mode: "tokens", usageMode: "tokens", usageView: "trend", calOff: 0, calSel: null, pSearch: "", pSort: "name", pFilter: "all", prov: null, cfg: null, _homeHash: "", _usageHash: "", _routeKey: "", _chanKey: "", _heatKey: "", _donutRatio: -1, proxyEndpoints: [], pxFeed: [], proxyLastErr: null };

/* ---------------- тема: единственная — «БД» (mono). Прошлые темы (ember/abyss/nebula/gray) удалены
   как темы: их палитры вырезаны из tokens.css, старые записи в localStorage больше не читаются. ---------------- */
const THEMES = ["mono"];
const THEME_LBL = {mono:"БД"};
const THEME_ICO = {mono:"○"};
// Перерисовка всего, что брало цвета до смены темы (SVG-график берёт var() сам; легенда/календарь — перерендер).
function redrawThemed() {
  try {
    try { if (typeof syncHomeThemes === "function") syncHomeThemes(); } catch (eHT) {}
    if (S.tab === "providers") { drawSparks(); drawBars(); }
    else if (S.tab === "home") { homeChart(); try { homeHeat(); } catch (e2) {} try { if (S.analytics && S.analytics.activity_map) renderActivityHeatmap(document.querySelector("#usage-hm") || document.createElement("div")); } catch (e3) {} }
    else if (S.tab === "usage") { if (S.usageView === "cal") renderUsageCalendar($("#usage-cal")); else drawUsageChart(); }
    redrawDonut();
  } catch (e) { }
}
function applyTheme(t, save) {
  if (!THEMES.includes(t)) t = "mono";            // любое чужое значение (старый localStorage, ?theme=, бэкенд) → БД
  S.theme = t;
  document.body.setAttribute("data-theme", t);
  refreshVars();
  const b = $("#theme-toggle");
  if (b) b.title = "Тема: " + (THEME_LBL[t] || t);
  const ico = $("#theme-ico");
  if (ico) ico.textContent = THEME_ICO[t] || "○";
  redrawThemed();
  if (save && !DEMO && Go()) { try { rt(() => ga().SetTheme(t)).catch(() => { }); } catch (e) { } }
}

/* ---------------- number tween ---------------- */
function animNum(el, num, fmt, suf) {
  fmt = fmt || nf;
  if (R.reduced || !num) { el.textContent = fmt(num) + (suf || ""); return; }
  if (el && el._anim) { try { cancelAnimationFrame(el._anim); } catch (e) {} el._anim = 0; }
  const t0 = performance.now(), dur = 600;
  const step = now => { const p = Math.min(1, (now - t0) / dur), e = 1 - Math.pow(1 - p, 3); el.textContent = fmt(num * e) + (suf || ""); if (p < 1) { el._anim = requestAnimationFrame(step); } else { el._anim = 0; } };
  el._anim = requestAnimationFrame(step);
}

/* ---------------- canvas ---------------- */
function box(cv, h, fixed) {
  const dpr = window.devicePixelRatio || 1;
  const hh = h || 220;
  // fixed: hold a canvas at an exact px size (donut) so it never stretches;
  // otherwise size to the container's live clientWidth with DPR scaling.
  const w = fixed ? fixed : Math.max(60, cv.clientWidth || cv.parentElement.clientWidth || 600);
  cv.style.width = (fixed ? fixed + "px" : "100%"); cv.style.height = hh + "px";
  const bw = Math.max(1, Math.round(w * dpr)), bh = Math.max(1, Math.round(hh * dpr));
  if (cv.width !== bw || cv.height !== bh) { cv.width = bw; cv.height = bh; }
  const ctx = cv.getContext("2d"); ctx.setTransform(dpr, 0, 0, dpr, 0, 0); ctx.clearRect(0, 0, w, hh);
  return { ctx, w, h: hh };
}
/* Столбчатый график по дням БЕЗ сглаживания: чёткие прямоугольники (fillRect),
   dpr-коррекция в box(); серии — колонки рядом внутри дня; наведение — тултип
   со значениями. Нет данных — рисуем рамку+сетку и «нет данных» по центру. */
/* Состояние графа на канвасе: один ResizeObserver и одна пара hover-
   слушателей на канвас, переживающие повторные barChart() (DEMO вкл/выкл,
   смена вкладок). Повторный вызов только подменяет draw() — новые
   слушатели не вешаются, RO не пересоздаётся (иначе initial-уведомление
   ResizeObserver запускало бесконечный цикл перерисовки). */
const g_chartState = new WeakMap();
function barChart(cv, labels, series, hgt, opts) {
  const o = opts || {};
  const stacked = !!o.stacked;
  let st = g_chartState.get(cv);
  if (!st) {
    const state = { draw: null, hover: -1 };
    let ro = null;
    const onMove = ev => {
      const d = state.draw; if (!d) return;
      const i = d.hit(ev); if (i !== state.hover) { state.hover = i; d.paint(1); d.tooltip(i); }
    };
    const onLeave = () => { const d = state.draw; if (!d) return; if (state.hover >= 0) { state.hover = -1; d.paint(1); } };
    state.cleanup = () => {
      if (ro) { try { ro.disconnect(); } catch (e) { } }
      cv.removeEventListener("mousemove", onMove);
      cv.removeEventListener("mouseleave", onLeave);
      if (g_chartState.get(cv) === state) g_chartState.delete(cv);
    };
    ro = new ResizeObserver(() => {
      const d = state.draw; if (!d) return;
      const w = cv.clientWidth || (cv.parentElement && cv.parentElement.clientWidth) || 0;
      if (w < 2) return;
      const _mk = state._make; if (typeof _mk !== "function") return;
      state.draw = _mk(); // свежий makeDraw последнего barChart, а не первый пустой (иначе RO затирает главную "нет данных")
      state.hover = -1;
      state.draw.paint(1);
    });
    ro.observe(cv.parentElement || cv);
    cv.addEventListener("mousemove", onMove);
    cv.addEventListener("mouseleave", onLeave);
    g_chartState.set(cv, state);
    st = state;
  }
  st.hover = -1;
  st._make = makeDraw;
  st.draw = makeDraw();
  if (o.animate === false) st.draw.paint(1); else st.draw.animate();

  function makeDraw() {
    const { ctx, w, h } = box(cv, hgt || 220);

    const L = 56, Rt = 14, T = 20, B = 26, iw = Math.max(10, w - L - Rt), ih = Math.max(10, h - T - B);
    const N = labels.length;
    const empty = !N || !series.length;
    let mx = 0;
    if (stacked) { for (let i = 0; i < N; i++) { let s = 0; series.forEach(sr => s += (sr.d[i] || 0)); if (s > mx) mx = s; } }
    else series.forEach(s => s.d.forEach(v => { if (v > mx) mx = v; }));
    mx = mx * 1.15 || 1; { const p = Math.pow(10, Math.floor(Math.log10(mx))); const r = mx / p; const nr = r <= 1 ? 1 : r <= 2 ? 2 : r <= 2.5 ? 2.5 : r <= 5 ? 5 : 10; mx = nr * p; }
    const GR = 4;
    const Y = v => T + ih * (1 - v / mx);
    function grid() {
      ctx.font = "10px " + E.mono;
      ctx.strokeStyle = rawA(E.border, .5); ctx.lineWidth = 1;
      ctx.setLineDash([2, 4]);
      for (let i = 0; i <= GR; i++) {
        const yy = T + ih * i / GR;
        ctx.beginPath(); ctx.moveTo(L, yy); ctx.lineTo(w - Rt, yy); ctx.stroke();
        if (!empty) { ctx.fillStyle = E.text; ctx.textAlign = "right"; ctx.fillText(compact(mx * (1 - i / GR)), L - 8, yy + 3); }
      }
      ctx.setLineDash([]);
      if (!empty) {
        const st = Math.max(1, Math.ceil(N / 10));
        ctx.fillStyle = E.text; ctx.textAlign = "center";
        const fs = N > 20 ? 9 : 10;
        ctx.font = fs + "px " + E.mono;
        for (let i = 0; i < N; i += st) ctx.fillText(labels[i], L + iw * (i + 0.5) / N, h - 6);
        if ((N - 1) % st !== 0) { ctx.fillText(labels[N - 1], L + iw * (N - 0.5) / N, h - 6); }
        ctx.textAlign = "start";
      }
       // подпись единиц измерения над осью Y
       if (!empty && o.unit && h >= 150) {
         ctx.fillStyle = E.text; ctx.font = "9.5px " + E.mono; ctx.textAlign = "right";
         ctx.fillText(o.unit, w - Rt, T - 3);
       }
      ctx.font = "10px " + E.mono;
    }
    function paint(t) {
      // Полный клир битмапа, а не зоны w×h: несвежий узкий замер (w=300/600 до layout)
      // иначе чистит только левую часть широкого битмапа и его бары остаются поверх.
      try { ctx.save(); ctx.setTransform(1, 0, 0, 1, 0, 0); ctx.clearRect(0, 0, cv.width, cv.height); ctx.restore(); } catch (eFC) { ctx.clearRect(0, 0, w, h); }
      grid(); // ось/сетка/подписи: вызов пропадал с fed7ded — графики шли без осей
      if (empty) {
        ctx.beginPath(); ctx.rect(L, T, iw, ih); ctx.strokeStyle = rawA(E.border, .7); ctx.stroke();
        ctx.fillStyle = E.muted; ctx.font = "12px " + E.mono; ctx.textAlign = "center"; ctx.textBaseline = "middle";
        ctx.fillText("нет данных", L + iw / 2, T + ih / 2);
        ctx.textBaseline = "alphabetic"; ctx.textAlign = "start";
        return;
      }
      const groupW = iw / N, nS = series.length, gap = 2;
      if (stacked) {
        const bw = Math.max(12, Math.min(groupW * 0.62, 34));
        for (let i = 0; i < N; i++) {
          const cx = L + iw * (i + 0.5) / N;
          const x0 = cx - bw / 2;
          let acc = 0;
          for (let si = nS - 1; si >= 0; si--) {
            const v = (series[si].d[i] || 0) * t;
            if (!v) continue;
            const bh = Math.max(1, ih * (v / mx));
            ctx.fillStyle = rawA(series[si].c, .72);
            ctx.fillRect(x0, Y(acc + v), bw, bh);
            acc += v;
          }
        }
      } else {
        const bw = Math.max(6, Math.min(groupW * 0.72, (groupW * 0.78 - gap * (nS - 1)) / nS));
        for (let i = 0; i < N; i++) {
          const cx = L + iw * (i + 0.5) / N;
          const x0 = cx - (nS * bw + gap * (nS - 1)) / 2;
          for (let si = 0; si < nS; si++) {
            const v = (series[si].d[i] || 0) * t;
            if (!v) continue;
            const bh = Math.max(1, ih * (v / mx));
            ctx.fillStyle = rawA(series[si].c, .72);
            ctx.fillRect(x0 + si * (bw + gap), Y(v), bw, bh);
          }
          }
      }
      if (o.costLine && o.costLine.d) {
        // Линия кэша в масштабе БАРОВ (mx — шкала стака): видно долю кэша в каждом дне, без своей оси.
        const cd = o.costLine.d, cc = o.costLine.c || "#f5f5f5";
        ctx.save(); ctx.setLineDash([5, 4]); ctx.strokeStyle = cc; ctx.lineWidth = 1.6; ctx.beginPath();
        for (let i = 0; i < N; i++) {
          const cx = L + iw * (i + 0.5) / N, cy = Y(cd[i] || 0);
          if (i === 0) ctx.moveTo(cx, cy); else ctx.lineTo(cx, cy);
        }
        ctx.stroke(); ctx.restore();
      }
    }
    function tooltip(i) {
      if (empty || i < 0 || i >= N) return;
      const cx = L + iw * (i + 0.5) / N;
      ctx.fillStyle = rawA(E.text, .05);
      ctx.fillRect(L + iw * i / N, T, iw / N, ih);
      ctx.font = "10px " + E.mono;
      const tot = stacked ? series.reduce((a, s) => a + (s.d[i] || 0), 0) : 0;
      const suf = o.unit ? " " + o.unit : "";
      const lines = [labels[i]].concat(series.map(s => s.n + ": " + compact(s.d[i] || 0) + suf));
      if (stacked) lines.push("итого: " + compact(tot) + suf);
      if (o.costLine && o.costLine.d && (o.costLine.d[i] || 0) > 0) lines.push("стоимость: $" + (+o.costLine.d[i]).toFixed(2));
      const lw = Math.max.apply(null, lines.map(t => ctx.measureText(t).width)) + 16;
      const lh = lines.length * 13 + 12;
      const tx = Math.max(L, Math.min(cx - lw / 2, w - Rt - lw));
      const ty = Math.max(4, T - 8 - lh);
      ctx.fillStyle = "rgba(14,10,10,.94)";
      ctx.strokeStyle = rawA(E.border2, .9); ctx.lineWidth = 1;
      ctx.beginPath(); ctx.roundRect(tx, ty, lw, lh, 6); ctx.fill(); ctx.stroke();
      ctx.fillStyle = E.text; ctx.textAlign = "left";
      lines.forEach((t, k) => ctx.fillText(t, tx + 8, ty + 16 + k * 13));
      ctx.textAlign = "start";
    }
    function hit(ev) {
      const r = cv.getBoundingClientRect();
      const x = ev.clientX - r.left;
      const i = Math.floor((x - L) / (iw / N));
      return (i >= 0 && i < N) ? i : -1;
    }
    function animate() {
      if (empty) { paint(1); return; }
      // Владелец цепочки: новый makeDraw молча отменяет летящие кадры старого —
      // иначе узкий замер до layout (w=600) штампует поверх свежего широкого (clear
      // 600px не стирает правую часть 1286px) и остаётся в левой половине навсегда.
      const _mine = st.draw;
      const t0 = performance.now();
      requestAnimationFrame(function step(now) {
        if (st.draw !== _mine) return;
        const p = Math.min(1, (now - t0) / 450);
        paint(1 - Math.pow(1 - p, 3));
        if (p < 1) { requestAnimationFrame(step); return; }
        // Самолечение финала: layout мог доехать после замера (узкий w) — тогда
        // перемеряем и рисуем свежий кадр синхронно, без новой анимации (цикла нет).
        try {
          const _nowW = cv.clientWidth || (cv.parentElement && cv.parentElement.clientWidth) || 0;
          if (_nowW > 2 && Math.abs(_nowW - w) > 60) { const _nd = makeDraw(); st.draw = _nd; _nd.paint(1); return; }
        } catch (eFH) {}
        paint(1);
      });
    }
    return { paint, tooltip, hit, animate };
  }
}
function donut(cv, p, col, animate) {
  const { ctx, w, h } = box(cv, 150, 150);
  const cx = w / 2, cy = h / 2, rr = Math.min(w, h) / 2 - 8;
  p = Math.max(0, Math.min(1, p));
  function dr(t) {
    ctx.clearRect(0, 0, w, h);
    ctx.beginPath(); ctx.lineWidth = 12; ctx.lineCap = "round"; ctx.strokeStyle = rawA(E.border2, .7); ctx.arc(cx, cy, rr, 0, 6.283); ctx.stroke();
    ctx.beginPath(); ctx.lineWidth = 12; ctx.lineCap = "round"; ctx.strokeStyle = col || E.accent; ctx.arc(cx, cy, rr, -1.5708, -1.5708 + 6.283 * t); ctx.stroke();
    ctx.fillStyle = E.text; ctx.font = "700 20px " + E.mono; ctx.textAlign = "center"; ctx.textBaseline = "middle"; ctx.fillText(pct(p), cx, cy - 6);
    ctx.fillStyle = E.muted; ctx.font = "10px " + E.mono; ctx.fillText("квоты", cx, cy + 12);
  }
  if (R.reduced || animate === false) dr(1);
  else { const t0 = performance.now(); requestAnimationFrame(function step(now) { const t = (now - t0) / 750; dr(Math.min(1, t)); if (t < 1) requestAnimationFrame(step); }); }
}

