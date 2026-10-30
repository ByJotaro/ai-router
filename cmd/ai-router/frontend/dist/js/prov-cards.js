/* prov-cards: поведение компактных карточек (v55 balance, прод-порт, без data-v guard).
   Только карточки: uptime-строка .pc-up под баром, моды 1 ряд (2 пилюли + +N),
   пустой gc-health прячем. Каталог/тулбар не трогаем. DOM shared не меняем (только append своих узлов). */
(function () {
  function G() { try { return (typeof S !== "undefined") ? S : {}; } catch (e) { return {}; } }
  function byName(nm) { try { var end = G().end || []; for (var i = 0; i < end.length; i++) if (end[i].name === nm) return end[i]; } catch (e) {} return null; }
  function winStats(nm) {
    var logs = [];
    try { logs = G().logs || []; } catch (e) { logs = []; }
    var rows = [];
    for (var i = logs.length - 1; i >= 0 && rows.length < 300; i--) {
      var r = logs[i];
      try { if (r && r.provider === nm) rows.push(r); } catch (e) {}
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
  function paintUp(card, nm) {
    var s = winStats(nm);
    var line = null;
    try { line = card.querySelector(":scope > .pc-up"); } catch (e) { line = null; }
    if (!line) {
      line = document.createElement("div");
      line.className = "pc-up";
      var h = null;
      try { h = card.querySelector(":scope > .gc-health"); } catch (eH) { h = null; }
      if (h && h.nextSibling) h.parentNode.insertBefore(line, h.nextSibling);
      else card.appendChild(line);
    }
    var e1 = byName(nm);
    var keys = (e1 && e1.keys != null) ? e1.keys : null;
    var live = (e1 && e1.keys_healthy != null) ? e1.keys_healthy : null;
    var up = (s.up == null) ? "—" : ("<b>" + s.up + "%</b>");
    var ec = (s.err > 0) ? '<span class="bad">' + s.err + " err</span>" : '<span class="ok">0 err</span>';
    var kk = (keys != null) ? (" · <b>" + esc(String(keys)) + "</b> keys") : "";
    var lv = (live != null) ? (" · <b>" + esc(String(live)) + "</b> live") : "";
    var html = up + " up · " + s.n + " req · " + ec + kk + lv;
    if (line._pchtml !== html) { line._pchtml = html; line.innerHTML = html; }
  }
  function trimMods(card) {
    var mods = null;
    try { mods = card.querySelector(":scope > .gc-mods"); } catch (e) { mods = null; }
    if (!mods) return;
    var pills = mods.querySelectorAll(".model-pill:not(.more)");
    var old = mods.querySelector(".model-pill.more");
    if (pills.length <= 2) { if (old) old.remove(); return; }
    var off = old && old.className.indexOf("has-off") >= 0;
    var title = old ? old.getAttribute("title") : null;
    var wantCls = "model-pill more" + (off ? " has-off" : "");
    var wantTxt = "+" + (pills.length - 2);
    var wantTitle = title || ("ещё " + (pills.length - 2));
    if (old && old.className === wantCls && old.textContent === wantTxt && old.getAttribute("title") === wantTitle) {
      for (var k = 2; k < pills.length; k++) { if (pills[k].style.display !== "none") pills[k].style.display = "none"; }
      return;
    }
    if (old) old.remove();
    for (var i = 2; i < pills.length; i++) pills[i].style.display = "none";
    var more = document.createElement("span");
    more.className = wantCls;
    more.textContent = wantTxt;
    more.setAttribute("title", wantTitle);
    mods.appendChild(more);
  }
  function hideEmptyHealth(card) {
    var h = null;
    try { h = card.querySelector(":scope > .gc-health"); } catch (e) { h = null; }
    if (!h) return;
    var bar = h.querySelector(".bar[data-pct]");
    if (!bar || bar.getAttribute("data-pct") == null || bar.getAttribute("data-pct") === "") h.classList.add("is-empty");
    else h.classList.remove("is-empty");
  }
  var busy = false;
  function layout() {
    if (busy) return;
    var grid = document.getElementById("prov-grid");
    if (!grid) return;
    if (grid._pcself) return;
    var cards = grid.querySelectorAll("#prov-grid .prov-card[data-p], #prov-grid .prov-card[data-name]");
    if (!cards.length) return;
    busy = true;
    try { grid._pcdom = true; } catch (eF) {}
    try {
      for (var i = 0; i < cards.length; i++) {
        var c = cards[i];
        var nm = c.getAttribute("data-p") || c.getAttribute("data-name") || "";
        try { paintUp(c, nm); } catch (eP) {}
        try { trimMods(c); } catch (eM) {}
        try { hideEmptyHealth(c); } catch (eH) {}
      }
    } finally { busy = false; try { var gg = document.getElementById("prov-grid"); if (gg) gg._pcdom = false; } catch (eF2) {} }
  }
  function hook() {
    layout();
    var grid = document.getElementById("prov-grid");
    if (grid && !grid._pcmo) {
      grid._pcmo = true;
      var moT = null;
      try {
        var mo = new MutationObserver(function () {
          if (moT) return;
          moT = setTimeout(function () {
            moT = null;
            var g = document.getElementById("prov-grid");
            if (g && g._pcself) return;
            layout();
          }, 300);
        });
        mo.observe(grid, { childList: true, subtree: true });
      } catch (e) {}
    }
    var tries = 0;
    (function tick() {
      if (tries++ > 20) return;
      layout();
      var n = 0;
      try { n = (G().logs || []).length; } catch (eL) { n = 0; }
      if (!n) setTimeout(tick, 1000);
    })();
  }
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", hook);
  else hook();
})();
