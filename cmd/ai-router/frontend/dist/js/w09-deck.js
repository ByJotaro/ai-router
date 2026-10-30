/* =====================================================================
   w09 · ДВИЖОК КОЛОДЫ (общий для четырёх вкладок)
   Главный объект каждой вкладки — КРУПНАЯ КАРТОЧКА ОДНОГО ОБЪЕКТА,
   между которыми листают: кнопки ‹ ›, клавиши ← →, Home/End, полоска
   выбора (та самая «полоска») и список-лента для быстрого перехода.
   Индекс всегда виден: «день 17 / 30».
   Все обработчики делегированы (карточки перерисовываются innerHTML).
   ===================================================================== */
"use strict";

var W09 = {
  i: { usage: 0, logs: 0, proxies: 0, settings: 0 },   // индекс текущей карточки в каждой колоде
  init: {},                                            // колода уже выставляла индекс по «последнему» объекту
  act: {},                                             // модульные действия: W09.act.usage(...), .logs, .proxies, .settings
  order: ["usage", "logs", "proxies", "settings"]
};

/* ---------- индекс / клампы ---------- */
function w09Clamp(i, n) { i = Math.floor(+(i || 0)); if (!isFinite(i)) i = 0; return Math.max(0, Math.min(n > 0 ? n - 1 : 0, i)); }
function w09Start(key) { W09.i[key] = w09Clamp(W09.i[key] || 0, w09Count(key)); }
function w09Count(key) {
  try {
    if (key === "usage") return typeof w09UsageCount === "function" ? w09UsageCount() : 0;
    if (key === "logs") return typeof w09LogCount === "function" ? w09LogCount() : 0;
    if (key === "proxies") return typeof w09ProxyCount === "function" ? w09ProxyCount() : 0;
    if (key === "settings") return typeof w09CfgCount === "function" ? w09CfgCount() : 0;
  } catch (e) { }
  return 0;
}
function w09Repaint(key) {
  var f = { usage: "renderUsage", logs: "renderLogs", proxies: "renderProxy", settings: "renderSettings" }[key];
  if (!f) return;
  var fn = window[f] || (typeof window[f] === "function" ? window[f] : null);
  try { if (typeof fn === "function") fn(); } catch (e) { try { console.error("w09-repaint:", key, e); } catch (e2) { } }
}
function w09Step(key, d) {
  var n = w09Count(key);
  if (n < 1) return;
  W09.i[key] = w09Clamp((W09.i[key] || 0) + d, n);
  w09Repaint(key);
}
function w09Goto(key, i) {
  var n = w09Count(key);
  if (n < 1) return;
  W09.i[key] = w09Clamp(i, n);
  w09Repaint(key);
}
/* Корень колоды: первый контейнер этой вкладки, внутри которого есть индекс.
   Нужен потому, что внутри вкладки бывает вложенный корень (календарь использования). */
function w09DeckRoot(key) {
  var all = $$('[data-w09-deck="' + key + '"]');
  for (var i = 0; i < all.length; i++) if (all[i].querySelector("[data-w09-idx]")) return all[i];
  return all[0] || null;
}
/* Синхронизация индекса/кнопок внутри колоды: вызывают рендеры панелей. */
function w09SyncIdx(key, i, n) {
  var box = w09DeckRoot(key);
  if (!box) return;
  var ix = box.querySelector("[data-w09-idx]");
  if (ix) ix.textContent = n > 0 ? (i + 1) + " / " + n : "— / —";
  var prev = box.querySelector('[data-w09="prev"]'), next = box.querySelector('[data-w09="next"]');
  if (prev) prev.disabled = (n < 1 || i <= 0);
  if (next) next.disabled = (n < 1 || i >= n - 1);
  var first = box.querySelector('[data-w09="first"]'), last = box.querySelector('[data-w09="last"]');
  if (first) first.disabled = (n < 1 || i <= 0);
  if (last) last.disabled = (n < 1 || i >= n - 1);
  var st = box.querySelector("[data-w09-strip]");
  if (st) {
    var cur = st.querySelector(".w09-tick.on"); if (cur) cur.classList.remove("on");
    var tk = st.children[i]; if (tk) tk.classList.add("on");
  }
  Array.prototype.forEach.call(box.querySelectorAll(".w09-lrow"), function (r) {
    var on = (+r.getAttribute("data-w09-i") === i);
    if (on) r.classList.add("on"); else r.classList.remove("on");
    if (on && r.setAttribute) r.setAttribute("aria-current", "true"); else if (r.removeAttribute) r.removeAttribute("aria-current");
  });
}

/* ---------- делегированный клик: вся навигация колоды ---------- */
function w09DelegClick(e) {
  var t = e.target && e.target.closest ? e.target.closest("[data-w09]") : null;
  if (!t) return;
  var act = t.getAttribute("data-w09");
  var box = t.closest ? t.closest("[data-w09-deck]") : null;
  var key = box ? box.getAttribute("data-w09-deck") : "";
  var handler = W09.act[key];
  if (typeof handler === "function") { try { if (handler(act, t, e) === true) return; } catch (eA) { try { console.error("w09-act:", key, act, eA); } catch (e2) { } } }
  if (!key) return;
  if (act === "prev") { w09Step(key, -1); return; }
  if (act === "next") { w09Step(key, 1); return; }
  if (act === "first") { w09Goto(key, 0); return; }
  if (act === "last") { w09Goto(key, 1e9); return; }
  if (act === "jump") { w09Goto(key, +t.getAttribute("data-w09-i")); return; }
}

/* ---------- клавиатура: ← → Home End на активной вкладке ---------- */
function w09DeckKey(key) {
  if (W09.order.indexOf(key) < 0) return false;
  if (key !== S.tab) return false;
  if (document.querySelector("#modal")) return false;
  if (document.querySelector(".dd.open")) return false;
  if (key === "logs" && S.logKind === "proxies") return false;   // лента egress — листания нет
  if (w09Count(key) < 1) return false;
  return true;
}
function w09DelegKey(e) {
  if (e.defaultPrevented || e.altKey || e.ctrlKey || e.metaKey) return;
  var k = e.key;
  if (k !== "ArrowLeft" && k !== "ArrowRight" && k !== "Home" && k !== "End") return;
  var a = document.activeElement;
  /* фокус, оставшийся в скрытой вкладке (её только что переключили), не должен глушить стрелки текущей */
  var panel = document.getElementById("panel-" + S.tab);
  if (a && a !== document.body && panel && a.closest && a.closest(".tab-panel") && !panel.contains(a)) a = null;
  if (a && a !== document.body && (a.tagName === "INPUT" || a.tagName === "TEXTAREA" || a.tagName === "SELECT" || a.isContentEditable)) return;
  if (a && a.closest && a.closest(".dd")) return;   // открытый выпадающий список сам ест стрелки
  if (!w09DeckKey(S.tab)) return;
  e.preventDefault();
  if (k === "ArrowLeft") w09Step(S.tab, -1);
  else if (k === "ArrowRight") w09Step(S.tab, 1);
  else if (k === "Home") w09Goto(S.tab, 0);
  else w09Goto(S.tab, 1e9);
}

/* ---------- мелкие форматтеры карточек ---------- */
function w09Tok(n) { n = iN(n); return n >= 1e9 ? (n / 1e9).toFixed(2) + "G" : n >= 1e6 ? (n / 1e6).toFixed(1) + "M" : n >= 1e3 ? (n / 1e3).toFixed(1) + "k" : "" + n; }
function w09Ms(ms) { ms = iN(ms); return ms < 1000 ? ms + " мс" : (ms / 1000).toFixed(ms < 10000 ? 2 : 1) + " с"; }
function w09Plural(n, a, b, c) { n = Math.abs(iN(n)) % 100; var d = n % 10; if (n > 10 && n < 20) return c; if (d > 1 && d < 5) return b; if (d === 1) return a; return c; }
var W09_WD = ["вс", "пн", "вт", "ср", "чт", "пт", "сб"];
var W09_MO = ["янв", "фев", "мар", "апр", "май", "июн", "июл", "авг", "сен", "окт", "ноя", "дек"];
function w09Iso(d) { return d.getFullYear() + "-" + String(d.getMonth() + 1).padStart(2, "0") + "-" + String(d.getDate()).padStart(2, "0"); }
function w09DayParts(iso) {
  var s = String(iso || "");
  var m = /^(\d{4})-(\d{2})-(\d{2})/.exec(s);
  if (!m) return { dd: s || "—", wd: "", y: "", iso: s, ok: false };
  var d = new Date(+m[1], +m[2] - 1, +m[3]);
  return { iso: s.slice(0, 10), dd: m[3] + "." + m[2], wd: W09_WD[d.getDay()], y: m[1], ok: true, dt: d };
}
function w09DeltaTxt(cur, prev, fmt) {
  cur = iN(cur); prev = iN(prev);
  if (!prev) return { txt: cur ? "без базы для сравнения" : "нет данных", cls: "flat" };
  var dp = Math.round(1000 * (cur - prev) / prev) / 10;
  var f = fmt || (function (v) { return w09Tok(v); });
  var sign = dp > 0 ? "+" : (dp < 0 ? "−" : "±");
  return { txt: sign + Math.abs(dp).toFixed(1) + "% · " + (cur >= prev ? "+" : "−") + f(Math.abs(cur - prev)) + " к пред. дню", cls: dp > 0 ? "up" : (dp < 0 ? "down" : "flat") };
}
/* строка «поле: значение» внутри карточки */
function w09Fld(k, v, cls, hint, wide) {
  return '<div class="w09-fld' + (wide ? " w09-wide" : "") + '"><div class="k">' + esc(k) + '</div>'
    + '<div class="v' + (cls ? " " + cls : "") + '">' + (v == null ? "—" : v) + '</div>'
    + (hint ? '<div class="hint">' + esc(hint) + '</div>' : "") + '</div>';
}
/* бары состава (вход/выход/кэш) */
function w09Bars(rows) {
  var mx = 0; rows.forEach(function (r) { mx = Math.max(mx, iN(r[1])); });
  if (!mx) return "";
  return '<div class="w09-bars">' + rows.map(function (r) {
    return '<div class="w09-brow"><span>' + esc(r[0]) + '</span><span class="w09-track"><i' + (r[3] ? ' class="' + r[3] + '"' : '') + ' style="width:' + Math.round(100 * iN(r[1]) / mx) + '%"></i></span><span class="v">' + esc(r[2]) + '</span></div>';
  }).join("") + '</div>';
}
function w09Prog(p, cls) {
  p = Math.max(0, Math.min(100, Math.round(100 * (+p || 0))));
  return '<div class="progress thin"><div class="bar' + (cls ? " " + cls : "") + '" style="width:' + p + '%"></div></div>';
}
function w09Sdot(cls) { return '<span class="sdot ' + (cls || "") + '" aria-hidden="true"></span>'; }
/* шапка карточки: дата/заголовок слева, крупное число и дельта справа */
function w09Lead(lbl, big, cls, side) {
  return '<div class="w09-lead"><div class="w09-lbl">' + esc(lbl) + '</div>'
    + '<div class="w09-big' + (String(big).length > 9 ? " w09-sm" : "") + '">' + esc(big) + '</div>'
    + (cls ? '<div class="w09-sline">' + cls + '</div>' : "") + '</div>'
    + (side ? '<div class="w09-side">' + side + '</div>' : "");
}
function w09StripTicks(vals, titles, extraCls) {
  var mx = 0; vals.forEach(function (v) { mx = Math.max(mx, Math.abs(iN(v))); });
  if (!mx) mx = 1;
  return '<div class="w09-strip" data-w09-strip role="group">' + vals.map(function (v, i) {
    var h = Math.max(6, Math.round(100 * Math.abs(iN(v)) / mx));
    return '<button class="w09-tick' + (extraCls && extraCls[i] ? " " + extraCls[i] : "") + '" data-w09="jump" data-w09-i="' + i + '" title="' + esc(titles[i] || "") + '" aria-label="' + esc(titles[i] || "") + '"><i style="height:' + h + '%"></i></button>';
  }).join("") + '</div>';
}
function w09SkeletonRows(n) {
  var h = "";
  for (var i = 0; i < (n || 5); i++) h += '<div class="w09-skrow"><span class="skeleton"></span><span class="skeleton" style="max-width:34%"></span></div>';
  return '<div class="w09-sk" id="usage-sk">' + h + '</div>';
}
/* сегсвитч: выставить .on/aria-pressed по актуальному состоянию */
function w09SegSync(sel, on) {
  try {
    $$(sel).forEach(function (b) {
      var v = b.dataset.v || b.dataset.m || b.dataset.k || "";
      var isOn = !!on(v, b);
      setCls(b, isOn, "on");
      b.setAttribute("aria-pressed", String(isOn));
    });
  } catch (e) { }
}
/* шапка панели: meta/live — канон задаёт статику, мы уточняем в демо */
function w09PanelMeta(tab, txt, extra) {
  var m = $("#" + tab + "-meta");
  if (m) { m.textContent = txt; m.title = txt; if (extra != null) m.setAttribute("data-extra", extra); }
}
function w09PanelLive(tab, txt, paused) {
  var l = $("#" + tab + "-live"), t = $("#" + tab + "-live-txt");
  if (t && txt) t.textContent = txt;
  if (l) { if (paused) l.classList.add("paused"); else l.classList.remove("paused"); }
}

/* ---------- контракт: живой тест провайдера (зовут providers.js и shell.js:369) ---------- */
/* Наша вкладка «Прокси» живой прогон не дублирует: тест — это проба ОДНОГО канала против ядра,
   поэтому функция общая и отчитывается тостом, а не строит второй экран поверх колоды. */
async function testProvider(preset) {
  var name = (preset && (preset.name || preset)) || (S.prov && S.prov.name) || "";
  if (!name || name === "__GLOBAL__") { toast("Выберите провайдера с ендпоинтом — тестировать нечего", "err"); return; }
  if (DEMO || !Go()) { toast("Демо-режим: живой тест «" + name + "» недоступен — ядро не опрошено", "warn"); return; }
  var mdl = (preset && preset.model) || S._mdlCur
    || ((S.prov && S.prov.models) || []).filter(function (m) { return ((S.prov.disabled_models || []).indexOf(m) < 0); })[0] || "";
  if (!mdl) { toast("У «" + name + "» нет доступных моделей", "err"); return; }
  toast("Тест «" + name + " / " + mdl + "»…", "ok");
  try {
    var r = await API.Test(name + "/" + mdl);
    var ok = r && !r.error && (iN(r.status) < 400 || !r.status);
    toast("Тест «" + name + " / " + mdl + "»: " + (ok ? "ok" : "ошибка")
      + (r && r.latency_ms ? " · " + r.latency_ms + " мс" : "") + (r && r.error ? " · " + r.error : ""), ok ? "ok" : "err");
  } catch (e) {
    toast("Тест «" + name + " / " + mdl + "»: " + ((e && e.message) || "сбой ядра"), "err");
  }
}

/* общая привязка: один раз на документ */
if (!window._w09Wired) {
  window._w09Wired = 1;
  document.addEventListener("click", w09DelegClick);
  document.addEventListener("keydown", w09DelegKey);
}
