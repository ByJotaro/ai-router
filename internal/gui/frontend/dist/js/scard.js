/* ==================================================================
   scard — вкладка «Настройки»: реестр параметров ядра.
   Один лист: полоса категорий сверху (единственный липкий элемент),
   ниже разделы с параметрами. Строка = подпись + путь в S.cfg + поле;
   изменённая строка показывает «было N», значение — акцентом, справа
   появляется отмена одной правки. Значения пишутся в S.cfg сразу,
   cfgDirty считается тем же w01SDiff(), что и сохранение в ядро.
   ================================================================== */
"use strict";

(function () {
  var CATS = [
    {
      id: "gw", n: "01", key: "server", s: ["Шлюз", "Gateway"], t: ["Шлюз и вход", "Gateway and access"],
      rows: [
        { k: "server.host", l: ["Адрес прослушивания", "Listen address"], t: "text" },
        { k: "server.port", l: ["Порт шлюза", "Gateway port"], t: "num", min: 1024, max: 65535, step: 1, u: "TCP" },
        { k: "server.log_level", l: ["Уровень журнала", "Log level"], t: "sel", opts: ["trace", "debug", "info", "warn", "error"] },
        { k: "admin_token", l: ["Admin-токен", "Admin token"], t: "pass" }
      ]
    },
    {
      id: "res", n: "02", key: "resilience", s: ["Защита", "Locks"], t: ["Сбои, паузы, блокировки", "Failures, pauses, locks"],
      rows: [
        { k: "resilience.breaker.max_failures", l: ["Сбоев до размыкания", "Failures to open"], t: "num", min: 1, max: 50, step: 1, u: "шт" },
        { k: "resilience.breaker.recovery_seconds", l: ["Пауза перед пробой", "Pause before probe"], t: "num", min: 5, max: 600, step: 5, u: "сек" },
        { k: "resilience.breaker.half_open_successes", l: ["Успехов для возврата", "Successes to close"], t: "num", min: 1, max: 10, step: 1, u: "шт" },
        { k: "resilience.cooldown.base_seconds", l: ["Базовая пауза", "Base pause"], t: "num", min: 1, max: 120, step: 1, u: "сек" },
        { k: "resilience.cooldown.max_seconds", l: ["Предел паузы", "Max pause"], t: "num", min: 30, max: 3600, step: 30, u: "сек" },
        { k: "resilience.cooldown.factor", l: ["Множитель роста", "Growth factor"], t: "num", min: 1, max: 5, step: 0.1, u: "×" },
        { k: "resilience.lockout.max_failures", l: ["Сбоев в окне", "Failures in window"], t: "num", min: 1, max: 50, step: 1, u: "шт" },
        { k: "resilience.lockout.window_seconds", l: ["Окно подсчёта", "Window"], t: "num", min: 5, max: 600, step: 5, u: "сек" },
        { k: "resilience.lockout.lockout_seconds", l: ["Длительность блокировки", "Block time"], t: "num", min: 10, max: 3600, step: 10, u: "сек" }
      ]
    },
    {
      id: "route", n: "03", key: "routing", s: ["Маршрут", "Routing"], t: ["Маршрут и уровни", "Routing and tiers"], tiers: true,
      rows: [
        { k: "routing.strategy", l: ["Стратегия провайдера", "Provider strategy"], t: "sel", opts: ["priority", "round_robin", "cheapest", "random", "weighted"] },
        { k: "routing.costs.enabled", l: ["Учитывать стоимость", "Consider cost"], t: "sw" }
      ]
    },
    {
      id: "egress", n: "04", key: "egress", s: ["Каналы", "Egress"], t: ["Каналы и ротация", "Egress and rotation"],
      rows: [
        { k: "egress.enabled", l: ["Пул каналов включён", "Channel pool enabled"], t: "sw" },
        { k: "egress.base_url", l: ["Адрес пула", "Pool address"], t: "text" },
        { k: "egress.token", l: ["Токен пула", "Pool token"], t: "pass" },
        { k: "egress.strategy", l: ["Выбор канала", "Channel pick"], t: "sel", opts: ["round_robin", "random", "sticky", "least_latency"] },
        { k: "egress.rotation.per_request", l: ["Ротация на запрос", "Rotate per request"], t: "sw" },
        { k: "egress.rotation.rotate_after", l: ["Запросов до ротации", "Requests before rotate"], t: "num", min: 1, max: 100, step: 1, u: "шт" }
      ]
    },
    {
      id: "prov", n: "05", key: "provider_defaults", s: ["Провайдеры", "Providers"], t: ["Поведение у провайдеров", "Provider defaults"],
      rows: [
        { k: "provider_defaults.key_swap", l: ["Подмена ключа", "Key swap"], t: "sel", opts: ["round_robin", "random", "sticky", "least_used"] },
        { k: "provider_defaults.error_policy.401", l: ["401 · ключ отклонён", "401 · key rejected"], t: "sel", pol: 1 },
        { k: "provider_defaults.error_policy.429", l: ["429 · лимит провайдера", "429 · upstream limit"], t: "sel", pol: 1 },
        { k: "provider_defaults.error_policy.5xx", l: ["5xx · сбой провайдера", "5xx · provider failure"], t: "sel", pol: 1 },
        { k: "provider_defaults.error_policy.timeout", l: ["Таймаут ответа", "Response timeout"], t: "sel", pol: 1 },
        { k: "provider_defaults.error_policy.network", l: ["Сетевой сбой", "Network failure"], t: "sel", pol: 1 },
        { k: "provider_defaults.error_policy.rate_limit", l: ["Локальный rate limit", "Local rate limit"], t: "sel", pol: 1 }
      ]
    },
    {
      id: "ui", n: "06", key: "ui", s: ["Интерфейс", "Interface"], t: ["Интерфейс", "Interface"],
      rows: [
        { k: "ui.language", l: ["Язык интерфейса", "Interface language"], t: "lang" }
      ]
    }
  ];
  var POLICY = ["retry", "retry_token", "retry_proxy", "swap_key", "swap_key_proxy", "fail", "skip", "disable"];

  function en() { return typeof I18N !== "undefined" && I18N && I18N.lang === "en"; }
  function L(ru, e) { return en() ? (e || ru) : ru; }
  function esc_(s) { return typeof esc === "function" ? esc(s) : String(s == null ? "" : s).replace(/[&<>"']/g, function (c) { return ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]; }); }
  function say(m, kind) { try { if (typeof toast === "function") toast(m, kind); } catch (e) {} }

  /* ---------- доступ к S.cfg по точечному пути ---------- */
  function getPath(path) {
    var p = String(path).split("."), o = (typeof S !== "undefined" && S) ? S.cfg : null, i;
    for (i = 0; i < p.length; i++) { if (o == null) return undefined; o = o[p[i]]; }
    return o;
  }
  function setPath(path, val) {
    if (typeof S === "undefined" || !S) return;
    var p = String(path).split("."), o = S.cfg, i;
    if (!o || typeof o !== "object") S.cfg = o = {};
    for (i = 0; i < p.length - 1; i++) {
      if (o[p[i]] == null || typeof o[p[i]] !== "object") o[p[i]] = {};
      o = o[p[i]];
    }
    o[p[p.length - 1]] = val;
  }
  function base() {
    if (typeof W01S_SAVED !== "undefined" && W01S_SAVED && typeof W01S_SAVED === "object") return W01S_SAVED;
    return null;
  }
  function diffPaths() {
    try { if (typeof w01SDiff === "function") return w01SDiff() || []; } catch (e) {}
    var b = base(); if (!b) return [];
    var out = [];
    (function walk(a, c, p) {
      var ao = a && typeof a === "object", co = c && typeof c === "object";
      if (ao || co) {
        var keys = {}, k;
        if (ao) for (k in a) if (Object.prototype.hasOwnProperty.call(a, k)) keys[k] = 1;
        if (co) for (k in c) if (Object.prototype.hasOwnProperty.call(c, k)) keys[k] = 1;
        for (k in keys) walk(ao ? a[k] : undefined, co ? c[k] : undefined, p ? p + "." + k : k);
        return;
      }
      if (a !== c) out.push(p || "");
    })(b, (S && S.cfg) || {}, "");
    return out;
  }

  /* ---------- форматирование ---------- */
  function dec(step) { return (String(step == null ? 1 : step).split(".")[1] || "").length; }
  function fmtNum(v, step) {
    var n = Number(v);
    if (!isFinite(n)) return "—";
    var d = dec(step);
    return d ? n.toFixed(d) : String(Math.round(n));
  }
  function quant(v, row) {
    var n = Number(v);
    if (!isFinite(n)) n = row.min == null ? 0 : row.min;
    if (row.min != null && n < row.min) n = row.min;
    if (row.max != null && n > row.max) n = row.max;
    if (row.step) {
      var s = Number(row.step), d = dec(row.step);
      n = +(Math.round(n / s) * s).toFixed(d + 2);
      if (row.min != null && n < row.min) n = row.min;
      if (row.max != null && n > row.max) n = row.max;
    }
    return n;
  }
  function plural(n, f) {
    var a = Math.abs(n) % 100, b = a % 10;
    if (a > 10 && a < 20) return f[2];
    if (b > 1 && b < 5) return f[1];
    if (b === 1) return f[0];
    return f[2];
  }
  function fieldWord(n) { return en() ? (n === 1 ? "field" : "fields") : plural(n, ["поле", "поля", "полей"]); }
  function paramWord(n) { return en() ? (n === 1 ? "param" : "params") : plural(n, ["параметр", "параметра", "параметров"]); }
  function rowById(k) {
    var i, j;
    for (i = 0; i < CATS.length; i++) for (j = 0; j < CATS[i].rows.length; j++) if (CATS[i].rows[j].k === k) return CATS[i].rows[j];
    return null;
  }
  function shortVal(k) {
    var b = base(); if (!b) return "—";
    var p = String(k).split("."), o = b, i;
    for (i = 0; i < p.length; i++) { if (o == null) return "—"; o = o[p[i]]; }
    if (o == null) return "—";
    if (typeof o === "boolean") return o ? L("вкл", "on") : L("выкл", "off");
    if (typeof o === "object") return L("объект", "object");
    var r = rowById(k);
    if (r && r.t === "num") return fmtNum(o, r.step);
    if (r && r.t === "pass") return o ? "•••" : "—";
    return String(o);
  }

  /* ---------- разметка поля ---------- */
  function valOf(row) {
    var v = getPath(row.k);
    if (v == null) v = row.t === "sw" ? false : "";
    return v;
  }
  function fieldHTML(row) {
    var v = valOf(row), lbl = esc_(L(row.l[0], row.l[1])), h;
    if (row.t === "num") {
      h = '<span class="sc-num">' +
        '<button type="button" class="sc-step" data-step="-1" aria-label="' + esc_(L("Уменьшить", "Decrease")) + '">−</button>' +
        '<input class="sc-nin" type="number" data-scard-field data-k="' + esc_(row.k) + '" value="' + esc_(fmtNum(v, row.step)) + '"' +
        (row.min != null ? ' min="' + row.min + '"' : "") + (row.max != null ? ' max="' + row.max + '"' : "") +
        (row.step ? ' step="' + row.step + '"' : "") + ' aria-label="' + lbl + '" inputmode="decimal">' +
        '<button type="button" class="sc-step" data-step="1" aria-label="' + esc_(L("Увеличить", "Increase")) + '">+</button>' +
        "</span>";
      return h;
    }
    if (row.t === "text") {
      return '<input class="sc-tin" type="text" data-scard-field data-k="' + esc_(row.k) + '" value="' + esc_(String(v)) +
        '" aria-label="' + lbl + '" spellcheck="false" autocomplete="off">';
    }
    if (row.t === "pass") {
      return '<span class="sc-pw"><input class="sc-tin" type="password" data-scard-field data-k="' + esc_(row.k) + '" value="' + esc_(String(v)) +
        '" aria-label="' + lbl + '" spellcheck="false" autocomplete="off">' +
        '<button type="button" class="sc-eye" data-eyes="1" aria-label="' + esc_(L("Показать значение", "Reveal value")) + '" title="' + esc_(L("показать значение", "reveal value")) + '">' +
        '<svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">' +
        '<path d="M2 12s3.6-6.4 10-6.4S22 12 22 12s-3.6 6.4-10 6.4S2 12 2 12z"/><circle cx="12" cy="12" r="2.5"/>' +
        '<path class="sc-eye-slash" d="M4 20 20 4"/></svg></button></span>';
    }
    if (row.t === "sw") {
      var on = !!v;
      return '<label class="sc-sw"><input type="checkbox" data-scard-field data-k="' + esc_(row.k) + '"' + (on ? " checked" : "") +
        ' aria-label="' + lbl + '"><span class="sc-sw-t" aria-hidden="true"><i></i></span>' +
        '<span class="sc-sw-x">' + esc_(on ? L("ВКЛ", "ON") : L("ВЫКЛ", "OFF")) + "</span></label>";
    }
    /* перечисление: значения политик + текущее, если его нет в каталоге */
    var opts = (row.pol ? POLICY : (row.opts || [])).slice();
    var cur = String(v);
    if (cur && opts.indexOf(cur) < 0) opts.push(cur);
    var oh = "";
    for (var i = 0; i < opts.length; i++) {
      oh += '<option value="' + esc_(opts[i]) + '"' + (opts[i] === cur ? " selected" : "") + ">" + esc_(opts[i]) + "</option>";
    }
    if (!cur) oh = '<option value="" selected>—</option>' + oh;
    return '<span class="sc-selw"><select class="sc-sel" data-scard-field data-k="' + esc_(row.k) + '" aria-label="' + lbl + '">' +
      oh + '</select><i class="sc-selc" aria-hidden="true">▼</i></span>';
  }
  function langHTML() {
    var modes = [["system", L("Система", "System")], ["ru", "Русский"], ["en", "English"]];
    var cur = (typeof I18N !== "undefined" && I18N && I18N.mode) || "system", h = "", i;
    for (i = 0; i < modes.length; i++) {
      h += '<option value="' + modes[i][0] + '"' + (cur === modes[i][0] ? " selected" : "") + ">" + esc_(modes[i][1]) + "</option>";
    }
    return '<span class="sc-selw"><select class="sc-sel" data-scard-lang aria-label="' + esc_(L("Язык интерфейса", "Interface language")) + '">' +
      h + '</select><i class="sc-selc" aria-hidden="true">▼</i></span>';
  }

  /* ---------- разметка лестницы уровней ---------- */
  function tiersHTML() {
    var r = (S && S.cfg && S.cfg.routing) || {}, t = r.tiers || [], def = String(r.default_tier == null ? "" : r.default_tier);
    var h = '<div class="sc-tiers" data-tiers>';
    if (!t.length) return h + '<span class="sc-tier-hint">' + esc_(L("ядро не отдало список уровней", "core returned no tiers")) + "</span></div>";
    for (var i = 0; i < t.length; i++) {
      var nm = String(t[i]);
      h += '<span class="sc-tier' + (nm === def ? " is-def" : "") + '" data-tier="' + esc_(nm) + '">' +
        '<span class="sc-tier-n">' + (i + 1) + "</span>" +
        '<span class="sc-tier-t">' + esc_(nm) + "</span>" +
        (nm === def
          ? '<span class="sc-tier-d">' + esc_(L("по умолчанию", "default")) + "</span>"
          : '<button type="button" class="sc-tier-b" data-tier-def="' + i + '" title="' + esc_(L("сделать по умолчанию", "make default")) + '" aria-label="' + esc_(L("Сделать уровнем по умолчанию", "Make default tier")) + '">★</button>') +
        '<button type="button" class="sc-tier-b" data-tier-up="' + i + '"' + (i === 0 ? " disabled" : "") + ' title="' + esc_(L("выше", "up")) + '" aria-label="' + esc_(L("Поднять уровень", "Move tier up")) + '">↑</button>' +
        '<button type="button" class="sc-tier-b" data-tier-down="' + i + '"' + (i === t.length - 1 ? " disabled" : "") + ' title="' + esc_(L("ниже", "down")) + '" aria-label="' + esc_(L("Опустить уровень", "Move tier down")) + '">↓</button>' +
        "</span>";
    }
    return h + '<span class="sc-tier-hint">' + esc_(L("первый уровень пробуется первым", "first tier is tried first")) + "</span></div>";
  }

  function relPath(k, key) {
    var pre = key + ".";
    return (key && String(k).indexOf(pre) === 0) ? String(k).slice(pre.length) : String(k);
  }
  function rowHTML(row, catKey) {
    var isLang = row.t === "lang";
    var ctl = isLang ? langHTML() : fieldHTML(row);
    var title = esc_(L(row.l[0], row.l[1]));
    var unit = row.u ? '<em class="sc-l-u">' + esc_(row.u) + "</em>" : "";
    return '<div class="sc-row" data-k="' + esc_(row.k) + '">' +
      '<span class="sc-l"><span class="sc-l-t" title="' + title + '">' + title + unit + "</span>" +
      '<code class="sc-l-p" title="' + esc_(row.k) + '">' + esc_(relPath(row.k, catKey)) + "</code>" +
      '<em class="sc-l-w" data-was>—</em></span>' +
      ctl +
      (isLang ? '<span class="sc-undo" aria-hidden="true"></span>'
        : '<button type="button" class="sc-undo" data-undo="' + esc_(row.k) + '" title="' + esc_(L("вернуть загруженное значение", "revert to loaded value")) + '" aria-label="' + esc_(L("Отменить правку", "Revert this change")) + '">⟲</button>') +
      "</div>";
  }

  function bandHTML(cat) {
    var i, rows = "";
    for (i = 0; i < cat.rows.length; i++) rows += rowHTML(cat.rows[i], cat.key);
    if (cat.tiers) rows += tiersHTML();
    var total = cat.rows.length + (cat.tiers ? 2 : 0);
    return '<section class="sc-band" data-scard-sec data-sec="' + cat.id + '" aria-label="' + esc_(L(cat.t[0], cat.t[1])) + '">' +
      '<div class="sc-band-h"><span class="sc-band-k"><b>' + cat.n + "</b> · " + esc_(cat.key) + "</span>" +
      '<span class="sc-band-c" data-band-c data-total="' + total + '">' + total + " " + paramWord(total) + "</span></div>" +
      '<h3 class="sc-band-t">' + esc_(L(cat.t[0], cat.t[1])) + "</h3>" +
      '<div class="sc-grid">' + rows + "</div></section>";
  }

  function countFields() {
    var n = 0, i;
    for (i = 0; i < CATS.length; i++) n += CATS[i].rows.length + (CATS[i].tiers ? 2 : 0);
    return n;
  }

  function shellHTML() {
    var chips = "", i, total = countFields();
    for (i = 0; i < CATS.length; i++) {
      chips += '<button type="button" class="sc-chip" data-go="' + CATS[i].id + '">' +
        '<span class="sc-chip-n">' + CATS[i].n + "</span>" + esc_(L(CATS[i].s[0], CATS[i].s[1])) +
        "</button>";
    }
    return '<div class="sc-bar" data-bar>' +
      '<div class="sc-chips" role="group" aria-label="' + esc_(L("Разделы конфига", "Config sections")) + '">' + chips + "</div>" +
      '<div class="sc-bar-act">' +
      '<span class="sc-pill" data-scard-dirty role="status">' + esc_(L("СИНХРОННО", "IN SYNC")) + "</span>" +
      '<button type="button" class="btn ghost sm" data-scard-revert>' + esc_(L("откатить", "revert")) + "</button>" +
      '<button type="button" class="btn primary sm" data-scard-save>' + esc_(L("сохранить", "save")) + "</button>" +
      "</div></div>" +
      '<div class="sc-note"><span>' + esc_(L("правки сразу ложатся в S.cfg", "edits land in S.cfg at once")) + "</span>" +
      '<span>' + esc_(L("сохранить → SaveConfig", "save → SaveConfig")) + "</span>" +
      '<span><b data-num-fields>' + total + "</b> " + paramWord(total) + " · " + CATS.length + " " + esc_(L("разделов", "sections")) + "</span></div>";
  }

  /* ---------- статус/шапка панели ---------- */
  function syncHeader() {
    var meta = document.getElementById("settings-meta");
    if (meta) {
      var s = (S && S.cfg && S.cfg.server) || {};
      var t = "S.cfg · " + String(s.host || "—") + ":" + String(s.port == null ? "—" : s.port) + " · " + countFields() + " " + paramWord(countFields());
      meta.textContent = t; meta.title = t;
    }
    var lt = document.getElementById("settings-live-txt");
    if (lt) lt.textContent = (typeof DEMO !== "undefined" && DEMO) ? "● DEMO · правки в S.cfg" : "● LIVE";
  }

  /* ---------- окраска состояния ---------- */
  function paint(root) {
    if (!root) return;
    var list = diffPaths(), n = list.length, set = {}, i;
    for (i = 0; i < n; i++) set[list[i]] = 1;
    try { cfgDirty = n > 0; } catch (e) { window.cfgDirty = n > 0; }

    var pill = root.querySelector("[data-scard-dirty]");
    if (pill) {
      pill.classList.toggle("is-dirty", n > 0);
      /* текст одинаковой длины в обоих состояниях: полоса не перескакивает на вторую строку */
      pill.textContent = n > 0 ? L("ИЗМЕНЕНО", "CHANGED") : L("СИНХРОННО", "IN SYNC");
      pill.title = n > 0 ? n + " " + fieldWord(n) : L("конфиг совпадает с ядром", "config matches the core");
    }
    var rows = root.querySelectorAll(".sc-row[data-k]");
    for (i = 0; i < rows.length; i++) {
      var row = rows[i], k = row.getAttribute("data-k"), dirty = !!set[k];
      row.classList.toggle("is-dirty", dirty);
      var was = row.querySelector("[data-was]");
      if (was && dirty) was.textContent = L("было ", "was ") + shortVal(k);
    }
    var bands = root.querySelectorAll("[data-scard-sec]");
    for (i = 0; i < bands.length; i++) {
      var b = bands[i], cnt = b.querySelectorAll(".sc-row.is-dirty").length;
      var bc = b.querySelector("[data-band-c]"), total = +(bc ? bc.getAttribute("data-total") : 0);
      if (bc) {
        bc.classList.toggle("is-dirty", cnt > 0);
        bc.textContent = cnt > 0
          ? L(cnt + " из " + total + " изменено", cnt + " of " + total + " changed")
          : total + " " + paramWord(total);
      }
      var chipBtn = root.querySelector('[data-go="' + b.getAttribute("data-sec") + '"]');
      if (chipBtn) {
        chipBtn.classList.toggle("is-dirty", cnt > 0);
        chipBtn.title = cnt > 0 ? L(cnt + " из " + total + " изменено", cnt + " of " + total + " changed") : "";
      }
    }
  }

  /* ---------- сборка ---------- */
  function html() {
    if (!S || !S.cfg) {
      return '<div class="sc-empty">' + esc_(L("ядро не отдало конфиг (GetConfig вернул пусто)", "core returned no config (GetConfig empty)")) + "</div>";
    }
    var out = shellHTML(), i;
    for (i = 0; i < CATS.length; i++) out += bandHTML(CATS[i]);
    out += '<div class="sc-foot"><span><b>S.cfg</b> → SaveConfig</span>' +
      '<span>' + esc_(L("правки видны построчно: акцент + «было N»", "every edit is visible per row: accent + “was N”")) + "</span>" +
      '<span>' + esc_(L("отмена одной правки — ⟲ справа", "single edit revert — ⟲ on the right")) + "</span></div>";
    return out;
  }

  function cont() { return document.querySelector(".content"); }

  function render(root) {
    var c = cont(), st = c ? c.scrollTop : 0;
    root.innerHTML = html();
    wire(root);
    paint(root);
    syncHeader();
    if (c && st) {
      var prev = c.style.scrollBehavior;
      c.style.scrollBehavior = "auto";
      c.scrollTop = st;
      c.style.scrollBehavior = prev || "";
    }
    spy();
    markCurrent();
  }

  /* ---------- прокрутка по категориям и подсветка текущего раздела ---------- */
  var lockChip = null;               /* раздел, выбранный кликом: держим подсветку,
                                        пока пользователь не прокрутит сам */
  function setActive(root, id) {
    var chips = root.querySelectorAll("[data-go]"), i;
    for (i = 0; i < chips.length; i++) chips[i].classList.toggle("is-on", chips[i].getAttribute("data-go") === id);
  }
  function jump(id) {
    var root = document.querySelector("#panel-settings [data-scard]");
    if (!root) return;
    var band = root.querySelector('[data-sec="' + id + '"]');
    var c = cont();
    if (!band || !c) return;
    var bar = root.querySelector("[data-bar]");
    var barH = bar ? bar.getBoundingClientRect().height : 0;
    var top = band.getBoundingClientRect().top - c.getBoundingClientRect().top + c.scrollTop - barH - 6;
    c.scrollTop = Math.max(0, top);
    /* последние разделы не достают до верха — подсветку держим на выбранном */
    lockChip = id;
    setActive(root, id);
  }
  function spy() {
    var c = cont();
    if (!c || c._scSpy) return;
    c._scSpy = 1;
    var raf = 0, unlock = function () { lockChip = null; };
    c.addEventListener("scroll", function () {
      if (raf) return;
      raf = requestAnimationFrame(function () {
        raf = 0;
        try { markCurrent(); } catch (e) {}
      });
    }, { passive: true });
    c.addEventListener("wheel", unlock, { passive: true });
    c.addEventListener("touchstart", unlock, { passive: true });
    c.addEventListener("keydown", unlock);
    c.addEventListener("pointerdown", function (e) {
      /* клик по чипу — это тоже pointerdown, но подсветку ставит jump() */
      if (!e.target || !e.target.closest || !e.target.closest("[data-go]")) unlock();
    });
  }
  function markCurrent() {
    var panel = document.getElementById("panel-settings");
    var root = panel && panel.querySelector("[data-scard]");
    if (!root || !panel.classList.contains("active")) return;
    if (lockChip) { setActive(root, lockChip); return; }
    var bands = root.querySelectorAll("[data-scard-sec]"), bar = root.querySelector("[data-bar]");
    var c = cont(); if (!c) return;
    var limit = c.getBoundingClientRect().top + (bar ? bar.getBoundingClientRect().height : 0) + 8;
    var act = bands.length ? bands[0] : null, i;
    for (i = 0; i < bands.length; i++) if (bands[i].getBoundingClientRect().top <= limit) act = bands[i];
    if (!act) return;
    setActive(root, act.getAttribute("data-sec"));
  }

  /* ---------- события ---------- */
  function fieldOf(el) {
    var r = el && el.closest ? el.closest(".sc-row") : null;
    return r;
  }
  function onInput(e) {
    var el = e.target;
    if (!el || !el.getAttribute || !el.getAttribute("data-scard-field")) return;
    var k = el.getAttribute("data-k"); if (!k) return;
    var row = fieldOf(el), def = rowById(k);
    if (!def) return;
    if (def.t === "num") {
      if (el.value === "" || el.value === "-" || el.value === ".") return;
      var v = Number(el.value);
      if (!isFinite(v)) return;
      setPath(k, v);
      paint(document.querySelector("#panel-settings [data-scard]"));
      return;
    }
    if (def.t === "sw") return;
    setPath(k, el.value);
    paint(document.querySelector("#panel-settings [data-scard]"));
  }
  function onChange(e) {
    var el = e.target;
    if (!el || !el.getAttribute) return;
    if (el.hasAttribute("data-scard-lang")) {
      var m = el.value;
      var go = function () { try { I18N.setMode(m); } catch (er) { say(L("язык не переключился", "language not switched"), "err"); } };
      try {
        if (typeof DEMO !== "undefined" && !DEMO && Go() && diffPaths().length) { Promise.resolve(saveCfg({ auto: true })).then(go, go); return; }
      } catch (eL) {}
      go();
      return;
    }
    if (!el.hasAttribute("data-scard-field")) return;
    var k = el.getAttribute("data-k"); if (!k) return;
    var def = rowById(k), root = document.querySelector("#panel-settings [data-scard]");
    if (!def) return;
    if (def.t === "sw") {
      setPath(k, !!el.checked);
      var x = el.parentNode && el.parentNode.querySelector(".sc-sw-x");
      if (x) x.textContent = el.checked ? L("ВКЛ", "ON") : L("ВЫКЛ", "OFF");
    } else if (def.t === "num") {
      var v = quant(el.value, def);
      setPath(k, v);
      el.value = fmtNum(v, def.step);
    } else {
      setPath(k, el.value);
    }
    paint(root);
  }
  function onClick(e) {
    var t = e.target;
    if (!t || !t.closest) return;
    var root = document.querySelector("#panel-settings [data-scard]");
    if (!root) return;
    var chip = t.closest("[data-go]");
    if (chip) { jump(chip.getAttribute("data-go")); return; }

    var st = t.closest("[data-step]");
    if (st) {
      var cell = st.closest(".sc-num"), inp = cell && cell.querySelector("input[data-k]");
      if (inp) {
        var k = inp.getAttribute("data-k"), def = rowById(k);
        var dir = +st.getAttribute("data-step");
        var step = def && def.step ? Number(def.step) : 1;
        var cur = Number(getPath(k));
        if (!isFinite(cur)) cur = def && def.min != null ? def.min : 0;
        var nv = quant(cur + dir * step, def || {});
        setPath(k, nv);
        inp.value = fmtNum(nv, def && def.step);
        paint(root);
      }
      return;
    }
    var eye = t.closest("[data-eyes]");
    if (eye) {
      var pw = eye.closest(".sc-pw"), pin = pw && pw.querySelector("input");
      if (pin) {
        var show = pin.type === "password";
        pin.type = show ? "text" : "password";
        eye.classList.toggle("is-on", show);
        eye.setAttribute("aria-label", show ? L("Скрыть значение", "Hide value") : L("Показать значение", "Reveal value"));
      }
      return;
    }
    var un = t.closest("[data-undo]");
    if (un) {
      var path = un.getAttribute("data-undo"), b = base();
      if (b) {
        var p = path.split("."), o = b, i;
        for (i = 0; i < p.length; i++) { o = (o == null) ? undefined : o[p[i]]; }
        if (o === undefined) { say(L("в загруженном конфиге этого поля нет", "this key is absent in the loaded config")); return; }
        setPath(path, o);
      }
      render(root);
      return;
    }
    var tu = t.closest("[data-tier-up]"), td = t.closest("[data-tier-down]"), tf = t.closest("[data-tier-def]");
    if (tu || td || tf) {
      var r = (S.cfg && S.cfg.routing) || {}, tiers = (r.tiers || []).slice(), idx;
      if (tf) {
        r.default_tier = tiers[+tf.getAttribute("data-tier-def")];
      } else {
        idx = +((tu || td).getAttribute(tu ? "data-tier-up" : "data-tier-down"));
        var j = tu ? idx - 1 : idx + 1;
        if (j < 0 || j >= tiers.length) return;
        var tmp = tiers[idx]; tiers[idx] = tiers[j]; tiers[j] = tmp;
        r.tiers = tiers;
      }
      render(root);
      return;
    }
    if (t.closest("[data-scard-revert]")) { revertAll(root); return; }
    if (t.closest("[data-scard-save]")) {
      try { if (typeof saveCfg === "function") saveCfg(); } catch (er) { say(L("сохранить не удалось: ", "save failed: ") + ((er && er.message) || er), "err"); }
      return;
    }
  }
  function onKey(e) {
    if (e.key !== "Enter") return;
    var t = e.target;
    if (t && t.tagName === "INPUT" && t.hasAttribute("data-scard-field")) { e.preventDefault(); t.blur(); }
  }

  function wire(root) {
    if (root._scWired) return;
    root._scWired = 1;
    root.addEventListener("input", onInput);
    root.addEventListener("change", onChange);
    root.addEventListener("click", onClick);
    root.addEventListener("keydown", onKey);
  }

  /* ---------- откат ко всему загруженному ---------- */
  function revertAll(root) {
    var b = base();
    if (!b) { say(L("конфиг ещё не загружен — откатывать нечего", "config not loaded yet — nothing to revert")); return; }
    if (!diffPaths().length) { say(L("правок нет", "no changes")); return; }
    S.cfg = JSON.parse(JSON.stringify(b));
    try { cfgDirty = false; } catch (e) { window.cfgDirty = false; }
    render(root || document.querySelector("#panel-settings [data-scard]"));
    say(L("правки откатаны к загруженному конфигу", "changes reverted to the loaded config"));
  }

  /* ---------- монтирование в панель ---------- */
  function mount(host) {
    var panel = document.getElementById("panel-settings");
    if (!panel) return null;
    var el = null;
    if (typeof host === "string") el = document.querySelector(host);
    else if (host && host.nodeType === 1) el = host;
    if (el && (!panel.contains(el) || !el.hasAttribute("data-scard"))) el = null;
    if (!el) el = panel.querySelector("[data-scard]");
    if (!el) {
      /* статическая вёрстка прошлой панели в index.html не правится — убираем её целиком,
         оставляя шапку .hd; дальше панель рисует только этот модуль */
      var kids = Array.prototype.slice.call(panel.children), i;
      for (i = 0; i < kids.length; i++) if (!(kids[i].classList && kids[i].classList.contains("hd"))) panel.removeChild(kids[i]);
      el = document.createElement("div");
      el.className = "sc-root";
      el.id = "settings-editor";
      el.setAttribute("data-scard", "");
      el.setAttribute("data-no-i18n", "");
      panel.appendChild(el);
    }
    render(el);
    return el;
  }

  window.SCard = { mount: mount, render: function () { return mount(); }, revert: revertAll };

  /* старая отрисовка панели больше не рисует: любой её вызов ведёт в этот модуль
     (w01SettingsInit дергает w01SettingsDraw при активной вкладке) */
  try { window.w01SettingsDraw = function () { return mount(); }; } catch (e) {}
})();
