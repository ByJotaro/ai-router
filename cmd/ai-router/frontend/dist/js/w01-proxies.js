/* ==================================================================
   w01 «ПОЛОТНО» · вкладка «Прокси»
   Главный объект — профиль задержек egress-пула во всю ширину: профиль
   каждого канала, непрерывная линия по живым и РАЗРЫВ на мёртвых,
   активный канал выделен. Ниже — полоса на аккаунт (спарклайн из окна
   egress-событий) и лента бэкендов WARP.
   ================================================================== */
"use strict";

var W01P = { W: 0, H: 232, hover: -1, back: -1, pin: -1, wired: false, max: 1, accs: [] };

function w01PxAccounts() { var p = S.proxy || {}; return (p.accounts || []).filter(Boolean); }
function w01PxBackends() { var w = (S.proxy && S.proxy.warp_pool) || {}; return (w.backends || []).filter(Boolean); }
function w01PxLat(a) { return +a.latency_ms || 0; }
function w01PxRate(v) { v = +v || 0; return v <= 1.5 ? Math.round(v * 100) : Math.round(v); }
function w01PxAct() { return String((S.proxy && S.proxy.active_account_id) || ""); }

/* Замеры канала — из загруженного окна egress-событий (API.ProxiesLogs -> S.plogs).
   Нет событий — на полосе остаётся одна текущая точка: ряд не выдумываем. */
function w01PxSamples(acc) {
  var out = [], pl = S.plogs || [], i;
  var id = String(acc.id == null ? "" : acc.id), ip = String(acc.virtual_ip == null ? "" : acc.virtual_ip);
  for (i = 0; i < pl.length; i++) {
    var e = pl[i] || {}, p = String(e.proxy == null ? "" : e.proxy);
    if (!p) continue;
    if (p === id || (id && id.indexOf(p) >= 0) || (ip && p.indexOf(ip) >= 0) || (id && p.indexOf(id) >= 0)) {
      var t = W01.parseTs(e.ts);
      out.push({ v: +e.latency_ms || 0, t: t ? t.getTime() : 0, ok: !!e.ok });
    }
  }
  out.sort(function (a, b) { return a.t - b.t; });
  return out;
}

/* ---------- строки чисел пула ---------- */
function w01PxNums() {
  var p = S.proxy || {}, items = [];
  if (p.egress_enabled != null) items.push([p.egress_enabled ? "вкл" : "выкл", "egress", p.egress_enabled ? "w01-okc" : "w01-bad", "S.proxy.egress_enabled"]);
  items.push([nf(p.total != null ? p.total : w01PxAccounts().length), "каналов", "w01-hi", "S.proxy.total"]);
  items.push([nf(p.alive != null ? p.alive : 0), "живых", "w01-okc", "S.proxy.alive"]);
  items.push([nf(p.dead != null ? p.dead : 0), "мёртвых", (+p.dead ? "w01-bad" : ""), "S.proxy.dead"]);
  if (p.connecting) items.push([nf(p.connecting), "подключается", "", "S.proxy.connecting"]);
  if (p.avg_latency_ms != null) items.push([W01.ms(p.avg_latency_ms), "средняя", "", "S.proxy.avg_latency_ms"]);
  if (p.connect_rate != null) items.push([w01PxRate(p.connect_rate) + "%", "успешных подключений", "", "S.proxy.connect_rate"]);
  if (p.failures_24h != null) items.push([nf(p.failures_24h), "сбоев за 24 ч", (+p.failures_24h ? "w01-warnc" : ""), "S.proxy.failures_24h"]);
  if (p.public_ip) items.push([esc(String(p.public_ip)), "публичный IP", "", "S.proxy.public_ip"]);
  if (p.degraded) items.push(["деградация", "состояние", "w01-bad", "S.proxy.degraded"]);
  return W01.numline(items);
}

function w01PxWarpNums() {
  var w = (S.proxy && S.proxy.warp_pool) || {}, items = [];
  if (!w || !(w.size || w.up || w.state)) return W01.numline([["нет данных", "warp-пул ядром не отдан", "", "S.proxy.warp_pool пуст"]]);
  if (w.state) items.push([esc(String(w.state)), "состояние", w.state === "ready" ? "w01-okc" : "w01-warnc", "S.proxy.warp_pool.state"]);
  if (w.size != null) items.push([nf(w.size), "бэкендов", "w01-hi", "S.proxy.warp_pool.size"]);
  if (w.up != null) items.push([nf(w.up), "up", "w01-okc", ""]);
  if (w.starting) items.push([nf(w.starting), "starting", "w01-warnc", ""]);
  if (w.down) items.push([nf(w.down), "down", "w01-bad", ""]);
  if (w.unique_egresses != null) items.push([nf(w.unique_egresses), "уникальных выходов", "", ""]);
  if (w.anycast_egresses) items.push([nf(w.anycast_egresses), "anycast", "", ""]);
  if (w.acquire_total != null) items.push([nf(w.acquire_ok) + "/" + nf(w.acquire_total), "acquire ok/всего", "", ""]);
  if (w.acquire_fail) items.push([nf(w.acquire_fail), "acquire fail", "w01-warnc", ""]);
  if (w.rotate_total != null) items.push([nf(w.rotate_total), "ротаций", "", ""]);
  if (w.restarts) items.push([nf(w.restarts), "рестартов", "w01-warnc", ""]);
  if (w.uptime_s != null) items.push([Math.floor(w.uptime_s / 3600) + "ч " + Math.floor((w.uptime_s % 3600) / 60) + "м", "аптайм пула", "", ""]);
  if (w.last_acquire_err) items.push([esc(W01.cut(w.last_acquire_err, 30)), "последняя ошибка acquire", "w01-bad", ""]);
  return W01.numline(items);
}
/* ---------- профиль задержек пула (главный объект вкладки) ---------- */
function w01PoolSVG(host, accs) {
  var W = Math.max(320, Math.round((host && host.clientWidth) || 900));
  var H = 232, P = 58, R = 18, T = 30, B = 54;
  var iw = Math.max(60, W - P - R), ih = Math.max(60, H - T - B);
  var n = accs.length;
  W01P.W = W; W01P.accs = accs;
  if (!n) return '<svg class="w01-pool" viewBox="0 0 ' + W + " " + H + '" width="100%" height="' + H + '" role="img" aria-label="Профиль задержек пула"></svg>';
  var aliveVals = [], i;
  for (i = 0; i < n; i++) if (accs[i].healthy !== false && w01PxLat(accs[i]) > 0) aliveVals.push(w01PxLat(accs[i]));
  var mx = W01.niceMax(Math.max.apply(null, aliveVals.concat([1])));
  W01P.max = mx;
  var slot = iw / n;
  var X = function (k) { return P + slot * (k + 0.5); };
  var Y = function (v) { return T + ih * (1 - W01.clamp(v / mx, 0, 1)); };
  var y0 = T + ih;
  var avg = (S.proxy && S.proxy.avg_latency_ms != null) ? +S.proxy.avg_latency_ms : (aliveVals.length ? W01.avg(aliveVals) : 0);

  var grid = "", yl = "";
  for (var g = 0; g <= 4; g++) {
    var gy = T + ih * g / 4;
    grid += W01.el("line", "w01-grid", { x1: P, y1: W01.f1(gy), x2: W - R, y2: W01.f1(gy) });
    yl += '<text class="w01-ax" x="' + (P - 8) + '" y="' + W01.f1(gy + 3) + '" text-anchor="end">' + esc(String(Math.round(mx * (1 - g / 4)))) + "</text>";
  }
  yl += '<text class="w01-unit" x="4" y="' + (T - 14) + '">мс</text>';

  var stems = "", dots = "", deads = "", labels = "", vals = "", segs = [], cur = [];
  var act = w01PxAct();
  for (i = 0; i < n; i++) {
    var a = accs[i], dead = a.healthy === false, v = w01PxLat(a), x = X(i), y = Y(v);
    var isAct = !!act && String(a.id) === act;
    labels += '<text class="w01-ax" x="' + W01.f1(x) + '" y="' + (H - 34) + '" text-anchor="middle">' + esc(W01.cut(W01.nn(a.country), 8)) + "</text>" +
      '<text class="w01-ax w01-ax-dim" x="' + W01.f1(x) + '" y="' + (H - 18) + '" text-anchor="middle">' + esc(W01.cut(String(a.id || "").replace(/^.*\//, ""), 9)) + "</text>";
    if (dead) {
      deads += W01.el("line", "w01-pdead", { x1: W01.f1(x), y1: y0, x2: W01.f1(x), y2: W01.f1(Y(avg || mx * 0.5)) }) +
        '<text class="w01-px" x="' + W01.f1(x) + '" y="' + W01.f1(y0 - 7) + '" text-anchor="middle">✕</text>';
      if (cur.length > 1) segs.push(cur);
      cur = [];
      continue;
    }
    stems += W01.el("line", "w01-stem" + (isAct ? " w01-stem-act" : ""), { x1: W01.f1(x), y1: y0, x2: W01.f1(x), y2: W01.f1(y) });
    dots += W01.el("circle", "w01-pdot" + (isAct ? " w01-pdot-act" : ""), { cx: W01.f1(x), cy: W01.f1(y), r: isAct ? 5 : 3.4 });
    vals += '<text class="w01-ax' + (isAct ? " w01-ax-act" : "") + '" x="' + W01.f1(x) + '" y="' + W01.f1(y - 10) + '" text-anchor="middle">' + esc(String(Math.round(v))) + "</text>";
    if (isAct) vals += '<text class="w01-ax-act" x="' + W01.f1(x) + '" y="' + W01.f1(y - 23) + '" text-anchor="middle">активен</text>';
    cur.push({ x: x, y: y });
  }
  if (cur.length > 1) segs.push(cur);
  var line = segs.map(function (s) { return '<polyline class="w01-pline" points="' + W01.poly(s) + '"/>'; }).join("");

  var meanY = Y(avg), mean = "";
  if (avg > 0) mean = W01.el("line", "w01-mean", { x1: P, y1: W01.f1(meanY), x2: W - R, y2: W01.f1(meanY) }) +
    '<text class="w01-mean-t" x="' + (W - R) + '" y="' + W01.f1(meanY - 6) + '" text-anchor="end">средняя пула ' + esc(W01.ms(avg)) + "</text>";

  var hits = "";
  for (i = 0; i < n; i++) hits += '<rect class="w01-phit" data-i="' + i + '" x="' + W01.f1(P + slot * i) + '" y="' + T + '" width="' + W01.f1(slot) + '" height="' + W01.f1(ih) + '"/>';
  if (W01P.hover >= 0 && W01P.hover < n) hits += W01.el("rect", "w01-hit-focus", { x: W01.f1(P + slot * W01P.hover + 0.5), y: T - 3, width: W01.f1(slot - 1), height: W01.f1(ih + 6), rx: 2 });

  return '<svg class="w01-pool" viewBox="0 0 ' + W + " " + H + '" width="100%" height="' + H + '" role="img" aria-label="Профиль задержек egress-пула: ' + n + ' каналов">' +
    grid + yl + mean + stems + deads + line + dots + vals + labels + hits + "</svg>";
}
/* ---------- полоса канала (спарклайн одного аккаунта) ---------- */
function w01ChanRow(a, i, act) {
  var dead = a.healthy === false, v = w01PxLat(a), smp = w01PxSamples(a);
  var isAct = !!act && String(a.id) === act;
  var cls = "w01-chan" + (dead ? " w01-chan-dead" : "") + (isAct ? " w01-chan-act" : "");
  var smpTxt = smp.length >= 2 ? nf(smp.length) + " замеров" : (smp.length === 1 ? "1 замер" : "нет замеров в окне");
  return '<div class="' + cls + '" data-i="' + i + '" title="' + W01.attr("канал " + W01.nn(a.id) + " · " + W01.nn(a.country) + " · " + W01.nn(a.virtual_ip)) + '">' +
    '<div class="w01-chan-h">' +
    '<span class="w01-chan-i">' + esc(String(a.id || "—")) + "</span>" +
    '<span class="w01-chan-c">' + esc(W01.nn(a.country)) + "</span>" +
    '<span class="w01-chan-ip">' + esc(W01.nn(a.virtual_ip)) + "</span>" +
    '<span class="spacer"></span>' +
    '<span class="w01-chan-smp">' + smpTxt + "</span>" +
    '<span class="w01-chan-v">' + (dead ? "мёртв" : W01.ms(v)) + "</span>" +
    '<span class="w01-chan-f">' + (a.fail_count ? nf(a.fail_count) + " сбоев" : "без сбоев") + "</span>" +
    (isAct ? '<span class="w01-chan-b">активен</span>' : "") +
    (dead ? '<span class="w01-chan-b w01-chan-b-dead">разрыв</span>' : "") +
    "</div>" +
    '<div class="w01-chan-spark"><svg class="w01-spark" data-i="' + i + '" aria-hidden="true"></svg></div>' +
    "</div>";
}

/* Спарклайн: >=2 замеров — линия по ним; 1/0 замеров — честная горизонталь
   текущего значения (ряд не выдумываем); мёртвый канал — обрыв линии. */
function w01SparkSVG(W, smp, v, max, dead) {
  var y = function (val) { return 22 - 18 * W01.clamp((val || 0) / (max || 1), 0, 1); };
  var base = W01.el("line", "w01-sp-base", { x1: 0, y1: 22, x2: W, y2: 22 });
  if (dead) {
    var cutX = Math.round(W * 0.55);
    return base + W01.el("line", "w01-sp-dead", { x1: 0, y1: W01.f1(y(v)), x2: cutX, y2: W01.f1(y(v)) }) +
      '<text class="w01-sp-x" x="' + (cutX + 10) + '" y="' + W01.f1(y(v) + 4) + '">✕ канал не отвечает — линия обрывается</text>';
  }
  if (smp && smp.length >= 2) {
    var t0 = smp[0].t, t1 = smp[smp.length - 1].t, span = t1 - t0;
    var pts = smp.map(function (s, k) {
      var x = span > 0 ? (s.t - t0) / span * (W - 4) + 2 : k / (smp.length - 1) * (W - 4) + 2;
      return { x: x, y: y(s.v) };
    });
    return base + '<polyline class="w01-sp-line" points="' + W01.poly(pts) + '"/>' +
      W01.el("circle", "w01-sp-dot", { cx: W01.f1(pts[pts.length - 1].x), cy: W01.f1(pts[pts.length - 1].y), r: 2.6 });
  }
  var ty = y(v), tx = Math.round(W * 0.86);
  return base + W01.el("line", "w01-sp-level", { x1: 0, y1: W01.f1(ty), x2: tx, y2: W01.f1(ty) }) +
    W01.el("line", "w01-sp-tick", { x1: tx, y1: W01.f1(ty), x2: W - 6, y2: W01.f1(ty) }) +
    W01.el("circle", "w01-sp-dot", { cx: W - 3, cy: W01.f1(ty), r: 2.6 });
}

/* ---------- лента бэкендов WARP ---------- */
function w01BackendStrip(host, backs) {
  var W = Math.max(320, Math.round((host && host.clientWidth) || 900));
  var H = 34, n = backs.length;
  if (!n) return '<svg class="w01-bstrip" viewBox="0 0 ' + W + " " + H + '" width="100%" height="' + H + '" role="img" aria-label="Бэкенды WARP"></svg>';
  var gap = 3, bw = Math.max(2, (W - gap * (n - 1)) / n), ticks = "", hits = "", i;
  for (i = 0; i < n; i++) {
    var st = String(backs[i].state || "");
    var cls = st === "up" ? "w01-b-up" : (st === "starting" ? "w01-b-start" : "w01-b-down");
    var x = i * (bw + gap);
    ticks += W01.el("rect", "w01-btick " + cls, { x: W01.f1(x), y: 6, width: W01.f1(Math.max(2, bw - 0.5)), height: 22, rx: 3 });
    hits += '<rect class="w01-phit w01-bhit" data-bk="' + i + '" x="' + W01.f1(x) + '" y="0" width="' + W01.f1(bw) + '" height="' + H + '"/>';
  }
  return '<svg class="w01-bstrip" viewBox="0 0 ' + W + " " + H + '" width="100%" height="' + H + '" role="img" aria-label="Бэкенды WARP: ' + n + ' штук">' + ticks + hits + "</svg>";
}
/* ---------- сборка вкладки ---------- */
function w01ProxyNote(accs, backs) {
  var smp = 0, i;
  for (i = 0; i < accs.length; i++) smp += w01PxSamples(accs[i]).length;
  var ev = (S.plogs || []).length;
  return "профиль — текущая задержка канала (мс), лента бэкендов — состояние каждого сокета WARP. " +
    "Полосы каналов: " + nf(smp) + " замер(ов) из окна egress-событий (" + nf(ev) + " событий в S.plogs)" +
    (backs.length ? "" : "; бэкенды ядром не отданы");
}

function w01ProxyFillSparks(accs, max) {
  var list = document.querySelectorAll(".w01-spark"), i;
  for (i = 0; i < list.length; i++) {
    var el = list[i], idx = +el.getAttribute("data-i");
    var a = accs[idx];
    if (!a) continue;
    var W = Math.max(120, Math.round(el.parentNode ? el.parentNode.clientWidth : 300));
    el.setAttribute("viewBox", "0 0 " + W + " 26");
    el.setAttribute("width", "100%");
    el.setAttribute("height", "26");
    el.innerHTML = w01SparkSVG(W, w01PxSamples(a), w01PxLat(a), max, a.healthy === false);
  }
}

function w01ProxyDraw() {
  var root = document.getElementById("w01-px");
  if (!root) return;
  var accs = w01PxAccounts(), backs = w01PxBackends();
  var hostPool = document.getElementById("w01-eg-pool"), hostChans = document.getElementById("w01-eg-chans");
  var hostStrip = document.getElementById("w01-eg-bstrip"), st = document.getElementById("w01-eg-state");
  W01.setHTML("w01-eg-nums", w01PxNums());
  W01.setHTML("w01-eg-note", w01ProxyNote(accs, backs));
  W01.setHTML("w01-eg-warpline", backs.length || (S.proxy && S.proxy.warp_pool) ? w01PxWarpNums() : "");
  W01.setHTML("w01-eg-bmeta", backs.length ? (nf(backs.filter(function (b) { return b.state === "up"; }).length) + " up · " +
    nf(backs.filter(function (b) { return b.state === "starting"; }).length) + " starting · " +
    nf(backs.filter(function (b) { return b.state && b.state !== "up" && b.state !== "starting"; }).length) + " down") : "нет бэкендов");
  var has = accs.length > 0;
  if (st) {
    st.innerHTML = has ? "" : emptyHint("ядро не отдало ни одного egress-канала: пул пуст или ещё поднимается");
    W01.setDisp(st, has ? "none" : "flex");
  }
  W01.setDisp(hostPool, has ? "" : "none");
  W01.setDisp(hostChans, has ? "" : "none");
  W01.setDisp(document.getElementById("w01-eg-poolbox"), has ? "" : "none");
  W01.setDisp(document.getElementById("w01-eg-chanbox"), has ? "" : "none");
  if (!has) { if (hostChans) hostChans.innerHTML = ""; return; }
  var max = 1, i;
  for (i = 0; i < accs.length; i++) if (accs[i].healthy !== false) max = Math.max(max, w01PxLat(accs[i]));
  W01P.max = W01.niceMax(max);
  var actId = (W01P.pin >= 0 && accs[W01P.pin]) ? String(accs[W01P.pin].id) : w01PxAct(), html = "";
  for (i = 0; i < accs.length; i++) html += w01ChanRow(accs[i], i, actId);
  if (hostPool) hostPool.innerHTML = w01PoolSVG(hostPool, accs);
  if (hostChans) { hostChans.innerHTML = html; w01ProxyFillSparks(accs, W01P.max); }
  if (hostStrip) hostStrip.innerHTML = w01BackendStrip(hostStrip, backs);
  W01.setHTML("w01-eg-evline", w01PxEvNums());
  var alive = 0, i2;
  for (i2 = 0; i2 < accs.length; i2++) if (accs[i2].healthy !== false) alive++;
  W01.setTxt("proxies-meta", (DEMO ? "демо-стенд · " : "") + "каналов " + nf(accs.length) + " · живых " + nf(alive) +
    " · мёртвых " + nf(accs.length - alive) + " · бэкендов WARP " + nf(backs.length) + " · источник S.proxy / S.plogs");
  W01.setTxt("proxies-live-txt", DEMO ? "● ДЕМО" : "● LIVE");
}
/* ---------- сводка окна egress-событий (S.plogs) ---------- */
function w01PxEvNums() {
  var pl = S.plogs || [], ok = 0, bad = 0, lat = [], last = null, i;
  for (i = 0; i < pl.length; i++) {
    var e = pl[i] || {};
    if (e.ok) ok++; else bad++;
    if (+e.latency_ms) lat.push(+e.latency_ms);
    var t = W01.parseTs(e.ts);
    if (t && (!last || t > last)) last = t;
  }
  var items = [[nf(pl.length), "событий в окне", "w01-hi", "S.plogs"]];
  items.push([nf(ok), "успешных", "w01-okc", ""]);
  items.push([nf(bad), "ошибок", bad ? "w01-bad" : "", ""]);
  if (lat.length) items.push([W01.ms(W01.avg(lat)), "средняя по событиям", "", ""]);
  if (last) items.push([W01.hhmmss(last), "последнее событие", "", ""]);
  return W01.numline(items);
}

/* ---------- тултипы ---------- */
function w01PxTip(el, a, cx, cy) {
  var smp = w01PxSamples(a), last = smp.length ? smp[smp.length - 1] : null;
  var dead = a.healthy === false;
  var h = '<div class="w01-tip-d">канал ' + esc(W01.nn(a.id)) + " · " + esc(W01.nn(a.country)) + "</div>" +
    '<div class="w01-tip-t">' + (dead ? "мёртв" : W01.ms(w01PxLat(a))) + (dead ? "" : " задержка") + "</div>" +
    '<div class="w01-tip-r"><span class="w01-tip-k">virtual ip</span><span class="w01-tip-v">' + esc(W01.nn(a.virtual_ip)) + "</span></div>" +
    '<div class="w01-tip-r"><span class="w01-tip-k">живой</span><span class="w01-tip-v">' + (dead ? "нет" : "да") + "</span></div>" +
    '<div class="w01-tip-r"><span class="w01-tip-k">сбоев</span><span class="w01-tip-v">' + esc(nf(a.fail_count || 0)) + "</span></div>" +
    '<div class="w01-tip-r"><span class="w01-tip-k">замеров</span><span class="w01-tip-v">' + esc(nf(smp.length)) + "</span></div>" +
    (last ? '<div class="w01-tip-r"><span class="w01-tip-k">последний</span><span class="w01-tip-v">' + (last.t ? esc(W01.hhmmss(new Date(last.t))) : "—") + " · " + esc(W01.ms(last.v)) + "</span></div>" : "") +
    '<div class="w01-tip-b">' + (dead ? "канал исключён из выдачи: линия профиля обрывается" : "клик — закрепить канал, повторный клик — снять") + "</div>";
  W01.tip(el, h, cx, cy);
}

function w01PxTipBack(el, b, cx, cy) {
  var st = String(b.state || "—");
  var h = '<div class="w01-tip-d">бэкенд ' + esc(W01.nn(b.id)) + "</div>" +
    '<div class="w01-tip-t">' + esc(st) + "</div>" +
    '<div class="w01-tip-r"><span class="w01-tip-k">публичный ip</span><span class="w01-tip-v">' + esc(W01.nn(b.public_ip)) + "</span></div>" +
    '<div class="w01-tip-r"><span class="w01-tip-k">страна</span><span class="w01-tip-v">' + esc(W01.nn(b.country)) + "</span></div>" +
    '<div class="w01-tip-r"><span class="w01-tip-k">socks</span><span class="w01-tip-v">' + esc(W01.nn(b.socks_addr)) + "</span></div>" +
    '<div class="w01-tip-r"><span class="w01-tip-k">http</span><span class="w01-tip-v">' + esc(W01.nn(b.http_addr)) + "</span></div>" +
    '<div class="w01-tip-r"><span class="w01-tip-k">запросов</span><span class="w01-tip-v">' + esc(nf(b.requests || 0)) + "</span></div>" +
    '<div class="w01-tip-r"><span class="w01-tip-k">ошибок</span><span class="w01-tip-v">' + esc(nf(b.errors || 0)) + " · сбоев " + esc(nf(b.fails || 0)) + "</span></div>";
  W01.tip(el, h, cx, cy);
}
/* ---------- события: наведение/закрепление (слушатели на стабильном #w01-px) ---------- */
function w01ProxyWire() {
  var root = document.getElementById("w01-px");
  if (!root || root._w01wired) return;
  root._w01wired = true;
  W01P.wired = true;
  var tip = document.getElementById("w01-eg-tip");
  var accs = function () { return w01PxAccounts(); };
  var backs = function () { return w01PxBackends(); };
  root.addEventListener("mousemove", function (e) {
    var t = e.target, hi = (t && t.getAttribute) ? t.getAttribute("data-i") : null;
    var bk = (t && t.getAttribute) ? t.getAttribute("data-bk") : null;
    if (hi == null) {
      var row = t && t.closest ? t.closest(".w01-chan") : null;
      if (row) hi = row.getAttribute("data-i");
    }
    if (bk != null) {
      var b = backs()[+bk];
      W01.tipHide(tip);
      if (b) w01PxTipBack(tip, b, e.clientX, e.clientY);
      return;
    }
    if (hi == null) {
      W01.tipHide(tip);
      if (W01P.hover !== -1) { W01P.hover = -1; w01ProxyDraw(); }
      return;
    }
    var i = +hi, a = accs()[i];
    if (!a) { W01.tipHide(tip); return; }
    if (W01P.hover !== i) { W01P.hover = i; w01ProxyDraw(); }
    w01PxTip(tip, a, e.clientX, e.clientY);
  });
  root.addEventListener("mouseleave", function () {
    W01.tipHide(tip);
    if (W01P.hover !== -1) { W01P.hover = -1; w01ProxyDraw(); }
  });
  root.addEventListener("click", function (e) {
    var t = e.target, hi = (t && t.getAttribute) ? t.getAttribute("data-i") : null;
    if (hi == null) {
      var row = t && t.closest ? t.closest(".w01-chan") : null;
      if (row) hi = row.getAttribute("data-i");
    }
    if (hi == null) return;
    var i = +hi;
    W01P.pin = (W01P.pin === i) ? -1 : i;
    var a = accs()[i];
    if (a) toast("канал " + W01.nn(a.id) + (W01P.pin === i ? " закреплён в профиле" : " снят с закрепления"));
    w01ProxyDraw();
  });
}

/* ---------- загрузка ---------- */
async function loadProxies() {
  if (!DEMO) {
    try {
      if (typeof API !== "undefined" && API && API.ProxyStatus) S.proxy = await API.ProxyStatus();
      if (typeof API !== "undefined" && API && API.ProxiesLogs) {
        var pl = await API.ProxiesLogs(200);
        if (pl) S.plogs = pl;
      }
    } catch (e) {
      var st = document.getElementById("w01-eg-state");
      if (st) { st.innerHTML = errHint(e); W01.setDisp(st, "flex"); }
      return;
    }
  }
  renderProxy();
}

function renderProxy(host) {
  var root = document.getElementById("w01-px") || host;
  if (!root) return;
  w01ProxyWire();
  w01ProxyDraw();
}

/* ---------- отзывчивость: пересчёт полотна при смене ширины ---------- */
function w01ProxyInitResponsive() {
  var root = document.getElementById("w01-px");
  if (!root || root._w01resize) return;
  root._w01resize = true;
  var apply = function () { w01ProxyDraw(); };
  var redraw = W01.debounced(apply, 120);
  if (typeof ResizeObserver !== "undefined") {
    try { new ResizeObserver(function () { if (S.tab === "proxies") redraw(); }).observe(root); return; } catch (e) {}
  }
  window.addEventListener("resize", function () { if (S.tab === "proxies") redraw(); });
}

function w01ProxyInit() {
  w01ProxyWire();
  w01ProxyInitResponsive();
  if (S.tab === "proxies") w01ProxyDraw();
}

if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", w01ProxyInit);
else setTimeout(w01ProxyInit, 0);
