/* ==================================================================
   r06 «РЕЕСТР ЯДРА» — вкладка «Настройки» (параметры ядра).

   Композиция (переработана относительно n09 «Пульт ядра»):
   1) липкий ПУЛЬТ: счётчик правок «N/30» с полосой заполнения, сегментный
      рельс разделов (номера + метки правок) и две кнопки записи;
   2) КАРТА ЯДРА как одна труба: пять ступеней в общей рамке с коннекторами,
      человеческие подписи значений и техническая подпись под ними;
   3) ЛИСТ раздела: крупная призрачная цифра, заголовок с подзаголовком,
      чип пути S.cfg, счётчик полей и полоса «сколько изменено» по низу;
   4) РЕЕСТР: внутри раздела строки сгруппированы по смыслу (подпись группы),
      строка = имя + чип ключа слева, шкала диапазона справа в той же колонке,
      управление фиксированной ширины; правка подменяет шкалу на «было N ⟲»
      в том же слоте — управление не сдвигается;
   5) ЛЕСТНИЦА уровней: рельс с рангами, «по умолчанию» метит строку,
      действия ★/↑/↓ собраны в сегмент.

   Числа — var(--sans) + tabular-nums; моно только ключи, токены, адреса.
   Значения пишутся в S.cfg сразу; cfgDirty считается w01SDiff() (тот же,
   что у SaveConfig). Демо: запись в ядро отключена — кнопка это говорит.
   ================================================================== */
"use strict";

var R06 = (function () {
  /* ---------- словари значений ---------- */
  var ENUM = {
    strategy: {
      priority: ["по приоритету", "by priority"],
      round_robin: ["по кругу", "round-robin"],
      cheapest: ["дешевле", "cheapest"],
      random: ["случайно", "random"],
      weighted: ["по весам", "weighted"]
    },
    key_swap: {
      round_robin: ["по кругу", "round-robin"],
      random: ["случайно", "random"],
      sticky: ["липкий", "sticky"],
      least_used: ["реже всех", "least used"]
    },
    egress_strategy: {
      round_robin: ["по кругу", "round-robin"],
      random: ["случайно", "random"],
      sticky: ["липкий", "sticky"],
      least_latency: ["быстрейший", "lowest latency"]
    },
    policy: {
      retry: ["повтор", "retry"],
      retry_token: ["другой ключ", "another key"],
      retry_proxy: ["другой канал", "another egress"],
      swap_key: ["сменить ключ", "swap key"],
      swap_key_proxy: ["ключ + канал", "key + egress"],
      fail: ["вернуть ошибку", "fail"],
      skip: ["пропустить", "skip"],
      disable: ["отключить", "disable"]
    },
    lang: { system: ["Система", "System"], ru: ["Русский", "Russian"], en: ["English", "English"] }
  };
  var POLICY = ["retry", "retry_token", "retry_proxy", "swap_key", "swap_key_proxy", "fail", "skip", "disable"];

  /* ---------- реестр параметров ---------- */
  var CATS = [
    {
      id: "gw", n: "01", key: "server",
      s: ["Шлюз", "Gateway"], t: ["Шлюз и вход", "Gateway and access"],
      d: ["где слушает шлюз, как пишет журнал и чем защищён админ-доступ",
          "where the gateway listens, how it logs and what guards admin access"],
      groups: [
        {
          id: "addr", s: ["адрес", "address"], rows: [
            { k: "server.host", l: ["Адрес прослушивания", "Listen address"], t: "text" },
            { k: "server.port", l: ["Порт шлюза", "Gateway port"], t: "num", min: 1024, max: 65535, step: 1, hint: "TCP" }
          ]
        },
        {
          id: "acc", s: ["доступ и журнал", "access and log"], rows: [
            { k: "server.log_level", l: ["Уровень журнала", "Log level"], t: "sel", opts: ["trace", "debug", "info", "warn", "error"] },
            { k: "admin_token", l: ["Admin-токен", "Admin token"], t: "pass" }
          ]
        }
      ]
    },
    {
      id: "res", n: "02", key: "resilience",
      s: ["Защита", "Guards"], t: ["Сбои, паузы, блокировки", "Failures, pauses, locks"],
      d: ["когда размыкается предохранитель, как растут паузы и на сколько блокируются ключи",
          "when the breaker opens, how pauses grow and how long keys stay locked"],
      groups: [
        {
          id: "br", s: ["предохранитель", "breaker"], rows: [
            { k: "resilience.breaker.max_failures", l: ["Сбоев до размыкания", "Failures to open"], t: "num", min: 1, max: 50, step: 1, u: ["шт", "pcs"] },
            { k: "resilience.breaker.recovery_seconds", l: ["Пауза перед пробой", "Pause before probe"], t: "num", min: 5, max: 600, step: 5, u: ["сек", "sec"] },
            { k: "resilience.breaker.half_open_successes", l: ["Успехов для возврата", "Successes to close"], t: "num", min: 1, max: 10, step: 1, u: ["шт", "pcs"] }
          ]
        },
        {
          id: "cd", s: ["паузы", "pauses"], rows: [
            { k: "resilience.cooldown.base_seconds", l: ["Базовая пауза", "Base pause"], t: "num", min: 1, max: 120, step: 1, u: ["сек", "sec"] },
            { k: "resilience.cooldown.max_seconds", l: ["Предел паузы", "Max pause"], t: "num", min: 30, max: 3600, step: 30, u: ["сек", "sec"] },
            { k: "resilience.cooldown.factor", l: ["Множитель роста", "Growth factor"], t: "num", min: 1, max: 5, step: 0.1, u: ["×", "×"] }
          ]
        },
        {
          id: "lk", s: ["блокировки", "locks"], rows: [
            { k: "resilience.lockout.max_failures", l: ["Сбоев в окне", "Failures in window"], t: "num", min: 1, max: 50, step: 1, u: ["шт", "pcs"] },
            { k: "resilience.lockout.window_seconds", l: ["Окно подсчёта", "Window"], t: "num", min: 5, max: 600, step: 5, u: ["сек", "sec"] },
            { k: "resilience.lockout.lockout_seconds", l: ["Длительность блокировки", "Block time"], t: "num", min: 10, max: 3600, step: 10, u: ["сек", "sec"] }
          ]
        }
      ]
    },
    {
      id: "route", n: "03", key: "routing", tiers: true,
      s: ["Маршрут", "Routing"], t: ["Маршрут и уровни", "Routing and tiers"],
      d: ["как выбирается провайдер и в каком порядке пробуются уровни",
          "how a provider is chosen and in which order tiers are tried"],
      groups: [
        {
          id: "pick", s: ["выбор провайдера", "provider pick"], rows: [
            { k: "routing.strategy", l: ["Стратегия провайдера", "Provider strategy"], t: "sel", opts: ["priority", "round_robin", "cheapest", "random", "weighted"], e: "strategy" },
            { k: "routing.costs.enabled", l: ["Учитывать стоимость", "Consider cost"], t: "sw" }
          ]
        }
      ]
    },
    {
      id: "egress", n: "04", key: "egress",
      s: ["Каналы", "Egress"], t: ["Каналы и ротация", "Egress and rotation"],
      d: ["пул внешних каналов и правило ротации запросов по ним",
          "the pool of external egresses and the rotation rule"],
      groups: [
        {
          id: "pool", s: ["пул", "pool"], rows: [
            { k: "egress.enabled", l: ["Пул каналов включён", "Channel pool enabled"], t: "sw" },
            { k: "egress.base_url", l: ["Адрес пула", "Pool address"], t: "text" },
            { k: "egress.token", l: ["Токен пула", "Pool token"], t: "pass" }
          ]
        },
        {
          id: "rot", s: ["ротация", "rotation"], rows: [
            { k: "egress.strategy", l: ["Выбор канала", "Channel pick"], t: "sel", opts: ["round_robin", "random", "sticky", "least_latency"], e: "egress_strategy" },
            { k: "egress.rotation.per_request", l: ["Ротация на запрос", "Rotate per request"], t: "sw" },
            { k: "egress.rotation.rotate_after", l: ["Запросов до ротации", "Requests before rotate"], t: "num", min: 1, max: 100, step: 1, u: ["шт", "pcs"] }
          ]
        }
      ]
    },
    {
      id: "prov", n: "05", key: "provider_defaults",
      s: ["Ключи", "Keys"], t: ["Поведение у провайдеров", "Provider defaults"],
      d: ["что делать с ключом и каналом при каждой ошибке провайдера",
          "what to do with key and egress on each provider error"],
      groups: [
        {
          id: "swap", s: ["подмена ключа", "key swap"], rows: [
            { k: "provider_defaults.key_swap", l: ["Подмена ключа", "Key swap"], t: "sel", opts: ["round_robin", "random", "sticky", "least_used"], e: "key_swap" }
          ]
        },
        {
          id: "pol", s: ["политики ошибок", "error policies"], rows: [
            { k: "provider_defaults.error_policy.401", l: ["401 · ключ отклонён", "401 · key rejected"], t: "sel", pol: 1, e: "policy" },
            { k: "provider_defaults.error_policy.429", l: ["429 · лимит провайдера", "429 · upstream limit"], t: "sel", pol: 1, e: "policy" },
            { k: "provider_defaults.error_policy.5xx", l: ["5xx · сбой провайдера", "5xx · provider failure"], t: "sel", pol: 1, e: "policy" },
            { k: "provider_defaults.error_policy.timeout", l: ["Таймаут ответа", "Response timeout"], t: "sel", pol: 1, e: "policy" },
            { k: "provider_defaults.error_policy.network", l: ["Сетевой сбой", "Network failure"], t: "sel", pol: 1, e: "policy" },
            { k: "provider_defaults.error_policy.rate_limit", l: ["Локальный rate limit", "Local rate limit"], t: "sel", pol: 1, e: "policy" }
          ]
        }
      ]
    },
    {
      id: "ui", n: "06", key: "ui",
      s: ["Интерфейс", "Interface"], t: ["Интерфейс", "Interface"],
      d: ["язык подписей панели", "language of the panel labels"],
      groups: [
        {
          id: "lang", s: ["язык", "language"], rows: [
            { k: "ui.language", l: ["Язык интерфейса", "Interface language"], t: "lang" }
          ]
        }
      ]
    }
  ];

  /* плоский список строк — по нему ищутся параметры и считается итог */
  var FLAT = [];
  (function () {
    for (var i = 0; i < CATS.length; i++)
      for (var j = 0; j < CATS[i].groups.length; j++)
        for (var m = 0; m < CATS[i].groups[j].rows.length; m++) FLAT.push(CATS[i].groups[j].rows[m]);
  })();
  function units() { return FLAT.length + 1; } /* 29 параметров + очередь уровней */

  var EYE = '<svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="1.8"' +
    ' stroke-linecap="round" stroke-linejoin="round"><path d="M2 12s3.6-6.4 10-6.4S22 12 22 12s-3.6 6.4-10 6.4S2 12 2 12z"/>' +
    '<circle cx="12" cy="12" r="2.5"/><path d="M4 20 20 4"/></svg>';

  /* ---------- мелочи ---------- */
  function en() { return typeof I18N !== "undefined" && I18N && I18N.lang === "en"; }
  function L(ru, e) { return en() ? (e || ru) : ru; }
  function esc_(s) {
    if (typeof esc === "function") return esc(s);
    return String(s == null ? "" : s).replace(/[&<>"']/g, function (c) {
      return ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c];
    });
  }
  function say(m, kind) { try { if (typeof toast === "function") toast(m, kind); } catch (e) {} }
  function pl(n, f) {
    var a = Math.abs(n) % 100, b = a % 10;
    if (a > 10 && a < 20) return f[2];
    if (b > 1 && b < 5) return f[1];
    if (b === 1) return f[0];
    return f[2];
  }
  function fieldWord(n) { return en() ? (n === 1 ? "field" : "fields") : pl(n, ["поле", "поля", "полей"]); }
  function paramWord(n) { return en() ? (n === 1 ? "param" : "params") : pl(n, ["параметр", "параметра", "параметров"]); }
  function tierWord(n) { return en() ? (n === 1 ? "tier" : "tiers") : pl(n, ["уровень", "уровня", "уровней"]); }
  function markWord(n) { return en() ? (n === 1 ? "change" : "changes") : pl(n, ["правка", "правки", "правок"]); }

  /* ---------- S.cfg по точечному пути ---------- */
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
  function getBase(path) {
    var b = base(), p = String(path).split("."), o = b, i;
    for (i = 0; i < p.length; i++) { if (o == null) return undefined; o = o[p[i]]; }
    return o;
  }
  function eq(a, b) {
    if (a === b) return true;
    if (a == null || b == null) return a == null && b == null;
    try { return JSON.stringify(a) === JSON.stringify(b); } catch (e) { return false; }
  }
  function setDirty(on) {
    try { cfgDirty = !!on; } catch (e) { try { window.cfgDirty = !!on; } catch (e2) {} }
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
  function rowById(k) {
    for (var i = 0; i < FLAT.length; i++) if (FLAT[i].k === k) return FLAT[i];
    return null;
  }
  function relPath(k, key) {
    var pre = key + ".";
    return (key && String(k).indexOf(pre) === 0) ? String(k).slice(pre.length) : String(k);
  }
  function valOf(row) {
    var v = getPath(row.k);
    if (v == null) v = row.t === "sw" ? false : "";
    return v;
  }
  /* подпись значения для «было N» */
  function shortVal(k) {
    var o = getBase(k), r = rowById(k);
    if (o === undefined) return "—";
    if (typeof o === "boolean") return o ? L("вкл", "on") : L("выкл", "off");
    if (typeof o === "object") return L("объект", "object");
    if (r && r.t === "num") return fmtNum(o, r.step);
    if (r && r.t === "pass") return o ? "•••" : "—";
    if (r && r.t === "sel") return String(o);
    if (r && r.t === "lang") return String(o);
    return String(o);
  }
  /* подпись перечисления: человеческие слова, токен остаётся в подписи и title */
  function enumLabel(row, v) {
    var d = row && row.e ? ENUM[row.e] : null, e = d && d[String(v)];
    return e ? L(e[0], e[1]) : String(v == null ? "—" : v);
  }

  /* ---------- живые значения ступеней карты ---------- */
  function nodeVals() {
    var c = (typeof S !== "undefined" && S && S.cfg) || {};
    var s = c.server || {}, r = c.routing || {}, e = c.egress || {}, p = c.provider_defaults || {}, res = c.resilience || {};
    var br = res.breaker || {}, lk = res.lockout || {}, rot = e.rotation || {}, pol = p.error_policy || {};
    var nt = (r.tiers || []).length;
    var rowStrat = rowById("routing.strategy"), rowSwap = rowById("provider_defaults.key_swap"), row429 = rowById("provider_defaults.error_policy.429");
    return {
      gw: [
        String(s.host || "—") + ":" + String(s.port == null ? "—" : s.port),
        L("журнал ", "log ") + String(s.log_level || "—")
      ],
      res: [
        fmtNum(br.max_failures == null ? 0 : br.max_failures) + " " + L("сбоев", "fails"),
        L("пауза ", "pause ") + fmtNum(br.recovery_seconds == null ? 0 : br.recovery_seconds) + L(" с · блок ", " s · lock ") +
          fmtNum(lk.lockout_seconds == null ? 0 : lk.lockout_seconds) + L(" с", " s")
      ],
      route: [
        enumLabel(rowStrat, r.strategy),
        nt + " " + tierWord(nt) + " · " + String(r.default_tier || "—")
      ],
      egress: [
        e.enabled ? L("включены", "on") : L("напрямую", "direct"),
        e.enabled
          ? L("ротация через ", "rotate every ") + fmtNum(rot.rotate_after == null ? 0 : rot.rotate_after) + L(" запр.", " req.")
          : L("трафик напрямую", "traffic goes direct")
      ],
      prov: [
        enumLabel(rowSwap, p.key_swap),
        "429 → " + enumLabel(row429, pol["429"])
      ]
    };
  }

  /* ---------- управление строкой ---------- */
  function stepBtn(dir) {
    return '<button type="button" class="r06-step" data-step="' + dir + '" tabindex="-1" aria-label="' +
      esc_(dir < 0 ? L("Уменьшить", "Decrease") : L("Увеличить", "Increase")) + '">' + (dir < 0 ? "−" : "+") + "</button>";
  }

  /* ---------- свой дропдаун вместо нативного <select> ----------
     Нативный <select> остаётся в DOM скрытым и остаётся источником значения:
     выбор в меню ставит selectedIndex и диспатчит настоящий change, поэтому
     обработчики onChange (log_level, перечисления, язык) не меняются.
     Кнопка-плашка — role=combobox, меню — role=listbox, пункты — role=option. */
  var ddSeq = 0;
  function ddHTML(selAttrs, optionsHTML, lbl) {
    var id = "r06dd" + (++ddSeq);
    return '<span class="r06-selw r06-dd" data-dd>' +
      '<select class="r06-sel" tabindex="-1" aria-hidden="true" ' + selAttrs + '>' + optionsHTML + '</select>' +
      '<button type="button" class="r06-dd-btn" role="combobox" aria-haspopup="listbox" aria-expanded="false"' +
        ' aria-controls="' + id + '" aria-label="' + lbl + '">' +
        '<span class="r06-dd-val"></span><i class="r06-dd-chev" aria-hidden="true"></i></button>' +
      '<ul class="r06-dd-menu" id="' + id + '" role="listbox" tabindex="-1" aria-label="' + lbl + '" hidden></ul>' +
      '</span>';
  }

  function ctlHTML(row) {
    var v = valOf(row), lbl = esc_(L(row.l[0], row.l[1]));
    if (row.t === "num") {
      var unit = row.u ? '<em class="r06-unit" aria-hidden="true">' + esc_(L(row.u[0], row.u[1] || row.u[0])) + "</em>" : "";
      return '<span class="r06-num">' + stepBtn(-1) +
        '<span class="r06-ninsp"><input class="r06-nin" type="number" data-k="' + esc_(row.k) + '" value="' +
        esc_(fmtNum(v, row.step)) + '"' + (row.min != null ? ' min="' + row.min + '"' : "") +
        (row.max != null ? ' max="' + row.max + '"' : "") + (row.step ? ' step="' + row.step + '"' : "") +
        ' aria-label="' + lbl + '" inputmode="decimal">' + unit + "</span>" + stepBtn(1) + "</span>";
    }
    if (row.t === "text") {
      return '<input class="r06-in" type="text" data-k="' + esc_(row.k) + '" value="' + esc_(String(v)) +
        '" aria-label="' + lbl + '" spellcheck="false" autocomplete="off">';
    }
    if (row.t === "pass") {
      return '<span class="r06-pw"><input class="r06-in" type="password" data-k="' + esc_(row.k) + '" value="' +
        esc_(String(v)) + '" aria-label="' + lbl + '" spellcheck="false" autocomplete="off">' +
        '<button type="button" class="r06-eye" data-eyes aria-label="' + esc_(L("Показать значение", "Reveal value")) +
        '" title="' + esc_(L("показать значение", "reveal value")) + '">' + EYE + "</button></span>";
    }
    if (row.t === "sw") {
      var on = !!v;
      return '<label class="r06-sw"><input type="checkbox" data-k="' + esc_(row.k) + '"' + (on ? " checked" : "") +
        ' aria-label="' + lbl + '"><span class="r06-sw-t" aria-hidden="true"><i></i></span>' +
        '<span class="r06-sw-x">' + esc_(on ? L("ВКЛ", "ON") : L("ВЫКЛ", "OFF")) + "</span></label>";
    }
    if (row.t === "lang") {
      var modes = ["system", "ru", "en"], cur0 = (typeof I18N !== "undefined" && I18N && I18N.mode) || "system", h0 = "";
      for (var q = 0; q < modes.length; q++) {
        h0 += '<option value="' + modes[q] + '"' + (cur0 === modes[q] ? " selected" : "") + ' title="' + modes[q] + '">' +
          esc_(L(ENUM.lang[modes[q]][0], ENUM.lang[modes[q]][1])) + "</option>";
      }
      var langLbl = esc_(L("Язык интерфейса", "Interface language"));
      return ddHTML('data-lang aria-label="' + langLbl + '"', h0, langLbl);
    }
    /* перечисление: человеческая подпись в option, машинный токен — в подписи строки */
    var opts = (row.pol ? POLICY : (row.opts || [])).slice(), cur = String(v), oh = "";
    if (cur && opts.indexOf(cur) < 0) opts.push(cur);
    for (var i = 0; i < opts.length; i++) {
      oh += '<option value="' + esc_(opts[i]) + '"' + (opts[i] === cur ? " selected" : "") + ' title="' + esc_(opts[i]) + '">' +
        esc_(enumLabel(row, opts[i])) + "</option>";
    }
    if (!cur) oh = '<option value="" selected>—</option>' + oh;
    return ddHTML('data-k="' + esc_(row.k) + '" aria-label="' + lbl + '"', oh, lbl);
  }

  /* ---------- движок дропдауна ---------- */
  var ddOpen = null; /* { wrap, btn, menu, sel, opts, act } */

  function ddSel(wrap) { return wrap ? wrap.querySelector("select.r06-sel") : null; }

  function ddSync(wrap) {
    var sel = ddSel(wrap), btn = wrap && wrap.querySelector(".r06-dd-btn");
    var val = wrap && wrap.querySelector(".r06-dd-val");
    if (!sel || !btn) return;
    var o = sel.options[sel.selectedIndex];
    var txt = o ? o.textContent : "—";
    if (val && val.textContent !== txt) val.textContent = txt;
    if (btn.title !== txt) btn.title = txt;
    var dis = !!sel.disabled;
    if (btn.disabled !== dis) btn.disabled = dis;
    if (dis) btn.setAttribute("aria-disabled", "true");
    else btn.removeAttribute("aria-disabled");
  }
  function ddSyncAll(root) {
    var list = root.querySelectorAll(".r06-dd"), i;
    for (i = 0; i < list.length; i++) ddSync(list[i]);
  }
  function ddVisibleOptions(sel) {
    var out = [], i;
    for (i = 0; i < sel.options.length; i++) if (!sel.options[i].hidden) out.push(sel.options[i]);
    return out;
  }

  /* меню держим fixed: оно не обрезается .content и не тянет за собой раскладку строки */
  function ddPlace(wrap) {
    var btn = wrap && wrap.querySelector(".r06-dd-btn");
    var menu = wrap && wrap.querySelector(".r06-dd-menu");
    if (!btn || !menu || menu.hidden) return;
    var rc = btn.getBoundingClientRect();
    menu.style.maxHeight = "none";
    var nat = menu.scrollHeight + 2;
    var below = window.innerHeight - rc.bottom - 8;
    var above = rc.top - 8;
    var up = (nat + 6 > below) && (above > below);
    var room = Math.max(80, (up ? above : below) - 6);
    var h = Math.min(nat, Math.min(210, room));
    var w = Math.max(Math.round(rc.width), 150);
    var left = Math.min(Math.max(8, rc.left), Math.max(8, window.innerWidth - w - 8));
    var top = up ? (rc.top - 4 - h) : (rc.bottom + 4);
    menu.style.width = w + "px";
    menu.style.maxHeight = Math.max(80, Math.round(h)) + "px";
    menu.style.left = Math.round(left) + "px";
    menu.style.top = Math.round(Math.max(8, top)) + "px";
    wrap.classList.toggle("is-up", up);
  }

  function ddActive(i) {
    if (!ddOpen) return;
    var menu = ddOpen.menu, items = menu.querySelectorAll(".r06-dd-opt"), k;
    if (!items.length) return;
    if (i < 0) i = 0;
    if (i > items.length - 1) i = items.length - 1;
    ddOpen.act = i;
    for (k = 0; k < items.length; k++) items[k].classList.toggle("is-act", k === i);
    var it = items[i], btn = ddOpen.btn;
    if (btn && it) btn.setAttribute("aria-activedescendant", it.id);
    if (it) {
      var top = it.offsetTop, bot = top + it.offsetHeight;
      if (top < menu.scrollTop) menu.scrollTop = top;
      else if (bot > menu.scrollTop + menu.clientHeight) menu.scrollTop = bot - menu.clientHeight;
    }
  }
  function ddMove(delta, abs) {
    if (!ddOpen) return;
    var i = abs == null ? ddOpen.act + delta : abs;
    ddActive(i);
  }

  function ddClose(refocus) {
    if (!ddOpen) return;
    var w = ddOpen.wrap, btn = ddOpen.btn, menu = ddOpen.menu;
    ddOpen = null;
    try {
      if (w && w.isConnected) w.classList.remove("is-open", "is-up");
      if (btn) {
        btn.setAttribute("aria-expanded", "false");
        btn.removeAttribute("aria-activedescendant");
      }
      if (menu) { menu.hidden = true; menu.innerHTML = ""; }
    } catch (e) {}
    if (refocus && btn && btn.isConnected) {
      try { btn.focus({ preventScroll: true }); } catch (e2) { try { btn.focus(); } catch (e3) {} }
    }
  }

  function ddOpenMenu(wrap) {
    var sel = ddSel(wrap), btn = wrap && wrap.querySelector(".r06-dd-btn");
    var menu = wrap && wrap.querySelector(".r06-dd-menu");
    if (!sel || !btn || !menu || sel.disabled) return;
    if (ddOpen && ddOpen.wrap === wrap) return;
    if (ddOpen) ddClose(false);
    var opts = ddVisibleOptions(sel), cur = -1, i;
    menu.innerHTML = "";
    for (i = 0; i < opts.length; i++) {
      var o = opts[i];
      var li = document.createElement("li");
      li.className = "r06-dd-opt" + (o.index === sel.selectedIndex ? " is-sel" : "");
      li.id = menu.id + "-" + i;
      li.setAttribute("role", "option");
      li.setAttribute("aria-selected", o.index === sel.selectedIndex ? "true" : "false");
      li.setAttribute("data-i", String(o.index));
      li.title = o.title || o.textContent || "";
      var tx = document.createElement("span");
      tx.className = "r06-dd-tx";
      tx.textContent = o.textContent;
      li.appendChild(tx);
      if (o.index === sel.selectedIndex) {
        cur = i;
        var ck = document.createElement("i");
        ck.className = "r06-dd-tick";
        ck.setAttribute("aria-hidden", "true");
        ck.textContent = "✓";
        li.appendChild(ck);
      }
      menu.appendChild(li);
    }
    if (!opts.length) {
      var em = document.createElement("li");
      em.className = "r06-dd-opt is-dis";
      em.setAttribute("role", "option");
      em.setAttribute("aria-selected", "false");
      em.setAttribute("aria-disabled", "true");
      em.textContent = "—";
      menu.appendChild(em);
    }
    menu.hidden = false;
    wrap.classList.add("is-open");
    btn.setAttribute("aria-expanded", "true");
    try { btn.focus({ preventScroll: true }); } catch (eF) { try { btn.focus(); } catch (eF2) {} }
    ddPlace(wrap);
    ddOpen = { wrap: wrap, btn: btn, menu: menu, sel: sel, opts: opts, act: cur >= 0 ? cur : 0 };
    ddActive(ddOpen.act);
  }

  function ddPick(optIndex) {
    if (!ddOpen) return;
    var st = ddOpen, sel = st.sel, wrap = st.wrap;
    if (!sel || optIndex == null || optIndex < 0) return;
    var changed = sel.selectedIndex !== optIndex;
    if (changed) {
      sel.selectedIndex = optIndex;
      ddSync(wrap);
    }
    ddClose(true);
    if (changed) {
      try { sel.dispatchEvent(new Event("change", { bubbles: true })); } catch (e) {}
    }
  }

  function onDdHover(e) {
    if (!ddOpen) return;
    var li = e.target && e.target.closest ? e.target.closest(".r06-dd-opt") : null;
    if (!li || li.parentNode !== ddOpen.menu || li.classList.contains("is-dis")) return;
    var items = ddOpen.menu.querySelectorAll(".r06-dd-opt"), i;
    for (i = 0; i < items.length; i++) if (items[i] === li) { ddActive(i); return; }
  }
  function onDdDown(e) {
    if (e.target && e.target.closest && e.target.closest(".r06-dd-menu")) e.preventDefault();
  }
  function wireDdDoc() {
    if (window._r06DdDoc) return;
    window._r06DdDoc = 1;
    document.addEventListener("pointerdown", function (e) {
      if (!ddOpen) return;
      if (ddOpen.wrap && ddOpen.wrap.contains(e.target)) return;
      ddClose(false);
    }, true);
    document.addEventListener("keydown", function (e) {
      if (ddOpen && e.key === "Escape") ddClose(true);
    });
    window.addEventListener("resize", function () {
      if (!ddOpen) return;
      if (!ddOpen.wrap || !ddOpen.wrap.isConnected) { ddClose(false); return; }
      ddPlace(ddOpen.wrap);
    });
    document.addEventListener("scroll", function (e) {
      if (!ddOpen) return;
      if (e.target === ddOpen.menu || ddOpen.menu.contains(e.target)) return;
      if (!ddOpen.wrap || !ddOpen.wrap.isConnected || !ddOpen.btn || !ddOpen.btn.isConnected) { ddClose(false); return; }
      var br = ddOpen.btn.getBoundingClientRect();
      if (br.bottom < 0 || br.top > window.innerHeight) { ddClose(false); return; }
      ddPlace(ddOpen.wrap);
    }, true);
  }

  /* шкала справа: диапазон / машинный токен / длина секрета */
  function metaParts(row) {
    var out = [];
    if (row.t === "num") {
      if (row.hint) out.push(row.hint);
      if (row.min != null) out.push(fmtNum(row.min, row.step) + "–" + fmtNum(row.max, row.step));
    } else if (row.t === "sel") {
      if (row.e) { /* токен нужен, когда в списке человеческие слова */
        var v = getPath(row.k);
        out.push(String(v == null || v === "" ? "—" : v));
      }
    } else if (row.t === "pass") {
      var p = String(getPath(row.k) == null ? "" : getPath(row.k));
      out.push(p ? p.length + " " + L("симв.", "chars") : L("пусто", "empty"));
    }
    return out;
  }

  function rowHTML(row, catKey) {
    var raw = L(row.l[0], row.l[1]);
    var sep = raw.indexOf(" · ");
    var head = sep > 0 ? '<b class="r06-code">' + esc_(raw.slice(0, sep)) + "</b>" : "";
    var tail = sep > 0 ? raw.slice(sep + 3) : raw;
    var meta = metaParts(row);
    var undo = row.t === "lang" ? "" :
      '<button type="button" class="r06-undo" data-undo="' + esc_(row.k) + '" title="' +
      esc_(L("вернуть загруженное значение", "revert to the loaded value")) + '" aria-label="' +
      esc_(L("Отменить правку", "Revert change")) + '">⟲</button>';
    return '<div class="r06-row" data-k="' + esc_(row.k) + '">' +
      '<span class="r06-l">' +
        '<span class="r06-l-t" title="' + esc_(raw) + '">' + head + esc_(tail) + "</span>" +
        '<span class="r06-l-m"><code class="r06-key">' + esc_(relPath(row.k, catKey)) + "</code>" +
          '<span class="r06-meta" data-meta>' +
            '<span class="r06-mv" data-mv>' + esc_(meta.join(" · ")) + "</span>" +
            '<b class="r06-was" data-was></b>' + undo +
          "</span>" +
        "</span>" +
      "</span>" +
      '<span class="r06-c">' + ctlHTML(row) + "</span>" +
      "</div>";
  }

  /* ---------- лестница уровней ---------- */
  function tiersHTML() {
    var r = ((typeof S !== "undefined" && S && S.cfg && S.cfg.routing) || {});
    var t = r.tiers || [], def = String(r.default_tier == null ? "" : r.default_tier), i;
    var h = '<div class="r06-ladder" data-tiers><div class="r06-ladder-h">' +
      '<b>' + esc_(L("Очередь уровней", "Tier queue")) + "</b>" +
      '<span class="r06-ladder-hh">' + esc_(L("сверху вниз — порядок проб", "top to bottom — order of attempts")) + "</span>" +
      '<span class="r06-ladder-tn">' + t.length + " " + tierWord(t.length) + "</span></div>";
    if (!t.length) {
      return h + '<div class="r06-ladder-empty">' + esc_(L("ядро не отдало список уровней", "core returned no tiers")) + "</div></div>";
    }
    h += '<ol class="r06-ladder-l">';
    for (i = 0; i < t.length; i++) {
      var nm = String(t[i]), isDef = nm === def;
      h += '<li class="r06-ladder-r' + (isDef ? " is-def" : "") + '">' +
        '<span class="r06-ladder-k" aria-hidden="true">' + (i + 1) + "</span>" +
        '<span class="r06-ladder-nm" title="' + esc_(nm) + '">' + esc_(nm) + "</span>" +
        (isDef ? '<span class="r06-ladder-tag">' + esc_(L("★ по умолчанию", "★ default")) + "</span>" : "") +
        '<span class="r06-ladder-a">' +
          '<button type="button" class="r06-tb" data-tier-def="' + i + '"' + (isDef ? " disabled" : "") +
            ' title="' + esc_(L("сделать по умолчанию", "make default")) + '" aria-label="' +
            esc_(L("Сделать уровнем по умолчанию", "Make default tier")) + '">★</button>' +
          '<button type="button" class="r06-tb" data-tier-up="' + i + '"' + (i === 0 ? " disabled" : "") +
            ' title="' + esc_(L("выше", "up")) + '" aria-label="' + esc_(L("Поднять уровень", "Move tier up")) + '">↑</button>' +
          '<button type="button" class="r06-tb" data-tier-down="' + i + '"' + (i === t.length - 1 ? " disabled" : "") +
            ' title="' + esc_(L("ниже", "down")) + '" aria-label="' + esc_(L("Опустить уровень", "Move tier down")) + '">↓</button>' +
        "</span></li>";
    }
    return h + "</ol></div>";
  }

  /* ---------- липкий пульт ---------- */
  function stripHTML() {
    var segs = "", i;
    for (i = 0; i < CATS.length; i++) {
      segs += '<button type="button" class="r06-seg" data-jump="' + CATS[i].id + '">' +
        '<b>' + CATS[i].n + "</b><span>" + esc_(L(CATS[i].s[0], CATS[i].s[1])) + "</span></button>";
    }
    var demo = (typeof DEMO !== "undefined" && DEMO);
    return '<div class="r06-strip" data-strip>' +
      '<span class="r06-gauge" data-gauge title="">' +
        '<span class="r06-gauge-v"><b data-cnt>0</b><span class="r06-gauge-of">/' + units() + "</span></span>" +
        '<span class="r06-gauge-t" data-state>' + esc_(L("синхронно", "in sync")) + "</span>" +
        '<i class="r06-gauge-m"><b data-meter></b></i>' +
      "</span>" +
      '<nav class="r06-rail" role="group" aria-label="' + esc_(L("Разделы конфига", "Config sections")) + '">' + segs + "</nav>" +
      '<span class="r06-grow"></span>' +
      '<button type="button" class="r06-btn" data-revert>' + esc_(L("откатить", "revert")) + "</button>" +
      '<button type="button" class="r06-btn is-primary" data-save>' +
        esc_(demo ? L("зафиксировать", "apply") : L("сохранить", "save")) + "</button>" +
      "</div>";
  }

  /* ---------- карта ядра ---------- */
  function pipeHTML() {
    var v = nodeVals(), out = '<section class="r06-pipe" aria-label="' + esc_(L("Карта ядра", "Core map")) + '">' +
      '<div class="r06-pipe-cap"><b>' + esc_(L("Карта ядра", "Core map")) + "</b>" +
      '<span>' + esc_(L("вход → защита → маршрут → каналы → ключи · клик по ступени — к разделу", "input → guards → routing → egress → keys · click a stage to jump")) + "</span></div>" +
      '<div class="r06-stages">';
    for (var i = 0; i < CATS.length; i++) {
      var cat = CATS[i];
      if (!v[cat.id]) continue;
      out += '<button type="button" class="r06-stage" data-jump="' + cat.id + '" title="' +
        esc_(L(cat.s[0], cat.s[1]) + ": " + v[cat.id][0] + " · " + v[cat.id][1]) + '">' +
        '<span class="r06-stage-i">' + cat.n + "</span>" +
        '<span class="r06-stage-t">' + esc_(L(cat.s[0], cat.s[1])) + "</span>" +
        '<span class="r06-stage-v" data-nv="' + cat.id + '">' + esc_(v[cat.id][0]) + "</span>" +
        '<span class="r06-stage-s" data-ns="' + cat.id + '">' + esc_(v[cat.id][1]) + "</span></button>";
    }
    return out + "</div></section>";
  }

  /* ---------- раздел ---------- */
  function groupHTML(g, cat) {
    var rows = "", i;
    for (i = 0; i < g.rows.length; i++) rows += rowHTML(g.rows[i], cat.key);
    /* подпись группы нужна, когда строк несколько или групп в разделе больше одной */
    var cap = (g.rows.length > 1 || cat.groups.length > 1)
      ? '<div class="r06-grp"><b>' + esc_(L(g.s[0], g.s[1])) + "</b><i></i></div>"
      : "";
    return '<div class="r06-grpbox">' + cap + '<div class="r06-grid">' + rows + "</div></div>";
  }

  function secHTML(cat) {
    var body = "", i;
    for (i = 0; i < cat.groups.length; i++) body += groupHTML(cat.groups[i], cat);
    if (cat.tiers) body += tiersHTML();
    var total = 0;
    for (i = 0; i < cat.groups.length; i++) total += cat.groups[i].rows.length;
    if (cat.tiers) total += 1;
    return '<section class="r06-sec" data-sec="' + cat.id + '" data-total="' + total + '" aria-label="' +
      esc_(L(cat.t[0], cat.t[1])) + '">' +
      '<header class="r06-sec-h">' +
        '<span class="r06-sec-n" aria-hidden="true">' + cat.n + "</span>" +
        '<span class="r06-sec-x">' +
          '<h2 class="r06-sec-t">' + esc_(L(cat.t[0], cat.t[1])) + "</h2>" +
          '<span class="r06-sec-d">' + esc_(L(cat.d[0], cat.d[1])) + "</span>" +
        "</span>" +
        '<span class="r06-sec-c" data-scnt>' + total + " " + fieldWord(total) + "</span>" +
        '<code class="r06-sec-k">' + esc_(cat.key) + "</code>" +
        '<i class="r06-sec-m"><b data-smeter></b></i>' +
      "</header>" +
      body + "</section>";
  }

  function footHTML() {
    var demo = (typeof DEMO !== "undefined" && DEMO);
    return '<div class="r06-foot">' +
      "<span><b>S.cfg</b> → SaveConfig</span>" +
      "<span>" + esc_(L("правка ложится в S.cfg сразу, запись в ядро — по кнопке",
        "an edit lands in S.cfg at once, the core is written by the button")) + "</span>" +
      "<span>" + esc_(L("⟲ в строке возвращает загруженное значение", "⟲ on a row reverts to the loaded value")) + "</span>" +
      (demo ? "<span class=\"r06-foot-demo\">" + esc_(L("демо: запись в ядро отключена", "demo: writing to the core is off")) + "</span>" : "") +
      "</div>";
  }

  function html() {
    if (typeof S === "undefined" || !S || !S.cfg) {
      return '<div class="r06-empty">' + esc_(L("ядро не отдало конфиг (GetConfig вернул пусто)", "core returned no config (GetConfig empty)")) + "</div>";
    }
    var out = stripHTML() + pipeHTML() + '<div class="r06-sheet">', i;
    for (i = 0; i < CATS.length; i++) out += secHTML(CATS[i]);
    return out + "</div>" + footHTML();
  }

  /* ---------- состояние ---------- */
  function rootEl() { return document.querySelector("#panel-settings [data-r06]"); }
  function tiersDirty() {
    var b = base();
    if (!b) return false;
    var cur = ((S.cfg || {}).routing || {});
    var was = (b.routing || {});
    return !eq(cur.tiers, was.tiers) || !eq(cur.default_tier, was.default_tier);
  }

  function paint(root) {
    root = root || rootEl();
    if (!root) return;
    var i, n = 0, changed = [];
    var rows = root.querySelectorAll(".r06-row[data-k]");
    for (i = 0; i < rows.length; i++) {
      var row = rows[i], k = row.getAttribute("data-k"), r = rowById(k);
      var dirty = !eq(getPath(k), getBase(k));
      if (dirty) { n++; changed.push(k); }
      row.classList.toggle("is-dirty", dirty);
      var was = row.querySelector("[data-was]");
      if (was) was.textContent = dirty ? L("было ", "was ") + shortVal(k) : "";
      var mv = row.querySelector("[data-mv]");
      if (mv && r && r.t === "sel" && r.e) mv.textContent = String(getPath(k) == null || getPath(k) === "" ? "—" : getPath(k));
    }
    ddSyncAll(root);
    var td = tiersDirty();
    if (td) { n++; changed.push("routing.tiers"); }
    setDirty(n > 0);

    var total = +units();
    var cnt = root.querySelector("[data-cnt]"), gauge = root.querySelector("[data-gauge]");
    var state = root.querySelector("[data-state]"), meter = root.querySelector("[data-meter]");
    if (cnt) cnt.textContent = String(n);
    if (state) {
      state.textContent = n > 0
        ? L("не записано", "not saved")
        : L("синхронно", "in sync");
    }
    if (gauge) gauge.classList.toggle("is-dirty", n > 0);
    if (meter) meter.style.width = Math.min(100, Math.round(n / total * 100)) + "%";
    if (gauge) gauge.title = n > 0
      ? changed.slice(0, 10).join(" · ") + (changed.length > 10 ? " …" : "")
      : L("конфиг совпадает с ядром · " + (total - 1) + " параметр и очередь уровней", "config matches the core");

    /* счётчики разделов + полоса заполнения + точки на рельсе */
    var secs = root.querySelectorAll("[data-sec]");
    for (i = 0; i < secs.length; i++) {
      var sec = secs[i], id = sec.getAttribute("data-sec");
      var c = sec.querySelectorAll(".r06-row.is-dirty").length;
      if (sec.querySelector("[data-tiers]") && td) c++;
      var tot = +sec.getAttribute("data-total") || 0;
      sec.classList.toggle("is-dirty", c > 0);
      var cc = sec.querySelector("[data-scnt]");
      if (cc) {
        cc.classList.toggle("is-dirty", c > 0);
        cc.innerHTML = c > 0
          ? "<b>" + c + "</b>" + esc_(L(" из ", " of ")) + tot
          : "<b>" + tot + "</b> " + esc_(fieldWord(tot));
      }
      var sm = sec.querySelector("[data-smeter]");
      if (sm) sm.style.width = (tot ? Math.round(c / tot * 100) : 0) + "%";
      var jumps = root.querySelectorAll('[data-jump="' + id + '"]');
      for (var j = 0; j < jumps.length; j++) jumps[j].classList.toggle("is-dirty", c > 0);
    }

    /* живые значения ступеней */
    var nv = nodeVals(), val, sub;
    for (var id2 in nv) {
      if (!Object.prototype.hasOwnProperty.call(nv, id2)) continue;
      val = root.querySelector('[data-nv="' + id2 + '"]');
      if (val) val.textContent = nv[id2][0];
      sub = root.querySelector('[data-ns="' + id2 + '"]');
      if (sub) sub.textContent = nv[id2][1];
    }

    var save = root.querySelector("[data-save]"), rev = root.querySelector("[data-revert]");
    if (save) { save.disabled = !n; save.setAttribute("aria-disabled", String(!n)); }
    if (rev) { rev.disabled = !n; rev.setAttribute("aria-disabled", String(!n)); }
  }

  /* ---------- шапка панели ---------- */
  function syncHeader() {
    var meta = document.getElementById("settings-meta");
    var s = ((typeof S !== "undefined" && S && S.cfg && S.cfg.server) || {});
    var n = FLAT.length;
    /* порядок важен: на узком окне обрезается хвост, и адрес уходит последним */
    var t = n + " " + paramWord(n) + " + " + L("очередь", "queue") +
      " · " + CATS.length + " " + (en() ? "sections" : pl(CATS.length, ["раздел", "раздела", "разделов"])) +
      " · " + String(s.host || "—") + ":" + String(s.port == null ? "—" : s.port);
    if (meta) { meta.textContent = t; meta.title = t; }
    var lt = document.getElementById("settings-live-txt");
    var live = document.getElementById("settings-live");
    if (lt) lt.textContent = (typeof DEMO !== "undefined" && DEMO)
      ? L("DEMO · запись в ядро отключена", "DEMO · writing to core is off")
      : L("LIVE · запись в ядро", "LIVE · writing to core");
    if (live && live.classList) live.classList.toggle("paused", !!(typeof DEMO !== "undefined" && DEMO));
  }

  /* ---------- навигация ---------- */
  function cont() { return document.querySelector(".content"); }
  var lockId = null;

  function setActive(root, id) {
    var chips = root.querySelectorAll(".r06-seg"), i;
    for (i = 0; i < chips.length; i++) {
      var on = chips[i].getAttribute("data-jump") === id;
      chips[i].classList.toggle("is-on", on);
      if (on) chips[i].setAttribute("aria-current", "true");
      else chips[i].removeAttribute("aria-current");
    }
  }
  function jump(id) {
    var root = rootEl();
    if (!root) return;
    var sec = root.querySelector('[data-sec="' + id + '"]'), c = cont();
    if (!sec || !c) return;
    var strip = root.querySelector("[data-strip]");
    var sh = strip ? strip.getBoundingClientRect().height : 0;
    var top = sec.getBoundingClientRect().top - c.getBoundingClientRect().top + c.scrollTop - sh - 2;
    c.scrollTop = Math.max(0, top);
    lockId = id;
    setActive(root, id);
    try {
      sec.setAttribute("data-flash", "1");
      setTimeout(function () { try { sec.removeAttribute("data-flash"); } catch (e) {} }, 700);
    } catch (e2) {}
  }
  function mark() {
    var root = rootEl();
    var panel = document.getElementById("panel-settings");
    if (!root || !panel || !panel.classList.contains("active")) return;
    var c = cont();
    if (!c) return;
    if (lockId) { setActive(root, lockId); return; }
    var secs = root.querySelectorAll("[data-sec]"), strip = root.querySelector("[data-strip]");
    if (!secs.length) return;
    var limit = c.getBoundingClientRect().top + (strip ? strip.getBoundingClientRect().height : 0) + 10;
    var at = secs[0].getAttribute("data-sec"), i;
    for (i = 0; i < secs.length; i++) if (secs[i].getBoundingClientRect().top <= limit) at = secs[i].getAttribute("data-sec");
    if (c.scrollTop + c.clientHeight >= c.scrollHeight - 2) at = secs[secs.length - 1].getAttribute("data-sec");
    setActive(root, at);
  }
  function spy() {
    var c = cont();
    if (!c || c._r06Spy) return;
    c._r06Spy = 1;
    var raf = 0;
    var unlock = function () { lockId = null; };
    c.addEventListener("scroll", function () {
      if (raf) return;
      raf = requestAnimationFrame(function () { raf = 0; try { mark(); } catch (e) {} });
    }, { passive: true });
    c.addEventListener("wheel", unlock, { passive: true });
    c.addEventListener("touchstart", unlock, { passive: true });
    c.addEventListener("keydown", unlock);
    c.addEventListener("pointerdown", function (e) {
      if (!e.target || !e.target.closest || !e.target.closest("[data-jump]")) unlock();
    });
  }

  /* ---------- события ---------- */
  function onInput(e) {
    var el = e.target;
    if (!el || el.tagName !== "INPUT" || !el.hasAttribute("data-k")) return;
    var k = el.getAttribute("data-k"), row = rowById(k);
    if (!row) return;
    if (row.t === "num") {
      if (el.value === "" || el.value === "-" || el.value === ".") return;
      var v = Number(el.value);
      if (!isFinite(v)) return;
      setPath(k, v);
    } else if (row.t === "text" || row.t === "pass") {
      setPath(k, el.value);
    } else return;
    paint(rootEl());
  }

  function onChange(e) {
    var el = e.target;
    if (!el || !el.hasAttribute) return;
    if (el.hasAttribute("data-lang")) {
      var m = el.value;
      var go = function () { try { I18N.setMode(m); } catch (er) { say(L("язык не переключился", "language not switched"), "err"); } };
      try {
        if (typeof DEMO !== "undefined" && !DEMO && typeof Go === "function" && Go() && !eq(S.cfg, base())) {
          Promise.resolve(saveCfg({ auto: true })).then(go, go);
          return;
        }
      } catch (eL) {}
      go();
      return;
    }
    var k = el.getAttribute("data-k");
    if (!k) return;
    var row = rowById(k), root = rootEl();
    if (!row) return;
    if (row.t === "num") {
      var v = quant(el.value, row);
      setPath(k, v);
      el.value = fmtNum(v, row.step);
    } else if (row.t === "sw") {
      setPath(k, !!el.checked);
      var x = el.parentNode && el.parentNode.querySelector(".r06-sw-x");
      if (x) x.textContent = el.checked ? L("ВКЛ", "ON") : L("ВЫКЛ", "OFF");
    } else {
      setPath(k, el.value);
    }
    paint(root);
  }

  function onClick(e) {
    var t = e.target;
    if (!t || !t.closest) return;
    var root = rootEl();
    if (!root) return;

    /* дропдаун: плашка открывает/закрывает своё меню, пункт выбирает значение */
    var ddb = t.closest(".r06-dd-btn");
    if (ddb) {
      if (ddb.disabled) return;
      var ddw = ddb.closest(".r06-dd");
      if (!ddw) return;
      if (ddOpen && ddOpen.wrap === ddw) ddClose(true);
      else ddOpenMenu(ddw);
      return;
    }
    var ddo = t.closest(".r06-dd-opt");
    if (ddo) {
      if (ddo.classList.contains("is-dis")) return;
      if (ddOpen && ddo.parentNode === ddOpen.menu) ddPick(+ddo.getAttribute("data-i"));
      return;
    }

    var j = t.closest("[data-jump]");
    if (j) { jump(j.getAttribute("data-jump")); return; }

    var st = t.closest("[data-step]");
    if (st) {
      var cell = st.closest(".r06-num"), inp = cell && cell.querySelector("input[data-k]");
      if (inp) {
        var k = inp.getAttribute("data-k"), row = rowById(k);
        var dir = +st.getAttribute("data-step");
        var step = row && row.step ? Number(row.step) : 1;
        var cur = Number(getPath(k));
        if (!isFinite(cur)) cur = row && row.min != null ? row.min : 0;
        var nv = quant(cur + dir * step, row || {});
        setPath(k, nv);
        inp.value = fmtNum(nv, row && row.step);
        paint(root);
      }
      return;
    }

    var eye = t.closest("[data-eyes]");
    if (eye) {
      var pw = eye.closest(".r06-pw"), pin = pw && pw.querySelector("input");
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
      if (!b) { say(L("конфиг ещё не загружен — откатывать нечего", "config not loaded yet — nothing to revert")); return; }
      var o = getBase(path);
      if (o === undefined) { say(L("в загруженном конфиге этого поля нет", "this key is absent in the loaded config")); return; }
      setPath(path, o);
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
        var nxt = tu ? idx - 1 : idx + 1;
        if (nxt < 0 || nxt >= tiers.length) return;
        var tmp = tiers[idx]; tiers[idx] = tiers[nxt]; tiers[nxt] = tmp;
        r.tiers = tiers;
      }
      render(root);
      return;
    }

    if (t.closest("[data-revert]")) {
      try { if (typeof w01SRevert === "function") w01SRevert(); else say(L("откат недоступен", "revert unavailable"), "err"); }
      catch (er) { say(L("откат не удался: ", "revert failed: ") + ((er && er.message) || er), "err"); }
      return;
    }
    if (t.closest("[data-save]")) {
      try { if (typeof saveCfg === "function") saveCfg(); else say(L("сохранение недоступно", "save unavailable"), "err"); }
      catch (er2) { say(L("сохранить не удалось: ", "save failed: ") + ((er2 && er2.message) || er2), "err"); }
      return;
    }
  }

  function onKey(e) {
    var t = e.target;
    var ddb = t && t.closest ? t.closest(".r06-dd-btn") : null;
    if (ddb) {
      var ddw = ddb.closest(".r06-dd");
      var isOpen = !!(ddOpen && ddOpen.wrap === ddw);
      var key = e.key;
      if (key === "ArrowDown" || key === "ArrowUp") {
        e.preventDefault(); e.stopPropagation();
        if (!isOpen) ddOpenMenu(ddw);
        else ddMove(key === "ArrowDown" ? 1 : -1);
        return;
      }
      if (key === "Home" || key === "End") {
        if (!isOpen) return;
        e.preventDefault(); e.stopPropagation();
        ddMove(0, key === "Home" ? 0 : 1e9);
        return;
      }
      if (key === "Enter" || key === " " || key === "Spacebar") {
        e.preventDefault(); e.stopPropagation();
        if (!isOpen) ddOpenMenu(ddw);
        else if (ddOpen && ddOpen.opts[ddOpen.act]) ddPick(ddOpen.opts[ddOpen.act].index);
        return;
      }
      if (key === "Escape") {
        if (isOpen) { e.preventDefault(); e.stopPropagation(); ddClose(true); }
        return;
      }
      if (key === "Tab") { if (isOpen) ddClose(false); return; }
      return; /* прочие клавиши на плашке не уходят в логику Enter/INPUT */
    }
    if (e.key !== "Enter") return;
    if (t && t.tagName === "INPUT" && t.hasAttribute("data-k")) { e.preventDefault(); t.blur(); }
  }

  function wire(root) {
    if (root._r06Wired) return;
    root._r06Wired = 1;
    root.addEventListener("input", onInput);
    root.addEventListener("change", onChange);
    root.addEventListener("click", onClick);
    root.addEventListener("keydown", onKey);
    root.addEventListener("mouseover", onDdHover);
    root.addEventListener("mousedown", onDdDown);
    wireDdDoc();
  }

  /* ---------- фокус, переживающий перерисовку ---------- */
  function focusSnap(root) {
    var ae = document.activeElement;
    if (!ae || !root.contains(ae)) return null;
    var dd = ae.closest ? ae.closest(".r06-dd") : null;
    if (dd) {
      var sl = ddSel(dd);
      if (sl) return { dd: sl.hasAttribute("data-lang") ? "@lang" : sl.getAttribute("data-k"), lang: sl.hasAttribute("data-lang") };
    }
    if (ae.hasAttribute && ae.hasAttribute("data-lang")) return { lang: true };
    if (ae.tagName === "INPUT" && ae.hasAttribute("data-k")) {
      return { k: ae.getAttribute("data-k"), s: ae.selectionStart, e: ae.selectionEnd };
    }
    return null;
  }
  function focusRestore(root, fs) {
    if (!fs) return;
    var el = null, i, sl;
    if (fs.dd) {
      var list = root.querySelectorAll(".r06-dd");
      for (i = 0; i < list.length; i++) {
        sl = ddSel(list[i]);
        if (!sl) continue;
        if (fs.lang ? sl.hasAttribute("data-lang") : sl.getAttribute("data-k") === fs.dd) {
          el = list[i].querySelector(".r06-dd-btn");
          break;
        }
      }
    } else if (fs.k) {
      el = root.querySelector('input[data-k="' + fs.k + '"]');
    } else if (fs.lang) {
      el = root.querySelector("input[data-lang], select[data-lang]");
    }
    if (!el) return;
    try { el.focus({ preventScroll: true }); } catch (e1) { try { el.focus(); } catch (e2) {} }
    if (fs.s != null && el.setSelectionRange) {
      try { el.setSelectionRange(fs.s, fs.e); } catch (e3) {}
    }
  }

  /* ---------- отрисовка ---------- */
  function render(root) {
    root = root || rootEl();
    if (!root) return;
    var c = cont(), st = c ? c.scrollTop : 0;
    var fs = focusSnap(root);
    ddClose(false);
    root.innerHTML = html();
    wire(root);
    paint(root);
    syncHeader();
    if (c && st) {
      var pb = c.style.scrollBehavior;
      c.style.scrollBehavior = "auto";
      c.scrollTop = st;
      c.style.scrollBehavior = pb || "";
    }
    focusRestore(root, fs);
    spy();
    mark();
  }

  function mount(host) {
    var panel = document.getElementById("panel-settings");
    if (!panel) return null;
    var el = null;
    if (typeof host === "string") el = document.querySelector(host);
    else if (host && host.nodeType === 1) el = host;
    if (el && (!panel.contains(el) || !el.hasAttribute("data-r06"))) el = null;
    if (!el) el = panel.querySelector("[data-r06]");
    if (!el) {
      /* статическая вёрстка прошлой панели не правится — снимаем её целиком,
         оставляя шапку .hd; дальше панель рисует только этот модуль */
      var kids = Array.prototype.slice.call(panel.children), i;
      for (i = 0; i < kids.length; i++) if (!(kids[i].classList && kids[i].classList.contains("hd"))) panel.removeChild(kids[i]);
      el = document.createElement("div");
      el.className = "r06";
      el.id = "settings-editor";
      el.setAttribute("data-r06", "");
      el.setAttribute("data-no-i18n", "");
      panel.appendChild(el);
    }
    render(el);
    return el;
  }

  var api = { mount: mount, render: function () { return mount(); } };
  /* js/w01-settings.js ищет модуль по имени N09 — оставляем то же имя входа */
  window.N09 = api;
  window.R06 = api;
  try { window.w01SettingsDraw = function () { return mount(); }; } catch (e) {}
  return api;
})();
