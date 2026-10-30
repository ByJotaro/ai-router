/* ==================================================================
   HOME
   ================================================================== */
function setFoot() {
  const st = S.status || {};
  const ver = $("#ver"); let vt = st.version ? "v" + st.version : "v—"; if (DEMO && !/demo/i.test(vt)) vt += " (demo)"; const vd = "";
  if (ver) { if (ver.textContent !== vt) ver.textContent = vt; if (ver.style.display !== vd) ver.style.display = vd; }
  const d = $("#foot-dot"), l = $("#foot-status");
  const px = S.proxy || {}; const total = +(px.total || 0), alive = +(px.alive || 0);
  const cls = total > 0 ? (alive > 0 ? "dot ok pulse" : "dot err") : "dot";
  const txt = total > 0 ? (alive > 0 ? "пул " + alive + "/" + total : "пул 0/" + total) : "напрямую";
  if (d && d.className !== cls) d.className = cls;
  if (l && l.textContent !== txt) l.textContent = txt;
}
/* Пустое состояние главной: аккуратная карточка с подсказкой вместо пустоты */
function homeEmpty(hint) {
  return '<div class="home-empty">' +
    '<div class="he-ico">◔</div>' +
    '<div class="he-body"><div class="he-tit">Данных пока нет</div>' +
    '<div class="he-hint">' + esc(hint || "запустите ядро или совершите первый запрос через шлюз") + '</div></div>' +
    '<button class="btn primary sm" data-demo>показать пример</button></div>';
}
async function loadHome() {
  const host = $("#home-kpis");
  if (DEMO) { setFoot(); homeKPIs(host); homeChart(); try { homeTops(); } catch (eT) {} try { homeHeat(); } catch (eH2) {} homeHeatSoon(); homeErrorsSoon(); return; }
  const _d = DEMO;
  const seq = (S._homeSeq = (S._homeSeq || 0) + 1);
  if (!host.innerHTML) host.innerHTML = '<div class="kpi"><div class="label">Загрузка данных…</div><div class="val">↻</div></div>';
  try {
    const _rs = await Promise.allSettled([
      API.Status(), API.Endpoints(), API.Breakers(), API.Lockouts(), API.Keys(), API.Usage(), API.DailyUsage(365), API.ProxyStatus().catch(() => null),
    ]);
    const _v = i => (_rs[i] && _rs[i].status === "fulfilled") ? _rs[i].value : null;
    const st = _v(0), end = _v(1) || [], brk = _v(2) || [], locks = _v(3) || [], keys = _v(4) || [], usage = _v(5), daily = _v(6), px = _v(7);
    if (!st && !end.length && !usage && !daily) throw new Error("шлюз недоступен — проверьте, что ядро запущено");
    if (DEMO !== _d || seq !== S._homeSeq || S.tab !== "home") return;
    S.status = st; S.end = end || []; S.breakers = brk || []; S.locks = locks || []; S.keys = keys || [];
    S.usage = usage || {}; S.daily = daily; if (px) S.proxy = px;
    // Хэш-гейт поллинга: те же ответы — выходим до рендеров, живые данные не теряются.
    try {
      const hstr = JSON.stringify([st, end, brk, locks, keys, usage, daily, px]);
      let hh = 0; for (let i = 0; i < hstr.length; i++) hh = ((hh << 5) - hh + hstr.charCodeAt(i)) | 0;
      const hk = "h" + (hh >>> 0).toString(36);
      if (S._homeHash === hk) return;
      S._homeHash = hk;
    } catch (eH) {}
    const hasData = (S.end.length > 0) || (S.daily && S.daily.series && S.daily.series.length > 0) || (S.usage && (S.usage.reqs || S.usage.tokens));
    if (!hasData) { host.innerHTML = homeEmpty("запустите ядро или сделайте первый запрос через шлюз"); homeChart(); homeHeatSoon(); homeErrorsSoon(); return; }
    S._topsLoaded = false;
    try {
      const ta = await API.UsageAnalytics(90).catch(() => null);
      if (ta) { S.tops90 = ta; S._topsLoaded = true; }
    } catch (eT) {}
    homeKPIs(host);
    homeChart();
    homeHeatSoon();
    try { homeTops(); } catch (eHT) {}
    homeErrorsSoon();
    // Годовой ряд догоняет фоном: календарь перерисовывается на все месяцы 06-13→сегодня.
    try {
      API.UsageAnalyticsLight(365).then(a => {
        if (DEMO !== _d || seq !== S._homeSeq || S.tab !== "home") return;
        if (a && a.daily_trend && a.daily_trend.length) { S.analytics = a; try { homeHeat(); } catch (e) {} }
      }).catch(() => {});
    } catch (e) {}
  } catch (e) {
    if (DEMO !== _d) return; // успели включить демо — ошибку stale-запроса не показываем
    host.innerHTML = errHint(e);
  }
}
function plural(n, a, b, c) { n = Math.abs(+n || 0) % 100; const d = n % 10; if (n > 10 && n < 20) return c; if (d > 1 && d < 5) return b; if (d === 1) return a; return c; }
function homeKPIs(host) {
  // 1-в-1 w08v6d: 4 блока (провайдеры / запросы сегодня / токены 30д / паузы-ошибки).
  const se = (S.daily && S.daily.series) || [];
  const eps = (S.end || []).filter(e => !e.catalog && e.status !== "unconfigured" && e.name !== "__GLOBAL__");
  const isLive = e => (e.keys_healthy || 0) > 0 && e.breaker === "closed";
  const alive = eps.filter(isLive).length;
  const brkOpen = (S.breakers || []).filter(b => b.state === "open").length;
  const errN = (S.logs || []).filter(l => l.status !== "ok").length;
  const _w = se.slice(-30);
  const totR = _w.reduce((a, p) => a + (p.requests || 0), 0);
  const totC = _w.reduce((a, p) => a + (p.cost_usd || 0), 0);
  const totP = _w.reduce((a, p) => a + (p.prompt || 0), 0);
  const totQ = _w.reduce((a, p) => a + (p.completion || 0), 0);
  const today = se.length ? se[se.length - 1] : null;
  const fmt = n => n >= 1e9 ? (n / 1e9).toFixed(2) + "G" : n >= 1e6 ? (n / 1e6).toFixed(1) + "M" : n >= 1e3 ? (n / 1e3).toFixed(1) + "k" : "" + n;
  const h = '<div class="kpi"><div class="label">Провайдеры живы</div><div class="val ' + (alive === eps.length ? 'good' : '') + '">' + alive + ' / ' + eps.length + '</div><div class="sub">ключи живы · брейкер closed</div></div>'
    + '<div class="kpi"><div class="label">Запросы сегодня</div><div class="val">' + (today ? today.requests : 0) + '</div><div class="sub">30 дн: ' + totR.toLocaleString('ru-RU') + ' · $' + (DEMO ? totC : Math.round(totC / 100)).toLocaleString('ru-RU') + '</div></div>'
    + '<div class="kpi"><div class="label">Токены · 30 дн</div><div class="val">' + fmt(totP + totQ) + '</div><div class="sub">вход ' + fmt(totP) + ' · выход ' + fmt(totQ) + '</div></div>'
    + '<div class="kpi"><div class="label">Паузы / ошибки</div><div class="val ' + ((brkOpen || errN) ? 'bad' : 'good') + '">' + brkOpen + ' / ' + errN + '</div><div class="sub">' + (brkOpen ? brkOpen + ' на паузе' : 'все каналы работают') + ' · лента 40</div></div>';
  if (host._h !== h) { host._h = h; host.innerHTML = h; }
}
// homeRisk/rxFor/rxHelp удалены: риск-центра нет. setFoot()/sidebar-dot остаются.
/* Главная · календарь: одна горизонтальная лента Пн..Вс, обрезка слева целыми неделями. */
function homeHeatSoon() {
  // Гард поллинга: новый годовой ряд или новые дни — перерисовку планируем.
  try {
    const _tr = (S.analytics && S.analytics.daily_trend) || [], _se = (S.daily && S.daily.series) || [];
    const _k = _tr.length + "|" + _se.length + "|" + (_se.length ? _se[_se.length - 1].day : "") + "|" + (_tr.length ? (_tr[_tr.length - 1].date || _tr[_tr.length - 1].day) : "");
    if (S._heatSoonKey === _k) return;
    S._heatSoonKey = _k;
  } catch (eHK) {}
  try {
    const f = () => { try { homeHeat(); } catch (e) {} };
    if ("requestIdleCallback" in window) requestIdleCallback(f, { timeout: 1200 });
    else setTimeout(f, 60);
  } catch (e) {}
}
/* Главная · год-календарь: 12 мес Пн..Вс одной лентой (колонки-недели).
   Источник — daily_usage роллап state.db (S.daily 365 + S.analytics фон).
   Цвет по токенам prompt+completion: 1 токен = lv1 зелень, 0 = холод.
   Пустые дни — ghost-рамка, тултип #home-tip как у графика. */
function homeCalWindow(host, days, pad){
  /* Одна горизонтальная лента: ширина = целые недели (куб 14 + gap 4 = шаг 18).
     Обрезка только слева целыми неделями; pad (дыры первой недели) отдельно.
     Перенос на .has-wrap — только если даже 8 недель не влезают. */
  var maxCols = 0, wrap = false;
  try {
    var hmw = (host.clientWidth || (host.parentElement && host.parentElement.clientWidth) || 0);
    if(hmw > 0) maxCols = Math.floor((hmw + 4) / 18);
  } catch(eW){}
  var tail = days, tailPad = pad;
  if(maxCols > 0){
    var totalCols = Math.ceil((days.length + pad) / 7);
    if(totalCols > maxCols){
      if(maxCols < 8){
        wrap = true;
      } else {
        var dropCols = totalCols - maxCols;
        var drop = dropCols * 7;
        tail = days.slice(drop);
        tailPad = pad;
        if(tailPad > 0 && dropCols > 0){
          /* pad относится к первой неделе исходника — при обрезке слева
             первая видимая неделя начинается с понедельника, дыр нет */
          tailPad = 0;
        }
      }
    }
  }
  return { tail: tail, pad: Math.max(0, tailPad), wrap: wrap };
}
function homeHeat() {
  const host = $("#home-hm");
  if (!host) return;
  const tr = (S.analytics && S.analytics.daily_trend) || [];
  const se = (S.daily && S.daily.series) || [];
  const byId = {}; se.forEach(p => { if (p && p.day) byId[p.day] = p; });
  if (!se.length && !tr.length) { const eh = '<span class="chart-empty-note">Нет дневных данных — календарь заполнится после первого запроса.</span>'; if (host._key !== "empty" || host.innerHTML !== eh) { host._key = "empty"; S._heatKey = "empty"; host.innerHTML = eh; } return; }
  /* Цвет — по токенам (prompt+completion, как график): 1 токен = уже зелень lv1, ноль — единственный холодный */
  const tokOf = p => ((p && p.prompt) || 0) + ((p && p.completion) || 0);
  const tmap = {}; tr.forEach(p => { const k = p && (p.date || p.day); if (k) tmap[k] = p; });
  const days = [];
  { const t = new Date(); t.setHours(12, 0, 0, 0);
    const start = new Date(t); start.setDate(start.getDate() - 364);
    while (((start.getDay() + 6) % 7) !== 0) start.setDate(start.getDate() - 1);
    const n = Math.round((t - start) / 864e5);
    for (let k = n; k >= 0; k--) { const dt = new Date(t); dt.setDate(dt.getDate() - k);
      const iso = dt.getFullYear() + "-" + String(dt.getMonth() + 1).padStart(2, "0") + "-" + String(dt.getDate()).padStart(2, "0");
      const known = byId[iso];
      if (known) days.push({ dt, iso, v: tokOf(known), req: known.requests || 0, fact: true, prompt: known.prompt || 0, compl: known.completion || 0 });
      else if (iso in tmap) { const tp = tmap[iso]; days.push({ dt, iso, v: tokOf(tp), req: tp.requests || 0, fact: true, prompt: tp.prompt || 0, compl: tp.completion || 0 }); }
      else days.push({ dt, iso, v: 0, req: 0, fact: false, prompt: 0, compl: 0 }); } }
  const vmax = Math.max(1, ...days.map(x => x.v));
  const lvl = v => { if (!v) return 0; const t = v / vmax; if (t > .75) return 5; if (t > .5) return 4; if (t > .25) return 3; if (t > .05) return 2; return 1; };
  const NL = ["янв", "фев", "мар", "апр", "май", "июн", "июл", "авг", "сен", "окт", "ноя", "дек"];
  const pad = 0;
  var _win = homeCalWindow(host, days, pad);
  var _tailRef = _win.tail, _padRef = _win.pad;
  const cells = Array(Math.max(0, _padRef)).fill(null).concat(_tailRef);
  const nCols = Math.ceil(cells.length / 7);
  const colMonth = new Array(nCols).fill(-1);
  for (let c = 0; c < nCols; c++) { for (let r = 0; r < 7; r++) { const cell = cells[c * 7 + r]; if (cell) { colMonth[c] = cell.dt.getMonth(); break; } } }
  /* monthline — абсолютные позиции по шагу колонки (куб 14 + gap 4 = 18px),
     подписи только на смене месяца, как на фото */
  let monthHtml = "", lastM = -1;
  for (let c = 0; c < nCols; c++) { const m = colMonth[c]; if (m !== lastM && m >= 0) { monthHtml += '<span style="left:' + (c * 18) + 'px">' + NL[m] + '</span>'; lastM = m; } }
  try { const mh = $("#home-monthline"); if (mh && mh._h !== monthHtml) { mh._h = monthHtml; mh.innerHTML = monthHtml; } } catch (eM) {}
  const factN = days.filter(x => x.fact).length;
  const peak = days.reduce((a, b) => b.v > a.v ? b : a, days[0]);
  const avgY = Math.round(days.reduce((a, x) => a + (x.req || 0), 0) / days.length);
  const acard = (v, l) => '<div class="astat"><div class="a-v">' + v + '</div><div class="a-l">' + esc(l) + '</div></div>';
  const ast = acard(factN + ' <em>дн.</em>', 'Активных дней')
    + acard(avgY, 'Запросов/день')
    + acard(fmtAx(peak.v), 'Пик · ' + peak.iso)
    + acard(Math.round(100 * factN / days.length) + '<em>%</em>', 'Активность');
  try { const ah = $("#home-astats"); if (ah && ah._h !== ast) { ah._h = ast; ah.innerHTML = ast; } } catch (eA) {}
  var tail = _win.tail, tailPad = _win.pad;
  /* Одна горизонтальная лента колонок Пн..Вс (как на фото): колонки слева
     направо, дни сверху вниз Пн→Вс. Старые дни обрезаются слева целыми
     неделями — никаких дыр/отступов слева. Перенос .has-wrap — только если
     даже 8 недель не влезают (экстремальная теснота). */
  var cells2 = Array(Math.max(0, tailPad)).fill(null).concat(tail);
  var nCols2 = Math.ceil(cells2.length / 7);
  var colsHtml = "";
  for (var cc = 0; cc < nCols2; cc++) {
    colsHtml += '<div class="cal-col">';
    for (var rr = 0; rr < 7; rr++) {
      var cell = cells2[cc * 7 + rr];
      if (!cell) { colsHtml += '<div class="cal-cell pad"></div>'; continue; }
      colsHtml += '<div class="cal-cell ' + (cell.fact ? 'fact' : 'recon') + ' lv' + lvl(cell.v) + '" data-day="' + cell.iso + '" data-req="' + (cell.req || 0) + '" data-fact="' + (cell.fact ? 1 : 0) + '" data-prompt="' + (cell.prompt || 0) + '" data-compl="' + (cell.compl || 0) + '" data-tok="' + (cell.v || 0) + '"></div>';
    }
    colsHtml += '</div>';
  }
  var html = '<div class="cal-grid' + (_win.wrap ? ' has-wrap' : '') + '">'
    + colsHtml
    + '</div><div class="legend">меньше <i class="lv0"></i><i class="lv1"></i><i class="lv2"></i><i class="lv3"></i><i class="lv4"></i><i class="lv5"></i> больше</div>';
  const hkey = tail.map(x => x.iso + ":" + x.v + (x.fact ? "f" : "r")).join(",") + "|" + S.theme + "|" + tailPad + "x" + tail.length + (_win.wrap ? "w" : "");
  if (host._key !== hkey || S._heatKey !== hkey) { host._key = hkey; S._heatKey = hkey; host.innerHTML = html; }
  else { try { homeSyncHeights(); } catch (eS) {} }
  homeHeatTip(host);
}
/* Тултип клеток календаря через #home-tip — как у графика:
   tt-day + tt-tot + вход/выход с барами, fixed-позиция от курсора. */
function homeHeatTip(host) {
  if (!host || host._tipW) return; host._tipW = 1;
  host.addEventListener("mousemove", e => {
    const tip = $("#home-tip"); if (!tip) return;
    const el = e.target && e.target.closest ? e.target.closest(".cal-cell[data-day]") : null;
    if (!el || !host.contains(el)) { tip.style.display = "none"; return; }
    try {
      const iso = el.getAttribute("data-day"), req = +(el.getAttribute("data-req") || 0);
      const pi = +(el.getAttribute("data-prompt") || 0), co = +(el.getAttribute("data-compl") || 0);
      const fact = el.getAttribute("data-fact") === "1";
      const tv = ((pi + co) || 0) || (+(el.getAttribute("data-tok") || 0));
      const pw = (pi + co) ? Math.round(100 * pi / (pi + co)) : 0, cw = (pi + co) ? 100 - pw : 0;
      if (!tv && !req) tip.innerHTML = '<div class="tt-day">' + escAx(iso) + '</div><div class="tt-tot">нет трафика</div>';
      else tip.innerHTML = '<div class="tt-day">' + escAx(iso) + '</div><div class="tt-tot">' + fmtAx(tv) + ' ток./день · ' + req + ' запр.</div>'
        + '<div class="tt-row"><span class="k">вход</span><span class="v">' + fmtAx(pi) + '</span></div><div class="tt-bar"><i class="tt-in" style="width:' + pw + '%"></i></div>'
        + '<div class="tt-row"><span class="k">выход</span><span class="v">' + fmtAx(co) + '</span></div><div class="tt-bar"><i class="tt-out" style="width:' + cw + '%"></i></div>';
    } catch (eT) { tip.textContent = el.getAttribute("data-day") || ""; }
    try {
      tip.style.display = "block";
      tip.style.position = "fixed";
      tip.style.left = Math.min(e.clientX + 14, window.innerWidth - 252) + "px";
      tip.style.top = (e.clientY + 12) + "px";
      tip.style.zIndex = 60;
    } catch (eP) { tip.style.display = "block"; }
  });
  host.addEventListener("mouseleave", () => { const tip = $("#home-tip"); if (tip) tip.style.display = "none"; });
}
/* Home duo: карточки независимой высоты — топы тянутся страницей без
   внутреннего скролла. homeSyncHeights — no-op для старых вызовов. */
function homeSyncHeights() {
  try {
    const tops = $("#home-tops");
    if (!tops) return;
    tops.style.maxHeight = "none";
    tops.style.overflowY = "visible";
  } catch (e) {}
}
/* Ресайз: ширина сменилась — окно ленты пересчитываем (сброс ключа). */
try {
  if (!window._homeHmRO && window.ResizeObserver) {
    window._homeHmRO = new ResizeObserver(() => {
      try {
        const host = document.querySelector("#home-hm");
        if (!host || S.tab !== "home") return;
        const w = host.clientWidth || 0;
        if (Math.abs(w - (window._homeHmW || 0)) > 20) {
          window._homeHmW = w;
          host._key = ""; S._heatKey = "";
          try { homeHeat(); } catch (e) {}
        }
      } catch (e) {}
    });
    window._homeHmRO.observe(document.querySelector("#home-hm") || document.body);
  }
} catch (eRO) {}
window.addEventListener("resize", () => {
  try {
    const host = document.querySelector("#home-hm");
    if (!host || S.tab !== "home") return;
    host._key = ""; S._heatKey = "";
    try { homeHeat(); } catch (e) {}
  } catch (e) {}
});
/* ---- Home: «Топы» — топ-3 моделей + топ-3 провайдеров, sub виден ---- */
function homeErrorsSoon() {
  try {
    const run = () => { try { homeTops(); } catch (e) {} };
    if ("requestIdleCallback" in window) requestIdleCallback(run, { timeout: 1200 });
    else setTimeout(run, 60);
  } catch (e) {}
  // Полные топы + годовой ряд догоняют фоном.
  // Once-guard поллинга: топы уже загружены и годовой ряд есть — фон не дёргаем.
  try { if (S._topsLoaded && S.analytics) return; } catch (eG) {}
  try {
    const bg = () => {
      if (DEMO || !Go()) return;
      API.UsageAnalytics(90).then(a => {
        if (!a) { S._topsLoaded = true; try { homeTops(); } catch (e) {} return; }
        S.tops90 = a; S._topsLoaded = true;
        try { homeTops(); } catch (e) {}
        try { homeHeat(); } catch (e) {}
      }).catch(() => { S._topsLoaded = true; try { homeTops(); } catch (e) {} });
      API.UsageAnalyticsLight(365).then(a => {
        if (!a) return;
        if (a && a.daily_trend && a.daily_trend.length) { S.analytics = a; try { homeHeat(); } catch (e) {} }
      }).catch(() => {});
    };
    if ("requestIdleCallback" in window) requestIdleCallback(bg, { timeout: 2500 });
    else setTimeout(bg, 1500);
  } catch (e) {}
}
 /* удалено: homeErrors() — употреблений 0, прямой вызов homeTops() */
/* ---- Home: топ-3 моделей (w08v6d дословно) ---- */
function homeTops() {
  const host = $("#home-tops");
  if (!host) return;
  const keepTop = host.scrollTop || 0;
  const full = S.tops90 || null;
  let models = (full && full.by_model) || [];
  // DEMO: бэкенда нет — топы из демо-логов (40 строк), без пустого виджета.
  if (DEMO && !models.length) models = homeTopsDemoFallback().models;
  models = (models || []).slice().sort((a, b) => ((b.requests || 0) - (a.requests || 0))).slice(0, 3);
  if (!models.length) {
    host.innerHTML = S._topsLoaded
      ? '<div class="state compact"><div class="hint">Пока нет данных за 90 дней — сделайте первый запрос через шлюз</div></div>'
      : '<div class="state compact"><div class="hint">считаем топы…</div></div>';
    return;
  }
  const RANKS = ["01", "02", "03"];
  const mMax = Math.max(1, ...models.map(m => (m.requests || 0)));
  const tkey = models.map(x => (x.model || "") + ":" + (x.requests || 0)).join(",") + "|" + S.theme;
  if (host._key === tkey) { host.scrollTop = keepTop; return; }
  host._key = tkey;
  host.innerHTML = models.map((m, i) =>
    '<div class="toprow" data-model="' + esc(m.model || "—") + '" tabindex="0" role="button" title="' + esc((m.model || "—") + " — фильтр ленты в Журналах") + '">'
    + '<span class="rk">' + RANKS[i] + '</span><span class="m" title="' + esc(m.model || "—") + '">' + esc(m.model || "—") + '</span><span class="n">' + (m.requests || 0) + '</span>'
    + '<span class="bar"><i style="width:' + Math.round(100 * (m.requests || 0) / mMax) + '%"></i></span>'
    + '<span class="sub" title="' + esc((m.provider || "") + " · ошибок " + (m.errors || 0)) + '">' + esc(m.provider || "") + ' · err ' + esc(nf(m.errors || 0)) + '</span></div>').join("");
  try { host.scrollTop = keepTop; } catch (e) {}
  try { if (typeof homeSyncHeights === "function") homeSyncHeights(); } catch (e2) {}
  const all = $("#home-tops-all");
  if (all && !all._w) { all._w = 1; all.addEventListener("click", () => { try { show("usage"); } catch (e) {} }); }
}
/* DEMO-фолбэк топов: агрегируем демо-логи S.logs (40 строк) по модели/провайдеру.
   Живой режим его не вызывает (там фон UsageAnalytics(90)). */
function homeTopsDemoFallback() {
  const mm = {}, pp = {};
  (S.logs || []).forEach(l => {
    const t = ((l.prompt || 0) + (l.completion || 0)) || 0;
    const mk = (l.model || l.requested_model || "—") + "" + (l.provider || "");
    const a = mm[mk] || (mm[mk] = { model: l.model || l.requested_model || "—", provider: l.provider || "", requests: 0, prompt: 0, completion: 0, total: 0, errors: 0 });
    a.requests++; a.prompt += l.prompt || 0; a.completion += l.completion || 0; a.total += t; if (l.status !== "ok") a.errors++;
    const pk = l.provider || "—";
    const b = pp[pk] || (pp[pk] = { provider: pk, requests: 0, prompt: 0, complete: 0, total: 0, errors: 0 });
    b.requests++; b.prompt += l.prompt || 0; b.complete += l.completion || 0; b.total += t; if (l.status !== "ok") b.errors++;
  });
  const models = Object.values(mm).sort((x, y) => (y.total - x.total) || (y.requests - x.requests));
  const provs = Object.values(pp).sort((x, y) => (y.total - x.total) || (y.requests - x.requests));
  return { models, provs };
}
function homeChart() {
  // SVG-канон: Tokens = вход+выход, вектор (чёткий бейдж/text, тема через CSS var).
  var cv = $("#home-chart");
  if(!cv) return;
  var all = (S.daily && S.daily.series) || [];
  var pts = all.slice(-30);
  var lg = $("#home-legend");
  if(!pts.length) return;
  try { cv.style.display = ""; } catch(eD){}
  var cap = $("#home-chart-cap"), rng = $("#home-chart-range");
  if(cap && cap.textContent !== "Токены · 30 дней") cap.textContent = "Токены · 30 дней";
  if(rng && pts.length){ var rt = pts[0].day + " → " + pts[pts.length - 1].day; if(rng.textContent !== rt) rng.textContent = rt; }
  drawEmberLine(cv, lg, pts, {});
  renderHomeHeader();
  syncHomeThemes();
  window.__W08_READY__ = true;
}
// Шапка w08v6d: brand + meta(шлюз/v/аптайм) + темы + live (пауза/ошибки).
// Единая для всех 6 вкладок: meta/live обновляются по id каждой панели.
var UNI_HDR_TABS = ["home","prov","combo","usage","logs","proxies","settings"];
function renderUniHeaders(){
  if(typeof DEMO !== "undefined" && DEMO) return; // стенд ?demo=1: meta/live статикой из index.html
  try {
    var st = S.status || {};
    var up = +st.uptime_s || 0, uh = Math.floor(up / 3600), um = Math.floor(up % 3600 / 60);
    var gw = st.gateway || "", ver = st.version || "";
    var base = gw + " · v" + ver + " · аптайм " + uh + "ч " + um + "м";
    var brkOpen = (S.breakers || []).filter(function(b){ return b.state === "open"; }).length;
    var liveTxt = "● LIVE · " + (brkOpen ? ("пауза: " + brkOpen) : "все каналы работают");
    UNI_HDR_TABS.forEach(function(p){
      var meta = document.getElementById(p === "home" ? "home-meta" : p + "-meta");
      var live = document.getElementById(p === "home" ? "home-live" : p + "-live");
      var txt = document.getElementById(p === "home" ? "home-live-txt" : p + "-live-txt");
      if(meta && p !== "home"){
        var extra = meta.getAttribute("data-extra") || "";
        var t = base + (extra ? " · " + extra : "");
        meta.textContent = t; meta.title = t;
      }
      if(live){
        if(brkOpen) live.classList.add("paused"); else live.classList.remove("paused");
        if(txt) txt.textContent = liveTxt;
      }
    });
    var pc = document.getElementById("prov-hd-count");
    if(pc){ var n = (S.end || []).filter(function(e){ return !e.catalog; }).length; pc.textContent = n ? "· " + n : ""; }
  } catch(eH){}
}
function renderHomeHeader(){
  if(typeof DEMO !== "undefined" && DEMO) return; // стенд ?demo=1: home-meta/live статикой из index.html
  try {
    var st = S.status || {};
    var up = +st.uptime_s || 0, uh = Math.floor(up / 3600), um = Math.floor(up % 3600 / 60);
    var gw = st.gateway || "", ver = st.version || "";
    var meta = $("#home-meta");
    if(meta){ var t = gw + " · v" + ver + " · аптайм " + uh + "ч " + um + "м"; meta.textContent = t; meta.title = t; }
    var brkOpen = (S.breakers || []).filter(function(b){ return b.state === "open"; }).length;
    var errN = (S.logs || []).filter(function(l){ return l.status !== "ok"; }).length;
    var live = $("#home-live"), txt = $("#home-live-txt");
    if(live){
      if(brkOpen) live.classList.add("paused"); else live.classList.remove("paused");
      if(txt) txt.textContent = "● LIVE · " + (brkOpen ? ("пауза: " + brkOpen) : "все каналы работают");
    }
    renderUniHeaders();
  } catch(eH){}
}
// Пилюля тем: осталась одна тема — «БД». Кнопка на месте, клик применяет её (+бэкенд).
var HOME_THEME_BTNS = [["mono","БД"]];
function syncHomeThemes(){
  try {
    var host = $("#home-themes");
    if(!host) return;
    var cur = S.theme || "mono";
    var h = HOME_THEME_BTNS.map(function(b){
      var on = (cur === b[0]);
      return '<button data-th="' + b[0] + '"' + (on ? ' class="on"' : "") + ' title="' + b[1] + '">' + b[1] + "</button>";
    }).join("");
    if(host._h !== h){
      host._h = h; host.innerHTML = h;
      Array.prototype.forEach.call(host.querySelectorAll("button"), function(btn){
        btn.addEventListener("click", function(){
          var th = btn.getAttribute("data-th");
          try { applyTheme(th, true); } catch(eA){}
          try { syncHomeThemes(); } catch(eS){}
        });
      });
    } else {
      Array.prototype.forEach.call(host.querySelectorAll("button"), function(btn){
        var th = btn.getAttribute("data-th");
        var on = (cur === th);
        if(on) btn.classList.add("on"); else btn.classList.remove("on");
      });
    }
  } catch(eT){}
}
// SVG-график w08v6d (v6d D, вектор вместо canvas): P=46, H=264, ih=H-64,
// ломаная точка-в-точку (polyline, miter/butt 2px), заливка area 14%,
// глубина нижней трети (dashed-линия), max-бейдж (SVG-text: всегда чёткий),
// hover-полоса + линейка + stacked-тултип вход/выход. Цвета — CSS var(),
// тема меняется без перерисовки. Кэш-полос фона НЕТ.
var _homePts = [], _homeHover = -1;
function fmtAx(n){ n = +n || 0; return n >= 1e9 ? (n/1e9).toFixed(2)+"G" : n >= 1e6 ? (n/1e6).toFixed(1)+"M" : n >= 1e3 ? (n/1e3).toFixed(1)+"k" : ""+Math.round(n); }
function escAx(s){ return String(s == null ? "" : s).replace(/[&<>"']/g, function(c){ return {"&":"&amp;","<":"&lt;",">":"&gt;",'"':"&quot;","'":"&#39;"}[c]; }); }
function drawEmberLine(box, lg, pts, opts) {
  opts = opts || {};
  function valOf(p){ if(opts.value) return opts.value(p); return ((p && p.prompt) || 0) + ((p && p.completion) || 0); }
  if(!box._w08) box._w08 = {};
  var ST = box._w08;
  ST.pos = ST.pos || []; if(typeof ST.hover !== "number") ST.hover = -1;
  function draw(){
    var host = (box && box.isConnected) ? box : document.getElementById("home-chart");
    if(!host || !host.isConnected) return;
    var svg = (box && box.querySelector) ? box.querySelector("svg") : null;
    if(!svg) svg = document.getElementById("home-svg");
    if(!svg) return;
    var W = host.clientWidth || host.parentElement.clientWidth || 800, H = 264;
    W = Math.max(60, Math.round(W));
    var P = 46, iw = W - P - 8, ih = H - 64;
    var d = pts;
    var vals = d.map(valOf);
    var mx = Math.max.apply(null, vals.concat([1]));
    var Y = function(v){ return P + ih * (1 - v / mx); }, y0 = P + ih;
    var n = vals.length, slot = iw / n;
    var X = function(i){ return P + slot * i + slot / 2; };
    ST.pos = vals.map(function(v,i){ return {x: X(i), y: Y(v), i: i}; });
    var f1 = function(v){ return Math.round(v * 10) / 10; };
    var grid = "", yl = "";
    for(var g = 0; g <= 3; g++){
      var v = mx * g / 3, y = Y(v);
      grid += '<line class="hl-grid" x1="' + P + '" y1="' + f1(y) + '" x2="' + (W - 8) + '" y2="' + f1(y) + '"/>';
      yl += '<text x="4" y="' + f1(y + 3) + '">' + fmtAx(Math.round(v)) + "</text>";
    }
    var line = ST.pos.map(function(p){ return f1(p.x) + "," + f1(p.y); }).join(" ");
    var area = "M" + f1(ST.pos[0].x) + "," + f1(y0) + " L" + ST.pos.map(function(p){ return f1(p.x) + " " + f1(p.y); }).join(" L") + " L" + f1(ST.pos[n-1].x) + "," + f1(y0) + " Z";
    var th = mx * 0.03, ex = {}, iMx = 0, iMn = 0, i;
    vals.forEach(function(v,i2){ if(v > vals[iMx]) iMx = i2; if(v < vals[iMn]) iMn = i2; });
    ex[iMx] = 1; ex[iMn] = 1;
    for(i = 1; i < n - 1; i++){
      var a = vals[i-1], v2 = vals[i], b = vals[i+1];
      var pk = (v2 >= a && v2 > b) || (v2 > a && v2 >= b), vl = (v2 <= a && v2 < b) || (v2 < a && v2 <= b);
      if((pk || vl) && Math.abs(v2 - (a + b) / 2) > th) ex[i] = 1;
    }
    var dots = Object.keys(ex).map(function(k){ i = +k; return '<circle class="hl-dot" cx="' + f1(ST.pos[i].x) + '" cy="' + f1(ST.pos[i].y) + '" r="3.4"/>'; }).join("");
    var mp = ST.pos[iMx], label = "max " + fmtAx(vals[iMx]);
    var blen = label.length, bw = blen * 6.6 + 16, bh = 19;
    var bx = mp.x - bw / 2; bx = Math.max(P, Math.min(W - 8 - bw, bx));
    var by = mp.y - bh - 11; if(by < P - 8) by = mp.y + 11;
    var badge = '<rect class="hl-badge" x="' + f1(bx) + '" y="' + f1(by) + '" width="' + f1(bw) + '" height="' + bh + '" rx="9"/>'
      + '<text class="hl-badge-t" x="' + f1(bx + 8) + '" y="' + f1(by + 13) + '">' + escAx(label) + "</text>";
    var hov = "";
    if(ST.hover >= 0 && ST.pos[ST.hover]){
      var hp = ST.pos[ST.hover];
      hov = '<rect class="hl-hovband" x="' + f1(hp.x - slot / 2) + '" y="' + P + '" width="' + f1(slot) + '" height="' + ih + '"/>'
        + '<line class="hl-hovline" x1="' + f1(hp.x) + '" y1="' + P + '" x2="' + f1(hp.x) + '" y2="' + f1(y0) + '"/>'
        + '<circle class="hl-hovdot" cx="' + f1(hp.x) + '" cy="' + f1(hp.y) + '" r="5"/>';
    }
    var xl = "";
    d.forEach(function(pp,i2){
      if(i2 % 5 === 0 || i2 === n - 1){
        var lbl = String(pp.day || "").slice(5);
        var tx = (i2 === n - 1) ? (W - 8 - lbl.length * 6) : (ST.pos[i2].x - lbl.length * 3);
        xl += '<text class="hl-x" x="' + f1(tx) + '" y="' + (H - 6) + '">' + escAx(lbl) + "</text>";
      }
    });
    var gid = "hl-grad";
    var defs = '<defs><linearGradient id="' + gid + '" x1="0" y1="0" x2="0" y2="1">'
      + '<stop offset="0" style="stop-color:var(--chart,#e0684a)" stop-opacity=".30"/>'
      + '<stop offset=".66" style="stop-color:var(--chart,#e0684a)" stop-opacity=".07"/>'
      + '<stop offset="1" style="stop-color:var(--chart,#e0684a)" stop-opacity=".02"/></linearGradient></defs>';
    svg.setAttribute("viewBox", "0 0 " + W + " " + H);
    svg.innerHTML = defs + grid + yl
      + '<path class="hl-area" d="' + area + '"/>'
      + '<polyline class="hl-line" points="' + line + '"/>'
      + dots + badge + hov + xl;
    if(lg){
      var lh = opts.legend || ("<span>токены/день · ломаная точка-в-точку</span>"
        + "<span>точка = пик/впадина · бейдж = max-день</span>");
      if(lg._h !== lh){ lg._h = lh; lg.innerHTML = lh; }
    }
  }
  function boxTip(){ return (box && box.parentElement && box.parentElement.querySelector("#usage-tip")) || document.getElementById("home-tip"); }
  function showTip(p){
    var tip = boxTip(); if(!tip) return;
    if(opts.tip){ tip.innerHTML = opts.tip(p); return; }
    var pi = p.prompt || 0, co = p.completion || 0, tv = pi + co;
    var pw = tv ? Math.round(100 * pi / tv) : 0, cw = tv ? 100 - pw : 0;
    tip.innerHTML = '<div class="tt-day">' + escAx(p.day) + '</div><div class="tt-tot">' + fmtAx(tv) + ' ток./день</div>'
      + '<div class="tt-row"><span class="k">вход</span><span class="v">' + fmtAx(pi) + '</span></div><div class="tt-bar"><i class="tt-in" style="width:' + pw + '%"></i></div>'
      + '<div class="tt-row"><span class="k">выход</span><span class="v">' + fmtAx(co) + '</span></div><div class="tt-bar"><i class="tt-out" style="width:' + cw + '%"></i></div>';
  }
  box._w08.pts = pts; box._w08.draw = draw; box._w08.showTip = showTip; box._w08.tipEl = boxTip;
  if(!box._w08hover){
    box._w08hover = 1;
    box.addEventListener("mousemove", function(e){
      var cur = box._w08; if(!cur) return;
      var r = box.getBoundingClientRect(), mx2 = e.clientX - r.left;
      var best = 0, bd = 1e9;
      (cur.pos || []).forEach(function(pp,i2){ var dd = Math.abs(pp.x - mx2); if(dd < bd){ bd = dd; best = i2; } });
      if(cur.hover !== best){ cur.hover = best; cur.draw(); }
      cur.showTip(cur.pts[best]);
      var tip = cur.tipEl ? cur.tipEl() : document.getElementById("home-tip");
      if(tip){
        tip.style.display = "block";
        tip.style.position = "fixed";
        tip.style.left = Math.min(e.clientX + 14, window.innerWidth - 252) + "px";
        tip.style.top = (e.clientY + 12) + "px";
        tip.style.zIndex = 60;
      }
    });
    box.addEventListener("mouseleave", function(){ box._w08.hover = -1; var tip = boxTip(); if(tip) tip.style.display = "none"; var cur = box._w08; if(cur) cur.draw(); });
    window.addEventListener("resize", function(){ var cur = box._w08; if(cur) cur.draw(); });
    try {
      var ro = new ResizeObserver(function(){ var cur = box._w08; if(cur) cur.draw(); });
      ro.observe(box.parentElement || box);
      box._w08ro = ro;
    } catch(eRO){}
  }
  try { var _hq = new URLSearchParams(location.search).get("hover"); if(_hq !== null && _hq !== ""){ ST.hover = Math.max(0, Math.min(pts.length - 1, parseInt(_hq, 10) || 0)); } } catch(eHQ){}
  draw();
  if(ST.hover >= 0 && pts[ST.hover]){
    showTip(pts[ST.hover]);
    try {
      var tip2 = boxTip();
      if(tip2 && ST.pos[ST.hover]){
        var cr2 = box.getBoundingClientRect();
        tip2.style.display = "block";
        tip2.style.position = "fixed";
        tip2.style.left = Math.min(cr2.left + ST.pos[ST.hover].x + 14, window.innerWidth - 252) + "px";
        tip2.style.top = (cr2.top + 60) + "px";
        tip2.style.zIndex = 60;
      }
    } catch(eTP){}
  }
}
