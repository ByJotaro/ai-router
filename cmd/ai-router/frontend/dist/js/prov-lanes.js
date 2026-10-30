/* prov-lanes: группировка карточек провайдеров в тир-лейны (v37-порт).
   Карточки группируются в секции Subscription/API/Cheap/Free + "Не настроено".
   Хедер лейна: точка-индикатор + название + dotted-лидер + пилюля-счётчик;
   постоянная рамка/подсветка отсутствует — только hover (фон + рамка).
   Клик/Enter/Space по хедеру сворачивает лейн. ID/классы карточек не меняем,
   клики делегированы в shell.js. Порядок: renderProvGrid (shared) -> lanes layout. */
(function () {
  var LANE_RU = { subscription: "Subscription", api: "API", cheap: "Cheap", free: "Free" };
  var LANE_ORDER = ["subscription", "api", "cheap", "free"];
  var collapsedLanes = {};
  function G() { try { return (typeof S !== "undefined") ? S : {}; } catch (e) { return {}; } }
  function byName(nm) { try { var end = G().end || []; for (var i = 0; i < end.length; i++) if (end[i].name === nm) return end[i]; } catch (e) {} return null; }
  function winStats(nm) {
    var logs = [];
    try { logs = G().logs || []; } catch (e) { logs = []; }
    var rows = [];
    for (var i = logs.length - 1; i >= 0 && rows.length < 500; i--) {
      var l = logs[i];
      if ((l.provider || "") === nm) rows.push(l);
    }
    if (!rows.length) return { up: null, err: 0, n: 0 };
    var ok = 0, err = 0;
    for (var j = 0; j < rows.length; j++) { if (rows[j].status === "ok") ok++; else err++; }
    return { up: Math.round(ok / rows.length * 100), err: err, n: rows.length };
  }
  function esc(s) {
    try { return String(s == null ? "" : s).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;"); }
    catch (e) { return ""; }
  }
  function laneSummary(list) {
    var on = 0, upSum = 0, upN = 0, brk = 0, off = 0;
    for (var i = 0; i < list.length; i++) {
      var nm = list[i] && list[i].getAttribute ? list[i].getAttribute("data-p") : null;
      var e = nm ? byName(nm) : null;
      if (!e || e.catalog) continue;
      if (e.breaker !== "closed") brk++;
      else if (e.breaker === "closed" && (e.keys_pct || 0) >= 0 && (e.keys_healthy || 0) > 0) on++;
      else off++;
      try { var s = nm ? winStats(nm) : null; if (s && s.up != null) { upSum += s.up; upN++; } } catch (eS) {}
    }
    return { n: list.length, on: on, up: upN ? Math.round(upSum / upN) : null, brk: brk, off: off };
  }
  function laneDotCls(sm) {
    if (!sm) return "";
    if (sm.brk > 0) return " err";
    if (sm.off > 0) return " warn";
    return " on";
  }
  function laneNhtml(sm) {
    var h = esc(String(sm.n));
    h += ' <span class="dim">· в сети ' + esc(String(sm.on)) + '</span>';
    if (sm.off > 0) h += ' <span class="dim">· пауза ' + esc(String(sm.off)) + '</span>';
    if (sm.brk > 0) h += ' <span class="dim">· брейкер ' + esc(String(sm.brk)) + '</span>';
    if (sm.up != null) h += ' <span class="dim">· up ' + esc(String(sm.up)) + '%</span>';
    return h;
  }
  function bindCollapse(sec, h, key) {
    var laneId = "pc-lane-" + key;
    try { var ln0 = sec.querySelector(".pc-lane"); if (ln0 && !ln0.id) ln0.id = laneId; } catch (eI) {}
    try { h.setAttribute("role", "button"); h.setAttribute("tabindex", "0"); h.setAttribute("aria-expanded", collapsedLanes[key] ? "false" : "true"); h.setAttribute("aria-controls", laneId); } catch (eA) {}
    if (collapsedLanes[key]) { try { sec.classList.add("collapsed"); } catch (eC) {} }
    if (h._pcc) return;
    h._pcc = true;
    h.addEventListener("click", function (ev) {
      if (ev.target && ev.target.closest && ev.target.closest("button,a,input,select,summary")) return;
      collapsedLanes[key] = !collapsedLanes[key];
      try { sec.classList.toggle("collapsed", !!collapsedLanes[key]); } catch (eT) {}
      try { h.setAttribute("aria-expanded", collapsedLanes[key] ? "false" : "true"); } catch (eX) {}
    });
    h.addEventListener("keydown", function (ev) {
      if (ev.key !== "Enter" && ev.key !== " ") return;
      ev.preventDefault();
      collapsedLanes[key] = !collapsedLanes[key];
      try { sec.classList.toggle("collapsed", !!collapsedLanes[key]); } catch (eT2) {}
      try { h.setAttribute("aria-expanded", collapsedLanes[key] ? "false" : "true"); } catch (eX2) {}
    });
  }
  function normTier(e) {
    if (e && e.catalog) return "__cat";
    var t = String((e && e.tier) || "").toLowerCase().trim();
    if (t === "premium" || t === "subscription") return "subscription";
    if (t === "standard" || t === "api") return "api";
    if (t === "economy" || t === "cheap") return "cheap";
    if (t === "free") return "free";
    if (LANE_ORDER.indexOf(t) >= 0) return t;
    return "free";
  }
  var busy = false, queued = false, retries = 0;
  function layout() {
    if (busy) { queued = true; return; }
    busy = true;
    try {
      var grid = document.getElementById("prov-grid");
      if (!grid) { busy = false; return; }
      var cards = Array.prototype.slice.call(grid.querySelectorAll("#prov-grid .prov-card[data-p]"));
      var cats = Array.prototype.slice.call(grid.querySelectorAll("#prov-grid details.prov-catalog"));
      var globals = Array.prototype.slice.call(grid.querySelectorAll('#prov-grid .prov-card[data-name]'));
      if (!cards.length && !cats.length) {
        if (retries < 10) { retries++; setTimeout(function () { try { busy = false; } catch (eB2) {} layout(); }, 500); return; }
        return;
      }
      retries = 0;
      try { grid._pcself = true; } catch (eF) {}
      cards.forEach(function (c) { try { grid.appendChild(c); } catch (eA) {} });
      cats.forEach(function (d) { try { grid.appendChild(d); } catch (eD2) {} });
      globals.forEach(function (g2) { try { grid.appendChild(g2); } catch (eG) {} });
      Array.prototype.slice.call(grid.querySelectorAll(":scope > .pc-sec")).forEach(function (s) { try { s.remove(); } catch (eR0) {} });
      var lanes = {};
      LANE_ORDER.forEach(function (t) { lanes[t] = []; });
      var catList = [];
      cards.forEach(function (c) {
        var nm2 = null;
        try { nm2 = c.getAttribute("data-p"); } catch (eG3) { return; }
        var t2 = "free";
        try { t2 = normTier(byName(nm2)); } catch (eT3) { t2 = "free"; }
        if (t2 === "__cat") catList.push(c);
        else if (lanes[t2]) lanes[t2].push(c);
        else lanes.free.push(c);
      });
      cats.forEach(function (d) { catList.push(d); });
      LANE_ORDER.forEach(function (t) {
        if (!lanes[t].length) return;
        var sec = document.createElement("div");
        sec.className = "pc-sec";
        sec.setAttribute("data-lane", t);
        var h = document.createElement("div");
        h.className = "pc-lane-h";
        var smT = laneSummary(lanes[t]);
        h.innerHTML = '<span class="pc-dot' + laneDotCls(smT) + '"></span><span class="pc-t">' + esc(LANE_RU[t]) + '</span><span class="pc-lead"></span><span class="pc-n">' + laneNhtml(smT) + "</span>";
        var lane = document.createElement("div");
        lane.className = "pc-lane";
        lanes[t].forEach(function (c) { lane.appendChild(c); });
        sec.appendChild(h); sec.appendChild(lane);
        grid.appendChild(sec);
        bindCollapse(sec, h, t);
      });
      if (catList.length) {
        var sec = document.createElement("div");
        sec.className = "pc-sec";
        sec.setAttribute("data-lane", "uncfg");
        var h = document.createElement("div");
        h.className = "pc-lane-h";
        var nCat = 0;
        catList.forEach(function (d) {
          if (d.tagName === "DETAILS") {
            var m = (d.querySelector("summary") || {}).textContent || "";
            var mm = m.match(/\((\d+)\)/);
            if (mm) nCat += +mm[1]; else nCat += d.querySelectorAll(".prov-cat-item").length;
          } else nCat++;
        });
        h.innerHTML = '<span class="pc-dot neutral"></span><span class="pc-t">Не настроено</span><span class="pc-lead"></span><span class="pc-n">' + esc(String(nCat)) + "</span>";
        var lane = document.createElement("div");
        lane.className = "pc-lane";
        catList.forEach(function (d) { try { if (d.tagName === "DETAILS") d.removeAttribute("open"); } catch (eO) {} lane.appendChild(d); });
        sec.appendChild(h); sec.appendChild(lane);
        grid.appendChild(sec);
        bindCollapse(sec, h, "uncfg");
      }
      globals.forEach(function (g) { try { grid.appendChild(g); } catch (eG2) {} });
    } finally {
      busy = false;
      try { var gr = document.getElementById("prov-grid"); if (gr) gr._pcself = false; } catch (eF2) {}
      if (queued) { queued = false; layout(); }
    }
  }
  function hook() {
    var grid = document.getElementById("prov-grid");
    if (!grid) { setTimeout(hook, 300); return; }
    var hookTries = 0;
    (function hookLayout() {
      hookTries++;
      try { layout(); } catch (eL0) {}
      try {
        var hasSec = !!grid.querySelector(":scope > .pc-sec");
        var hasCards = false;
        try { hasCards = grid.querySelectorAll(".pc-sec .prov-card[data-p]").length > 0; } catch (eHC) { hasCards = false; }
        if (((!hasSec || !hasCards) && hookTries < 40) || (!hasSec && hookTries < 40)) setTimeout(hookLayout, 500);
      } catch (eH) {}
    })();
    var moT = null;
    function schedule() {
      if (moT) return;
      moT = setTimeout(function () {
        moT = null;
        try {
          var g = document.getElementById("prov-grid");
          if (!g) return;
          var hasSec = !!g.querySelector(":scope > .pc-sec");
          var stray = g.querySelector(":scope > .prov-card[data-p], :scope > details.prov-catalog");
          if (g._pcself || g._pcdom) return;
          if (!hasSec || stray) layout();
        } catch (eM) {}
      }, 300);
    }
    try {
      var mo = new MutationObserver(function () { schedule(); });
      mo.observe(grid, { childList: true, subtree: false });
    } catch (e) {}
  }
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", hook);
  else hook();
})();
