/* Аварийный перебор (вручную) — вкладка «Прокси-пул».
   Никакой автоматики: оператор рисует новые IP, смотрит матрицу вариантов
   (UA / имена параметров инструментов) и отдельной кнопкой применяет
   выигравший вариант. */
"use strict";

var W01B = { st: null, cat: null, timer: 0, busy: false };

function w01BypassApi(name) {
  return (typeof API !== "undefined" && API && API[name]) ? API[name] : null;
}

function w01BypassActiveText(act) {
  act = act || {};
  var ua = String(act.ua || "встроенный UA").split(" ")[0];
  return "текущие: " + ua + " · kit " + (act.kit || "camel");
}

function w01BypassLoad() {
  var host = document.getElementById("w01-bypass-rows");
  if (!host) return;
  if (W01B.busy) return;
  W01B.busy = true;
  var jobs = [];
  var cat = w01BypassApi("BypassCatalog");
  var st = w01BypassApi("BypassState");
  if (!DEMO && cat) jobs.push(Promise.resolve(cat()).then(function (c) { if (c) W01B.cat = c; }).catch(function () { }));
  if (!DEMO && st) jobs.push(Promise.resolve(st()).then(function (s) { if (s) W01B.st = s; }).catch(function () { }));
  Promise.all(jobs).finally(function () {
    W01B.busy = false;
    w01BypassWire();
    w01BypassDraw();
    if (W01B.st && W01B.st.running) w01BypassPoll();
  });
}

function w01BypassPoll() {
  if (W01B.timer) return;
  W01B.timer = setTimeout(function () {
    W01B.timer = 0;
    var st = w01BypassApi("BypassState");
    if (!st) return;
    Promise.resolve(st()).then(function (s) {
      if (s) W01B.st = s;
      w01BypassDraw();
      if (s && s.running) w01BypassPoll();
      else w01BypassLoad();
    }).catch(function () { });
  }, 900);
}

function w01BypassWire() {
  var box = document.getElementById("w01-eg-bypassbox");
  if (!box || box._w01bwired) return;
  box._w01bwired = true;
  var run = document.getElementById("w01-bypass-run");
  var rst = document.getElementById("w01-bypass-reset");
  if (run) run.addEventListener("click", function () {
    var fn = w01BypassApi("BypassRun");
    if (!fn) { toast("биндинги Wails недоступны", "err"); return; }
    var n = +(((document.getElementById("w01-bypass-n") || {}).value) || 3);
    if (!(n >= 1 && n <= 5)) n = 3;
    var all = !!(document.getElementById("w01-bypass-all") || {}).checked;
    run.disabled = true;
    Promise.resolve(fn(n, (W01B.cat && W01B.cat.default_model) || "deepseek-v4.1-flash-free", all)).then(function (s) {
      if (s) W01B.st = s;
      w01BypassDraw();
      w01BypassPoll();
    }).catch(function (e) {
      toast(String((e && e.message) || e), "err");
    }).finally(function () { run.disabled = false; });
  });
  if (rst) rst.addEventListener("click", function () {
    var fn = w01BypassApi("BypassReset");
    if (!fn) return;
    Promise.resolve(fn()).then(function (a) {
      toast("параметры сброшены: " + w01BypassActiveText(a));
      w01BypassLoad();
    }).catch(function (e) { toast(String((e && e.message) || e), "err"); });
  });
  box.addEventListener("click", function (e) {
    var b = e.target && e.target.closest ? e.target.closest("[data-bapply]") : null;
    if (!b) return;
    var fn = w01BypassApi("BypassApply");
    if (!fn) return;
    b.disabled = true;
    Promise.resolve(fn(b.getAttribute("data-bapply"))).then(function (a) {
      toast("применено: " + w01BypassActiveText(a));
      w01BypassLoad();
    }).catch(function (er) {
      toast(String((er && er.message) || er), "err");
    }).finally(function () { b.disabled = false; });
  });
}

function w01BypassCells(row) {
  return (row.results || []).map(function (r) {
    var base = (r.base_before || 0) + " → " + (r.status || "ERR") + " → " + (r.base_after || 0);
    var cls = r.verdict === "unlocked" ? "ok" : (r.status === 200 ? "wait" : "bad");
    var txt = r.status === 200 ? "200" : (r.status ? String(r.status) : "ERR");
    if (r.verdict === "unlocked") txt = "200 снято";
    var tip = (r.ip || r.proxy || "") + " · база " + base + " · " + (r.ms || 0) + " ms" +
      (r.verdict ? " · " + r.verdict : "") + (r.body ? " · " + r.body : "") + (r.err ? " · " + r.err : "");
    return '<span class="w01-bypass-out ' + cls + '" title="' + esc(tip) + '">' + esc(txt) + "</span>";
  }).join("");
}

function w01BypassDraw() {
  var host = document.getElementById("w01-bypass-rows");
  if (!host) return;
  var st = W01B.st || {};
  var act = (W01B.cat && W01B.cat.active) || st.active || {};
  var meta = document.getElementById("w01-bypass-meta");
  if (meta) {
    var tp = (W01B.cat && W01B.cat.template) || st.template;
    var tps = tp ? (" · шаблон: " + (tp.tools || 0) + " тул. / " + ((tp.props || []).length) + " парам.") : " · шаблон MITM не найден";
    meta.textContent = DEMO ? "в демо недоступно" : ((st.running
      ? ("идёт: " + (st.phase || "…") + (st.lanes ? " · IP " + st.lanes.length : ""))
      : (st.finished_at ? ("готово · " + ((st.rows || []).length) + " вар.") : "свежие IP → варианты UA и имён параметров")) + tps);
  }
  var actEl = document.getElementById("w01-bypass-act");
  if (actEl) actEl.textContent = w01BypassActiveText(act);
  var runBtn = document.getElementById("w01-bypass-run");
  if (runBtn) runBtn.disabled = !!st.running;
  if (DEMO) {
    host.innerHTML = '<div class="w01-bypass-note">в демо-режиме перебор не запускается</div>';
    return;
  }
  var rows = st.rows || [];
  if (!rows.length) {
    host.innerHTML = '<div class="w01-bypass-note">' + (st.running ? "…" : "нет данных — нажми «Перебрать»") + "</div>";
    return;
  }
  var html = rows.map(function (r) {
    var win = (r.unlocked || 0) > 0 && r.axis !== "base";
    var apply = win
      ? '<button class="btn sm" type="button" data-bapply="' + esc(r.variant) + '">применить</button>'
      : "";
    var tag = "";
    if (win) tag = " · снято " + r.unlocked;
    else if ((r.drift || 0) > 0) tag = " · дрейф " + r.drift;
    else if ((r.uncertain || 0) > 0) tag = " · неясно " + r.uncertain;
    return '<div class="w01-bypass-row">' +
      '<span class="w01-bypass-name"' + (win ? ' data-win="1"' : "") + '>' + esc(r.label || r.variant) +
      ' <em>' + (r.pass || 0) + "/" + ((r.pass || 0) + (r.fail || 0)) + esc(tag) + "</em></span>" +
      '<span class="w01-bypass-cells">' + w01BypassCells(r) + "</span>" +
      apply +
      (r.results && r.results[0] && r.results[0].err ? '<span class="w01-bypass-err">' + esc(r.results[0].err) + "</span>" : "") +
      "</div>";
  }).join("");
  host.innerHTML = html;
}
