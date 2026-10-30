/* ==================================================================
   w01 «ПОЛОТНО» · вкладка «Журналы»
   Главный объект — лента плотности событий по времени: колонки, стек по
   статусам (ok / медленные / ошибки) + огибающая линия во всю ширину.
   Точное число событий — строкой над лентой; фильтр по статусу меняет форму
   ленты; поиск, экспорт и очистка работают (их зовёт замороженный shell.js).
   ================================================================== */
"use strict";

var W01L = { rows: [], rib: [], focus: -1, hover: -1, pausedUntil: 0, slowMs: 900, wired: false, W: 0, t0: 0, t1: 0, buckets: 0 };

/* ---------- источник данных ветки ---------- */
function w01LogKind() { return S.logKind === "proxies" ? "proxies" : "request"; }

/* Уровень: ok → info, медленный/3xx → warn, ошибка/4xx+ → error. */
function w01LogLevel(l) {
  var st = String(l.status == null ? "" : l.status).toLowerCase();
  var http = +l.http_status || 0;
  if ((st && st !== "ok") || http >= 400) return "error";
  if (http >= 300 || (+l.duration_ms || 0) >= W01L.slowMs) return "warn";
  return "info";
}
function w01ProxyLevel(l) { return l.ok ? "info" : "error"; }
function w01LogLvOn(lv) { var m = S.logLv || { info: true, warn: true, error: true }; return m[lv] !== false; }

/* Порог «медленного» — p90 окна, но не ниже 0.7 с: строка не врёт про «медленно». */
function w01LogSlow(rows) {
  var d = W01.nums(rows, function (l) { return +l.duration_ms || 0; }).filter(function (v) { return v > 0; });
  W01L.slowMs = Math.max(700, Math.round(W01.quant(d, 0.9)));
}

function w01LogMatch(l, q) {
  if (!q) return true;
  var hay = [l.model, l.requested_model, l.provider, l.account, l.error, l.status, l.http_status, l.id, l.proxy, l.action, l.method, l.path].join(" ").toLowerCase();
  return hay.indexOf(q) >= 0;
}

/* Полный список ветки, прошедший поиск и чипы уровня (без фильтра окна). */
function w01LogView() {
  var kind = w01LogKind();
  var q = String(S.logQuery || "").trim().toLowerCase();
  var src = kind === "proxies" ? (S.plogs || []) : (S.logs || []);
  if (kind !== "proxies") w01LogSlow(src);
  var out = [];
  for (var i = 0; i < src.length; i++) {
    var l = src[i] || {};
    var lv = kind === "proxies" ? w01ProxyLevel(l) : w01LogLevel(l);
    if (!w01LogLvOn(lv)) continue;
    if (!w01LogMatch(l, q)) continue;
    var t = W01.parseTs(l.ts);
    out.push({ l: l, lv: lv, t: t ? t.getTime() : 0, ts: l.ts });
  }
  out.sort(function (a, b) { return b.t - a.t; });
  return out;
}

/* ---------- лента плотности ---------- */
function w01LogRibbon(host, view) {
  var box = host || document.getElementById("log-table");
  var W = Math.max(320, Math.round((box && box.clientWidth) || 900));
  var H = 168, P = 44, R = 14, T = 16, B = 30;
  var iw = Math.max(60, W - P - R), ih = Math.max(48, H - T - B);
  W01L.W = W;
  var n = 0, i;
  var times = [];
  for (i = 0; i < view.length; i++) if (view[i].t) times.push(view[i].t);
  times.sort(function (a, b) { return a - b; });
  var t0 = times.length ? times[0] : 0, t1 = times.length ? times[times.length - 1] : 0;
  if (view.length && t1 === t0) t1 = t0 + 60000;
  W01L.t0 = t0; W01L.t1 = t1;
  var buckets = W01.clamp(Math.round(iw / 16), 12, 64);
  W01L.buckets = buckets;
  var cols = [];
  for (i = 0; i < buckets; i++) cols.push({ ok: 0, warn: 0, err: 0, t: buckets > 1 ? t0 + (t1 - t0) * i / buckets : t0 });
  var span = Math.max(1, t1 - t0);
  for (i = 0; i < view.length; i++) {
    var v = view[i];
    if (!v.t) continue;
    var bi = buckets > 1 ? Math.floor((v.t - t0) / span * buckets) : 0;
    bi = W01.clamp(bi, 0, buckets - 1);
    if (v.lv === "error") cols[bi].err++; else if (v.lv === "warn") cols[bi].warn++; else cols[bi].ok++;
  }
  var totals = cols.map(function (c) { return c.ok + c.warn + c.err; });
  var mx = W01.niceMax(Math.max.apply(null, totals.concat([1])));
  var slot = iw / buckets;
  var y0 = T + ih;
  var Y = function (v) { return y0 - ih * (v / mx); };
  W01L.rib = cols;

  var grid = "", yl = "";
  for (var g = 0; g <= 4; g++) {
    var gy = T + ih * g / 4;
    grid += W01.el("line", "w01-grid", { x1: P, y1: W01.f1(gy), x2: W - R, y2: W01.f1(gy) });
    yl += '<text class="w01-ax" x="' + (P - 8) + '" y="' + W01.f1(gy + 3) + '" text-anchor="end">' + esc(String(Math.round(mx * (1 - g / 4)))) + "</text>";
  }
  var bars = "", env = [];
  for (i = 0; i < buckets; i++) {
    var c = cols[i], x = P + slot * i + slot * 0.16, bw = Math.max(1.6, slot * 0.68);
    var yOk = Y(c.ok);
    var yW = Y(c.ok + c.warn);
    var yE = Y(c.ok + c.warn + c.err);
    var minH = (c.ok || c.warn || c.err) ? 2 : 0;
    var mk = function (yb, yt, cls) {
      var hh = yb - yt;
      if (hh > 0 && hh < minH) hh = minH;
      if (hh < 0) hh = 0;
      if (hh <= 0) return "";
      return W01.el("rect", "w01-seg " + cls, { x: W01.f1(x), y: W01.f1(yb - hh), width: W01.f1(bw), height: W01.f1(hh), rx: 1.5 });
    };
    bars += mk(y0, yOk, "w01-seg-ok") + mk(yOk, yW, "w01-seg-warn") + mk(yW, yE, "w01-seg-err");
    env.push({ x: P + slot * i + slot / 2, y: Y(totals[i]) });
  }
  var dots = "";
  for (i = 0; i < buckets; i++) if (totals[i] > 0 && totals[i] === mx) dots += W01.el("circle", "w01-env-dot", { cx: W01.f1(env[i].x), cy: W01.f1(env[i].y), r: 2.6 });
  var hits = "";
  for (i = 0; i < buckets; i++) {
    hits += '<rect class="w01-hit' + (W01L.focus === i ? " w01-hit-on" : "") + '" data-b="' + i + '" x="' + W01.f1(P + slot * i) + '" y="' + T + '" width="' + W01.f1(slot) + '" height="' + W01.f1(ih) + '"/>';
  }
  if (W01L.focus >= 0) {
    hits += W01.el("rect", "w01-hit-focus", { x: W01.f1(P + slot * W01L.focus + 0.5), y: T - 2, width: W01.f1(slot - 1), height: W01.f1(ih + 4), rx: 2 });
  }
  if (W01L.hover >= 0) {
    hits += W01.el("line", "w01-hovline", { x1: W01.f1(P + slot * W01L.hover + slot / 2), y1: T, x2: W01.f1(P + slot * W01L.hover + slot / 2), y2: y0 });
  }
  var xl = "";
  var marks = Math.min(6, buckets);
  for (i = 0; i < marks; i++) {
    var ti = marks > 1 ? Math.round((buckets - 1) * i / (marks - 1)) : 0;
    var tt = t0 + span * (ti + 0.5) / buckets;
    var anc = ti === 0 ? "start" : (ti === buckets - 1 ? "end" : "middle");
    var tx = ti === 0 ? P : (ti === buckets - 1 ? W - R : P + slot * (ti + 0.5));
    xl += '<text class="w01-ax" x="' + W01.f1(tx) + '" y="' + (H - 10) + '" text-anchor="' + anc + '">' + esc(W01.hhmm(new Date(tt))) + "</text>";
  }
  var unit = '<text class="w01-unit" x="' + (W - R) + '" y="' + (T - 4) + '" text-anchor="end">событий / ' + esc(w01BucketSpan(t0, t1, buckets)) + "</text>";
  var empty = view.length ? "" : '<text class="w01-rib-empty" x="' + W01.f1(P + iw / 2) + '" y="' + W01.f1(T + ih / 2) + '" text-anchor="middle">событий нет — измените поиск или фильтр</text>';
  var svgId = "w01-rib-svg";
  var html = '<svg id="' + svgId + '" class="w01-rib" viewBox="0 0 ' + W + " " + H + '" width="100%" height="' + H + '" role="img" aria-label="Лента плотности событий по времени">' +
    grid + yl + unit + bars + dots + hits + xl + empty + "</svg>";
  return html;
}

function w01BucketSpan(t0, t1, buckets) {
  var d = Math.max(1, (t1 - t0) / Math.max(1, buckets)) / 1000;
  if (d < 90) return Math.max(1, Math.round(d)) + " с";
  if (d < 5400) return Math.round(d / 60) + " мин";
  return (d / 3600).toFixed(1).replace(".", ",") + " ч";
}

/* Русская форма числительного: 1 запрос / 2 запроса / 5 запросов. */
function w01Plural(n, one, few, many) {
  var a = Math.abs(n) % 100, b = a % 10;
  if (a > 10 && a < 20) return many;
  if (b > 1 && b < 5) return few;
  if (b === 1) return one;
  return many;
}

/* ---------- статус-полоса: доли HTTP-классов + строка точных чисел ---------- */
function w01LogStatus(view, kind) {
  var n = view.length, i, l;
  var cls = { ok: 0, rate: 0, srv: 0, other: 0 };
  var lat = [], tok = 0, okN = 0, failN = 0;
  for (i = 0; i < n; i++) {
    l = (view[i] && view[i].l) || {};
    if (kind === "proxies") {
      if (l.ok) okN++; else failN++;
      if (+l.latency_ms > 0) lat.push(+l.latency_ms);
    } else {
      var h = +l.http_status || 0;
      if (!h || h < 400) cls.ok++;
      else if (h === 429) cls.rate++;
      else if (h >= 500) cls.srv++;
      else cls.other++;
      if (+l.duration_ms > 0) lat.push(+l.duration_ms);
      tok += (+l.prompt || 0) + (+l.completion || 0);
    }
  }
  var seg = "", leg = "";
  function part(k, v, label, title) {
    if (!v) return;
    seg += '<i class="' + k + '" style="flex:' + v + '"></i>';
    leg += '<span class="' + k + '" title="' + W01.attr(title) + '"><s></s>' + nf(v) + " × " + label + "</span>";
  }
  if (kind === "proxies") {
    part("ok", okN, "ok", "события egress без отказа");
    part("srv", failN, "отказ", "события egress с отказом");
  } else {
    part("ok", cls.ok, "2xx", "успешные ответы (HTTP < 400)");
    part("rate", cls.rate, "429", "upstream rate-limited");
    part("srv", cls.srv, "5xx", "ошибки апстрима");
    part("other", cls.other, "4xx", "прочие клиентские ошибки");
  }
  var nums = "";
  function num(v, label, title, bad) {
    nums += '<span class="n" title="' + W01.attr(title) + '"><b' + (bad ? ' class="bad"' : "") + ">" + v + "</b><i>" + label + "</i></span>";
  }
  num(nf(n), w01Plural(n, kind === "proxies" ? "событие" : "запрос",
    kind === "proxies" ? "события" : "запроса",
    kind === "proxies" ? "событий" : "запросов"),
    "ровно столько записей отдал поиск и фильтр уровня");
  if (kind === "proxies") {
    num(lat.length ? W01.ms(W01.quant(lat, 0.5)) : "—", "медиана", "латентность egress, p50");
    num(lat.length ? W01.ms(W01.quant(lat, 0.9)) : "—", "p90", "хвост латентности egress");
  } else {
    num(lat.length ? W01.ms(W01.quant(lat, 0.5)) : "—", "p50", "медиана длительности");
    num(lat.length ? W01.ms(W01.quant(lat, 0.95)) : "—", "p95", "хвост длительности");
    num(W01.toks(tok), "токенов", "prompt + completion загруженных записей");
  }
  var row = '<div class="w01-sb-row"><span class="w01-sb-leg">' + (leg || '<span title="под фильтром нет записей">пусто</span>') +
    '</span><span class="w01-sb-nums">' + nums + "</span></div>";
  if (!n) return row;
  return '<div class="w01-sb-bar" role="img" aria-label="доли статусов выборки">' + seg + "</div>" + row;
}

/* Подпись ленты справа — интервал событий, как диапазон дат у графика главной. */
function w01LogRange() {
  if (!W01L.t0) return "";
  var a = new Date(W01L.t0), b = new Date(W01L.t1);
  var pad = function (n) { return n < 10 ? "0" + n : "" + n; };
  var day = function (d) { return pad(d.getDate()) + "." + pad(d.getMonth() + 1); };
  var hm = function (d) { return W01.hhmm(d); };
  if (a.toDateString() === b.toDateString()) return day(a) + " · " + hm(a) + " → " + hm(b);
  return day(a) + " " + hm(a) + " → " + day(b) + " " + hm(b);
}

/* ---------- точные строки журнала ---------- */
function w01LogRowsHTML(view, kind, host) {
  var shown = view, focused = null;
  if (W01L.focus >= 0 && W01L.rib[W01L.focus]) {
    var slot = (W01L.t1 - W01L.t0) / Math.max(1, W01L.buckets);
    var a = W01L.t0 + slot * W01L.focus, b = a + slot;
    focused = { a: a, b: b };
    shown = view.filter(function (v) { return v.t >= a && v.t < b; });
  }
  var cap = 200, cutN = Math.max(0, shown.length - cap);
  var rows = shown.slice(0, cap);
  var head = '<thead><tr><th class="w01-th-t">время</th><th>статус</th><th>' + (kind === "proxies" ? "прокси" : "модель") + "</th><th>" +
    (kind === "proxies" ? "действие" : "провайдер") + '</th><th class="w01-numc">' + (kind === "proxies" ? "мс" : "длит.") + '</th><th class="w01-numc">' +
    (kind === "proxies" ? "ok" : "токены") + '</th><th class="w01-numc">' + (kind === "proxies" ? "код" : "http") + "</th></tr></thead>";
  var body = rows.map(function (v) {
    var l = v.l;
    if (kind === "proxies") {
      return '<tr class="w01-row w01-r-' + v.lv + '" data-id="' + W01.attr(l.id) + '" data-kind="proxies" tabindex="0" title="Открыть событие egress">' +
        '<td class="w01-th-t">' + esc(W01.hhmmss(l.ts)) + "</td>" +
        '<td><span class="w01-lvl w01-lvl-' + v.lv + '">' + (l.ok ? "ok" : "отказ") + "</span></td>" +
        '<td class="w01-mono">' + esc(W01.nn(l.proxy)) + "</td>" +
        '<td class="w01-mono">' + esc(W01.nn(l.action)) + "</td>" +
        '<td class="w01-numc">' + esc(l.latency_ms ? nf(l.latency_ms) : "—") + "</td>" +
        '<td class="w01-numc">' + (l.ok ? "да" : "нет") + "</td>" +
        '<td class="w01-numc">' + esc(W01.cut(l.error || "", 22) || "—") + "</td></tr>";
    }
    return '<tr class="w01-row w01-r-' + v.lv + '" data-id="' + W01.attr(l.id) + '" data-kind="request" tabindex="0" title="Открыть запись журнала">' +
      '<td class="w01-th-t">' + esc(W01.hhmmss(l.ts)) + "</td>" +
      '<td><span class="w01-lvl w01-lvl-' + v.lv + '">' + (v.lv === "error" ? "ошибка" : (v.lv === "warn" ? "медленно" : "ok")) + "</span></td>" +
      '<td class="w01-mono">' + esc(W01.nn(l.model)) + "</td>" +
      '<td class="w01-mono">' + esc(W01.nn(l.provider)) + "</td>" +
      '<td class="w01-numc">' + esc(nf(l.duration_ms)) + "</td>" +
      '<td class="w01-numc">' + esc(W01.toks((+l.prompt || 0) + (+l.completion || 0))) + "</td>" +
      '<td class="w01-numc">' + esc(W01.nn(l.http_status)) + (l.error ? ' <i class="w01-errmark" title="' + W01.attr(l.error) + '">!</i>' : "") + "</td></tr>";
  }).join("");
  var foot = '<tfoot><tr><td colspan="7" class="w01-foot">' +
    "показано " + nf(rows.length) + " из " + nf(shown.length) + (cutN ? " (обрезано на " + nf(cutN) + ")" : "") +
    (focused ? " · окно " + esc(W01.hhmmss(new Date(focused.a))) + "–" + esc(W01.hhmmss(new Date(focused.b))) : "") +
    " · клик по строке — карточка записи</td></tr></tfoot>";
  return '<table class="w01-tbl">' + head + "<tbody>" + body + "</tbody>" + foot + "</table>";
}

/* ---------- рендеры веток ---------- */
function renderLogs(host) {
  var h = host || document.getElementById("log-table");
  if (!h) return;
  var kind = w01LogKind();
  if (kind === "proxies") { renderPlogs(h); return; }
  if (DEMO) { W01.setTxt("logs-meta", "демо-стенд · лента плотности по S.logs: " + (S.logs || []).length + " записей"); W01.setTxt("logs-live-txt", "● ДЕМО"); }
  var view = w01LogView();
  if (!DEMO) {
    W01.setTxt("logs-meta", "запросов в выборке: " + nf(view.length) + (S.logQuery ? " · поиск «" + S.logQuery + "»" : "") + " · источник S.logs");
    W01.setTxt("logs-live-txt", "● LIVE");
  }
  W01L.rows = view;
  var rib = w01LogRibbon(h, view);
  h.innerHTML = '<div class="sec-t w01-ribcap"><span id="w01-log-kind-t"></span><span class="spacer"></span><span class="w01-ribrange" id="w01-log-range"></span></div>' +
    '<div class="w01-ribbox">' + rib + "</div>" + w01LogRowsHTML(view, "request");
  renderLogStats(view, "request");
  w01LogKindChrome(kind);
}

function renderPlogs(host) {
  var h = host || document.getElementById("log-table");
  if (!h) return;
  if (DEMO) { W01.setTxt("logs-meta", "демо-стенд · лента событий egress по S.plogs: " + (S.plogs || []).length + " записей"); W01.setTxt("logs-live-txt", "● ДЕМО"); }
  var view = w01LogView();
  W01L.rows = view;
  var rib = w01LogRibbon(h, view);
  h.innerHTML = '<div class="sec-t w01-ribcap"><span id="w01-log-kind-t"></span><span class="spacer"></span><span class="w01-ribrange" id="w01-log-range"></span></div>' +
    '<div class="w01-ribbox">' + rib + "</div>" + w01LogRowsHTML(view, "proxies");
  renderLogStats(view, "proxies");
  w01LogKindChrome("proxies");
}

/* Строка точных чисел + подпись ленты (её ждёт и замороженный код: renderLogStats()). */
function renderLogStats(view, kind) {
  var v = view || W01L.rows || [];
  var k = kind || w01LogKind();
  W01.setHTML("w01-log-stats", w01LogStatus(v, k));
  W01.setTxt("w01-log-range", w01LogRange());
  var note = document.getElementById("w01-log-note");
  if (note) {
    var lvTxt = [];
    var m = S.logLv || {};
    if (m.info === false) lvTxt.push("без ok");
    if (m.warn === false) lvTxt.push("без медленных");
    if (m.error === false) lvTxt.push("без ошибок");
    note.textContent = "колонка = интервал " + w01BucketSpan(W01L.t0 || 0, W01L.t1 || 0, W01L.buckets || 1) +
      " · стек снизу вверх: ok → медленные → ошибки · клик по колонке сужает список" +
      (lvTxt.length ? " · фильтр уровня: " + lvTxt.join(", ") : "") +
      (S.logQuery ? " · поиск: «" + S.logQuery + "»" : "");
  }
  var chip = document.getElementById("w01-log-focus");
  if (chip) {
    if (W01L.focus >= 0 && W01L.rib[W01L.focus]) {
      var slot = (W01L.t1 - W01L.t0) / Math.max(1, W01L.buckets);
      chip.hidden = false;
      chip.textContent = "окно " + W01.hhmmss(new Date(W01L.t0 + slot * W01L.focus)) + "–" + W01.hhmmss(new Date(W01L.t0 + slot * (W01L.focus + 1))) + " ✕";
    } else { chip.hidden = true; chip.textContent = ""; }
  }
}

/* Счётчики уровней для чипов: поиск учтён, фильтр уровня — нет,
   иначе выключенный чип обнулял бы сам себя и терял масштаб. */
function w01LogCounts() {
  var kind = w01LogKind();
  var q = String(S.logQuery || "").trim().toLowerCase();
  var src = kind === "proxies" ? (S.plogs || []) : (S.logs || []);
  var c = { info: 0, warn: 0, error: 0 };
  for (var i = 0; i < src.length; i++) {
    var l = src[i] || {};
    if (!w01LogMatch(l, q)) continue;
    c[kind === "proxies" ? w01ProxyLevel(l) : w01LogLevel(l)]++;
  }
  return c;
}

/* Кнопка «×» внутри поиска: показывается только когда есть что чистить. */
function w01LogSearchSync() {
  var i = document.getElementById("log-search"), x = document.getElementById("log-search-x");
  if (x) x.hidden = !(i && i.value);
}

function w01LogKindChrome(kind) {
  w01LogSearchSync();
  var lk = document.getElementById("log-search");
  if (lk) {
    lk.placeholder = kind === "proxies" ? "прокси, действие, ошибка…" : "модель, провайдер, rid…";
    lk.title = kind === "proxies" ? "Фильтр по загруженным событиям egress: прокси, действие, ошибка" : "Поиск по модели, провайдеру, ошибке, статусу и HTTP-коду";
  }
  var t = document.getElementById("w01-log-kind-t");
  if (t) t.textContent = kind === "proxies" ? "лента egress-событий" : "лента запросов";
  var cnt = w01LogCounts(), lvs = ["info", "warn", "error"];
  for (var i = 0; i < lvs.length; i++) {
    var b = document.querySelector('#log-lvchips [data-lvc="' + lvs[i] + '"]');
    if (b) b.textContent = nf(cnt[lvs[i]]);
    var chip = document.querySelector('#log-lvchips .chip[data-lv="' + lvs[i] + '"]');
    // у egress-ветки warn-записей не бывает: чип не показываем, чтобы не врал нулём
    if (chip && lvs[i] === "warn") chip.hidden = kind === "proxies";
  }
}

/* ---------- тултип и клик по колонкам ленты ----------
   Слушатели живут на #log-table (стабильный хост), а не на .w01-ribbox:
   лента перерисовывается на каждое изменение наведения и убила бы свои же слушатели. */
function w01RibbonTip(el, c, i, cx, cy) {
  var slot = (W01L.t1 - W01L.t0) / Math.max(1, W01L.buckets);
  var a = new Date(W01L.t0 + slot * i), z = new Date(W01L.t0 + slot * (i + 1));
  var tot = c.ok + c.warn + c.err;
  W01.tip(el, '<div class="w01-tip-d">' + esc(W01.hhmmss(a)) + "–" + esc(W01.hhmmss(z)) + "</div>" +
    '<div class="w01-tip-t">' + esc(nf(tot)) + " событий</div>" +
    '<div class="w01-tip-r"><span class="w01-tip-k">ok</span><span class="w01-tip-v">' + esc(nf(c.ok)) + "</span></div>" +
    '<div class="w01-tip-r"><span class="w01-tip-k">медленных</span><span class="w01-tip-v">' + esc(nf(c.warn)) + "</span></div>" +
    '<div class="w01-tip-r"><span class="w01-tip-k">ошибок</span><span class="w01-tip-v">' + esc(nf(c.err)) + "</span></div>" +
    '<div class="w01-tip-b">клик — показать это окно в списке</div>', cx, cy);
}

/* ---------- загрузка ---------- */
async function loadLogs(opts) {
  var kind = w01LogKind();
  var force = !!(opts && opts.force);
  if (DEMO) { renderLogs(); return; }
  var h = document.getElementById("log-table");
  if (h && !h.firstChild) h.innerHTML = '<div class="sk-rows"><div class="sk-row"><div class="skeleton" style="width:120px"></div><div class="skeleton" style="width:40%"></div><div class="skeleton" style="width:60px"></div></div><div class="sk-row"><div class="skeleton" style="width:120px"></div><div class="skeleton" style="width:52%"></div><div class="skeleton" style="width:60px"></div></div><div class="sk-row"><div class="skeleton" style="width:120px"></div><div class="skeleton" style="width:33%"></div><div class="skeleton" style="width:60px"></div></div></div>';
  var snapshot = DEMO;
  var q = String(S.logQuery || "").trim();
  try {
    if (kind === "proxies") {
      var pl = await API.ProxiesLogs(200);
      if (DEMO !== snapshot) return;
      S.plogs = pl || [];
    } else if (q.length >= 3) {
      var rq = await API.LogsByQuery(q, 200);
      if (DEMO !== snapshot) return;
      S.logs = rq || [];
    } else {
      var rl = await API.Logs(200);
      if (DEMO !== snapshot) return;
      S.logs = rl || [];
    }
    renderLogs();
  } catch (e) {
    if (DEMO !== snapshot) return;
    if (h) h.innerHTML = errHint(e);
  }
}

/* ---------- экспорт / очистка / пауза чтения ---------- */
function w01Csv(rows) {
  return "\ufeff" + rows.map(function (r) {
    return r.map(function (c) { return '"' + String(c == null ? "" : c).replace(/"/g, '""') + '"'; }).join(";");
  }).join("\r\n");
}
function w01SaveFile(name, text) {
  try {
    var blob = new Blob([text], { type: "text/csv;charset=utf-8" });
    var url = URL.createObjectURL(blob);
    var a = document.createElement("a");
    a.href = url; a.download = name;
    document.body.appendChild(a); a.click();
    setTimeout(function () { try { URL.revokeObjectURL(url); a.remove(); } catch (e) {} }, 400);
    return true;
  } catch (e) { return false; }
}
function w01Stamp() {
  var d = new Date();
  return W01.iso(d).replace(/-/g, "") + "-" + W01.p2(d.getHours()) + W01.p2(d.getMinutes());
}

async function exportLogs(btn) {
  var kind = w01LogKind();
  var view = w01LogView();
  if (!view.length) { toast("Экспорт: выборка пуста — измените поиск или фильтр", "warn"); return; }
  if (!DEMO) {
    if (btn && btn.disabled) return;
    if (btn) btn.disabled = true;
    try {
      var r = await API.ExportLogs(kind === "proxies" ? "proxies" : "requests", "");
      toast("Экспорт ядра: " + (typeof r === "string" && r ? r : "готово"), "ok");
    } catch (e) { toast("Экспорт не удался: " + ((e && e.message) || e), "err"); }
    finally { if (btn) btn.disabled = false; }
    return;
  }
  var rows = [["ts", "status", "http", "model", "provider", "duration_ms", "prompt", "completion", "cache_read", "cache_write", "error"]];
  var i;
  if (kind === "proxies") {
    rows = [["ts", "proxy", "action", "ok", "latency_ms", "error"]];
    for (i = 0; i < view.length; i++) { var p = view[i].l; rows.push([p.ts, p.proxy, p.action, p.ok ? "1" : "0", p.latency_ms, p.error || ""]); }
  } else {
    for (i = 0; i < view.length; i++) {
      var l = view[i].l;
      rows.push([l.ts, l.status, l.http_status, l.model, l.provider, l.duration_ms, l.prompt, l.completion, l.cache_read, l.cache_write, l.error || ""]);
    }
  }
  var name = "ai-router-" + (kind === "proxies" ? "egress" : "logs") + "-" + w01Stamp() + ".csv";
  var ok = w01SaveFile(name, w01Csv(rows));
  toast(ok ? "Демо-экспорт: " + view.length + " строк → " + name : "Браузер запретил сохранение файла", ok ? "ok" : "err");
}

function clearLogs(btn) {
  var kind = w01LogKind();
  var n = ((kind === "proxies" ? S.plogs : S.logs) || []).length;
  var dlg = modal(kind === "proxies" ? "Очистить события egress?" : "Очистить журнал запросов?", '<div><div class="w01-mdrow"><span class="w01-mdk">записей в окне</span><code>' + n + "</code></div>" +
    '<div class="w01-mdrow"><span class="w01-mdk">ветка</span><code>' + esc(kind === "proxies" ? "egress / прокси" : "запросы") + "</code></div>" +
    '<div class="hint">Очистка необратима, ключи и провайдеры не затрагиваются.</div></div>',
    { ok: "Очистить", onOk: async function (o) {
      if (DEMO) {
        if (kind === "proxies") S.plogs = []; else S.logs = [];
        o.close(); W01L.focus = -1; renderLogs();
        toast("Демо: окно журнала очищено (в ядре ничего не менялось)", "ok");
        return false;
      }
      try {
        await API.ClearLogs(kind === "proxies" ? "proxies" : "requests");
        o.close();
        if (kind === "proxies") S.plogs = []; else S.logs = [];
        W01L.focus = -1;
        loadLogs({ force: true });
        toast("Журнал очищен", "ok");
      } catch (e) { toast("Не очищено: " + ((e && e.message) || e), "err"); }
      return false;
    } });
  try { var c = dlg && dlg.el ? dlg.el.querySelector("#md-cancel") : null; if (c) c.focus(); } catch (e) {}
  if (btn) btn.disabled = false;
}

/* Читает журнал руками — автообновление вкладки не срывает позицию. */
function logsUserReading() { return W01L.pausedUntil > Date.now(); }
function setLogsPaused(on) {
  W01L.pausedUntil = on ? (Date.now() + 20000) : 0;
  var chip = document.getElementById("logs-paused");
  if (chip) {
    chip.hidden = !on;
    chip.textContent = on ? "⏸ пауза чтения — автообновление ждёт" : "";
    chip.title = on ? "Журнал листают: автообновление отложено на 20 с · клик — снять паузу" : "";
  }
}

/* Переход из «Главной»/«Провайдеров» в журнал с запросом. */
function gotoLogsQuery(q) {
  S.logQuery = String(q || "");
  S.logKind = "request";
  var inp = document.getElementById("log-search");
  if (inp) inp.value = S.logQuery;
  if (typeof window.show === "function") window.show("logs");
}

/* ---------- карточка записи ---------- */
function w01LogModal(id, kind) {
  var list = kind === "proxies" ? (S.plogs || []) : (S.logs || []);
  var l = null, i;
  for (i = 0; i < list.length; i++) if (String(list[i].id) === String(id)) { l = list[i]; break; }
  if (!l) { toast("Запись не найдена в загруженном окне", "warn"); return; }
  var row = function (k, v) { return '<div class="w01-mdrow"><span class="w01-mdk">' + esc(k) + "</span><code>" + esc(v == null || v === "" ? "—" : v) + "</code></div>"; };
  var body;
  if (kind === "proxies") {
    body = row("время", l.ts) + row("прокси", l.proxy) + row("действие", l.action) + row("результат", l.ok ? "ok" : "отказ") +
      row("задержка", l.latency_ms ? nf(l.latency_ms) + " мс" : "—") + row("ошибка", l.error);
  } else {
    body = row("время", l.ts) + row("статус", l.status) + row("HTTP", l.http_status) + row("модель", l.model) +
      row("запрошена", l.requested_model) + row("провайдер", l.provider) + row("аккаунт", l.account) +
      row("длительность", nf(l.duration_ms) + " мс") +
      row("токены", "вход " + nf(l.prompt) + " · выход " + nf(l.completion) + " · кэш-ч " + nf(l.cache_read) + " · кэш-з " + nf(l.cache_write)) +
      row("ошибка", l.error) +
      (l.path ? row("путь", (l.method || "POST") + " " + l.path) : "");
  }
  modal(kind === "proxies" ? "Событие egress #" + id : "Запись журнала #" + id, '<div class="w01-md">' + body + "</div>");
}

/* ---------- своя обвязка (замороженный shell.js держит поиск, чипы, кнопки) ---------- */
function w01LogsWire() {
  var h = document.getElementById("log-table");
  if (!h || h._w01wired) return;
  h._w01wired = true;
  h.addEventListener("click", function (e) {
    var tr = e.target && e.target.closest ? e.target.closest(".w01-row") : null;
    if (!tr) return;
    w01LogModal(tr.getAttribute("data-id"), tr.getAttribute("data-kind"));
  });
  h.addEventListener("keydown", function (e) {
    if (e.key !== "Enter" && e.key !== " ") return;
    var tr = e.target && e.target.closest ? e.target.closest(".w01-row") : null;
    if (!tr) return;
    e.preventDefault();
    w01LogModal(tr.getAttribute("data-id"), tr.getAttribute("data-kind"));
  });
  h.addEventListener("scroll", function () { setLogsPaused(true); }, { passive: true });
  h.addEventListener("mousemove", function (e) {
    var t = e.target, b = (t && t.getAttribute) ? t.getAttribute("data-b") : null;
    var tip = document.getElementById("w01-log-tip");
    if (b == null) {
      W01.tipHide(tip);
      if (W01L.hover !== -1) { W01L.hover = -1; renderLogs(); }
      return;
    }
    var i = +b, c = W01L.rib[i];
    if (!c) { W01.tipHide(tip); return; }
    if (W01L.hover !== i) { W01L.hover = i; renderLogs(); }
    w01RibbonTip(tip, c, i, e.clientX, e.clientY);
  });
  h.addEventListener("mouseleave", function () {
    W01.tipHide(document.getElementById("w01-log-tip"));
    if (W01L.hover !== -1) { W01L.hover = -1; renderLogs(); }
  });
  h.addEventListener("click", function (e) {
    var b = (e.target && e.target.getAttribute) ? e.target.getAttribute("data-b") : null;
    if (b == null) return;
    W01L.focus = (+b === W01L.focus) ? -1 : +b;
    setLogsPaused(true);
    renderLogs();
  });
  var chip = document.getElementById("logs-paused");
  if (chip && !chip._w01wired) { chip._w01wired = true; chip.addEventListener("click", function () { setLogsPaused(false); }); }
  var si = document.getElementById("log-search");
  var sx = document.getElementById("log-search-x");
  if (si && !si._w01x) {
    si._w01x = true;
    si.addEventListener("input", w01LogSearchSync);
    si.addEventListener("change", w01LogSearchSync);
  }
  if (sx && !sx._w01wired) {
    sx._w01wired = true;
    sx.addEventListener("click", function () {
      if (!si) return;
      si.value = "";
      S.logQuery = "";
      si.dispatchEvent(new Event("input", { bubbles: true }));
      si.focus();
      w01LogSearchSync();
    });
  }
  w01LogSearchSync();
  var fx = document.getElementById("w01-log-focus");
  if (fx && !fx._w01wired) { fx._w01wired = true; fx.addEventListener("click", function () { W01L.focus = -1; setLogsPaused(true); renderLogs(); }); }
  var ex = document.getElementById("log-export");
  if (ex && !ex._w01wired) { ex._w01wired = true; }        // клик уже навешен shell.js → exportLogs()
  var cl = document.getElementById("log-clear");
  if (cl && !cl._w01wired) { cl._w01wired = true; }        // клик уже навешен shell.js → clearLogs()
}

function w01LogsInit() {
  w01LogsWire();
  window.addEventListener("resize", W01.debounced(function () {
    try { if (!document.hidden && S.tab === "logs") renderLogs(); } catch (e) {}
  }, 200));
  try { if (S.tab === "logs" && DEMO) renderLogs(); } catch (e) {}
}
if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", w01LogsInit);
else w01LogsInit();
