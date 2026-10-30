/* Variant 08 "Flow": decorator over home .toprow (smooth Catmull-Rom spark under text).
Parametrized via initFlowVariant; only acts when body[data-top] matches cfg.top. */
(function () {
  "use strict";

  var DEFAULT_SERIES = [
    [4, 7, 5, 9, 8, 12, 15],
    [12, 10, 11, 8, 9, 7, 8],
    [6, 8, 7, 10, 9, 11, 10]
  ];

  function smoothPath(xy) {
    if (xy.length < 2) return "";
    var d = "M" + xy[0][0] + "," + xy[0][1];
    for (var i = 0; i < xy.length - 1; i++) {
      var p0 = xy[Math.max(0, i - 1)], p1 = xy[i], p2 = xy[i + 1], p3 = xy[Math.min(xy.length - 1, i + 2)];
      var c1x = p1[0] + (p2[0] - p0[0]) / 6, c1y = p1[1] + (p2[1] - p0[1]) / 6;
      var c2x = p2[0] - (p3[0] - p1[0]) / 6, c2y = p2[1] - (p3[1] - p1[1]) / 6;
      d += "C" + c1x.toFixed(1) + "," + c1y.toFixed(1) + " " + c2x.toFixed(1) + "," + c2y.toFixed(1) + " " + p2[0].toFixed(1) + "," + p2[1].toFixed(1);
    }
    return d;
  }

  function sparkSVG(points, W, cfg) {
    // Main width method: dynamic viewBox = row width (no fixed-120 stretch).
    // Right padding PR keeps the end dot unclipped.
    var H = cfg.H || 24, P = 2, PR = cfg.PR || 6;
    W = Math.max(120, Math.round(W || 120));
    var max = Math.max.apply(null, points), min = Math.min.apply(null, points);
    var span = Math.max(1, max - min);
    var step = (W - P - PR) / (points.length - 1);
    var xy = points.map(function (v, i) {
      return [P + i * step, H - P - ((v - min) / span) * (H - P * 2)];
    });
    var line = smoothPath(xy);
    var last = xy[xy.length - 1];
    return '<svg class="' + (cfg.svgCls || "spark") + '" viewBox="0 0 ' + W + " " + H + '" preserveAspectRatio="none" aria-hidden="true">'
      + '<path class="flow-fill" d="' + line + "L" + (W - PR).toFixed(1) + "," + H + "L" + P + "," + H + 'Z"/>'
      + '<path class="flow-line" d="' + line + '"/>'
      + '<circle cx="' + last[0].toFixed(1) + '" cy="' + last[1].toFixed(1) + '" r="2.5"/></svg>';
  }

  function cleanupOwn(host, cfg) {
    host.querySelectorAll(":scope > " + cfg.rowCls + " > svg." + cfg.svgCls).forEach(function (s) { s.remove(); });
  }

  function initFlowVariant(cfg) {
    cfg = cfg || {};
    cfg.top = cfg.top || "8";
    cfg.host = cfg.host || "home-tops";
    cfg.rowCls = cfg.rowCls || ".toprow";
    cfg.svgCls = cfg.svgCls || "spark";
    cfg.series = cfg.series || DEFAULT_SERIES;
    cfg.H = cfg.H || 24;
    cfg.PR = cfg.PR || 6;

    function active() {
      return document.body.getAttribute("data-top") === cfg.top;
    }

    function inject() {
      var host = document.getElementById(cfg.host);
      if (!host) return;
      if (!active()) { cleanupOwn(host, cfg); return; }
      host.querySelectorAll(":scope > " + cfg.rowCls).forEach(function (row, i) {
        var w = 120;
        try { w = Math.max(120, Math.round(row.clientWidth || row.offsetWidth || 120)); } catch (eW) {}
        var old = row.querySelector(":scope > svg." + cfg.svgCls);
        if (old && +old._w08w === w) return;
        if (old) old.remove();
        row.insertAdjacentHTML("beforeend", sparkSVG(cfg.series[i % cfg.series.length], w, cfg));
        try { row.querySelector(":scope > svg." + cfg.svgCls)._w08w = w; } catch (eS) {}
      });
    }

    var mo = null, ro = null, fallback = null;

    function watch() {
      var host = document.getElementById(cfg.host);
      if (!host) return;
      inject();
      if (host._top08w) return;
      host._top08w = 1;
      try {
        mo = new MutationObserver(inject);
        mo.observe(host, { childList: true });
      } catch (e) {}
      try {
        ro = new ResizeObserver(inject);
        host.querySelectorAll(":scope > " + cfg.rowCls).forEach(function (row) { ro.observe(row); });
      } catch (eR) {}
      if (!fallback) {
        fallback = setTimeout(function () { inject(); }, 1500);
      }
    }

    function stop() {
      try { if (mo) mo.disconnect(); } catch (eM) {}
      try { if (ro) ro.disconnect(); } catch (eR) {}
      if (fallback) { clearTimeout(fallback); fallback = null; }
      var host = document.getElementById(cfg.host);
      if (host) { host._top08w = 0; cleanupOwn(host, cfg); }
    }

    if (document.readyState === "loading") {
      document.addEventListener("DOMContentLoaded", watch);
    } else {
      watch();
    }
    return { inject: inject, watch: watch, stop: stop, cfg: cfg };
  }

  initFlowVariant({ top: "8", host: "home-tops", rowCls: ".toprow", svgCls: "spark", series: DEFAULT_SERIES, H: 24, PR: 6 });
})();
