/* ============================================================
   p02 · экран провайдера — свой модуль.
   Вызывается из renderProvDetail() (тело функции заменено на вызов PCard).
   Данные — те же S.prov / S.kbp / S.breakers / S.health / S.logs, ничего
   своего не держим, кроме позиции экрана, фильтра моделей и черновика
   имени ключа. Один провайдер = шапка + KPI + 4 внутренних экрана.
   ============================================================ */
(function () {
  "use strict";

  var SCREENS = ["Сводка", "Ключи", "Модели", "Связь"];
  var LV = ["auto", "low", "high", "xhigh", "max"];
  var FAMC = {
    gpt: "#10a37f", claude: "#d97757", gemini: "#4285f4", grok: "#e8e8e8",
    deepseek: "#7c6cf0", qwen: "#7b61ff", mistral: "#ff7000", muse: "#00d4aa",
    mimo: "#ff4d8d", nemo: "#76b900", other: "#c22a31"
  };
  var AUTH = ["bearer", "none", "public"];
  var EGRESS = ["off", "pool", "warp", "iprotate", "direct"];
  var NAVCO = 700; // мс между переключениями колесом на границе прокрутки

  var ST = { prov: "", screen: 0, scroll: [0, 0, 0, 0], filter: "", revealed: "", lastNav: 0 };
  var TESTMODEL = {}; // модель для «▶ Тест» по провайдеру (переживает перерисовку)

  /* ---------- мелкие форматтеры ---------- */
  function pct(a, b) { a = +a || 0; b = +b || 0; return b > 0 ? Math.max(0, Math.min(100, Math.round(a / b * 100))) : 0; }
  function num(n) { n = +n || 0; return n >= 1000 ? (n / 1000).toFixed(n >= 10000 ? 0 : 1) + "k" : String(Math.round(n)); }
  function pl(n, a, b, c) { try { return plural(n, a, b, c); } catch (x) { return c; } }
  function fam(m) {
    m = String(m || "").toLowerCase();
    if (/^gpt|^o\d|codex/.test(m)) return "gpt";
    if (/claude/.test(m)) return "claude";
    if (/gemini|gemma/.test(m)) return "gemini";
    if (/grok/.test(m)) return "grok";
    if (/deepseek/.test(m)) return "deepseek";
    if (/qwen/.test(m)) return "qwen";
    if (/mistral|mixtral/.test(m)) return "mistral";
    if (/muse/.test(m)) return "muse";
    if (/mimo/.test(m)) return "mimo";
    if (/nemotron|laguna|ling-/.test(m)) return "nemo";
    return "other";
  }
  function mask(s) {
    s = String(s || "");
    if (!s) return "—";
    if (s.length <= 12) return s.slice(0, 3) + "…" + s.slice(-2);
    return s.slice(0, 9) + "…" + s.slice(-4);
  }

  /* ---------- данные ---------- */
  function poolKeys(e) {
    if (!S.kbp) return [];
    if (e.name === "__GLOBAL__") return S.kbp["глобальные"] || [];
    return S.kbp[e.name] || [];
  }
  function healthOf(e) { return (S.health || []).find(function (x) { return x.name === e.name; }) || null; }
  function brk(e) { return (S.breakers || []).find(function (x) { return x.provider === e.name; }) || {}; }
  function brkState(e) { var b = brk(e); return b.state || e.breaker || "closed"; }
  function untilS(e) { var b = brk(e); return Math.max(0, +(b.opened_until_s || e.breaker_until_s || 0)); }
  function fails(e) {
    var b = brk(e);
    return b.failures != null ? b.failures : (b.consecutive_failures != null ? b.consecutive_failures : (e.breaker_failures || 0));
  }
  function lastErr(e) { var b = brk(e); return b.last_error || e.last_err || ""; }
  function stateId(e) {
    var s = brkState(e);
    if (s === "open") return "open";
    if (s === "half_open") return "half";
    var tot = e.keys_pct < 0 ? 0 : (e.keys || 0);
    if (tot <= 0) return "nokeys";
    if (!(e.keys_healthy || 0)) return "backoff";
    return "ok";
  }
  function agg(keys) {
    var o = { tq: 0, tu: 0, rq: 0, ru: 0, bu: 0, bus: 0 };
    keys.forEach(function (k) {
      o.tq += +k.quota_tokens || 0; o.tu += +k.usage_tokens || 0;
      o.rq += +k.quota_reqs || 0; o.ru += +k.usage_reqs || 0;
      o.bu += +k.budget_usd || 0; o.bus += +k.usage_usd || 0;
    });
    return o;
  }
  function modelStats(e) {
    var map = {};
    (S.logs || []).forEach(function (l) {
      var m = l.model || l.requested_model;
      if (!m) return;
      if (l.provider && l.provider !== e.name) return;
      var s = map[m] || (map[m] = { n: 0, err: 0 });
      s.n++;
      var code = +l.http_status || 0;
      if (code && code !== 200) s.err++;
    });
    return map;
  }

  /* ---------- тексты состояния ---------- */
  function verdict(e) {
    var id = stateId(e);
    var live = (e.keys_healthy || 0), tot = e.keys_pct < 0 ? 0 : (e.keys || 0);
    if (id === "ok") return {
      lvl: "on", t: "Все каналы в норме",
      d: "Брейкер закрыт · живых ключей " + live + " из " + tot + " · модели в каталоге отвечают.",
      a: ""
    };
    if (id === "open") return {
      lvl: "err", t: "Брейкер открыт — трафик идёт в обход",
      d: "Провайдер исключён из маршрута на " + fmtMmSs(untilS(e)) + (lastErr(e) ? " · причина: " + lastErr(e) : "") + ".",
      a: "Ничего не нажимайте — вернётся сам по таймеру. Если повторяется — смотрите Журналы и ключи."
    };
    if (id === "half") return {
      lvl: "warn", t: "Пробные запросы (полуоткрытие)",
      d: "До следующей проверки " + fmtMmSs(untilS(e)) + " · сбоев подряд: " + fails(e) + ".",
      a: "Не гоняйте «Тест» пачками — пробы прерываются."
    };
    if (id === "backoff") return {
      lvl: "warn", t: "Все ключи в бэкоффе",
      d: "Живых ключей нет" + (untilS(e) ? " · ожидание " + fmtMmSs(untilS(e)) : "") + (lastErr(e) ? " · причина: " + lastErr(e) : "") + ".",
      a: "Дождитесь снятия кулдауна или добавьте ключ — кнопка «+ ключ» в шапке."
    };
    return {
      lvl: "warn", t: "Ключи не настроены",
      d: "Без ключей маршрут через провайдера невозможен.",
      a: "«+ ключ» в шапке — добавьте хотя бы один."
    };
  }
  function stateChip(e) {
    var id = stateId(e);
    if (id === "ok") return { lvl: "on", t: "в сети" };
    if (id === "open") return { lvl: "err", t: "брейкер" };
    if (id === "half") return { lvl: "warn", t: "проба" };
    if (id === "backoff") return { lvl: "warn", t: "бэкофф" };
    return { lvl: "warn", t: "нет ключей" };
  }

  /* ---------- шапка / KPI / навигация ---------- */
  function headHTML(e) {
    var sc = stateChip(e);
    var initials = String(e.name || "?").replace(/[^A-Za-zА-Яа-я0-9]/g, "").slice(0, 2) || "?";
    var ty = String(e.type || "").toLowerCase();
    var nm = String(e.name || "").toLowerCase();
    var sameType = !ty || ty === nm || nm.indexOf(ty) === 0; // «OpenAI · openai» не дублируем
    var tot = e.keys_pct < 0 ? 0 : (e.keys || 0);
    var sub = [];
    if (e.base_url) sub.push("<code title=\"" + esc(e.base_url) + "\">" + esc(e.base_url) + "</code>");
    else sub.push("<code>—</code>");
    if (!sameType) sub.push("<i>·</i>" + esc(e.type));
    if (e.tier) sub.push("<i>·</i>" + esc(e.tier));
    sub.push("<i>·</i>" + tot + " " + pl(tot, "ключ", "ключа", "ключей"));
    return '<div class="pv-head">' +
      '<button type="button" class="pv-back" data-pcard-back title="Назад к списку провайдеров" aria-label="Назад к списку">←</button>' +
      '<span class="pv-mono" aria-hidden="true">' + esc(initials) + '</span>' +
      '<div class="pv-id"><div class="pv-nm"><span title="' + esc(e.name) + '">' + esc(e.name === "__GLOBAL__" ? "Глобальные ключи" : e.name) + '</span>' +
        (sameType ? "" : '<span class="pv-ty">' + esc(e.type) + '</span>') + '</div>' +
        '<div class="pv-sub">' + sub.join(" ") + '</div></div>' +
      '<span class="pv-st ' + sc.lvl + '"><i></i>' + esc(sc.t) + '</span>' +
      '<div class="pv-acts">' +
        '<button type="button" class="pv-b" data-pcard-test title="Живой тест модели «' + esc(testModel(e)) + '»">▶ тест</button>' +
        '<button type="button" class="pv-b" data-pcard-copy title="Скопировать base_url">⧉ url</button>' +
        '<button type="button" class="pv-b acc" data-pcard-add-key title="Добавить ключ в пул">+ ключ</button>' +
        '<button type="button" class="pv-b acc" data-pcard-add-model title="Добавить модель в каталог">+ модель</button>' +
      '</div></div>';
  }
  function kpi(l, v, s, t) {
    return '<div class="pv-kpi" data-pcard-kpi data-t="' + t + '"><span class="l">' + l + '</span><span class="v">' + v + '</span><span class="s">' + s + '</span></div>';
  }
  function kpisHTML(e) {
    var hc = healthOf(e);
    var lat = hc && hc.ok && hc.latency_ms != null ? hc.latency_ms : null;
    var tot = e.keys_pct < 0 ? 0 : (e.keys || 0);
    var live = tot ? (e.keys_healthy || 0) : 0;
    var st = stateId(e);
    var brkT = st === "open" ? "открыт" : st === "half" ? "проба" : st === "nokeys" ? "—" : "закрыт";
    var brkS = st === "open" || st === "half" ? "возврат " + fmtMmSs(untilS(e)) : "сбоев подряд: " + fails(e);
    var mods = (e.models || []).length;
    return '<div class="pv-kpis">' +
      kpi("ключи", live + "<i>/" + tot + "</i>", live + " " + pl(live, "живой", "живых", "живых") + " из " + tot, live ? "on" : "err") +
      kpi("rtt", lat == null ? "—" : String(lat) + "<i>мс</i>", lat == null ? "нет замера" : "healthcheck", lat == null ? "" : (lat <= 1000 ? "on" : "err")) +
      kpi("модели", mods ? String(mods) : "∞", mods ? "в каталоге" : "wildcard", mods ? "on" : "") +
      kpi("брейкер", brkT, brkS, st === "open" ? "err" : (st === "half" ? "err" : "on")) +
      '</div>';
  }
  function navHTML(e) {
    var cnt = ["", String(e.keys_pct < 0 ? 0 : (e.keys || 0)), String((e.models || []).length), ""];
    return '<div class="pv-nav" role="tablist" aria-label="Разделы провайдера">' +
      SCREENS.map(function (s, i) {
        return '<button type="button" role="tab" data-pcard-nav data-sc="' + i + '" aria-selected="' + (i === ST.screen) + '" aria-controls="pv-scr-' + i + '">' + s +
          (cnt[i] ? '<span class="n">' + cnt[i] + '</span>' : "") + '</button>';
      }).join("") + '</div>';
  }

  /* ---------- сводка ---------- */
  function chip(k, v) { return '<span class="pv-chip"><span class="pv-hint">' + esc(k) + '</span> ' + esc(v) + '</span>'; }
  function flowHTML(e) {
    var s = brkState(e);
    var nodes = [["закрыт", s === "closed"], ["открыт", s === "open"], ["полуоткрытие", s === "half_open"]];
    var out = nodes.map(function (n, i) {
      var cls = "pv-node" + (n[1] ? " on" : "") + (n[1] && s === "open" ? " err" : n[1] && s === "half_open" ? " warn" : "");
      return '<span class="' + cls + '">' + n[0] + '</span>' + (i < nodes.length - 1 ? '<i>→</i>' : "");
    }).join("");
    var tail = s === "closed" ? "" : '<span class="pv-sp"></span><span>возврат через <b style="color:var(--ink2);font-weight:500">' + fmtMmSs(untilS(e)) + '</b></span>';
    return '<div class="pv-flow">' + out + tail + '</div>';
  }
  function metersHTML(keys, a) {
    if (!keys.length) return '<div class="pv-empty"><b>нет ключей с метаданными</b><span>квота считается по ключам пула</span></div>';
    var rows = [["токены", a.tu, a.tq], ["запросы", a.ru, a.rq], ["usd", a.bus, a.bu]];
    return '<div class="pv-meters">' + rows.map(function (r) {
      var p = pct(r[1], r[2]), has = r[2] > 0;
      return '<div class="pv-m">' +
        '<span class="k">' + r[0] + '</span>' +
        '<span class="pv-track' + (p >= 90 ? " hi" : "") + '"><i style="width:' + (has ? p : 0) + '%"></i></span>' +
        '<span class="v">' + compact(Math.round(r[1])) + ' / ' + (has ? compact(Math.round(r[2])) : "—") + '</span>' +
        '</div>';
    }).join("") + '</div>';
  }
  function logRowsHTML(e, onlyErr, max) {
    var logs = S.logs || [];
    var out = [];
    for (var i = 0; i < logs.length && out.length < max; i++) {
      var l = logs[i];
      if (l.provider && l.provider !== e.name) continue;
      var c0 = +l.http_status || 0;
      if (onlyErr && (!c0 || c0 === 200)) continue;
      out.push(l);
    }
    if (!out.length) return '<div class="pv-empty"><b>' + (onlyErr ? "отказов в журнале нет" : "журнал пуст") + '</b><span>' +
      (onlyErr ? "последние вызовы прошли без ошибок" : "вызовы появятся после первого запроса через шлюз") + '</span></div>';
    return '<div class="pv-logs">' + out.map(function (l) {
      var code = +l.http_status || 0;
      var ok = !code || code === 200;
      var t = String(l.ts || "").slice(11, 19);
      var err = l.error ? String(l.error) : "";
      return '<div class="pv-lrow" title="' + esc((l.model || "") + (err ? " · " + err : "")) + '">' +
        '<span class="pv-lt">' + esc(t || "—") + '</span>' +
        '<span class="pv-lm">' + esc(l.model || l.requested_model || "—") + '</span>' +
        '<span class="pv-lc' + (ok ? "" : " err") + '">' + esc(ok ? "200" : String(code)) + '</span>' +
        '<span class="pv-ld">' + (l.duration_ms != null ? Math.round(+l.duration_ms) + " мс" : "") + '</span>' +
        '</div>';
    }).join("") + '</div>';
  }

  function topModelsHTML(e) {
    var st = modelStats(e);
    var rows = Object.keys(st).map(function (m) { return { m: m, n: st[m].n, err: st[m].err }; })
      .sort(function (a, b) { return b.n - a.n || (a.m < b.m ? -1 : 1); }).slice(0, 7);
    if (!rows.length) return '<div class="pv-empty"><b>вызовов пока не было</b><span>счётчики появятся из журнала шлюза</span></div>';
    return '<div class="pv-trows">' +
      '<div class="pv-trow th"><span></span><span class="n">модель</span><span class="c">вызовы</span><span class="e">ошибки</span></div>' +
      rows.map(function (r) {
        return '<div class="pv-trow" title="' + esc(r.m + " · " + r.n + " вызовов" + (r.err ? " · " + r.err + " ошибок" : "")) + '">' +
          '<span class="pv-fam" style="background:' + (FAMC[fam(r.m)] || FAMC.other) + '"></span>' +
          '<span class="n">' + esc(r.m) + '</span>' +
          '<span class="c">' + r.n + '</span>' +
          '<span class="e' + (r.err ? " bad" : "") + '">' + (r.err || "—") + '</span>' +
          '</div>';
      }).join("") + '</div>';
  }

  function sumHTML(e) {
    var v = verdict(e);
    var keys = poolKeys(e), a = agg(keys);
    var hc = healthOf(e);
    var h = '<div class="pv-grid2">';
    h += '<div class="pv-col">';
    h += '<div class="pv-sec" data-pcard-sec>' +
      '<div class="pv-sec-h"><span class="t">Состояние</span><span class="pv-hint">брейкер · ключи · маршрут</span></div>' +
      '<div class="pv-verdict ' + v.lvl + '"><div class="pv-vt">' + esc(v.t) + '</div><div class="pv-vd">' + esc(v.d) + '</div>' +
      (v.a ? '<div class="pv-va">' + esc(v.a) + '</div>' : "") + '</div>' +
      flowHTML(e) +
      '<div class="pv-facts"><span>сбоев подряд: <b>' + fails(e) + '</b></span>' +
      (lastErr(e) ? '<span>последняя: <code>' + esc(lastErr(e)) + '</code></span>' : '<span>последней ошибки нет</span>') +
      '<span>RTT: <b>' + (hc && hc.ok && hc.latency_ms != null ? hc.latency_ms + " мс" : "—") + '</b></span>' +
      '</div></div>';
    h += '<div class="pv-sec" data-pcard-sec>' +
      '<div class="pv-sec-h"><span class="t">Входящие и маршрут</span><span class="pv-hint">что разрешено через шлюз</span></div>' +
      '<div class="pv-chips">' +
      chip("входящий", e.type || "openai-compatible") +
      chip("моделей", (e.models || []).length ? String((e.models || []).length) : "wildcard") +
      chip("таймаут", (e.timeout_s || 0) + " с") +
      chip("транспорт", e.egress_mode || "off") +
      (e.egress ? chip("egress", e.egress) : "") +
      (e.tier ? chip("тир", e.tier) : "") +
      ((e.fallback || []).length ? chip("фолбэк", (e.fallback || []).join(" → ")) : "") +
      (e.auth_kind === "no-auth" ? chip("auth", "keyless — ключи не нужны") : "") +
      '</div></div>';
    h += '<div class="pv-sec grow" data-pcard-sec>' +
      '<div class="pv-sec-h"><span class="t">Модели в работе</span><span class="pv-hint">вызовы из журнала шлюза</span></div>' +
      topModelsHTML(e) + '</div>';
    h += '</div>';
    h += '<div class="pv-col">' +
      '<div class="pv-sec" data-pcard-sec>' +
      '<div class="pv-sec-h"><span class="t">Пул ключей</span><span class="pv-hint">' + (keys.length ? "по метаданным" : "метаданных нет") + '</span></div>' +
      metersHTML(keys, a) + '</div>' +
      '<div class="pv-sec grow" data-pcard-sec>' +
      '<div class="pv-sec-h"><span class="t">Последние вызовы</span><span class="pv-hint">журнал шлюза</span></div>' +
      logRowsHTML(e, false, 40) + '</div>' +
      '</div>';
    h += '</div>';
    return h;
  }

  /* ---------- ключи ---------- */
  function keyTag(k) {
    if (k.enabled === false) return { lvl: "err", t: "выкл" };
    var bo = +(k.backoff_secs || 0);
    if (bo > 0) return { lvl: "warn", t: "кулдаун " + fmtMmSs(bo) };
    if (+(k.failures || 0) > 0) return { lvl: "warn", t: k.failures + " сбоев" };
    if (pct(k.usage_tokens, k.quota_tokens) >= 100) return { lvl: "err", t: "квота" };
    return { lvl: "on", t: "чист" };
  }
  function keyRow(e, k, i) {
    var tag = keyTag(k);
    var maskTxt = mask(k.key || k.masked || "");
    var tr = pct(k.usage_tokens, k.quota_tokens);
    return '<div class="pv-key" data-pcard-key data-i="' + i + '">' +
      '<span class="pv-ki">' + String(i + 1).padStart(2, "0") + '</span>' +
      '<div class="pv-km">' +
        '<div class="pv-kl">' +
          '<input class="pv-in" data-pcard-edit data-edit-id="kalias:' + i + '" data-kalias="' + i + '" value="' + esc(k.name || ("ключ " + (i + 1))) + '" spellcheck="false" aria-label="Имя ключа">' +
          '<span class="pv-kcode" title="' + esc(maskTxt) + '">' + esc(maskTxt) + '</span>' +
          '<span class="pv-tag ' + tag.lvl + '">' + esc(tag.t) + '</span>' +
          ((k.models || []).length ? '<span class="pv-tag" title="' + esc((k.models || []).join(", ")) + '">' + esc(k.models[0]) + '</span>' : "") +
        '</div>' +
        '<div class="pv-kbar">' +
          '<span class="pv-track' + (tr >= 90 ? " hi" : "") + '"><i style="width:' + tr + '%"></i></span>' +
          '<span class="pv-v">токены ' + compact(k.usage_tokens || 0) + "/" + compact(k.quota_tokens || 0) + '</span>' +
          '<span class="pv-v">запросы ' + compact(k.usage_reqs || 0) + "/" + compact(k.quota_reqs || 0) + '</span>' +
          '<span class="pv-v">$' + num(k.usage_usd) + "/" + num(k.budget_usd) + '</span>' +
        '</div>' +
      '</div>' +
      '<div class="pv-ka">' +
        '<button type="button" class="pv-ib" data-kcopy="' + i + '" title="Скопировать ключ">⧉</button>' +
        '<button type="button" class="pv-ib" data-kdel="' + i + '" title="Убрать ключ из пула">✕</button>' +
      '</div></div>';
  }
  function phKeyRow(n) {
    return '<div class="pv-key ph" data-pcard-key>' +
      '<span class="pv-ki">…</span>' +
      '<span class="pv-kph">ещё ' + n + " " + pl(n, "ключ", "ключа", "ключей") + ' · маска и квота придут из ядра</span>' +
      '</div>';
  }
  function keysHTML(e) {
    var keys = poolKeys(e);
    var tot = e.keys_pct < 0 ? 0 : (e.keys || 0);
    var live = tot ? (e.keys_healthy || 0) : 0;
    var bo = Math.max(0, tot - live);
    var ph = Math.max(0, Math.min(40, tot - keys.length));
    var rows = keys.map(function (k, i) { return keyRow(e, k, i); }).join("");
    if (ph > 0) rows += phKeyRow(ph);
    return '<div class="pv-grid2 k">' +
      '<div class="pv-sec grow" data-pcard-sec>' +
        '<div class="pv-sec-h"><span class="t">Ключи пула</span><span class="pv-hint">исходящие · кулдаун локальный</span><span class="pv-sp"></span>' +
          '<span class="pv-chip on">живых ' + live + '</span>' + (bo ? '<span class="pv-chip">бэкофф ' + bo + '</span>' : "") + '</div>' +
        (ST.revealed === "key" ? addRowHTML() : "") +
        (tot ? '<div class="pv-keys">' + rows + '</div>'
          : '<div class="pv-empty"><b>в пуле нет ключей</b><span>добавьте ключ кнопкой «+ ключ» — без ключей провайдер не используется</span></div>') +
        '</div>' +
      '<div class="pv-col">' +
        policyHTML(e) +
        '<div class="pv-sec grow" data-pcard-sec>' +
          '<div class="pv-sec-h"><span class="t">Отказы в журнале</span><span class="pv-hint">429 · 5xx · таймаут</span></div>' +
          logRowsHTML(e, true, 30) +
        '</div>' +
      '</div>' +
      '</div>';
  }
  function policyHTML(e) {
    var D = (S.cfg && S.cfg.provider_defaults) || {};
    var R = (S.cfg && S.cfg.resilience) || {};
    var cd = R.cooldown || {}, bk = R.breaker || {};
    var ep = {};
    var src = [D.error_policy || {}, e.error_policy || {}];
    src.forEach(function (o) { Object.keys(o || {}).forEach(function (k) { ep[k] = o[k]; }); });
    return '<div class="pv-sec" data-pcard-sec>' +
      '<div class="pv-sec-h"><span class="t">Политика пула</span><span class="pv-hint">как ключи сменяют друг друга</span></div>' +
      '<div class="pv-chips">' +
        chip("свап", e.key_swap || D.key_swap || "round_robin") +
        chip("кулдаун", (cd.base_seconds || 15) + "с → " + (cd.max_seconds || 300) + "с ×" + (cd.factor || 2)) +
        chip("брейкер", (bk.max_failures || 5) + " сбоев / " + (bk.recovery_seconds || 60) + "с") +
        chip("401", ep["401"] || "retry_token") +
        chip("429", ep["429"] || "retry_proxy") +
        chip("5xx", ep["5xx"] || "retry") +
      '</div>' +
      '<div class="pv-note">ключ с 429 паркуется на base·2^n секунд до max — остальные ключи пула продолжают работать</div>' +
      '</div>';
  }

  /* ---------- модели ---------- */
  function testModel(e) {
    var models = e.models || [];
    var dis = e.disabled_models || [];
    if (e.test_model && models.indexOf(e.test_model) >= 0) return e.test_model;
    var tm = TESTMODEL[e.name];
    if (tm && models.indexOf(tm) >= 0) return tm;
    var on = models.filter(function (m) { return dis.indexOf(m) < 0; });
    return on[0] || models[0] || "модель не выбрана";
  }
  function modelRow(e, m, dis, hid, stats) {
    var off = dis.has(m), hd = hid.has(m);
    var lvl = (e.model_reasoning && e.model_reasoning[m]) || "auto";
    var s = stats[m];
    var stTag = s ? (s.err ? '<span class="pv-tag warn" title="' + s.err + " ошибок из " + s.n + ' запросов в журнале">' + s.n + " · " + s.err + " err</span>"
      : '<span class="pv-tag on">' + s.n + " ок</span>") : '<span class="pv-tag">нет вызовов</span>';
    return '<div class="pv-mrow' + (off ? " off" : "") + (hd ? " hd" : "") + '" data-pcard-model data-m="' + esc(m) + '">' +
      '<span class="pv-fam" style="background:' + (FAMC[fam(m)] || FAMC.other) + '" title="' + fam(m) + '"></span>' +
      '<span class="pv-mn" title="' + esc(m) + '">' + esc(m) + '</span>' +
      stTag + '<span class="pv-sp"></span>' +
      '<select class="pv-sel sm" data-pcard-edit data-edit-id="mreason:' + esc(m) + '" data-mreason="' + esc(m) + '" title="Уровень reasoning для модели" aria-label="Reasoning для ' + esc(m) + '">' +
        LV.map(function (v) { return '<option value="' + v + '"' + (v === lvl ? " selected" : "") + '>' + v + "</option>"; }).join("") +
      '</select>' +
      '<button type="button" class="pv-tgl' + (off ? "" : " on") + '" data-mtgl="' + esc(m) + '" aria-pressed="' + (!off) + '" title="' + (off ? "Включить маршрутизацию" : "Выключить маршрутизацию (404)") + '">' + (off ? "выкл" : "вкл") + '</button>' +
      '<button type="button" class="pv-tgl' + (hd ? " dim" : "") + '" data-meye="' + esc(m) + '" title="' + (hd ? "Показать в каталоге" : "Скрыть из каталога") + '">' + (hd ? "вернуть" : "скрыть") + '</button>' +
      '</div>';
  }
  function modelsHTML(e) {
    var models = e.models || [];
    var dis = new Set(e.disabled_models || []), hid = new Set(e.hidden_models || []);
    var onN = models.filter(function (m) { return !dis.has(m) && !hid.has(m); }).length;
    return '<div class="pv-grid2 k">' +
      '<div class="pv-sec grow" data-pcard-sec>' +
        '<div class="pv-sec-h"><span class="t">Модели</span><span class="pv-hint">каталог провайдера</span><span class="pv-sp"></span>' +
          '<span class="pv-chip on">включено ' + onN + '</span>' + (hid.size ? '<span class="pv-chip">скрыто ' + hid.size + '</span>' : "") + '</div>' +
        '<div class="pv-tools">' +
          '<input class="pv-in grow" data-pcard-filter data-edit-id="mfilter" placeholder="Фильтр по имени модели…" value="' + esc(ST.filter) + '" autocomplete="off" spellcheck="false" aria-label="Фильтр моделей">' +
          '<span class="pv-hint" data-mcnt></span>' +
        '</div>' +
        (ST.revealed === "model" ? addRowHTML() : "") +
        '<div class="pv-models" data-mlist>' + modelsListHTML(e) + '</div>' +
        '<div class="pv-note">каталог правится на месте: reasoning, вкл/выкл, скрыть · «+ модель» в шапке</div>' +
      '</div>' +
      '<div class="pv-col">' +
        '<div class="pv-sec" data-pcard-sec>' +
          '<div class="pv-sec-h"><span class="t">Тест модели</span><span class="pv-hint">живой вызов через шлюз</span></div>' +
          (models.length
            ? '<label class="pv-f"><span class="pv-lbl">модель под тест</span><select class="pv-sel" data-pcard-edit data-edit-id="testmodel" data-testmodel="1" title="Модель, которой бьёт «▶ тест»" aria-label="Модель для теста">' +
              models.map(function (m) { return '<option value="' + esc(m) + '"' + (m === testModel(e) ? " selected" : "") + '>' + esc(m) + "</option>"; }).join("") +
              '</select></label>'
            : '<div class="pv-empty"><b>тестировать нечего</b><span>каталог пуст — добавьте модель</span></div>') +
          '<div class="pv-foot"><span class="pv-hint">ответ и код — в Журналах</span><span class="pv-sp"></span>' +
            '<button type="button" class="pv-b acc" data-pcard-test>▶ тест</button></div>' +
        '</div>' +
        '<div class="pv-sec" data-pcard-sec>' +
          '<div class="pv-sec-h"><span class="t">Каталог</span><span class="pv-hint">upstream /models</span></div>' +
          '<div class="pv-chips">' +
            chip("всего", String(models.length)) + chip("включено", String(onN)) +
            chip("выключено", String(dis.size)) + chip("скрыто", String(hid.size)) +
            chip("wildcard", models.length ? "нет" : "да") +
          '</div>' +
          '<div class="pv-foot"><span class="pv-hint">↻ подтягивает новые модели из upstream</span><span class="pv-sp"></span>' +
            '<button type="button" class="pv-b" data-msync>↻ из каталога</button></div>' +
          '<div class="pv-note">выключенная модель отвечает 404 у этого провайдера; скрытая — не попадает в списки выбора</div>' +
        '</div>' +
      '</div>' +
      '</div>';
  }
  function modelsListHTML(e) {
    var models = e.models || [];
    var dis = new Set(e.disabled_models || []), hid = new Set(e.hidden_models || []);
    var q = ST.filter.trim().toLowerCase();
    var shown = models.filter(function (m) { return !q || m.toLowerCase().indexOf(q) >= 0; });
    var stats = modelStats(e);
    if (!shown.length) return '<div class="pv-empty"><b>' + (models.length ? "ничего не найдено" : "моделей нет") + '</b><span>' +
      (models.length ? "сбросьте фильтр или добавьте модель кнопкой «+ модель»" : "провайдер работает как wildcard — примет любую модель") + '</span></div>';
    return shown.map(function (m) { return modelRow(e, m, dis, hid, stats); }).join("");
  }

  /* ---------- связь ---------- */
  function selOpts(list, val) {
    return list.map(function (v) { return '<option value="' + esc(v) + '"' + (v === val ? " selected" : "") + '>' + esc(v) + "</option>"; }).join("");
  }
  function linkHTML(e) {
    var isKL = e.auth_kind === "no-auth";
    var tr = e.transport || {};
    var SW = ["round_robin", "random", "least_used", "best_latency"];
    var fan = +tr.fanout || 5, rnd = +tr.rounds || 2;
    return '<div class="pv-sec" data-pcard-sec>' +
      '<div class="pv-sec-h"><span class="t">Подключение</span><span class="pv-hint">base_url · auth · ua · таймаут</span></div>' +
      '<div class="pv-form c3">' +
        '<label class="pv-f wide"><span class="pv-lbl">base_url</span><input class="pv-in" data-pcard-edit data-edit-id="base" data-f="base_url" value="' + esc(e.base_url || "") + '" placeholder="https://api.example.com" spellcheck="false"></label>' +
        '<label class="pv-f"><span class="pv-lbl">auth mode</span><select class="pv-sel" data-pcard-edit data-edit-id="auth" data-f="auth_mode" data-dd="1"' + (isKL ? " disabled" : "") + '>' + selOpts(AUTH, e.auth_mode || (isKL ? "none" : "bearer")) + '</select></label>' +
        '<label class="pv-f"><span class="pv-lbl">user-agent</span><input class="pv-in" data-pcard-edit data-edit-id="ua" data-f="user_agent" value="' + esc(e.user_agent || "") + '" placeholder="opencode/1.18.99" spellcheck="false"></label>' +
        '<label class="pv-f"><span class="pv-lbl">таймаут, с</span><input class="pv-in" data-pcard-edit data-edit-id="timeout" data-f="timeout_s" type="number" min="1" max="3600" value="' + (e.timeout_s || 60) + '"></label>' +
        '<label class="pv-f"><span class="pv-lbl">egress mode</span><select class="pv-sel" data-pcard-edit data-edit-id="egmode" data-f="egress_mode" data-dd="1">' + selOpts(EGRESS, e.egress_mode || "off") + '</select></label>' +
        '<label class="pv-f span2"><span class="pv-lbl">egress url</span><input class="pv-in" data-pcard-edit data-edit-id="egress" data-f="egress" value="' + esc(e.egress || "") + '" placeholder="http://127.0.0.1:8118 (пусто — решает транспорт)" spellcheck="false"></label>' +
      '</div>' +
      '<div class="pv-foot"><span class="pv-hint">поля правятся на месте; «Сохранить» отправляет их в ядро</span><span class="pv-sp"></span>' +
        '<button type="button" class="pv-b" data-copy2>⧉ base_url</button>' +
        '<button type="button" class="pv-b acc" data-save>Сохранить</button></div>' +
      '</div>' +
      '<div class="pv-sec" data-pcard-sec>' +
      '<div class="pv-sec-h"><span class="t">Транспорт и пул</span><span class="pv-hint">ротация ключей · fanout · кулдаун egress</span></div>' +
      '<div class="pv-form c4">' +
        '<label class="pv-f"><span class="pv-lbl">свап ключей</span><select class="pv-sel" data-pcard-edit data-edit-id="swap" data-f="key_swap" data-dd="1">' + selOpts(SW, e.key_swap || "round_robin") + '</select></label>' +
        '<label class="pv-f"><span class="pv-lbl">fanout × rounds</span><span class="pv-duo">' +
          '<input class="pv-in" type="number" min="1" max="10" data-pcard-edit data-edit-id="fanout" data-tf="fanout" value="' + fan + '" aria-label="fanout">' +
          '<input class="pv-in" type="number" min="1" max="5" data-pcard-edit data-edit-id="rounds" data-tf="rounds" value="' + rnd + '" aria-label="rounds"></span></label>' +
        '<label class="pv-f"><span class="pv-lbl">кулдаун egress, с</span><input class="pv-in" type="number" min="10" data-pcard-edit data-edit-id="tcd" data-tf="cooldown_s" value="' + (+tr.cooldown_s || 60) + '"></label>' +
        '<div class="pv-f"><span class="pv-lbl">попыток на 429</span><span class="pv-calc" title="429 паркует egress на кулдаун">' + fan + " × " + rnd + " = " + (fan * rnd) + '</span></div>' +
      '</div>' +
      '</div>' +
      checkHTML(e);
  }
  function checkHTML(e) {
    var hc = healthOf(e);
    var ok = !!(hc && hc.ok);
    return '<div class="pv-sec" data-pcard-sec>' +
      '<div class="pv-sec-h"><span class="t">Проверка связи</span><span class="pv-hint">healthcheck шлюза</span></div>' +
      '<div class="pv-facts">' +
        '<span>endpoint: <b>' + esc((hc && hc.endpoint) || e.base_url || "—") + '</b></span>' +
        '<span>RTT: <b>' + (hc && hc.latency_ms != null ? hc.latency_ms + " мс" : "—") + '</b></span>' +
        '<span>http: <b>' + (hc && hc.http ? hc.http : "—") + '</b></span>' +
        '<span>состояние: <b>' + (hc ? (ok ? "ok" : "ошибка") : "нет замера") + '</b></span>' +
      '</div>' +
      (hc && hc.last_err ? '<div class="pv-note">последняя ошибка: ' + esc(hc.last_err) + '</div>' : "") +
      '<div class="pv-foot"><span class="pv-hint">RTT обновляется фоном; кнопка гоняет замер сразу</span><span class="pv-sp"></span>' +
        '<button type="button" class="pv-b" data-check>▶ healthcheck</button></div>' +
      '</div>';
  }

  /* ---------- строка добавления ---------- */
  function addRowHTML() {
    var key = ST.revealed === "key";
    return '<div class="pv-add">' +
      '<input class="pv-in" ' + (key ? 'data-pcard-add-key-input' : 'data-pcard-add-model-input') +
        ' placeholder="' + (key ? "sk-… новый ключ (Enter — добавить)" : "имя модели (Enter — добавить)") + '" autocomplete="off" spellcheck="false" aria-label="' + (key ? "Новый ключ" : "Новая модель") + '">' +
      '<button type="button" class="pv-b acc sm" data-add-go="' + (key ? "key" : "model") + '">добавить</button>' +
      '<button type="button" class="pv-b sm" data-add-cancel>отмена</button>' +
      '</div>';
  }

  /* ---------- рендер ---------- */
  function render(e) {
    if (!e) return;
    var host = document.getElementById("provDetail");
    if (!host) return;
    if (ST.prov !== e.name) {
      ST.prov = e.name; ST.screen = 0; ST.scroll = [0, 0, 0, 0]; ST.filter = ""; ST.revealed = ""; ST.lastNav = 0;
    }
    var act = host.querySelector('[data-pcard-screen][data-sc="' + ST.screen + '"]');
    if (act) ST.scroll[ST.screen] = act.scrollTop;
    var ae = document.activeElement;
    var fid = ae && host.contains(ae) && ae.getAttribute ? ae.getAttribute("data-edit-id") : null;
    var caret = null;
    try { if (fid && ae.selectionStart != null) caret = ae.selectionStart; } catch (x) { caret = null; }

    var key = ST.revealed === "key", mod = ST.revealed === "model";
    host.innerHTML = '<div class="pv" data-pcard>' +
      headHTML(e) + kpisHTML(e) + navHTML(e) +
      '<div class="pv-body">' +
        '<section class="pv-scr" id="pv-scr-0" data-pcard-screen data-sc="0" role="tabpanel">' + sumHTML(e) + '</section>' +
        '<section class="pv-scr" id="pv-scr-1" data-pcard-screen data-sc="1" role="tabpanel">' + keysHTML(e) + '</section>' +
        '<section class="pv-scr" id="pv-scr-2" data-pcard-screen data-sc="2" role="tabpanel">' + modelsHTML(e) + '</section>' +
        '<section class="pv-scr" id="pv-scr-3" data-pcard-screen data-sc="3" role="tabpanel">' + linkHTML(e) + '</section>' +
      '</div></div>';
    void key; void mod;

    var root = host.querySelector("[data-pcard]");
    wire(root, e);
    paint(root);
    paintModels(root, e);
    if (fid) {
      var back = host.querySelector('[data-edit-id="' + cssEsc(fid) + '"]');
      if (back && back.focus) {
        try { back.focus({ preventScroll: true }); } catch (x) { try { back.focus(); } catch (x2) {} }
        try { if (caret != null && back.setSelectionRange) back.setSelectionRange(caret, caret); } catch (x3) {}
      }
    }
  }
  function cssEsc(s) { try { return CSS.escape(s); } catch (x) { return String(s).replace(/["\\]/g, "\\$&"); } }

  function paint(root) {
    if (!root) return;
    var secs = Array.prototype.slice.call(root.querySelectorAll("[data-pcard-screen]"));
    secs.forEach(function (s, i) { s.classList.toggle("on", i === ST.screen); });
    root.querySelectorAll("[data-pcard-nav]").forEach(function (b) {
      var on = +b.getAttribute("data-sc") === ST.screen;
      b.setAttribute("aria-selected", String(on));
    });
    var a = secs[ST.screen];
    if (a) a.scrollTop = ST.scroll[ST.screen] || 0;
  }
  function paintModels(root, e) {
    var list = root.querySelector("[data-mlist]");
    if (!list) return;
    list.innerHTML = modelsListHTML(e);
    var c = root.querySelector("[data-mcnt]");
    if (c) {
      var models = e.models || [];
      var q = ST.filter.trim().toLowerCase();
      var shown = models.filter(function (m) { return !q || m.toLowerCase().indexOf(q) >= 0; }).length;
      c.textContent = "показано " + shown + " из " + models.length;
    }
  }
  function go(i) {
    var root = document.querySelector("#provDetail [data-pcard]");
    if (!root) return;
    var n = SCREENS.length;
    i = ((i % n) + n) % n;
    var cur = root.querySelector('[data-pcard-screen][data-sc="' + ST.screen + '"]');
    if (cur) ST.scroll[ST.screen] = cur.scrollTop;
    ST.screen = i;
    paint(root);
  }

  /* ---------- правки ---------- */
  function storeField(e, f, v) {
    if (f === "timeout_s") { var n = parseInt(v, 10); e.timeout_s = Number.isFinite(n) ? Math.max(1, Math.min(3600, n)) : 0; return; }
    if (f === "auth_mode" || f === "egress_mode") { e[f] = v; return; }
    e[f] = v;
  }
  function addKey(e, raw) {
    var v = String(raw || "").trim();
    if (!v) { toast("Пустое значение — ключ не добавлен", "warn"); return; }
    var k = {
      name: "key-" + (v.length > 6 ? v.slice(-4) : v), key: v, models: [],
      quota_tokens: 0, quota_reqs: 0, budget_usd: 0,
      usage_tokens: 0, usage_reqs: 0, usage_usd: 0, token_pct: 0, req_pct: 0, usd_pct: 0
    };
    if (!S.kbp) S.kbp = {};
    var listName = e.name === "__GLOBAL__" ? "глобальные" : e.name;
    S.kbp[listName] = (S.kbp[listName] || []).concat([k]);
    e.keys = (e.keys || 0) + 1;
    e.keys_healthy = (e.keys_healthy || 0) + 1;
    e.keys_pct = e.keys ? e.keys_healthy / e.keys : -1;
    if (DEMO || typeof API === "undefined") toast("ключ добавлен на экран (демо: в ядро не пишется)", "ok");
    else {
      try {
        API.AddProviderKeys(e.name, [v]).then(function (r) { toast(r && r.message ? r.message : "ключ добавлен", r && r.ok ? "ok" : "err"); })
          .catch(function (x) { toast("Ключ: " + x, "err"); });
      } catch (x) { toast("Ключ: " + x, "err"); }
    }
  }
  function addModel(e, raw) {
    var v = String(raw || "").trim();
    if (!v) { toast("Пустое имя модели", "warn"); return; }
    e.models = e.models || [];
    if (e.models.indexOf(v) >= 0) { toast("Модель уже в каталоге", "warn"); return; }
    e.models = e.models.concat([v]);
    try { if (typeof customAdd === "function") customAdd(e.name, v); } catch (x) {}
    toast("модель «" + v + "» в каталоге (демо)", "ok");
  }
  function delKey(e, i) {
    var list = poolKeys(e).slice();
    if (!list[i]) return;
    var gone = list.splice(i, 1)[0];
    if (S.kbp) {
      if (e.name === "__GLOBAL__") S.kbp["глобальные"] = list;
      else S.kbp[e.name] = list;
    }
    e.keys = Math.max(0, (e.keys || 0) - 1);
    e.keys_healthy = Math.min(e.keys_healthy || 0, e.keys);
    e.keys_pct = e.keys ? (e.keys_healthy || 0) / e.keys : -1;
    toast("ключ «" + (gone.name || mask(gone.key)) + "» убран с экрана", "warn");
  }

  /* ---------- проводка ---------- */
  function wire(root, e) {
    if (!root) return;

    root.addEventListener("click", function (ev) {
      var t = ev.target;
      if (!t || !t.closest) return;
      var nav = t.closest("[data-pcard-nav]");
      if (nav) { ev.preventDefault(); go(+nav.getAttribute("data-sc")); return; }
      if (t.closest("[data-pcard-back]")) { ev.preventDefault(); try { backProv(); } catch (x) {} return; }
      if (t.closest("[data-pcard-test]")) { ev.preventDefault(); try { testProvider(); } catch (x) { toast("Тест: " + ((x && x.message) || x), "err"); } return; }
      if (t.closest("[data-pcard-copy]") || t.closest("[data-copy2]")) { ev.preventDefault(); try { copyTo(e.base_url || ""); } catch (x) {} return; }
      if (t.closest("[data-pcard-add-key]")) { ev.preventDefault(); reveal("key"); return; }
      if (t.closest("[data-pcard-add-model]")) { ev.preventDefault(); reveal("model"); return; }
      var ag = t.closest("[data-add-go]");
      if (ag) { ev.preventDefault(); commit(ag.getAttribute("data-add-go")); return; }
      if (t.closest("[data-add-cancel]")) { ev.preventDefault(); ST.revealed = ""; render(S.prov); return; }
      var kd = t.closest("[data-kdel]");
      if (kd) { ev.preventDefault(); delKey(e, +kd.getAttribute("data-kdel")); render(e); return; }
      var kc = t.closest("[data-kcopy]");
      if (kc) { ev.preventDefault(); var kk = poolKeys(e)[+kc.getAttribute("data-kcopy")]; if (kk) try { copyTo(kk.key || kk.name || ""); } catch (x) {} return; }
      var mt = t.closest("[data-mtgl]");
      if (mt) { ev.preventDefault(); toggleModel(e, mt.getAttribute("data-mtgl"), "off"); return; }
      var me = t.closest("[data-meye]");
      if (me) { ev.preventDefault(); toggleModel(e, me.getAttribute("data-meye"), "hid"); return; }
      if (t.closest("[data-msync]")) { ev.preventDefault(); syncModels(e); return; }
      if (t.closest("[data-check]")) { ev.preventDefault(); runCheck(e); return; }
      if (t.closest("[data-save]")) { ev.preventDefault(); saveConn(e); return; }
    });

    root.addEventListener("input", function (ev) {
      var t = ev.target;
      if (!t || !t.getAttribute) return;
      if (t.hasAttribute("data-pcard-filter")) { ST.filter = t.value; paintModels(root, e); return; }
      var ka = t.getAttribute("data-kalias");
      if (ka != null) { var k = poolKeys(e)[+ka]; if (k) k.name = t.value; return; }
      var mr = t.getAttribute("data-mreason");
      if (mr != null) { e.model_reasoning = e.model_reasoning || {}; e.model_reasoning[mr] = t.value; return; }
      if (t.hasAttribute("data-testmodel")) { TESTMODEL[e.name] = t.value; e.test_model = t.value; return; }
      var tf = t.getAttribute("data-tf");
      if (tf) {
        var n0 = parseInt(t.value, 10);
        e.transport = e.transport || {};
        if (Number.isFinite(n0)) e.transport[tf] = Math.max(tf === "rounds" ? 1 : 10, n0);
        return;
      }
      var f = t.getAttribute("data-f");
      if (f) { storeField(e, f, t.value); return; }
    });

    root.addEventListener("keydown", function (ev) {
      var t = ev.target;
      if (!t || !t.matches) return;
      if (ev.key === "Enter" && t.matches("[data-pcard-add-key-input]")) { ev.preventDefault(); commit("key"); return; }
      if (ev.key === "Enter" && t.matches("[data-pcard-add-model-input]")) { ev.preventDefault(); commit("model"); return; }
      if (ev.key === "Escape" && t.matches("[data-pcard-add-key-input],[data-pcard-add-model-input]")) {
        ev.preventDefault(); ST.revealed = ""; render(S.prov);
      }
    });

    root.addEventListener("wheel", function (ev) {
      var scr = root.querySelector("[data-pcard-screen].on");
      if (!scr) return;
      var down = ev.deltaY > 0;
      var canScroll = scr.scrollHeight > scr.clientHeight + 2;
      var edge = !canScroll || (down ? scr.scrollTop + scr.clientHeight >= scr.scrollHeight - 2 : scr.scrollTop <= 0);
      if (!edge) return;
      var now = Date.now();
      ev.preventDefault();
      if (now - ST.lastNav < NAVCO) return;
      ST.lastNav = now;
      go(ST.screen + (down ? 1 : -1));
    }, { passive: false });
  }

  function reveal(kind) {
    ST.revealed = kind;
    ST.screen = kind === "key" ? 1 : 2;
    render(S.prov);
    var inp = document.querySelector(kind === "key" ? "[data-pcard-add-key-input]" : "[data-pcard-add-model-input]");
    if (inp) { try { inp.focus(); } catch (x) {} }
  }
  function commit(kind) {
    var sel = kind === "key" ? "[data-pcard-add-key-input]" : "[data-pcard-add-model-input]";
    var inp = document.querySelector(sel);
    if (!inp) return;
    var v = inp.value;
    var e = S.prov;
    if (!e) return;
    if (kind === "key") addKey(e, v); else addModel(e, v);
    ST.revealed = "";
    render(e);
  }
  function toggleModel(e, m, kind) {
    var field = kind === "hid" ? "hidden_models" : "disabled_models";
    var arr = new Set(e[field] || []);
    if (arr.has(m)) arr.delete(m); else arr.add(m);
    e[field] = Array.from(arr);
    render(e);
  }
  function syncModels(e) {
    if (DEMO || typeof API === "undefined") { toast("демо: каталог моделей не опрашивается", "warn"); return; }
    toast("Синхронизация «" + e.name + "» из /models…", "ok");
    try {
      API.SyncModels(e.name).then(function (r) {
        toast(r && r.message ? r.message : "синхронизировано", r && r.ok ? "ok" : "err");
        try { loadProviders(); } catch (x) {}
      }).catch(function (x) { toast("Синхронизация: " + x, "err"); });
    } catch (x) { toast("Синхронизация: " + x, "err"); }
  }
  function runCheck(e) {
    if (DEMO || typeof API === "undefined" || !Go()) { toast("демо: healthcheck не опрашивает ядро", "warn"); return; }
    toast("healthcheck «" + e.name + "»…", "ok");
    try {
      API.HealthCheck().then(function (h) {
        S.health = h || [];
        toast("healthcheck: замеры обновлены", "ok");
        render(S.prov);
      }).catch(function (x) { toast("healthcheck: " + x, "err"); });
    } catch (x) { toast("healthcheck: " + x, "err"); }
  }
  function saveConn(e) {
    if (DEMO || typeof API === "undefined" || !Go()) { toast("демо: правки живут на экране, в ядро не пишутся", "warn"); return; }
    var patch = { base_url: e.base_url || "", user_agent: e.user_agent || "", egress: e.egress || "", egress_mode: e.egress_mode || "off" };
    if (e.timeout_s) patch.timeout_s = e.timeout_s;
    if (e.auth_mode && e.auth_kind !== "no-auth") patch.auth_mode = e.auth_mode;
    try {
      API.UpdateProvider(e.name, patch).then(function (r) {
        toast(r && r.message ? r.message : "сохранено", r && r.ok ? "ok" : "err");
        if (r && r.ok) { try { loadProviders(); } catch (x) {} }
      }).catch(function (x) { toast("Сохранение: " + x, "err"); });
    } catch (x) { toast("Сохранение: " + x, "err"); }
  }

  /* ---------- автооткрытие по ?card=1 (стенд/гейт: карточка сразу) ---------- */
  function autoCard() {
    var want = false;
    try { want = new URLSearchParams(location.search).get("card") === "1"; } catch (x) { return; }
    if (!want) return;
    var tries = 0;
    (function tick() {
      try {
        if (document.querySelector("#provDetail [data-c8card], #provDetail [data-pcard]")) return;
        if (typeof S !== "undefined" && S.end && S.end.length) {
          var first = S.end.filter(function (x) { return !x.catalog; })[0] || S.end[0];
          if (S.tab !== "providers" && typeof show === "function") show("providers");
          if (first && typeof openProv === "function") openProv(first.name);
          if (document.querySelector("#provDetail [data-c8card], #provDetail [data-pcard]")) return;
        }
      } catch (x) {}
      if (++tries > 50) return;
      setTimeout(tick, 90);
    })();
  }
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", function () { setTimeout(autoCard, 0); });
  else setTimeout(autoCard, 0);

  window.PCard = { render: render, go: go, state: ST };
})();
