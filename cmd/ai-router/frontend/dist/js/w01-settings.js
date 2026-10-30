/* ==================================================================
   w01 «ПОЛОТНО» · вкладка «Настройки»
   Главный объект — ШКАЛА КОНФИГА: сплошные горизонтальные линии, у каждой
   значение висит строкой чисел над линией; слева — путь в S.cfg. Числа
   тянутся указателем/стрелками, булевы — переключателями на линии, строки и
   перечисления — нитями. Всё пишется в S.cfg сразу (cfgDirty), сохраняется
   кнопкой или автоматически при уходе с вкладки.
   ================================================================== */
"use strict";

var W01S = { values: [], wired: false, active: null, order: null };
var cfgDirty = false;
var W01S_SAVED = null;

/* ---------- доступ к S.cfg по точечному пути ---------- */
function w01CfgGet(path) {
  var p = String(path).split("."), o = S.cfg, i;
  for (i = 0; i < p.length; i++) { if (o == null) return undefined; o = o[p[i]]; }
  return o;
}
function w01CfgSet(path, val) {
  var p = String(path).split("."), o = S.cfg, i;
  if (!o) S.cfg = o = {};
  for (i = 0; i < p.length - 1; i++) { if (o[p[i]] == null || typeof o[p[i]] !== "object") o[p[i]] = {}; o = o[p[i]]; }
  o[p[p.length - 1]] = val;
}
function w01SNum(v, step) {
  var s = String(step == null ? 1 : step), d = (s.split(".")[1] || "").length;
  var n = +v;
  if (!isFinite(n)) n = 0;
  return n.toFixed(d);
}
function w01SPct(v, min, max) { return W01.clamp(((+v) - min) / Math.max(1e-9, max - min), 0, 1) * 100; }
function w01SClampStep(v, min, max, step) {
  var n = +v;
  if (!isFinite(n)) n = min;
  var k = Math.round((n - min) / step);
  n = min + k * step;
  var d = (String(step).split(".")[1] || "").length;
  return +n.toFixed(d + 2);
}
function w01SDirtyCount() {
  if (!W01S_SAVED) return 0;
  var a = JSON.stringify(W01S_SAVED), b = JSON.stringify(S.cfg || {});
  return a === b ? 0 : 1;
}

/* ---------- строки ---------- */
function w01Sh(title) { return '<div class="w01-sh"><span class="w01-sh-t">' + esc(title) + "</span><span class=\"w01-sh-r\"></span></div>"; }

function w01ScaleRow(o) {
  var v = w01CfgGet(o.k);
  if (v == null) v = o.def;
  var pct = w01SPct(v, o.min, o.max);
  return '<div class="w01-scale" data-k="' + esc(o.k) + '">' +
    '<div class="w01-scale-h"><span class="w01-scale-l">' + esc(o.l) + "</span>" +
    '<span class="w01-scale-u">' + esc(o.hint || "") + "</span>" +
    '<span class="w01-scale-v" data-v>' + esc(w01SNum(v, o.step)) + "</span></div>" +
    '<div class="w01-track" tabindex="0" role="slider" data-min="' + o.min + '" data-max="' + o.max + '" data-step="' + o.step + '"' +
    ' aria-label="' + W01.attr(o.l) + '" aria-valuemin="' + o.min + '" aria-valuemax="' + o.max + '" aria-valuenow="' + esc(String(v)) + '">' +
    '<span class="w01-track-g"></span>' +
    '<span class="w01-track-fill" data-fill style="width:' + W01.f1(pct) + '%"></span>' +
    '<span class="w01-track-knob" data-knob style="left:' + W01.f1(pct) + '%"></span>' +
    "</div>" +
    '<div class="w01-scale-f"><span>' + esc(w01SNum(o.min, o.step)) + "</span><span>" + esc(w01SNum(o.max, o.step)) + "</span></div>" +
    '<div class="w01-scale-p">' + esc(o.k) + "</div>" +
    "</div>";
}

function w01SwRow(o) {
  var on = !!w01CfgGet(o.k);
  return '<div class="w01-nrow" data-k="' + esc(o.k) + '">' +
    '<span class="w01-nrow-l">' + esc(o.l) + "</span>" +
    '<span class="w01-nrow-p">' + esc(o.k) + "</span>" +
    '<button class="w01-sw" type="button" role="switch" data-sw aria-checked="' + (on ? "true" : "false") + '" aria-label="' + W01.attr(o.l) + '">' +
    '<span class="w01-sw-t"></span><span class="w01-sw-x">' + (on ? "включено" : "выключено") + "</span></button>" +
    "</div>";
}

function w01InpRow(o) {
  var v = w01CfgGet(o.k);
  if (v == null) v = "";
  var inp = '<input class="w01-inp" data-in type="' + (o.pw ? "password" : "text") + '" value="' + W01.attr(String(v)) + '" aria-label="' + W01.attr(o.l) + '" spellcheck="false" autocomplete="off">';
  if (o.pw) inp += '<button class="w01-eye" type="button" data-reveal aria-label="Показать значение">показать</button>';
  return '<div class="w01-nrow" data-k="' + esc(o.k) + '">' +
    '<span class="w01-nrow-l">' + esc(o.l) + "</span>" +
    '<span class="w01-nrow-p">' + esc(o.k) + "</span>" +
    inp + "</div>";
}

function w01SelRow(o) {
  var v = String(w01CfgGet(o.k) == null ? "" : w01CfgGet(o.k));
  var opts = o.opts.slice();
  if (v && opts.indexOf(v) < 0) opts.push(v);
  var h = "";
  for (var i = 0; i < opts.length; i++) h += '<option value="' + W01.attr(opts[i]) + '"' + (opts[i] === v ? " selected" : "") + ">" + esc(opts[i]) + "</option>";
  return '<div class="w01-nrow" data-k="' + esc(o.k) + '">' +
    '<span class="w01-nrow-l">' + esc(o.l) + "</span>" +
    '<span class="w01-nrow-p">' + esc(o.k) + "</span>" +
    '<select class="w01-inp w01-sel" data-in aria-label="' + W01.attr(o.l) + '">' + h + "</select></div>";
}
/* ---------- что именно изменено относительно загруженного конфига ---------- */
function w01SDiff() {
  var out = [];
  var walk = function (a, b, p) {
    var ao = a && typeof a === "object", bo = b && typeof b === "object";
    if (ao || bo) {
      var keys = {}, i, k;
      if (ao) for (k in a) if (Object.prototype.hasOwnProperty.call(a, k)) keys[k] = 1;
      if (bo) for (k in b) if (Object.prototype.hasOwnProperty.call(b, k)) keys[k] = 1;
      var list = Object.keys(keys);
      for (i = 0; i < list.length; i++) walk(ao ? a[list[i]] : undefined, bo ? b[list[i]] : undefined, p ? p + "." + list[i] : list[i]);
      return;
    }
    if (a !== b) out.push(p || "(корень)");
  };
  if (!W01S_SAVED) return out;
  walk(W01S_SAVED, S.cfg || {}, "", out);
  return out;
}

/* ---------- порядок уровней маршрутизации: лестница, не таблица ---------- */
function w01TiersLadder() {
  var r = (S.cfg && S.cfg.routing) || {}, t = r.tiers || [], def = String(r.default_tier || "");
  var h = w01Sh("Порядок уровней маршрутизации");
  if (!t.length) return h + '<div class="w01-flat">ядро не отдало список уровней (S.cfg.routing.tiers пуст)</div>';
  for (var i = 0; i < t.length; i++) {
    var name = String(t[i]);
    h += '<div class="w01-lad" data-rank="' + i + '">' +
      '<span class="w01-lad-n">' + (i + 1) + "</span>" +
      '<span class="w01-lad-t">' + esc(name) + "</span>" +
      '<span class="w01-lad-l"></span>' +
      (name === def ? '<span class="w01-lad-d">по умолчанию</span>'
        : '<button class="w01-lad-b" type="button" data-def="' + i + '">сделать по умолчанию</button>') +
      '<button class="w01-lad-b" type="button" data-up="' + i + '"' + (i === 0 ? " disabled" : "") + ' aria-label="Поднять уровень">выше</button>' +
      '<button class="w01-lad-b" type="button" data-down="' + i + '"' + (i === t.length - 1 ? " disabled" : "") + ' aria-label="Опустить уровень">ниже</button>' +
      "</div>";
  }
  return h + '<div class="w01-flat">порядок = приоритет при равных условиях: первый уровень пробуется первым</div>';
}

/* ---------- полоса модулей: одна лента, сегменты по состоянию ---------- */
function w01ModulesList() {
  var g = function (k, d) { var v = w01CfgGet(k); return v == null ? d : v; };
  return [
    { n: "Egress-пул", k: "egress.enabled", on: !!g("egress.enabled", false), d: "каналы " + String(g("egress.base_url", "—")) },
    { n: "Ротация на запрос", k: "egress.rotation.per_request", on: !!g("egress.rotation.per_request", false), d: "rotate_after=" + String(g("egress.rotation.rotate_after", "—")) },
    { n: "Предохранитель", k: "resilience.breaker.max_failures", on: +g("resilience.breaker.max_failures", 0) > 0, d: "сбоев до размыкания: " + String(g("resilience.breaker.max_failures", "—")) },
    { n: "Кулдаун", k: "resilience.cooldown.factor", on: +g("resilience.cooldown.factor", 0) > 0, d: "рост задержки ×" + String(g("resilience.cooldown.factor", "—")) },
    { n: "Локаут", k: "resilience.lockout.max_failures", on: +g("resilience.lockout.max_failures", 0) > 0, d: "сбоев в окне: " + String(g("resilience.lockout.max_failures", "—")) },
    { n: "Подмена ключей", k: "provider_defaults.key_swap", on: !!g("provider_defaults.key_swap", ""), d: String(g("provider_defaults.key_swap", "—")) },
    { n: "Учёт стоимости", k: "routing.costs.enabled", on: !!g("routing.costs.enabled", false), d: "в маршрутизации" }
  ];
}

function renderModules(host) {
  var root = host || (document.getElementById("w01-cf-modules") ? "#w01-cf-modules" : null);
  var el = (typeof root === "string") ? document.querySelector(root) : root;
  if (!el) return;
  var mods = w01ModulesList(), h = "";
  for (var i = 0; i < mods.length; i++) {
    var m = mods[i];
    h += '<button class="w01-mod ' + (m.on ? "w01-mod-on" : "w01-mod-off") + '" type="button" data-goto="' + esc(m.k) + '" title="' + W01.attr(m.n + " · " + m.d + " · " + m.k) + '">' +
      '<span class="w01-mod-t">' + esc(m.n) + "</span>" +
      '<span class="w01-mod-s">' + (m.on ? "вкл" : "выкл") + "</span>" +
      '<span class="w01-mod-d">' + esc(m.d) + "</span></button>";
  }
  el.innerHTML = h;
}
/* ---------- схема: что именно выведено на полотно ---------- */
function w01ScaleDefs() {
  return [
    { k: "resilience.breaker.max_failures", l: "предохранитель · сбоев до размыкания", min: 1, max: 50, step: 1, hint: "штук" },
    { k: "resilience.breaker.recovery_seconds", l: "предохранитель · пауза перед пробой", min: 5, max: 600, step: 5, hint: "сек" },
    { k: "resilience.breaker.half_open_successes", l: "предохранитель · успехов для возврата", min: 1, max: 10, step: 1, hint: "штук" },
    { k: "resilience.cooldown.base_seconds", l: "кулдаун · базовая пауза", min: 1, max: 120, step: 1, hint: "сек" },
    { k: "resilience.cooldown.max_seconds", l: "кулдаун · предел паузы", min: 30, max: 3600, step: 30, hint: "сек" },
    { k: "resilience.cooldown.factor", l: "кулдаун · множитель роста", min: 1, max: 5, step: 0.1, hint: "×" },
    { k: "resilience.lockout.max_failures", l: "локаут · сбоев в окне", min: 1, max: 50, step: 1, hint: "штук" },
    { k: "resilience.lockout.window_seconds", l: "локаут · окно подсчёта", min: 5, max: 600, step: 5, hint: "сек" },
    { k: "resilience.lockout.lockout_seconds", l: "локаут · длительность блокировки", min: 10, max: 3600, step: 10, hint: "сек" },
    { k: "server.port", l: "сервер · порт шлюза", min: 1024, max: 65535, step: 1, hint: "TCP" },
    { k: "egress.rotation.rotate_after", l: "egress · запросов до ротации", min: 1, max: 100, step: 1, hint: "штук" }
  ];
}
function w01SwDefs() {
  return [
    { k: "egress.enabled", l: "egress-пул · каналы включены" },
    { k: "egress.rotation.per_request", l: "egress · ротация на каждый запрос" },
    { k: "routing.costs.enabled", l: "маршрутизация · учитывать стоимость" }
  ];
}
function w01InpDefs() {
  return [
    { k: "server.host", l: "сервер · адрес прослушивания" },
    { k: "egress.base_url", l: "egress · адрес пула" },
    { k: "egress.token", l: "egress · токен пула", pw: true }
  ];
}
function w01SelDefs() {
  return [
    { k: "server.log_level", l: "сервер · уровень журнала", opts: ["trace", "debug", "info", "warn", "error"] },
    { k: "routing.strategy", l: "маршрутизация · стратегия", opts: ["priority", "round_robin", "cheapest", "random", "weighted"] },
    { k: "egress.strategy", l: "egress · стратегия выбора канала", opts: ["round_robin", "random", "sticky", "least_latency"] },
    { k: "provider_defaults.key_swap", l: "провайдеры · подмена ключа", opts: ["round_robin", "random", "sticky", "least_used"] }
  ];
}
function w01PolicyDefs() {
  var t = (S.cfg && S.cfg.routing && S.cfg.routing.tiers) || [];
  var defs = [
    { k: "provider_defaults.error_policy.401", l: "ошибка 401 · ключ отклонён" },
    { k: "provider_defaults.error_policy.429", l: "ошибка 429 · лимит" },
    { k: "provider_defaults.error_policy.5xx", l: "ошибка 5xx · сбой провайдера" },
    { k: "provider_defaults.error_policy.timeout", l: "таймаут ответа" },
    { k: "provider_defaults.error_policy.network", l: "сетевой сбой" },
    { k: "provider_defaults.error_policy.rate_limit", l: "локальный rate limit" }
  ];
  var catalog = ["retry", "retry_token", "retry_proxy", "swap_key", "swap_key_proxy", "fail", "skip", "disable"];
  var i;
  for (i = 0; i < defs.length; i++) {
    defs[i].opts = catalog.slice();
    var cur = w01CfgGet(defs[i].k);
    if (cur && defs[i].opts.indexOf(String(cur)) < 0) defs[i].opts.push(String(cur));
  }
  void t;
  return defs;
}

/* ---------- карточки: компоновка «пульта» (w06) + строки-шкалы (w01) ---------- */
function mixCard(title, hint, inner) {
  return '<div class="card section mix-card">' +
    '<div class="card-head"><h3>' + esc(title) + '</h3><span class="hint">' + esc(hint) + '</span><span class="spacer"></span></div>' +
    inner + "</div>";
}
function mixRowsHTML() {
  var h = "", i, defs;
  var pick = function (arr, keys) {
    var out = [];
    for (var j = 0; j < keys.length; j++)
      for (var k = 0; k < arr.length; k++) if (arr[k].k === keys[j]) out.push(arr[k]);
    return out;
  };
  var scales = w01ScaleDefs(), sw = w01SwDefs(), inp = w01InpDefs(), sel = w01SelDefs(), pol = w01PolicyDefs();

  /* 1. шлюз и протокол */
  defs = pick(inp, ["server.host"]);
  var c1 = '<div class="w01-nrows">';
  for (i = 0; i < defs.length; i++) c1 += w01InpRow(defs[i]);
  defs = pick(scales, ["server.port"]);
  c1 += "</div><div class=\"w01-scales\">";
  for (i = 0; i < defs.length; i++) c1 += w01ScaleRow(defs[i]);
  c1 += "</div><div class=\"w01-nrows\">";
  defs = pick(sel, ["server.log_level"]);
  for (i = 0; i < defs.length; i++) c1 += w01SelRow(defs[i]);
  c1 += "</div>";
  h += mixCard("Шлюз и протокол", "пульт ядра", c1);

  /* 2. сбои, выдержки, блокировки */
  defs = scales.filter(function (d) { return d.k.indexOf("resilience.") === 0; });
  var c2 = '<div class="w01-scales">';
  for (i = 0; i < defs.length; i++) c2 += w01ScaleRow(defs[i]);
  h += mixCard("Сбои, выдержки, блокировки", "защита", c2 + "</div>");

  /* 3. выбор провайдера */
  defs = pick(sel, ["routing.strategy"]);
  var c3 = '<div class="w01-nrows">';
  for (i = 0; i < defs.length; i++) c3 += w01SelRow(defs[i]);
  defs = pick(sw, ["routing.costs.enabled"]);
  for (i = 0; i < defs.length; i++) c3 += w01SwRow(defs[i]);
  c3 += "</div>" + w01TiersLadder();
  h += mixCard("Выбор провайдера", "маршрут", c3);

  /* 4. каналы и ротация */
  defs = pick(sw, ["egress.enabled", "egress.rotation.per_request"]);
  var c4 = '<div class="w01-nrows">';
  c4 += w01SwRow(defs[0]);
  defs = pick(inp, ["egress.base_url", "egress.token"]);
  for (i = 0; i < defs.length; i++) c4 += w01InpRow(defs[i]);
  defs = pick(sel, ["egress.strategy"]);
  for (i = 0; i < defs.length; i++) c4 += w01SelRow(defs[i]);
  c4 += w01SwRow(pick(sw, ["egress.rotation.per_request"])[0]) + "</div>";
  defs = pick(scales, ["egress.rotation.rotate_after"]);
  c4 += '<div class="w01-scales">';
  for (i = 0; i < defs.length; i++) c4 += w01ScaleRow(defs[i]);
  h += mixCard("Каналы и ротация", "egress", c4 + "</div>");

  /* 5. поведение у провайдеров */
  defs = pick(sel, ["provider_defaults.key_swap"]).concat(pol);
  var c5 = '<div class="w01-nrows">';
  for (i = 0; i < defs.length; i++) c5 += w01SelRow(defs[i]);
  h += mixCard("Поведение у провайдеров", "по умолчанию", c5 + "</div>");

  /* 6. модули ядра */
  h += mixCard("Модули ядра", "сборка", '<div class="w01-modband" id="w01-cf-modules"></div>');
  return h;
}
/* ---------- строки чисел ---------- */
function w01SNums() {
  var c = S.cfg || {}, s = c.server || {}, r = c.routing || {}, e = c.egress || {}, res = c.resilience || {};
  var diff = w01SDiff();
  var items = [];
  items.push([esc(String((S.status && S.status.config) || "—")), "файл конфига", "", "S.status.config"]);
  items.push([esc(String(s.host || "—")) + ":" + esc(String(s.port == null ? "—" : s.port)), "шлюз", "w01-hi", "S.cfg.server"]);
  items.push([esc(String(s.log_level || "—")), "журнал", "", "S.cfg.server.log_level"]);
  items.push([esc(String(r.strategy || "—")), "стратегия", "", "S.cfg.routing.strategy"]);
  items.push([nf((r.tiers || []).length), "уровней", "", "S.cfg.routing.tiers"]);
  items.push([esc(String(r.default_tier || "—")), "уровень по умолчанию", "", "S.cfg.routing.default_tier"]);
  items.push([esc(String(e.strategy || "—")), "выбор канала", "", "S.cfg.egress.strategy"]);
  items.push([esc(String(((res.cooldown || {}).factor) == null ? "—" : ((res.cooldown || {}).factor))), "множитель кулдауна", "", "S.cfg.resilience.cooldown.factor"]);
  items.push([diff.length ? "есть правки" : "синхронно с ядром", "состояние конфига", diff.length ? "w01-warnc" : "w01-okc", "cfgDirty"]);
  items.push([nf(diff.length), "полей изменено", diff.length ? "w01-hi" : "", "сравнение с загруженным"]);
  if (diff.length) items.push([esc(W01.cut(diff.slice(0, 3).join(", "), 46)), "первые правки", "", ""]);
  return W01.numline(items);
}

function w01SLiveNums() {
  var h = S.health || [], end = S.end || [], br = S.breakers || [], lk = S.locks || [], i, ok = 0;
  for (i = 0; i < h.length; i++) if (h[i] && (h[i].ok === true || h[i].status === "ok" || h[i].healthy === true)) ok++;
  var items = [[nf(end.length), "провайдеров в ядре", "", "S.end"]];
  items.push([nf(((S.status || {}).models) || 0), "моделей", "", "S.status.models"]);
  items.push([nf(((S.keys || []).length)), "ключей", "", "S.keys"]);
  items.push([nf(ok) + "/" + nf(h.length), "health ok", ok === h.length ? "w01-okc" : "w01-warnc", "S.health"]);
  items.push([nf(br.length), "разомкнуто цепей", br.length ? "w01-warnc" : "w01-okc", "S.breakers"]);
  items.push([nf(lk.length), "заблокировано", lk.length ? "w01-warnc" : "w01-okc", "S.locks"]);
  if (S.status && S.status.version) items.push([esc(String(S.status.version)), "версия", "", "S.status.version"]);
  if (S.status && S.status.uptime_s != null) items.push([Math.floor(S.status.uptime_s / 60) + " мин", "аптайм", "", "S.status.uptime_s"]);
  return W01.numline(items);
}

function w01SNote() {
  var dirty = cfgDirty ? "правки лежат в S.cfg и уйдут в ядро по кнопке «сохранить» (или автоматически при уходе с вкладки)." : "все значения совпадают с загруженным конфигом.";
  return "тяните линию мышью или правьте стрелками (таблица фокуса: ←→ на шаг, Home/End в границы); " + dirty;
}
/* ---------- запись значения в S.cfg и обновление одной строки ---------- */
function w01SRowPaint(row) {
  var k = row.getAttribute("data-k"), tr = row.querySelector(".w01-track");
  if (!tr) return;
  var min = +tr.getAttribute("data-min"), max = +tr.getAttribute("data-max"), step = +tr.getAttribute("data-step");
  var v = w01CfgGet(k);
  if (v == null) return;
  var pct = w01SPct(v, min, max);
  var fill = row.querySelector("[data-fill]"), knob = row.querySelector("[data-knob]"), val = row.querySelector("[data-v]");
  if (fill) fill.style.width = W01.f1(pct) + "%";
  if (knob) knob.style.left = W01.f1(pct) + "%";
  if (val) val.textContent = w01SNum(v, step);
  tr.setAttribute("aria-valuenow", String(v));
  if (row.classList) row.classList.toggle("w01-scale-dirty", w01SDiff().indexOf(k) >= 0);
}

function w01SChanged() {
  var d = w01SDiff();
  cfgDirty = d.length > 0;
  W01.setHTML("w01-cf-nums", w01SNums());
  W01.setHTML("w01-cf-note", w01SNote());
  renderModules();
  var badge = document.getElementById("w01-cf-dirty");
  if (badge) {
    badge.textContent = cfgDirty ? "не сохранено" : "синхронно";
    if (badge.classList) badge.classList.toggle("w01-warnc", cfgDirty);
  }
}

function w01SSetNum(row, raw) {
  var tr = row.querySelector(".w01-track");
  if (!tr) return;
  var v = w01SClampStep(raw, +tr.getAttribute("data-min"), +tr.getAttribute("data-max"), +tr.getAttribute("data-step"));
  w01CfgSet(row.getAttribute("data-k"), v);
  w01SRowPaint(row);
  w01SChanged();
}

function w01STrackFromX(tr, clientX) {
  var r = tr.getBoundingClientRect();
  var p = W01.clamp((clientX - r.left) / Math.max(1, r.width), 0, 1);
  var min = +tr.getAttribute("data-min"), max = +tr.getAttribute("data-max");
  return min + p * (max - min);
}

function w01SScaleRowOf(el) { return el && el.closest ? el.closest(".w01-scale") : null; }

function w01SSwPaint(btn, on) {
  btn.setAttribute("aria-checked", on ? "true" : "false");
  var x = btn.querySelector(".w01-sw-x");
  if (x) x.textContent = on ? "включено" : "выключено";
  if (btn.classList) btn.classList.toggle("w01-sw-on", on);
}

/* ---------- сборка вкладки ---------- */
function w01SettingsDraw() {
  var ed = document.getElementById("settings-editor");
  if (!ed) return;
  if (!S.cfg) {
    ed.innerHTML = emptyHint("ядро не отдало конфиг: GetConfig вернул пусто");
    return;
  }
  ed.innerHTML =
    '<div class="w01-nums" id="w01-cf-nums">' + w01SNums() + "</div>" +
    '<div class="w01-note" id="w01-cf-note">' + esc(w01SNote()) + "</div>" +
    '<div class="mix-cards">' + mixRowsHTML() + "</div>" +
    '<div class="w01-nums" id="w01-cf-live">' + w01SLiveNums() + "</div>" +
    '<div class="w01-flat">шкалы показывают положение значения внутри допустимого диапазона; путь ключа под каждой линией</div>';
  renderModules();
}
/* ---------- события ---------- */
function w01SettingsWire() {
  var ed = document.getElementById("settings-editor");
  if (!ed || ed._w01wired) return;
  ed._w01wired = true;
  W01S.wired = true;

  ed.addEventListener("pointerdown", function (e) {
    var tr = e.target && e.target.closest ? e.target.closest(".w01-track") : null;
    if (!tr) return;
    e.preventDefault();
    W01S.active = tr;
    try { tr.setPointerCapture(e.pointerId); } catch (eC) {}
    var row = w01SScaleRowOf(tr);
    if (row) w01SSetNum(row, w01STrackFromX(tr, e.clientX));
    try { tr.focus({ preventScroll: true }); } catch (eF) {}
  });
  ed.addEventListener("pointermove", function (e) {
    var tr = W01S.active;
    if (!tr) return;
    var row = w01SScaleRowOf(tr);
    if (row) w01SSetNum(row, w01STrackFromX(tr, e.clientX));
  });
  var release = function (e) {
    var tr = W01S.active;
    if (!tr) return;
    W01S.active = null;
    try { tr.releasePointerCapture(e.pointerId); } catch (eR) {}
  };
  ed.addEventListener("pointerup", release);
  ed.addEventListener("pointercancel", release);

  ed.addEventListener("keydown", function (e) {
    var tr = e.target && e.target.closest ? e.target.closest(".w01-track") : null;
    if (!tr) return;
    var step = +tr.getAttribute("data-step"), min = +tr.getAttribute("data-min"), max = +tr.getAttribute("data-max");
    var row = w01SScaleRowOf(tr);
    if (!row) return;
    var cur = +w01CfgGet(row.getAttribute("data-k"));
    var v = null;
    if (e.key === "ArrowRight" || e.key === "ArrowUp") v = cur + step;
    else if (e.key === "ArrowLeft" || e.key === "ArrowDown") v = cur - step;
    else if (e.key === "PageUp") v = cur + step * 5;
    else if (e.key === "PageDown") v = cur - step * 5;
    else if (e.key === "Home") v = min;
    else if (e.key === "End") v = max;
    if (v === null) return;
    e.preventDefault();
    w01SSetNum(row, v);
  });

  ed.addEventListener("change", function (e) {
    var el = e.target;
    if (!el || !el.getAttribute) return;
    var row = el.closest ? el.closest(".w01-nrow") : null;
    if (!row || !el.hasAttribute("data-in")) return;
    var k = row.getAttribute("data-k");
    var v = el.value;
    if (el.tagName === "INPUT" && /^server\.port$/.test(k)) v = +v;
    w01CfgSet(k, v);
    w01SChanged();
    if (k === "egress.base_url" || k === "egress.rotation.rotate_after" || (k.indexOf("resilience.") === 0)) {
      renderModules();
    }
  });

  ed.addEventListener("click", function (e) {
    var t = e.target;
    var sw = t && t.closest ? t.closest("[data-sw]") : null;
    if (sw) {
      var row = sw.closest(".w01-nrow");
      var k = row && row.getAttribute("data-k");
      if (k) {
        var on = !w01CfgGet(k);
        w01CfgSet(k, on);
        w01SSwPaint(sw, on);
        w01SChanged();
      }
      return;
    }
    var rev = t && t.closest ? t.closest("[data-reveal]") : null;
    if (rev) {
      var inp = rev.closest(".w01-nrow").querySelector("[data-in]");
      if (inp) {
        var show = inp.type === "password";
        inp.type = show ? "text" : "password";
        rev.textContent = show ? "скрыть" : "показать";
      }
      return;
    }
    var up = t && t.closest ? t.closest("[data-up]") : null;
    var dn = t && t.closest ? t.closest("[data-down]") : null;
    var df = t && t.closest ? t.closest("[data-def]") : null;
    var gt = t && t.closest ? t.closest("[data-goto]") : null;
    if (gt) { w01SReveal(gt.getAttribute("data-goto")); return; }
    if (!up && !dn && !df) return;
    var r = (S.cfg && S.cfg.routing) || {}, tiers = (r.tiers || []).slice();
    var i;
    if (df) {
      r.default_tier = tiers[+df.getAttribute("data-def")];
    } else {
      i = +((up || dn).getAttribute(up ? "data-up" : "data-down"));
      var j = up ? i - 1 : i + 1;
      if (j < 0 || j >= tiers.length) return;
      var tmp = tiers[i]; tiers[i] = tiers[j]; tiers[j] = tmp;
      r.tiers = tiers;
    }
    w01CfgSet("routing", r);
    W01.setHTML("w01-cf-tiers", w01TiersLadder());
    w01SChanged();
  });

  var save = document.getElementById("w01-cf-save");
  if (save && !save._w01wired) { save._w01wired = true; save.addEventListener("click", function () { saveCfg(); }); }
  var rev2 = document.getElementById("w01-cf-revert");
  if (rev2 && !rev2._w01wired) { rev2._w01wired = true; rev2.addEventListener("click", function () { w01SRevert(); }); }
}

function w01SReveal(k) {
  var rows = document.querySelectorAll("#w01-cf-rows [data-k]"), i;
  for (i = 0; i < rows.length; i++) {
    if (rows[i].getAttribute("data-k") === k) {
      var tr = rows[i].querySelector(".w01-track");
      if (tr) { try { tr.focus({ preventScroll: true }); } catch (e) {} }
      try { rows[i].scrollIntoView({ block: "center", behavior: "smooth" }); } catch (e2) { try { rows[i].scrollIntoView(); } catch (e3) {} }
      if (rows[i].classList) {
        rows[i].classList.add("w01-flash");
        setTimeout(function () { try { rows[i].classList.remove("w01-flash"); } catch (e4) {} }, 900);
      }
      toast("настройка " + k + " — на полотне");
      return;
    }
  }
  toast("эта настройка на полотно не выведена: " + k);
}
/* ---------- загрузка/сохранение ---------- */
async function loadSettings() {
  var ed = document.getElementById("settings-editor");
  if (!DEMO) {
    if (cfgDirty) { toast("есть несохранённые правки: сохраните или откатите — конфиг не перечитываю"); renderSettings(); return; }
    try {
      if (typeof API !== "undefined" && API && API.GetConfig) {
        var c = await API.GetConfig();
        if (c) S.cfg = c;
      }
    } catch (e) {
      if (ed) ed.innerHTML = errHint(e);
      return;
    }
  }
  if (!S.cfg) { if (ed) ed.innerHTML = emptyHint("ядро не отдало конфиг (GetConfig вернул пусто)"); return; }
  W01S_SAVED = JSON.parse(JSON.stringify(S.cfg));
  cfgDirty = false;
  renderSettings();
}

function renderSettings(host) {
  var ed = host || document.getElementById("settings-editor");
  if (!ed) return;
  w01SettingsWire();
  w01SettingsDraw();
  var badge = document.getElementById("w01-cf-dirty");
  if (badge) {
    badge.textContent = cfgDirty ? "не сохранено" : "синхронно";
    if (badge.classList) badge.classList.toggle("w01-warnc", cfgDirty);
  }
  var live = document.getElementById("settings-live-txt");
  if (live) live.textContent = DEMO ? "● DEMO — правки в S.cfg" : "● LIVE";
  var meta = document.getElementById("settings-meta");
  if (meta && S.cfg) {
    var s = S.cfg.server || {};
    meta.textContent = "конфиг ядра · " + String(s.host || "—") + ":" + String(s.port == null ? "—" : s.port) +
      " · путь правок: S.cfg → SaveConfig";
  }
}

function saveCfg(opts) {
  opts = opts || {};
  var diff = w01SDiff();
  if (!diff.length) { if (!opts.auto) toast("изменений нет — конфиг совпадает с ядром"); return Promise.resolve(false); }
  if (DEMO || typeof API === "undefined" || !API || !API.SaveConfig) {
    W01S_SAVED = JSON.parse(JSON.stringify(S.cfg || {}));
    cfgDirty = false;
    renderSettings();
    toast(opts.auto ? "демо: правки зафиксированы в S.cfg (ядру не пишем)" : "демо: правки зафиксированы в S.cfg, запись в ядро отключена");
    return Promise.resolve(false);
  }
  var btn = document.getElementById("w01-cf-save");
  if (btn) btn.disabled = true;
  var payload = JSON.parse(JSON.stringify(S.cfg));
  return Promise.resolve(API.SaveConfig(payload)).then(function (r) {
    W01S_SAVED = JSON.parse(JSON.stringify(S.cfg));
    cfgDirty = false;
    if (btn) btn.disabled = false;
    renderSettings();
    toast("конфиг сохранён: " + nf(diff.length) + " поле(й)");
    return r;
  }).catch(function (e) {
    if (btn) btn.disabled = false;
    toast("не удалось сохранить: " + ((e && e.message) || e), "err");
    return false;
  });
}

function w01SRevert() {
  if (!W01S_SAVED) { toast("нечего откатывать: конфиг ещё не загружен"); return; }
  if (!cfgDirty) { toast("правок нет"); return; }
  S.cfg = JSON.parse(JSON.stringify(W01S_SAVED));
  cfgDirty = false;
  renderSettings();
  toast("правки откатаны к загруженному конфигу");
}

/* ---------- инициализация ---------- */
function w01SettingsInit() {
  w01SettingsWire();
  if (document.getElementById("settings-editor") && document.getElementById("w01-cf-dirty")) {
    var badge = document.getElementById("w01-cf-dirty");
    badge.textContent = cfgDirty ? "не сохранено" : "синхронно";
  }
  if (S.tab === "settings" && (S.cfg || DEMO)) {
    if (!W01S_SAVED && S.cfg) W01S_SAVED = JSON.parse(JSON.stringify(S.cfg));
    w01SettingsDraw();
  }
}

if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", w01SettingsInit);
else setTimeout(w01SettingsInit, 0);
