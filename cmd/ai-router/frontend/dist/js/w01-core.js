/* ==================================================================
   w01 «ПОЛОТНО» — общие помощники варианта.
   Четыре вкладки («Использование», «Журналы», «Прокси», «Настройки») рисуют
   непрерывные линии во всю ширину; числа — компактной строкой над линией.
   Стиль опирается на «Главную» (serif-заголовки, mono-числа, var(--…) из tokens.css).
   Ничего, кроме своих классов w01-*; внешних библиотек и canvas нет.
   ================================================================== */
"use strict";

var W01 = (function () {
  var p2 = function (n) { n = +n || 0; return (n < 10 ? "0" : "") + n; };

  /* ---------- время и числа (без зависимостей от чужих хелперов) ---------- */
  function parseTs(ts) {
    var s = String(ts == null ? "" : ts).trim();
    if (!s || s === "…") return null;
    var d = new Date(s.indexOf("T") >= 0 ? s : s.replace(" ", "T"));
    if (isNaN(d.getTime())) d = new Date(s);
    return isNaN(d.getTime()) ? null : d;
  }
  function hhmm(v) { var d = (v instanceof Date) ? v : parseTs(v); return d ? p2(d.getHours()) + ":" + p2(d.getMinutes()) : "—"; }
  function hhmmss(v) { var d = (v instanceof Date) ? v : parseTs(v); return d ? p2(d.getHours()) + ":" + p2(d.getMinutes()) + ":" + p2(d.getSeconds()) : "—"; }
  /* MM-DD из ISO-даты */
  function mmdd(iso) { var s = String(iso == null ? "" : iso); return s.length >= 10 ? s.slice(8, 10) + "." + s.slice(5, 7) : s; }
  /* ISO-день по ЛОКАЛЬНОЙ дате (toISOString() уводит на сутки назад в UTC+N) */
  function iso(d) { return d.getFullYear() + "-" + p2(d.getMonth() + 1) + "-" + p2(d.getDate()); }
  /* DD.MM из ISO-даты */
  function ddmm(iso) { var s = String(iso == null ? "" : iso); return s.length >= 10 ? s.slice(8, 10) + "." + s.slice(5, 7) : s; }

  function clamp(v, a, b) { return v < a ? a : (v > b ? b : v); }
  function sum(arr, f) { var a = arr || [], t = 0; for (var i = 0; i < a.length; i++) t += +(f(a[i]) || 0); return t; }
  function nums(arr, f) { return (arr || []).map(function (x) { return +(f(x) || 0); }); }
  function sorted(a) { return a.slice().sort(function (x, y) { return x - y; }); }
  function quant(a, q) { if (!a.length) return 0; var s = sorted(a); var i = clamp(Math.round((s.length - 1) * q), 0, s.length - 1); return s[i]; }
  function avg(a) { return a.length ? sum(a, function (x) { return x; }) / a.length : 0; }

  /* Округление верхней границы оси: 1 / 2 / 2.5 / 5 / 10 × 10^n — как на «Главной». */
  function niceMax(mx) {
    mx = +mx || 0;
    if (!(mx > 0)) return 1;
    var p = Math.pow(10, Math.floor(Math.log10(mx))), r = mx / p;
    var nr = r <= 1 ? 1 : r <= 2 ? 2 : r <= 2.5 ? 2.5 : r <= 5 ? 5 : 10;
    return nr * p;
  }
  /* Шаг сетки под 4 линии: округлённая четверть верхней границы оси. */
  function gridStep(mx) { return niceMax(+mx || 1) / 4; }

  function ms(v) { v = +v || 0; return v >= 1000 ? (v / 1000).toFixed(2).replace(".", ",") + " с" : Math.round(v) + " мс"; }
  function toks(v) { return compact(Math.round(+v || 0)); }
  function money(c) { return moneyC(c); }
  function pctStr(p) { return (p == null || !isFinite(p) || p < 0) ? "∞" : Math.round(p * 100) + "%"; }
  function attr(s) { return esc(s == null ? "" : s); }
  function cut(s, n) { s = String(s == null ? "" : s); return s.length > n ? s.slice(0, Math.max(1, n - 1)) + "…" : s; }
  function nn(v) { return (v == null || v === "" ) ? "—" : v; }

  /* ---------- строки чисел: «значение подпись» через точку-разделитель ---------- */
  /* items: [[значение, подпись, доп.класс?], …] */
  function numline(items) {
    var out = [], i;
    for (i = 0; i < (items || []).length; i++) {
      var it = items[i] || [];
      if (it[0] == null || it[0] === "") continue;
      out.push('<span class="w01-num' + (it[2] ? " " + it[2] : "") + '"' + (it[3] ? ' title="' + attr(it[3]) + '"' : "") +
        '><b>' + it[0] + '</b><i>' + (it[1] == null ? "" : it[1]) + '</i></span>');
    }
    return out.join("");
  }

  /* ---------- SVG-примитивы (строки, без библиотек) ---------- */
  function f1(v) { return Math.round((+v || 0) * 10) / 10; }
  function poly(pts) { return pts.map(function (p) { return f1(p.x) + "," + f1(p.y); }).join(" "); }
  function area(points, y0) {
    if (points.length < 2) return "";
    return "M" + f1(points[0].x) + "," + f1(y0) + " L" + points.map(function (p) { return f1(p.x) + " " + f1(p.y); }).join(" L") + " L" + f1(points[points.length - 1].x) + "," + f1(y0) + " Z";
  }
  function el(tag, cls, attrs) {
    var s = "<" + tag + (cls ? ' class="' + cls + '"' : "");
    for (var k in (attrs || {})) { if (attrs[k] == null) continue; s += " " + k + '="' + attrs[k] + '"'; }
    return s + "/>";
  }

  /* ---------- тултип: одна функция на все вкладки ---------- */
  /* Позиция — fixed от курсора, прижата к вьюпорту (как #home-tip на «Главной»). */
  function tip(el, html, cx, cy, w) {
    if (!el) return;
    if (html == null) { el.style.display = "none"; return; }
    el.innerHTML = html;
    el.style.display = "block";
    var ww = w || 250;
    var x = Math.min((cx || 0) + 14, Math.max(8, window.innerWidth - ww - 10));
    var y = Math.min((cy || 0) + 12, Math.max(8, window.innerHeight - 150));
    el.style.left = Math.max(8, x) + "px";
    el.style.top = Math.max(8, y) + "px";
  }
  function tipHide(el) { if (el) el.style.display = "none"; }
  /* Позиция элемента относительно документа — для fixed-тултипа над линией. */
  function clientOf(node) {
    try { var r = node.getBoundingClientRect(); return { x: r.left, y: r.top, w: r.width, h: r.height }; }
    catch (e) { return { x: 0, y: 0, w: 0, h: 0 }; }
  }

  /* ---------- мелкие DOM-помощники ---------- */
  function setHTML(id, html) {
    var el = document.getElementById(id);
    if (!el) return el;
    /* кэш строки; внешняя очистка (shell.js чистит #usage-kpis в виде «год») снимает кэш */
    if (el._w01h === html && el.firstChild) return el;
    el._w01h = html; el.innerHTML = html;
    return el;
  }
  function setTxt(id, txt) { var el = document.getElementById(id); if (el && el.textContent !== txt) el.textContent = txt; return el; }
  function show(id, on) { var el = document.getElementById(id); if (el) el.hidden = !on; return el; }
  function setDisp(id, v) { var el = document.getElementById(id); if (el) el.style.display = v; return el; }
  function debounced(fn, ms) { try { return debounce(fn, ms); } catch (e) { return fn; } }

  return {
    p2: p2, parseTs: parseTs, hhmm: hhmm, hhmmss: hhmmss, mmdd: mmdd, ddmm: ddmm, iso: iso,
    clamp: clamp, sum: sum, nums: nums, quant: quant, avg: avg, sorted: sorted,
    niceMax: niceMax, gridStep: gridStep, ms: ms, toks: toks, money: money, pctStr: pctStr,
    attr: attr, cut: cut, nn: nn, numline: numline,
    f1: f1, poly: poly, area: area, el: el,
    tip: tip, tipHide: tipHide, clientOf: clientOf,
    setHTML: setHTML, setTxt: setTxt, show: show, setDisp: setDisp, debounced: debounced
  };
})();
