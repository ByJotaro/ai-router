/* =====================================================================
   w09 · ИСПОЛЬЗОВАНИЕ — КОЛОДА ДНЕЙ
   Главный объект: крупная карточка ОДНОГО дня (дата, объём, запросы,
   стоимость, доля бюджета, сравнение с предыдущим днём). Перелистывание:
   ‹ ›, ← →, Home/End, полоска 30 дней (она же 30-дневная динамика),
   список-лента. Переключатель метрики — токены / запросы / стоимость.
   Второй вид — календарь 12 месяцев (выбор периода), клик по дню
   возвращает в колоду на этот день.
   ===================================================================== */
"use strict";

/* ---------- данные ---------- */
function w09UsageDays() {
  var se = (S.daily && S.daily.series) || [];
  if (se.length) return se;
  var tr = (S.analytics && S.analytics.daily_trend) || [];
  return tr.map(function (p) {
    return { day: p.date || p.day, requests: p.requests, prompt: p.prompt, completion: p.completion, cache_read: 0, cache_write: 0, reasoning: 0, cost_usd: p.cost_usd };
  });
}
function w09UsageCount() {
  var n = w09UsageDays().length;
  return S.usageView === "cal" ? 0 : n;
}
function w09UMode() { return (S.usageMode === "reqs" || S.usageMode === "cost") ? S.usageMode : "tokens"; }
function w09UVal(d, mode) {
  mode = mode || w09UMode();
  if (mode === "reqs") return iN(d && d.requests);
  if (mode === "cost") return iN(d && d.cost_usd);
  return iN(d && d.prompt) + iN(d && d.completion);
}
function w09UFmt(v, mode) {
  mode = mode || w09UMode();
  return mode === "cost" ? moneyC(v) : (mode === "reqs" ? nf(v) : w09Tok(v));
}
function w09UName(mode) {
  mode = mode || w09UMode();
  return mode === "cost" ? "Стоимость" : (mode === "reqs" ? "Запросы" : "Токены");
}
function w09UUnit(mode) { mode = mode || w09UMode(); return mode === "cost" ? "доля бюджета" : (mode === "reqs" ? "запросов/день" : "токенов/день"); }
function w09UsageByDay() {
  var m = {};
  w09UsageDays().forEach(function (d) { if (d && d.day) m[String(d.day).slice(0, 10)] = d; });
  return m;
}

/* ---------- загрузка ---------- */
function loadUsage() {
  if (S.usageView === "cal") { renderUsageCalendar($("#usage-cal")); }
  if (DEMO) { renderUsage(); drawUsageChart(); return Promise.resolve(true); }
  var _d = DEMO;
  var seq = (S._w09USEQ = (S._w09USEQ || 0) + 1);
  showUsageSkeleton();
  return Promise.all([
    Promise.resolve(API.Usage()).catch(function () { return null; }),
    Promise.resolve(API.DailyUsage(30)).catch(function () { return null; })
  ]).then(function (r) {
    if (DEMO !== _d || seq !== S._w09USEQ) { hideUsageSkeleton(); return; }
    if (r[0]) S.usage = r[0];
    if (r[1]) S.daily = r[1];
    hideUsageSkeleton();
    if (!S.usage && !(S.daily && S.daily.series && S.daily.series.length)) { w09UsageEmpty("запустите ядро или сделайте первый запрос через шлюз"); return; }
    renderUsage(); drawUsageChart();
    // годовой ряд — фоном: календарь рисуется на все 12 месяцев
    try {
      Promise.resolve(API.UsageAnalyticsLight(365)).then(function (a) {
        if (DEMO !== _d || seq !== S._w09USEQ) return;
        if (a && a.daily_trend && a.daily_trend.length) {
          S.analytics = a;
          if (S.usageView === "cal") renderUsageCalendar($("#usage-cal")); else renderUsage();
        }
      }).catch(function () { });
    } catch (e) { }
  }).catch(function (e) {
    hideUsageSkeleton();
    if (DEMO !== _d) return;
    w09UsageEmpty(null, e);
  });
}
function w09UsageEmpty(hint, e) {
  var card = $("#usage-card"), strip = $("#usage-strip"), list = $("#usage-list");
  if (strip) strip.innerHTML = "";
  if (list) list.innerHTML = "";
  if (card) card.innerHTML = e ? errHint(e) : emptyHint(hint);
  w09SyncIdx("usage", 0, 0);
}

/* ---------- рендер: KPI → карточка дня → полоска → лента ---------- */
function renderUsage(host) {
  var days = w09UsageDays();
  var mode = w09UMode();
  if (S.usageView !== "cal") w09UsageKPIs(days);
  w09SyncSegs();
  var scope = host || $("#usage-deck");
  var card = (scope && scope.querySelector) ? scope.querySelector("#usage-card") : $("#usage-card");
  var strip = $("#usage-strip"), list = $("#usage-list");
  if (!days.length) {
    if (S.usageView !== "cal") w09UsageEmpty("запустите ядро или сделайте первый запрос через шлюз");
    return;
  }
  if (!W09.init.usage) { W09.i.usage = days.length - 1; W09.init.usage = 1; }
  if (S.calSel && S.calSel.day) {
    var found = -1;
    days.forEach(function (d, i) { if (String(d.day).slice(0, 10) === String(S.calSel.day)) found = i; });
    if (found >= 0) W09.i.usage = found;
    S.calSel = null;
  }
  W09.i.usage = w09Clamp(W09.i.usage, days.length);
  var i = W09.i.usage;
  if (card) card.innerHTML = w09UsageCard(days[i], days[i - 1], mode, i, days);
  if (strip) strip.innerHTML = w09UsageStrip(days, mode, i);
  if (list) list.innerHTML = w09UsageList(days, mode, i);
  w09SyncIdx("usage", i, days.length);
  var r = list && list.querySelector(".w09-lrow.on");
  if (r && r.scrollIntoView) { try { r.scrollIntoView({ block: "nearest" }); } catch (e) { } }
  var mm = $("#usage-meta");
  if (mm && DEMO && days.length) {
    var p0 = w09DayParts(days[0].day), p1 = w09DayParts(days[i].day);
    w09PanelMeta("usage", days.length + " дн · " + p0.dd + " → " + w09DayParts(days[days.length - 1].day).dd + " · " + w09UName(mode).toLowerCase() + " · выбран " + p1.dd, "30 дней");
  }
  /* метрику/вид переключает канон через renderUsage(), поэтому график перерисовываем здесь же,
     но только когда сменилась метрика, вид или длина серии — иначе лишняя перерисовка на каждой стрелке */
  var sig = mode + "|" + S.usageView + "|" + days.length;
  if (S.usageView !== "cal" && W09.chartSig !== sig) drawUsageChart();
  if (S.usageView === "cal") renderUsageCalendar($("#usage-cal"));
}

/* KPI итогов: 4 плитки, деньги в центах → $ */
function w09UsageKPIs(days) {
  var kp = $("#usage-kpis");
  if (!kp) return;
  var N = days.length, totR = 0, totP = 0, totC = 0, totCost = 0, totCache = 0;
  days.forEach(function (d) {
    totR += iN(d.requests); totP += iN(d.prompt); totC += iN(d.completion);
    totCost += iN(d.cost_usd); totCache += iN(d.cache_read) + iN(d.cache_write);
  });
  var u = S.usage || {};
  var budget = iN(u.budget_usd);
  var share = budget > 0 ? totCost / budget : 0;
  var logs = S.logs || [];
  var okN = 0; logs.forEach(function (l) { if (l.status === "ok") okN++; });
  var okPct = logs.length ? okN / logs.length : 0;
  var h = '<div class="kpi"><div class="label">Запросы · ' + N + ' дн</div><div class="val">' + nf(totR) + '</div>'
    + '<div class="sub">в среднем ' + nf(Math.round(totR / Math.max(1, N))) + '/день</div></div>'
    + '<div class="kpi"><div class="label">Токены · ' + N + ' дн</div><div class="val">' + w09Tok(totP + totC) + '</div>'
    + '<div class="sub">вход ' + w09Tok(totP) + ' · выход ' + w09Tok(totC) + ' · кэш ' + w09Tok(totCache) + '</div></div>'
    + '<div class="kpi"><div class="label">Стоимость · ' + N + ' дн</div><div class="val">' + moneyC(totCost) + '</div>'
    + '<div class="sub">' + (budget > 0 ? 'бюджет ' + pct(share) + ' из ' + moneyC(budget) : 'бюджет не задан')
    + (budget > 0 ? w09Prog(share, share > .9 ? "danger" : "") : "") + '</div></div>'
    + '<div class="kpi"><div class="label">Ключи · лента</div><div class="val">' + nf(iN(u.keys)) + ' / ' + nf(iN(u.revoked)) + '</div>'
    + '<div class="sub">ключей / отозвано · ok ' + pct(okPct) + ' в окне ' + logs.length + '</div></div>';
  if (kp._h !== h) { kp._h = h; kp.innerHTML = h; }
}

/* карточка одного дня: крупно всё, что есть по дню */
function w09UsageCard(d, prev, mode, i, days) {
  var p = w09DayParts(d.day);
  var cur = w09UVal(d, mode), was = w09UVal(prev, mode);
  var dl = w09DeltaTxt(cur, was, function (v) { return w09UFmt(v, mode); });
  var u = S.usage || {}, budget = iN(u.budget_usd);
  var cost = iN(d.cost_usd);
  var dayShare = budget > 0 ? cost / budget : 0;
  var sum = 0, sumCost = 0;
  days.forEach(function (x) { sum += w09UVal(x, mode); sumCost += iN(x.cost_usd); });
  var winShare = sum > 0 ? cur / sum : 0;
  var cache = iN(d.cache_read) + iN(d.cache_write);
  var tot = iN(d.prompt) + iN(d.completion) + cache;
  var winCost = budget > 0 ? sumCost / budget : 0;
  var tone = "ok";
  var side = '<span class="w09-delta ' + dl.cls + '">' + esc(dl.txt) + '</span>'
    + '<span class="w09-hint">' + w09Sdot(dl.cls === "up" ? "warn" : (dl.cls === "down" ? "ok" : "")) + ' день ' + (i + 1) + ' из ' + days.length + '</span>';
  var grid = w09Fld("Запросы", nf(d.requests), "", null)
    + w09Fld("Вход", w09Tok(d.prompt), "", nf(d.prompt) + " ток.")
    + w09Fld("Выход", w09Tok(d.completion), "", nf(d.completion) + " ток.")
    + w09Fld("Кэш-чтение", w09Tok(d.cache_read), "", cache ? "экономия входа" : "кэш не использовался")
    + w09Fld("Кэш-запись", w09Tok(d.cache_write), "", null)
    + w09Fld("Reasoning", w09Tok(d.reasoning), "", "токены рассуждений")
    + w09Fld("Стоимость", moneyC(cost), (budget > 0 && dayShare > .1 ? "w09-err" : "w09-dim"), budget > 0 ? pct(dayShare) + " бюджета" : null)
    + w09Fld("Доля периода", pct(winShare), "w09-dim", "от объёма выбранных " + days.length + " дней");
  var budgetBlock = budget > 0
    ? '<div class="w09-brow"><span>бюджет · ' + days.length + ' дн</span><span class="w09-track"><i' + (winCost > .9 ? ' class="soft"' : '') + ' style="width:' + Math.max(0, Math.min(100, Math.round(100 * winCost))) + '%"></i></span><span class="v">' + moneyC(sumCost) + ' / ' + moneyC(budget) + '</span></div>'
    : "";
  return '<div class="w09-card" data-w09-tone="' + tone + '">'
    + '<div class="w09-card-top">'
    + '<div class="w09-date">день ' + String(i + 1) + ' · <b>' + esc(p.dd) + '</b>' + esc(p.wd) + ' ' + esc(p.y) + '</div>'
    + w09Lead(w09UName(mode) + " · день", w09UFmt(cur, mode), '<span class="w09-hint">' + esc(w09UUnit(mode)) + ' · ' + nf(iN(d.requests)) + ' запр.</span>', side)
    + '</div>'
    + '<div class="w09-grid">' + grid + '</div>'
    + w09Bars([
      ["вход", iN(d.prompt), w09Tok(d.prompt), ""],
      ["выход", iN(d.completion), w09Tok(d.completion), "alt"],
      ["кэш", cache, w09Tok(cache), "soft"]
    ])
    + '<div class="w09-bars">' + budgetBlock
    + '<div class="w09-brow"><span>состав дня</span><span class="w09-track"><i style="width:' + Math.max(0, Math.min(100, tot ? Math.round(100 * (iN(d.prompt) + iN(d.completion)) / tot) : 0)) + '%"></i></span><span class="v">' + w09Tok(d.prompt + d.completion + cache) + '</span></div>'
    + '</div>'
    + '<div class="w09-badge-row">'
    + '<span class="badge ' + (dl.cls === "up" ? "warn" : (dl.cls === "down" ? "on" : "off")) + '">' + esc(dl.txt) + '</span>'
    + '<span class="badge off">' + esc(mode === "cost" ? "цена дня " + moneyC(cur) : w09UName(mode).toLowerCase() + " " + w09UFmt(cur, mode)) + '</span>'
    + '<span class="badge off">' + (cache ? "кэш " + w09Tok(cache) : "без кэша") + '</span>'
    + '</div></div>';
}

/* полоска дней: она же 30-дневная динамика выбранной метрики */
function w09UsageStrip(days, mode, cur) {
  var vals = days.map(function (d) { return w09UVal(d, mode); });
  var titles = days.map(function (d, k) {
    var p = w09DayParts(d.day);
    return p.dd + " · " + w09UFmt(vals[k], mode) + " · " + nf(d.requests) + " запр." + (d.cost_usd ? " · " + moneyC(d.cost_usd) : "");
  });
  var mx = 0; vals.forEach(function (v) { mx = Math.max(mx, v); });
  var cls = vals.map(function (v) { return (mx && v >= mx * .75) ? "w09-hot" : ""; });
  var p0 = w09DayParts(days[0].day), p1 = w09DayParts(days[days.length - 1].day);
  return w09StripTicks(vals, titles, cls)
    + '<div class="w09-strip-cap"><span>' + esc(p0.dd) + '</span><span>' + esc(w09UName(mode).toLowerCase()) + ' по дням · клик — перейти</span><span>' + esc(p1.dd) + '</span></div>';
}

/* лента дней: быстрый переход, новые сверху */
function w09UsageList(days, mode, cur) {
  var out = [];
  for (var k = days.length - 1; k >= 0; k--) {
    var d = days[k], p = w09DayParts(d.day);
    var hot = (d.cost_usd && iN(d.cost_usd) > 0);
    out.push('<button class="w09-lrow' + (k === cur ? " on" : "") + '" data-w09="jump" data-w09-i="' + k + '" title="день ' + esc(p.dd) + '">'
      + '<span class="a">' + esc(p.dd) + '</span>'
      + '<span class="b">' + esc(p.wd) + ' · ' + nf(d.requests) + ' запр. · ' + moneyC(d.cost_usd) + '</span>'
      + '<span class="c">' + esc(w09UFmt(w09UVal(d, mode), mode)) + '</span></button>');
  }
  return out.join("");
}

/* ---------- график 30 дней (SVG-канон drawEmberLine) ---------- */
function drawUsageChart() {
  var box = $("#usage-chart-box");
  if (!box || !box.isConnected) return;
  if (S.usageView === "cal") return;   // календарь — график скрыт, рисовать нечего
  var days = w09UsageDays();
  var pts = days.slice(-30);
  if (!pts.length) return;
  var mode = w09UMode();
  W09.chartSig = mode + "|" + S.usageView + "|" + days.length;
  var cap = $("#usage-chart-cap");
  if (cap) {
    var h = esc(w09UName(mode)) + ' · 30 дней <i>' + esc(mode === "cost" ? "стоимость" : (mode === "reqs" ? "число запросов" : "вход + выход")) + '</i>';
    if (cap._h !== h) { cap._h = h; cap.innerHTML = h; }
  }
  var rng = $("#usage-chart-range");
  if (rng) { var r = w09DayParts(pts[0].day).dd + " → " + w09DayParts(pts[pts.length - 1].day).dd; if (rng.textContent !== r) rng.textContent = r; }
  var note = $("#usage-chart-note");
  if (note) { var nt = "выбран " + w09DayParts(days[W09.i.usage] ? days[W09.i.usage].day : pts[pts.length - 1].day).dd + " · клик по дню в колоде"; if (note.textContent !== nt) note.textContent = nt; }
  drawEmberLine(box, $("#usage-legend"), pts, {
    value: function (p) { return w09UVal(p, mode); },
    legend: "<span>" + esc(w09UName(mode).toLowerCase()) + " по дням · ломаная точка-в-точку</span><span>точка = пик/впадина · бейдж = max-день</span>",
    tip: function (p) {
      var pi = iN(p.prompt), co = iN(p.completion), cr = iN(p.cache_read), cw = iN(p.cache_write);
      var tv = pi + co, pw = tv ? Math.round(100 * pi / tv) : 0, cw2 = tv ? 100 - pw : 0;
      return '<div class="tt-day">' + esc(p.day) + '</div>'
        + '<div class="tt-tot">' + w09UFmt(w09UVal(p, mode), mode) + ' · ' + nf(p.requests) + ' запр.</div>'
        + '<div class="tt-row"><span class="k">вход</span><span class="v">' + w09Tok(pi) + '</span></div><div class="tt-bar"><i class="tt-in" style="width:' + pw + '%"></i></div>'
        + '<div class="tt-row"><span class="k">выход</span><span class="v">' + w09Tok(co) + '</span></div><div class="tt-bar"><i class="tt-out" style="width:' + cw2 + '%"></i></div>'
        + '<div class="tt-cache">кэш ч ' + w09Tok(cr) + ' · з ' + w09Tok(cw) + ' · ' + moneyC(p.cost_usd) + '</div>';
    }
  });
}

/* ---------- календарь 12 месяцев: выбор периода ---------- */
function renderUsageCalendar(host) {
  host = host || $("#usage-cal");
  if (!host || !host.isConnected) return;
  var map = w09UsageByDay();
  var off = Math.max(0, Math.min(240, iN(S.calOff)));
  var t = new Date(); t.setHours(12, 0, 0, 0);
  var end = new Date(t.getFullYear(), t.getMonth() - off, 1);
  var months = [];
  for (var k = 11; k >= 0; k--) months.push(new Date(end.getFullYear(), end.getMonth() - k, 1));
  var today = w09Iso(t);
  var curIso = String((w09UsageDays()[W09.i.usage] || {}).day || "").slice(0, 10);
  var vmax = 1, vmap = {};
  Object.keys(map).forEach(function (iso) {
    var d = map[iso], v = iN(d.prompt) + iN(d.completion);
    vmap[iso] = v; if (v > vmax) vmax = v;
  });
  var lvl = function (v) { if (!v) return 0; var r = v / vmax; return r > .75 ? 5 : r > .5 ? 4 : r > .25 ? 3 : r > .05 ? 2 : 1; };
  var WD = ["пн", "вт", "ср", "чт", "пт", "сб", "вс"];
  var mos = months.map(function (m) {
    var y = m.getFullYear(), mo = m.getMonth();
    var first = new Date(y, mo, 1), last = new Date(y, mo + 1, 0);
    var cells = '';
    var pad = (first.getDay() + 6) % 7;
    for (var p = 0; p < pad; p++) cells += '<span class="w09-day w09-pad"></span>';
    for (var dd = 1; dd <= last.getDate(); dd++) {
      var iso = w09Iso(new Date(y, mo, dd));
      var d = map[iso], v = vmap[iso] || 0, L = lvl(v);
      var title = dd + " " + W09_MO[mo] + " " + y + (d ? " · " + w09Tok(v) + " ток. · " + nf(d.requests) + " запр. · " + moneyC(d.cost_usd) : " · нет трафика");
      cells += '<button class="w09-day' + (L ? " w09-l" + L : "") + (iso === curIso ? " w09-sel" : "") + '" data-w09="calday" data-day="' + iso + '"'
        + ' title="' + esc(title) + '" aria-label="' + esc(title) + '">' + dd + '</button>';
    }
    return '<div class="w09-mo"><div class="w09-mo-h">' + W09_MO[mo] + ' ' + y + '</div>'
      + '<div class="w09-mo-g">' + WD.map(function (w) { return '<span class="w09-mo-wd">' + w[0] + '</span>'; }).join("") + '</div>'
      + '<div class="w09-mo-g">' + cells + '</div></div>';
  }).join("");
  var cap = off === 0 ? "текущие 12 месяцев" : "сдвиг " + off + " мес. назад";
  var bar = '<div class="w09-calbar">'
    + '<button class="w09-nav" data-w09="cal-prev" title="12 месяцев назад">‹</button>'
    + '<button class="w09-nav" data-w09="cal-next" title="12 месяцев вперёд"' + (off === 0 ? " disabled" : "") + '>›</button>'
    + '<button class="btn sm ghost" data-w09="cal-now" title="Вернуться к текущему периоду">текущий</button>'
    + '<span class="w09-calcap">период <b>' + esc(W09_MO[months[0].getMonth()]) + ' ' + months[0].getFullYear() + ' → ' + esc(W09_MO[months[11].getMonth()]) + ' ' + months[11].getFullYear() + '</b></span>'
    + '<span class="w09-sp"></span><span class="w09-hint">' + esc(cap) + ' · клик по дню — выбрать его в колоде</span></div>';
  var foot = '<div class="w09-calfoot"><span>меньше</span>'
    + '<i class="w09-l1" style="background:var(--g50-heat-0)"></i><i class="w09-l2" style="background:var(--g50-heat-1)"></i><i class="w09-l3" style="background:var(--g50-heat-2)"></i><i class="w09-l4" style="background:var(--g50-heat-3)"></i><i class="w09-l5" style="background:var(--g50-heat-4)"></i>'
    + '<span>больше</span><span class="w09-hint">цвет — токены дня · янтарная рамка — день, выбранный в колоде</span></div>';
  host.innerHTML = bar + '<div class="w09-mos">' + mos + '</div>' + foot;
}
function w09CalVis(isCal) {
  var deck = $("#usage-deck"), cal = $("#usage-cal"), kp = $("#usage-kpis"), box = $("#usage-chart-box"), leg = $("#usage-legend"), mseg = $("#usage-seg");
  if (deck) deck.style.display = isCal ? "none" : "";
  if (cal) cal.style.display = isCal ? "" : "none";
  if (box) box.style.display = isCal ? "none" : "";
  if (leg) leg.style.display = isCal ? "none" : "";
  if (mseg) mseg.style.display = isCal ? "none" : "";
  if (kp && isCal) kp.innerHTML = "";
  if (cal && isCal) renderUsageCalendar(cal);
  w09SegSync("#usage-view-seg", function (v) { return v === (isCal ? "cal" : "trend"); });
}

/* ---------- привязки ---------- */
function wireCal() {
  if (window._w09CalWired) return;
  window._w09CalWired = 1;
  var seg = $("#usage-view-seg");
  if (seg) {
    $$("#usage-view-seg button").forEach(function (b) {
      b.addEventListener("click", function () {
        S.usageView = b.dataset.v === "cal" ? "cal" : "trend";
        w09CalVis(S.usageView === "cal");
        if (S.usageView === "cal") { S.calOff = 0; S.calSel = null; }
        renderUsage();
        if (S.usageView !== "cal") drawUsageChart();
      });
    });
  }
  var cal = $("#usage-cal");
  if (cal && cal.setAttribute && !cal.getAttribute("data-w09-deck")) cal.setAttribute("data-w09-deck", "usage");
}
/* действия календаря/колоды дней */
W09.act.usage = function (act, t) {
  if (act === "cal-prev") { S.calOff = iN(S.calOff) + 12; renderUsageCalendar($("#usage-cal")); return true; }
  if (act === "cal-next") { S.calOff = Math.max(0, iN(S.calOff) - 12); renderUsageCalendar($("#usage-cal")); return true; }
  if (act === "cal-now") { S.calOff = 0; S.calSel = null; renderUsageCalendar($("#usage-cal")); return true; }
  if (act === "calday") {
    var iso = t.getAttribute("data-day") || "";
    var days = w09UsageDays(), found = -1;
    days.forEach(function (d, i) { if (String(d.day).slice(0, 10) === iso) found = i; });
    if (found < 0) { toast("Нет данных за " + w09DayParts(iso).dd + " — в выборке " + days.length + " дней", "warn"); return true; }
    S.usageView = "trend"; S.calSel = null; W09.init.usage = 1; W09.i.usage = found;
    w09CalVis(false);
    renderUsage(); drawUsageChart();
    toast("День " + w09DayParts(iso).dd + " выбран в колоде", "ok");
    return true;
  }
  return false;
};

/* ---------- скелетоны ---------- */
function showUsageSkeleton() {
  var deck = $("#usage-deck");
  if (!deck || $("#usage-sk")) return;
  deck.setAttribute("data-w09-sk", "1");
  deck.insertAdjacentHTML("afterbegin", w09SkeletonRows(5));
}
function hideUsageSkeleton() {
  var deck = $("#usage-deck");
  if (!deck) return;
  var sk = $("#usage-sk");
  if (sk && sk.parentNode) sk.parentNode.removeChild(sk);
  deck.removeAttribute("data-w09-sk");
}
/* алиас для redrawThemed в core.js (home-ветка): считает календарь в переданный хост */
function renderActivityHeatmap(host) {
  if (host && host.isConnected) renderUsageCalendar(host);
}
function w09SyncSegs() {
  w09SegSync("#usage-seg", function (v) { return v === w09UMode(); });
  w09SegSync("#usage-view-seg", function (v) { return v === (S.usageView === "cal" ? "cal" : "trend"); });
}
