/* ============================================================
   c08 · «Бланк узла» — экран провайдера (renderProvDetail → C8).
   Одно окно: паспортная строка, полоса показателей, разделы.
   Окно заперто — листается только тело активного раздела.
   Все правки — на месте, данные только из S.
   ============================================================ */
(function () {
  "use strict";

  const SCREENS = ["сводка", "ключи", "модели", "связь"];
  const LV = ["auto", "low", "high", "xhigh", "max"];
  const AUTH = ["bearer", "none", "public"];
  const EGRESS = ["off", "pool", "warp", "iprotate", "direct"];
  const SWAP = ["round_robin", "random", "least_used", "best_latency"];
  const FAML = { gpt: "g", claude: "c", gemini: "m", grok: "k", deepseek: "d", qwen: "q", mistral: "s", muse: "u", mimo: "i", nemo: "n", other: "•" };
  const famMark = m => FAML[fam(m)] || FAML.other;

  const SORTS = [["calls", "вызовы"], ["name", "имя"], ["errs", "ошибки"], ["share", "доля"]];

  const freshProbe = e => ({ model: e ? testModel(e) : "", prompt: "", out: null, hist: [], open: true, busy: false, pick: false, pickQ: "", histOpen: false });
  let CUR = null;
  const ST = {
    prov: "", screen: 0, add: "", acc: {},
    kq: "", kf: "all", ko: null,
    mq: "", sort: "calls", dir: -1, mo: null,
    probe: freshProbe(null)
  };

  /* ---------- формат ---------- */
  const pctv = (a, b) => { a = +a || 0; b = +b || 0; return b > 0 ? Math.max(0, Math.min(100, Math.round(a / b * 100))) : 0; };
  const cm = n => { try { return compact(n); } catch (x) { return String(Math.round(+n || 0)); } };
  const money = n => "$" + cm(Math.round(+n || 0));
  const pl = (n, a, b, c) => { try { return plural(n, a, b, c); } catch (x) { return c; } };
  const mmss = s => { try { return fmtMmSs(s); } catch (x) { return "0:00"; } };
  const initials = name => {
    if (name === "__GLOBAL__") return "GL";
    const s = String(name || "?").replace(/[^A-Za-zА-Яа-я0-9]/g, "");
    return (s.slice(0, 2) || "?").toUpperCase();
  };
  const fam = m => {
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
  };
  const mask = s => {
    s = String(s || "");
    if (!s) return "—";
    if (s.length <= 12) return s.slice(0, 3) + "…" + s.slice(-2);
    return s.slice(0, 9) + "…" + s.slice(-4);
  };
  const isDemo = () => { try { return typeof DEMO !== "undefined" && DEMO; } catch (x) { return true; } };
  const liveCore = () => { try { return !isDemo() && typeof API !== "undefined" && (typeof Go !== "function" || Go()); } catch (x) { return false; } };

  /* ---------- данные ---------- */
  const poolKeys = e => (!S.kbp ? [] : ((e.name === "__GLOBAL__" ? S.kbp["глобальные"] : S.kbp[e.name]) || []));
  const keyTotal = e => (e.keys_pct < 0 ? 0 : (+e.keys || 0));
  const healthOf = e => (S.health || []).find(x => x.name === e.name) || null;
  const brk = e => (S.breakers || []).find(x => x.provider === e.name) || {};
  const brkState = e => { const b = brk(e); return b.state || e.breaker || "closed"; };
  const untilS = e => { const b = brk(e); return Math.max(0, +(b.opened_until_s || e.breaker_until_s || 0)); };
  const fails = e => {
    const b = brk(e);
    return b.failures != null ? b.failures : (b.consecutive_failures != null ? b.consecutive_failures : (e.breaker_failures || 0));
  };
  function stateId(e) {
    const s = brkState(e);
    if (s === "open") return "open";
    if (s === "half_open") return "half";
    if (keyTotal(e) <= 0) return "nokeys";
    if (!(e.keys_healthy || 0)) return "backoff";
    return "ok";
  }
  function agg(keys) {
    const o = { tq: 0, tu: 0, rq: 0, ru: 0, bu: 0, bus: 0 };
    keys.forEach(k => {
      o.tq += +k.quota_tokens || 0; o.tu += +k.usage_tokens || 0;
      o.rq += +k.quota_reqs || 0; o.ru += +k.usage_reqs || 0;
      o.bu += +k.budget_usd || 0; o.bus += +k.usage_usd || 0;
    });
    return o;
  }
  function modelStats(e) {
    const map = {};
    (S.logs || []).forEach(l => {
      const m = l.model || l.requested_model;
      if (!m) return;
      if (l.provider && l.provider !== e.name) return;
      const s = map[m] || (map[m] = { n: 0, err: 0 });
      s.n++;
      const code = +l.http_status || 0;
      if (code && code !== 200) s.err++;
    });
    return map;
  }
  function winStats(e) {
    let n = 0, err = 0;
    (S.logs || []).forEach(l => {
      if (l.provider && l.provider !== e.name) return;
      n++;
      const c = +l.http_status || 0;
      if ((c && c !== 200) || l.status === "error") err++;
    });
    return { n: n, err: err, ok: n - err };
  }
  const provLogs = e => (S.logs || []).filter(l => !l.provider || l.provider === e.name)
    .slice().sort((a, b) => String(b.ts || "").localeCompare(String(a.ts || "")));
  const callBad = l => { const c = +l.http_status || 0; return (c && c !== 200) || l.status === "error"; };

  /* ---------- метка состояния для шапки ---------- */
  function chipOf(e) {
    if (e.catalog) return { lvl: "dim", t: "каталог" };
    const id = stateId(e);
    if (id === "open") return { lvl: "err", t: "в обходе" };
    if (id === "half") return { lvl: "warn", t: "проба" };
    if (id === "nokeys") return { lvl: "warn", t: "нет ключей" };
    if (id === "backoff") return { lvl: "warn", t: "бэкофф" };
    return { lvl: "on", t: "в сети" };
  }
  function testModel(e) {
    const models = e.models || [], dis = e.disabled_models || [];
    if (e.test_model && models.indexOf(e.test_model) >= 0) return e.test_model;
    const on = models.filter(m => dis.indexOf(m) < 0);
    return on[0] || models[0] || "";
  }

  /* ---------- контролы: свой дропдаун и степпер ---------- */
  const DDC = '<svg class="c8-dd-c" viewBox="0 0 10 6" aria-hidden="true"><path d="M1 1l4 4 4-4"/></svg>';
  function ddHTML(kind, val, opts, extra, cls, shown) {
    const pairs = opts.map(o => Array.isArray(o) ? o : [o, o]);
    const cur = pairs.find(p => String(p[0]) === String(val)) || pairs[0] || ["", ""];
    return '<div class="c8-dd' + (cls ? " " + cls : "") + '" data-c8dd data-kind="' + kind + '"' + (extra || "") +
      ' tabindex="0" role="combobox" aria-haspopup="listbox" aria-expanded="false">' +
      '<span class="c8-dd-v">' + esc(shown != null ? shown : cur[1]) + "</span>" + DDC +
      '<div class="c8-dd-m" role="listbox" hidden>' +
      pairs.map(p => '<button type="button" class="c8-dd-o' + (String(p[0]) === String(val) ? " sel" : "") +
        '" role="option" aria-selected="' + (String(p[0]) === String(val)) + '" data-v="' + esc(p[0]) + '">' +
        esc(p[1]) + "</button>").join("") +
      "</div></div>";
  }
  function stepHTML(f, val, opt) {
    opt = opt || {};
    const min = opt.min != null ? opt.min : 0, max = opt.max != null ? opt.max : 3600, st = opt.step || 1;
    const attrs = 'data-f="' + f + '" data-min="' + min + '" data-max="' + max + '" data-step="' + st + '"';
    const bind = opt.tf ? 'data-c8-tf="' + f + '"' : 'data-c8-f="' + f + '"';
    return '<span class="c8-step">' +
      '<button type="button" class="c8-step-b" data-c8="step" data-d="-1" ' + attrs + ' aria-label="Меньше" tabindex="-1">−</button>' +
      '<input class="c8-in" ' + bind + ' type="number" inputmode="numeric" step="' + st + '" min="' + min + '" max="' + max +
        '" value="' + esc(String(val)) + '" aria-label="' + esc(opt.label || f) + '">' +
      '<button type="button" class="c8-step-b" data-c8="step" data-d="1" ' + attrs + ' aria-label="Больше" tabindex="-1">+</button>' +
      "</span>";
  }
  /* ---------- паспортная строка ---------- */
  function headHTML(e) {
    const sc = chipOf(e);
    const nm = e.name === "__GLOBAL__" ? "Глобальные ключи" : e.name;
    const tags = [];
    if (e.type && String(e.type).toLowerCase() !== String(e.name).toLowerCase() && e.type !== "catalog") tags.push(e.type);
    if (e.tier) tags.push(e.tier);
    if (e.catalog) tags.push("каталог");
    const meta = [];
    if (e.tier) meta.push(["тир", e.tier]);
    meta.push(["транспорт", e.egress_mode || "off"]);
    meta.push(["моделей", (e.models || []).length ? String((e.models || []).length) : "wildcard"]);
    meta.push(["ключей", keyTotal(e) ? (e.keys_healthy || 0) + "/" + keyTotal(e) : "0"]);
    const tm = testModel(e);
    return '<div class="c8-top">' +
      '<button type="button" class="c8-back" data-c8="back" title="К списку провайдеров" aria-label="Назад к списку провайдеров">' +
        '<svg viewBox="0 0 16 16" aria-hidden="true"><path d="M10.5 2.5 5 8l5.5 5.5"/></svg></button>' +
      '<div class="c8-id">' +
        '<div class="c8-id1">' +
          '<span class="c8-plate" aria-hidden="true">' + esc(initials(e.name)) + '</span>' +
          '<h2 class="c8-nm" title="' + esc(nm) + '">' + esc(nm) + '</h2>' +
          (tags.length ? '<div class="c8-tags">' + tags.map(t => '<span class="c8-tag">' + esc(t) + '</span>').join("") + '</div>' : "") +
          '<div class="c8-acts">' +
            '<button type="button" class="c8-b prim" data-c8="test" title="Живой тест «' + esc(tm || "модель не выбрана") + '»">▶ тест</button>' +
            '<button type="button" class="c8-b" data-c8="addkey" title="Добавить ключ в пул">+ ключ</button>' +
            '<button type="button" class="c8-b" data-c8="addmodel" title="Добавить модель в каталог">+ модель</button>' +
          '</div>' +
        '</div>' +
        '<div class="c8-id2">' +
          '<span class="c8-st ' + sc.lvl + '"><i></i>' + esc(sc.t) + '</span>' +
          '<button type="button" class="c8-url" data-c8="url" title="Скопировать base_url"><code>' + esc(e.base_url || "—") + '</code><i aria-hidden="true">⧉</i></button>' +
          meta.map(m => '<span class="sep">·</span><span>' + m[0] + ' <b>' + esc(m[1]) + '</b></span>').join("") +
        '</div>' +
      '</div>' +
      '</div>';
  }

  /* ---------- полоса показателей ---------- */
  function cell(k, v, sub, bar, cls, title, right) {
    return '<div class="c8-cell' + (cls ? " " + cls : "") + '"' + (title ? ' title="' + esc(title) + '"' : "") + '>' +
      '<span class="k">' + k + (right ? '<em>' + right + '</em>' : '') + '</span>' +
      '<span class="v">' + v + '</span>' + (bar || "") + '<span class="s">' + sub + '</span></div>';
  }
  function segBar(tot, live) {
    const n = Math.min(12, Math.max(0, tot));
    if (!n) return "";
    let s = "";
    for (let i = 0; i < n; i++) s += '<i class="' + (i < live ? "on" : "") + '"></i>';
    return '<span class="b" style="display:grid;grid-template-columns:repeat(' + n + ',minmax(0,1fr))">' + s + '</span>';
  }
  function lineBar(p) {
    return '<span class="b line"><u style="width:' + Math.max(0, Math.min(100, p)) + '%"></u></span>';
  }
  function railHTML(e) {
    const hc = healthOf(e);
    const lat = hc && hc.ok && hc.latency_ms != null ? hc.latency_ms : null;
    const tot = keyTotal(e), live = tot ? (+e.keys_healthy || 0) : 0;
    const bo = Math.max(0, tot - live);
    const st = stateId(e);
    const win = winStats(e);
    const mods = (e.models || []).length;
    const dis = new Set(e.disabled_models || []);
    const onN = (e.models || []).filter(m => !dis.has(m)).length;
    const brkT = st === "open" ? "открыт" : st === "half" ? "проба" : st === "nokeys" ? "—" : "закрыт";
    const brkS = st === "open" || st === "half" ? "возврат <b>" + mmss(untilS(e)) + "</b>" : "сбоев <b>" + fails(e) + "</b>";
    const latP = lat == null ? 0 : Math.min(100, Math.round(lat / 15));
    const latS = lat == null ? "нет замера" : (lat <= 400 ? "быстро" : lat <= 1000 ? "норма" : "медленно");
    return '<div class="c8-rail">' +
      cell("ключи", live + "<small>/" + tot + "</small>",
        tot ? (bo ? bo + " в бэкоффе" : "все живы") : (e.catalog ? "из каталога" : "пула нет"),
        segBar(tot, live), live ? "" : "bad",
        live + " живых из " + tot + (bo ? " · " + bo + " в бэкоффе" : ""), tot ? bo ? "−" + bo : "ok" : "") +
      cell("rtt", lat == null ? "—" : lat + "<small>мс</small>", latS, lineBar(latP),
        lat == null ? "" : (lat > 1000 ? "bad" : ""),
        lat == null ? "healthcheck не проходил" : "healthcheck: " + lat + " мс", hc && hc.http ? "http " + hc.http : "") +
      cell("модели", mods ? String(mods) : "∞", mods ? "в каталоге · вкл " + onN : "wildcard",
        mods ? lineBar(Math.round(onN / mods * 100)) : "", mods ? "" : "dim",
        mods ? mods + " " + pl(mods, "модель", "модели", "моделей") + " в каталоге" : "wildcard — примет любую модель", mods ? "вкл " + onN : "") +
      cell("успех", win.n ? Math.round(win.ok / win.n * 100) + "<small>%</small>" : "—",
        win.n ? "доля без ошибок" : "журнал пуст",
        win.n ? lineBar(Math.round(win.ok / win.n * 100)) : "", win.n && win.err ? "bad" : "",
        win.n ? win.ok + " из " + win.n + " вызовов без ошибок" + (win.err ? ", ошибок " + win.err : "") : "вызовов ещё не было", "") +
      cell("брейкер", brkT, brkS,
        '<span class="b">' + [["закрыт", "closed"], ["открыт", "open"], ["полуоткрытие", "half_open"]].map(n =>
          '<i class="' + (brkState(e) === n[1] ? "on" : "") + '"></i>').join("") + '</span>',
        st === "open" || st === "half" ? "bad" : (st === "nokeys" ? "dim" : ""),
        st === "open" ? "открыт — трафик идёт в обход, возврат через " + mmss(untilS(e)) :
        st === "half" ? "полуоткрытие — пробные запросы" : "закрыт — каскад использует провайдера", "") +
      '</div>';
  }

  /* ---------- разделы ---------- */
  function navHTML(e) {
    const cnt = ["", String(keyTotal(e)), String((e.models || []).length), ""];
    return '<div class="c8-nav" role="tablist" aria-label="Разделы провайдера">' +
      SCREENS.map((s, i) =>
        '<button type="button" role="tab" data-c8="nav" data-sc="' + i + '" aria-selected="' + (i === ST.screen) + '"' +
        ' aria-controls="c8-scr-' + i + '">' + s + (cnt[i] ? '<span class="n">' + cnt[i] + '</span>' : "") + '</button>').join("") +
      '</div>';
  }

  /* ---------- сводка: отметки узла (вместо баннера вердикта) ---------- */
  function routeChain(e) {
    const parts = [e.type || "openai-compatible"];
    if (e.tier) parts.push(e.tier);
    parts.push(e.egress_mode && e.egress_mode !== "off" ? e.egress_mode : "напрямую");
    return parts.map(p => "<b>" + esc(p) + "</b>").join('<i class="ar" aria-hidden="true">→</i>');
  }
  function pulseHTML(e, max) {
    const rows = provLogs(e).slice(0, max || 14);
    if (!rows.length) return '<span class="c8-pulse idle">пусто</span>';
    const chrono = rows.slice().reverse();
    const top = Math.max(1, ...chrono.map(l => +l.duration_ms || 0));
    return '<span class="c8-pulse" title="последние ' + chrono.length + " " + pl(chrono.length, "вызов", "вызова", "вызовов") +
      ' · высота — задержка, красная засечка — ошибка">' + chrono.map(l => {
        const bad = callBad(l);
        const ms = Math.round(+l.duration_ms || 0);
        const h = 5 + Math.round(Math.min(1, ms / top) * 14);
        const code = bad ? (+l.http_status || "ERR") : 200;
        return '<i class="' + (bad ? "e" : "") + '" style="height:' + h + 'px" title="' +
          esc(String(l.ts || "").slice(11, 19) + " · " + (l.model || "—") + " · " + code + " · " + ms + " мс") + '"></i>';
      }).join("") + "</span>";
  }
  function markRow(k, v, s, cls) {
    return '<div class="c8-mk-r' + (cls ? " " + cls : "") + '"><span class="k">' + k + '</span>' +
      '<span class="v">' + v + '</span><span class="s">' + s + '</span></div>';
  }
  function marksHTML(e) {
    const hc = healthOf(e);
    const win = winStats(e);
    const all = provLogs(e);
    const lat = hc && hc.ok && hc.latency_ms != null ? hc.latency_ms + "<small>мс</small>" : "—";
    const http = hc && hc.http ? "http " + hc.http : "http —";
    const times = all.length
      ? String(all[all.length - 1].ts || "").slice(11, 16) + "–" + String(all[0].ts || "").slice(11, 16)
      : "—";
    const fb = (e.fallback || []).length ? "фолбэк " + (e.fallback || []).join(" → ") : "фолбэк —";
    const jour = win.n ? "окно " + esc(times) : "журнал пуст";
    return '<div class="c8-sec c8-marks"><div class="c8-sh"><span class="t">отметки узла</span>' +
      '<span class="h">маршрут · канал · журнал</span><span class="sp"></span>' +
      '<button type="button" class="c8-link" data-c8="nav" data-sc="3">связь →</button></div>' +
      '<div class="c8-mk">' +
        markRow("маршрут", routeChain(e), esc(fb)) +
        markRow("канал", esc(e.egress_mode || "off") + '<i class="dot"></i>' + lat + '<i class="dot"></i>' + esc(http),
          "таймаут " + (+e.timeout_s || 60) + " с") +
        markRow("журнал", jour, pulseHTML(e, 14), win.err ? "bad" : "") +
      "</div>" +
      '<div class="c8-note">высота засечки — задержка вызова, красная — ошибка · ' +
        '<button type="button" class="c8-link" data-goto="logs">журнал ядра →</button></div></div>';
  }
  function quotaHTML(e) {
    const keys = poolKeys(e), a = agg(keys);
    let body;
    if (e.catalog) {
      body = '<div class="c8-empty">провайдер из каталога: ключей в пуле нет — квота появится вместе с первым ключом</div>';
    } else if (!keys.length) {
      body = '<div class="c8-empty">метаданных ключей нет — квота не считается, ключи работают без учёта</div>';
    } else {
      const tile = (k, used, quota, fmt) => {
        const p = pctv(used, quota), has = quota > 0;
        return '<div class="c8-qt' + (has && p >= 90 ? " hot" : "") + '" title="' + esc(k + ": " + (has ? p + "% из " + fmt(quota) : "квота не задана")) + '">' +
          '<span class="k">' + k + '</span>' +
          '<span class="v">' + (has ? p + '<small>%</small>' : "—") + '</span>' +
          '<span class="bar"><i style="width:' + (has ? p : 0) + '%"></i></span>' +
          '<span class="s"><b>' + fmt(used) + '</b> / ' + (has ? fmt(quota) : "—") + '</span></div>';
      };
      body = '<div class="c8-qts">' +
        tile("токены", a.tu, a.tq, cm) + tile("запросы", a.ru, a.rq, cm) + tile("usd", a.bus, a.bu, money) + '</div>' +
        '<div class="c8-note">остаток пула: ' + cm(Math.max(0, a.tq - a.tu)) + ' токенов · ' +
          cm(Math.max(0, a.rq - a.ru)) + ' запросов · ' + money(Math.max(0, a.bu - a.bus)) + '</div>';
    }
    return '<div class="c8-sec"><div class="c8-sh"><span class="t">квоты пула</span><span class="h">' +
      (e.catalog ? "ключей нет" : keys.length ? "по метаданным " + keys.length + " " + pl(keys.length, "ключа", "ключей", "ключей") : "метаданных нет") +
      '</span></div>' + body + '</div>';
  }
  function topModelsHTML(e) {
    const st = modelStats(e);
    const models = (e.models || []).slice();
    Object.keys(st).forEach(m => { if (models.indexOf(m) < 0) models.push(m); });
    const rows = models.map(m => ({ m: m, n: (st[m] || {}).n || 0, err: (st[m] || {}).err || 0 }))
      .sort((a, b) => b.n - a.n || (a.m < b.m ? -1 : 1)).slice(0, 6);
    let body;
    if (!rows.length) {
      body = '<div class="c8-empty">каталог пуст — провайдер работает как wildcard</div>';
    } else {
      const total = rows.reduce((s, r) => s + r.n, 0) || 1;
      body = '<div class="c8-bars">' + rows.map(r =>
        '<div class="c8-bar' + (r.n ? "" : " zero") + '" title="' + esc(r.m + " · " + r.n + " вызовов" + (r.err ? " · " + r.err + " ошибок" : "")) + '">' +
          '<span class="n">' + esc(r.m) + '</span>' +
          '<span class="bar"><i style="width:' + Math.max(2, Math.round(r.n / Math.max(1, rows[0].n) * 100)) + '%"></i></span>' +
          '<span class="c">' + r.n + '</span>' +
          '<span class="e' + (r.err ? " bad" : "") + '">' + (r.err || "—") + '</span>' +
        '</div>').join("") + '</div>' +
        '<div class="c8-note">доля в журнале: ' + rows.map(r => Math.round(r.n / total * 100) + "%").join(" · ") + '</div>';
    }
    return '<div class="c8-sec"><div class="c8-sh"><span class="t">модели в работе</span><span class="h">вызовы из журнала</span></div>' + body + '</div>';
  }
  function callRow(l) {
    const code = +l.http_status || 0;
    const bad = callBad(l);
    const t = String(l.ts || "").slice(11, 19);
    const err = l.error ? String(l.error) : "";
    return '<div class="c8-call" title="' + esc((l.model || "") + (err ? " · " + err : "")) + '">' +
      '<span class="t">' + esc(t || "—") + '</span>' +
      '<span class="n">' + esc(l.model || l.requested_model || "—") + '</span>' +
      '<span class="c' + (bad ? " err" : " ok") + '">' + esc(bad ? String(code || "ERR") : "200") + '</span>' +
      '<span class="ms"><b>' + (l.duration_ms != null ? Math.round(+l.duration_ms) : "—") + '</b> ms</span></div>';
  }
  function callsHTML(e, max) {
    const all = provLogs(e);
    const rows = all.slice(0, max);
    const errs = all.filter(callBad).length;
    let body;
    if (!rows.length) {
      body = '<div class="c8-empty">журнал пуст — вызовы появятся после первого запроса через шлюз</div>';
    } else {
      const half = Math.ceil(rows.length / 2);
      body = '<div class="c8-tape"><div>' + rows.slice(0, half).map(callRow).join("") + '</div>' +
        '<div>' + rows.slice(half).map(callRow).join("") + '</div></div>';
    }
    return '<div class="c8-sec"><div class="c8-sh"><span class="t">лента вызовов</span><span class="h">' + all.length +
      ' в журнале' + (errs ? " · ошибок " + errs : "") + '</span><span class="sp"></span>' +
      '<button type="button" class="c8-link" data-goto="logs">журнал →</button></div>' + body + '</div>';
  }
  function sumHTML(e) {
    return '<div class="c8-sum">' + railHTML(e) + marksHTML(e) +
      '<div class="c8-duo">' + quotaHTML(e) + topModelsHTML(e) + '</div>' +
      callsHTML(e, 14) + '</div>';
  }

  /* ============================================================
     c75 · «одна строка — одно решение»
     Ключи и модели — сплошные ведомости: в строке всё, что нужно
     для решения (масштаб, доля, причина, тумблер), подробности —
     уровнем ниже. Тест-слой живёт внизу экрана «модели».
     ============================================================ */

  /* ---------- ключи ---------- */
  function keyState(k) {
    if (k.enabled === false) return { lv: "err", t: "выкл", why: "вручную" };
    const bo = +(k.backoff_secs || 0);
    if (bo > 0) return { lv: "warn", t: "пауза", why: mmss(bo) };
    if (pctv(k.usage_tokens, k.quota_tokens) >= 100) return { lv: "err", t: "квота", why: "выбрана" };
    if (+(k.failures || 0) > 0) return { lv: "warn", t: "сбоил", why: (+k.failures) + " сб." };
    return { lv: "on", t: "чист", why: "готов" };
  }
  function meterInline(tag, used, quota, fmt) {
    const p = pctv(used, quota), has = quota > 0;
    return '<span class="c75-mtr' + (has && p >= 90 ? " hot" : "") + '" title="' +
      esc(tag + " · " + (has ? fmt(used) + " из " + fmt(quota) + " · " + p + "%" : "квота не задана")) + '">' +
      '<i>' + tag + '</i><b>' + (has ? p + "%" : "—") + '</b>' +
      '<span class="bar"><span style="width:' + (has ? Math.max(1, p) : 0) + '%"></span></span></span>';
  }
  function keyRow(k, i, open) {
    const st = keyState(k);
    const off = k.enabled === false;
    const nm = k.name || ("ключ " + (i + 1));
    return '<div class="c75-kr ' + st.lv + (open ? " open" : "") + '" data-c75-kr="' + i + '">' +
        '<span class="c75-n">' + String(i + 1).padStart(2, "0") + '</span>' +
        '<button type="button" class="c75-exp" data-c8="kexp" data-i="' + i + '" aria-expanded="' + (open ? "true" : "false") + '" aria-label="' + (open ? "Свернуть ключ" : "Подробности ключа") + '"></button>' +
        '<span class="c75-id">' +
          '<input class="c75-alias" data-c8-alias="' + i + '" value="' + esc(nm) + '" spellcheck="false" autocomplete="off" aria-label="Имя ключа ' + (i + 1) + '">' +
          '<button type="button" class="c75-mask" data-c8="kcopy" data-i="' + i + '" title="' + esc("Скопировать ключ " + (k.key || k.masked || "")) + '">' + esc(mask(k.key || k.masked || "")) + '</button>' +
        '</span>' +
        '<span class="c75-q">' +
          meterInline("т", k.usage_tokens || 0, k.quota_tokens || 0, cm) +
          meterInline("з", k.usage_reqs || 0, k.quota_reqs || 0, cm) +
          meterInline("$", k.usage_usd || 0, k.budget_usd || 0, money) +
        '</span>' +
        '<span class="c75-st ' + st.lv + '" title="' + esc(st.t + " · " + st.why) + '"><i></i><b>' + esc(st.t) + '</b><em>' + esc(st.why) + '</em></span>' +
        '<button type="button" class="c75-dec' + (off ? "" : " on") + '" data-c8="ktgl" data-i="' + i + '" aria-pressed="' + (off ? "false" : "true") + '" title="' + (off ? "Вернуть ключ в пул" : "Вывести ключ из пула") + '">' + (off ? "вернуть" : "в пуле") + '</button>' +
      '</div>' + (open ? keyDetail(k, i, st) : "");
  }
  function keyDetail(k, i, st) {
    const tq = +k.quota_tokens || 0, tu = +k.usage_tokens || 0;
    const rq = +k.quota_reqs || 0, ru = +k.usage_reqs || 0;
    const bu = +k.budget_usd || 0, bus = +k.usage_usd || 0;
    const dc = (tag, val, sub) => '<span class="c75-dc"><i class="k">' + tag + '</i><span class="v">' + val + (sub ? '<em>' + sub + '</em>' : "") + '</span></span>';
    return '<div class="c75-kd" data-c75-kd="' + i + '">' +
      '<div class="c75-dg">' +
        dc("секрет", '<code>' + esc(k.key || k.masked || "—") + '</code>', "копируется кнопкой в строке") +
        dc("состояние", esc(st.t), esc(st.why) + " · " + (k.default ? "ключ по умолчанию · " : "") + esc((k.models || []).length ? "модели: " + (k.models || []).join(", ") : "модели: весь каталог")) +
        dc("токены", cm(tu) + " / " + (tq ? cm(tq) : "—"), tq ? "осталось " + cm(Math.max(0, tq - tu)) : "квота не задана") +
        dc("запросы", cm(ru) + " / " + (rq ? cm(rq) : "—"), rq ? "осталось " + cm(Math.max(0, rq - ru)) : "квота не задана") +
        dc("бюджет", money(bus) + " / " + (bu ? money(bu) : "—"), bu ? "осталось " + money(Math.max(0, bu - bus)) : "бюджет не задан") +
        dc("решение", k.enabled === false ? "выведен" : "в пуле", "тумблер справа решает, участвует ключ в маршруте") +
      '</div>' +
      '<div class="c75-dact">' +
        '<span class="c75-hint">имя и секрет правятся на месте · строка решает, участвует ключ в маршруте или нет</span>' +
        '<span class="sp"></span>' +
        '<button type="button" class="c75-b danger" data-c8="kdel" data-i="' + i + '">убрать из пула</button>' +
      '</div></div>';
  }
  function keyCounts(e, keys) {
    const total = Math.max(keys.length, keyTotal(e), 0);
    let off = 0, pause = 0, pool = 0;
    keys.forEach(k => {
      if (k.enabled === false) off++;
      else { pool++; if (+(k.backoff_secs || 0) > 0) pause++; }
    });
    const hidden = Math.max(0, total - keys.length);
    return { total: total, pool: pool + hidden, pause: pause, off: off, hidden: hidden };
  }
  const keyMatch = k => {
    const q = ST.kq.trim().toLowerCase();
    if (!q) return true;
    return String(k.name || "").toLowerCase().indexOf(q) >= 0 || String(k.key || k.masked || "").toLowerCase().indexOf(q) >= 0;
  };
  function keyVisible(k) {
    if (ST.kf === "pool" && k.enabled === false) return false;
    if (ST.kf === "off" && k.enabled !== false) return false;
    if (ST.kf === "pause" && !(k.enabled !== false && +(k.backoff_secs || 0) > 0)) return false;
    return keyMatch(k);
  }
  function kChips(c) {
    return [["all", "все " + c.total], ["pool", "вкл " + c.pool], ["pause", "пауза " + c.pause], ["off", "выкл " + c.off]]
      .map(x => '<button type="button" class="c75-chip' + (ST.kf === x[0] ? " on" : "") + '" data-c8="kf" data-v="' + x[0] + '">' + x[1] + '</button>').join("");
  }
  function keysListHTML(e) {
    const keys = poolKeys(e);
    const c = keyCounts(e, keys);
    let html = "", shown = 0;
    keys.forEach((k, i) => {
      if (!keyVisible(k)) return;
      html += keyRow(k, i, ST.ko === i);
      shown++;
    });
    if (!keys.length) {
      html = '<div class="c75-empty"><b>в пуле нет ключей</b><span>без ключа провайдер не используется</span>' +
        '<button type="button" class="c75-b prim" data-c8="addkey">+ ключ</button></div>';
    } else if (!shown) {
      html = '<div class="c75-empty"><b>ничего не найдено</b><span>поиск «' + esc(ST.kq) + '» и фильтр не дали совпадений</span>' +
        '<button type="button" class="c75-b" data-c8="kreset">сбросить</button></div>';
    } else if (c.hidden > 0) {
      html += '<div class="c75-kph"><span>…</span><b>ещё ' + c.hidden + " " + pl(c.hidden, "ключ", "ключа", "ключей") + '</b><em>маска и квота придут из ядра</em></div>';
    }
    return { html: html, shown: shown, keys: keys, c: c };
  }
  function keysHTML(e) {
    const keys = poolKeys(e);
    const pack = keysListHTML(e);
    return '<div class="c75-k">' +
      '<div class="c75-kbar">' +
        '<span class="c75-lab">ключи пула</span>' +
        '<div class="c75-find"><input class="c75-in" data-c8-filter="k" placeholder="имя или секрет…" value="' + esc(ST.kq) + '" autocomplete="off" spellcheck="false" aria-label="Поиск по ключам">' +
          '<button type="button" class="c75-x" data-c8="kclear" title="Сбросить поиск"' + (ST.kq ? "" : " hidden") + '>✕</button></div>' +
        '<span class="c75-chips" data-c75-kchips>' + kChips(pack.c) + '</span>' +
        '<span class="sp"></span>' +
        '<button type="button" class="c75-b" data-c8="addkey">+ ключ</button>' +
      '</div>' +
      (ST.add === "key" ? addRowHTML("key") : "") +
      '<div class="c75-kbody">' +
        '<div class="c75-khead"><span>№</span><span></span><span>ключ</span><span title="три метра квот: токены / запросы / бюджет">квоты</span><span>состояние</span><span>решение</span></div>' +
        '<div class="c75-klist" data-c75-klist>' + pack.html + '</div>' +
        '<div class="c75-foot"><span>429 паркует один ключ на base×2ⁿс — остальные работают</span><span class="sp"></span><span data-c75-kcnt>показано ' + pack.shown + ' из ' + keys.length + '</span></div>' +
      '</div></div>';
  }

  function addRowHTML(kind) {
    const key = kind === "key";
    return '<div class="c75-add">' +
      '<input class="c75-in" data-c8-add placeholder="' + (key ? "sk-… новый ключ · Enter — добавить" : "имя модели · Enter — добавить") +
      '" autocomplete="off" spellcheck="false" aria-label="' + (key ? "Новый ключ" : "Новая модель") + '">' +
      '<button type="button" class="c75-b prim" data-c8="addgo">добавить</button>' +
      '<button type="button" class="c75-b" data-c8="addcancel">отмена</button></div>';
  }

  /* ---------- модели ---------- */
  function sortModels(e) {
    const st = modelStats(e);
    const models = (e.models || []).slice();
    const total = Math.max(1, Object.keys(st).reduce((s, m) => s + (st[m].n || 0), 0));
    const key = ST.sort, dir = ST.dir;
    const val = m => {
      const s = st[m] || {};
      if (key === "calls") return s.n || 0;
      if (key === "errs") return s.err || 0;
      if (key === "share") return (s.n || 0) / total;
      return 0;
    };
    models.sort((a, b) => key === "name"
      ? dir * (a < b ? -1 : a > b ? 1 : 0)
      : dir * (val(a) - val(b)) || (a < b ? -1 : 1));
    return { models: models, stats: st, total: total, max: Math.max(1, ...models.map(m => (st[m] || {}).n || 0)) };
  }
  const sortLabel = () => "сорт: " + ((SORTS.filter(s => s[0] === ST.sort)[0] || SORTS[0])[1]) + (ST.dir > 0 ? " ↑" : " ↓");
  function modelRow(e, m, pack, dis, hid) {
    const off = dis.has(m), hd = hid.has(m);
    const s = pack.stats[m] || { n: 0, err: 0 };
    const share = s.n ? Math.max(2, Math.round(s.n / pack.max * 100)) : 0;
    const lvl = (e.model_reasoning && e.model_reasoning[m]) || "auto";
    const open = ST.mo === m, armed = ST.probe.model === m;
    const pct = s.n ? Math.round(s.n / pack.total * 100) : 0;
    const pctTxt = s.n ? (pct || "<1") + "%" : "";
    return '<div class="c75-mr' + (off ? " off" : "") + (hd ? " hid" : "") + (open ? " open" : "") + (armed ? " armed" : "") + '" data-c75-mr="' + esc(m) + '">' +
        '<button type="button" class="c75-exp" data-c8="mexp" data-m="' + esc(m) + '" aria-expanded="' + (open ? "true" : "false") + '" aria-label="' + (open ? "Свернуть модель" : "Подробности модели") + '"></button>' +
        '<span class="c75-mn" title="' + esc(m + " · семейство " + fam(m)) + '"><i class="fam">' + famMark(m) + '</i><b>' + esc(m) + '</b>' + (hd ? '<em class="tag">скрыта</em>' : "") + '</span>' +
        '<span class="c75-mc" title="' + esc(s.n + " вызовов · доля " + pctTxt + " журнала") + '">' +
          '<b>' + (s.n || "—") + '</b><span class="bar"><span style="width:' + share + '%"></span></span>' +
          '<em>' + pctTxt + '</em></span>' +
        '<span class="c75-me">' + (s.err ? '<b>' + s.err + '</b>' : '<span>0</span>') + '</span>' +
        '<span class="c75-mlv" title="' + esc("reasoning " + lvl + " · меняется в раскрытой строке") + '"><b>' + esc(lvl) + '</b></span>' +
        '<button type="button" class="c75-play' + (armed ? " on" : "") + '" data-c8="mtest" data-m="' + esc(m) + '" title="Проверить: модель уйдёт в тест-слой" aria-label="Проверить ' + esc(m) + '"><svg viewBox="0 0 6 8" width="6" height="8" aria-hidden="true"><path d="M0 0l6 4-4 4z" fill="currentColor"/></svg></button>' +
        '<button type="button" class="c75-dec' + (off ? "" : " on") + '" data-c8="mtgl" data-m="' + esc(m) + '" aria-pressed="' + (off ? "false" : "true") + '" title="' + (off ? "Включить маршрутизацию" : "Выключить маршрутизацию (404)") + '">' + (off ? "выкл" : "вкл") + '</button>' +
      '</div>' + (open ? modelDetail(e, m, pack, off, hd, s, lvl) : "");
  }
  function modelDetail(e, m, pack, off, hd, s, lvl) {
    const last = (provLogs(e).find(l => (l.model || l.requested_model) === m) || {}).ts || "";
    return '<div class="c75-md" data-c75-md="' + esc(m) + '">' +
      '<div class="c75-dg">' +
        '<span class="c75-dc wide"><i class="k">reasoning</i><span class="c75-seg" role="group" aria-label="Уровень reasoning">' +
          LV.map(x => '<button type="button" class="' + (x === lvl ? "on" : "") + '" data-c8="mreason" data-m="' + esc(m) + '" data-v="' + x + '">' + x + '</button>').join("") +
        '</span></span>' +
        '<span class="c75-dc"><i class="k">вызовы</i><span class="v">' + (s.n || 0) + '<em>' + (s.n ? Math.round(s.n / pack.total * 100) + "% журнала · ошибок " + s.err : "ещё не вызывалась") + '</em></span></span>' +
        '<span class="c75-dc"><i class="k">последний</i><span class="v">' + (last ? esc(String(last).slice(11, 19)) : "—") + '<em>' + (last ? esc(String(last).slice(0, 10)) : "нет записей") + '</em></span></span>' +
        '<span class="c75-dc"><i class="k">каталог</i><span class="v">' + (hd ? "скрыта" : "видна") + '<em>' + (off ? "маршрут выключен" : "маршрут включён") + '</em></span></span>' +
      '</div>' +
      '<div class="c75-dact">' +
        '<span class="c75-hint">reasoning и маршрут правятся на месте</span><span class="sp"></span>' +
        '<button type="button" class="c75-b" data-c8="meye" data-m="' + esc(m) + '">' + (hd ? "вернуть в каталог" : "скрыть из каталога") + '</button>' +
        '<button type="button" class="c75-b prim" data-c8="mtest" data-m="' + esc(m) + '">проверить в тест-слое</button>' +
      '</div></div>';
  }
  function mstatText(e) {
    const models = e.models || [];
    const dis = new Set(e.disabled_models || []), hid = new Set(e.hidden_models || []);
    const win = winStats(e);
    return '<span><i>каталог</i><b>' + models.length + '</b></span>' +
      '<span><i>в маршруте</i><b>' + models.filter(m => !dis.has(m)).length + '</b></span>' +
      '<span><i>выкл</i><b>' + dis.size + '</b></span>' +
      '<span><i>скрыто</i><b>' + hid.size + '</b></span>' +
      '<span><i>вызовов</i><b>' + win.n + '</b><em>' + win.err + ' отказ</em></span>';
  }
  function modelsListHTML(e) {
    const models = e.models || [];
    const dis = new Set(e.disabled_models || []), hid = new Set(e.hidden_models || []);
    const pack = sortModels(e);
    const q = ST.mq.trim().toLowerCase();
    const shown = pack.models.filter(m => !q || m.toLowerCase().indexOf(q) >= 0);
    let html;
    if (!shown.length) {
      html = models.length
        ? '<div class="c75-empty"><b>ничего не найдено</b><span>фильтр «' + esc(ST.mq) + '» не совпал ни с одной моделью</span>' +
          '<button type="button" class="c75-b" data-c8="mclear">сбросить</button></div>'
        : '<div class="c75-empty"><b>каталог пуст</b><span>провайдер работает как wildcard и примет любую модель</span>' +
          '<button type="button" class="c75-b prim" data-c8="addmodel">+ модель</button></div>';
    } else {
      html = shown.map(m => modelRow(e, m, pack, dis, hid)).join("");
    }
    return { html: html, shown: shown.length, total: models.length };
  }
  function modelsHTML(e) {
    const models = e.models || [];
    const pack = modelsListHTML(e);
    return '<div class="c75-mods">' +
      '<div class="c75-mbar">' +
        '<span class="c75-lab">модели</span>' +
        '<div class="c75-find"><input class="c75-in" data-c8-filter="m" placeholder="фильтр по имени…" value="' + esc(ST.mq) + '" autocomplete="off" spellcheck="false" aria-label="Фильтр моделей">' +
          '<button type="button" class="c75-x" data-c8="mclear" title="Сбросить фильтр"' + (ST.mq ? "" : " hidden") + '>✕</button></div>' +
        '<button type="button" class="c75-b sort" data-c8="msort" title="Сменить сортировку">' + sortLabel() + '</button>' +
        '<span class="c75-cnt" data-c75-mcnt>' + pack.shown + ' / ' + models.length + '</span>' +
        '<span class="sp"></span>' +
        '<button type="button" class="c75-link" data-c8="msync" title="Подтянуть каталог из upstream /models">↻ каталог</button>' +
        '<button type="button" class="c75-b" data-c8="addmodel">+ модель</button>' +
      '</div>' +
      (ST.add === "model" ? addRowHTML("model") : "") +
      '<div class="c75-mbody">' +
        '<div class="c75-mstat" data-c75-mstat>' + mstatText(e) + '</div>' +
        '<div class="c75-mhead"><span></span><span>модель</span><span>вызовы</span><span>ош.</span><span>разум.</span><span></span><span>маршрут</span></div>' +
        '<div class="c75-mlist" data-c75-mlist>' + pack.html + '</div>' +
      '</div>' +
      testHTML(e) +
    '</div>';
  }

  /* ---------- тест-слой: нижняя панель одной строкой ---------- */
  function testCurrent(e) { return ST.probe.model || testModel(e); }
  function nowHMS() {
    const d = new Date();
    return [d.getHours(), d.getMinutes(), d.getSeconds()].map(x => String(x).padStart(2, "0")).join(":");
  }
  function resHTML() {
    const o = ST.probe.out;
    if (!o) {
      return '<span class="c75-rh">ответ</span><span class="c75-rb">' + (liveCore()
        ? "промпт уйдёт в выбранную модель через API.ChatProbe"
        : "синтетика · промпт в ядро не уходил") + '</span>';
    }
    return '<span class="c75-rh ' + o.kind + '">' + (o.kind === "demo" ? "синтетика" : o.kind === "ok" ? "ответ" : o.kind === "wait" ? "ожидание" : "ошибка") + '</span>' +
      '<span class="c75-rm">' + esc(o.m || "") + '</span>' +
      '<span class="c75-rd">' + (o.kind === "wait" ? "…" : (o.dur != null ? o.dur + " мс" : "—")) + '</span>' +
      '<span class="c75-rb">' + o.text + '</span>';
  }
  function histHTML() {
    const h = ST.probe.hist;
    return '<div class="c75-hist" data-c75-hist>' +
      '<div class="c75-hh"><span class="c75-lab">история теста</span><span>' + h.length + " " + pl(h.length, "запись", "записи", "записей") + '</span>' +
        '<span class="sp"></span><span>клик по строке — вернуть промпт в поле</span></div>' +
      (h.length ? h.map((x, i) =>
        '<button type="button" class="c75-hr" data-c8="hload" data-i="' + i + '" title="' + esc(x.prompt) + '">' +
          '<span class="t">' + esc(x.hhmmss) + '</span>' +
          '<span class="m">' + esc(x.m) + '</span>' +
          '<span class="s ' + x.kind + '">' + (x.kind === "demo" ? "синт." : x.kind === "ok" ? "ok" : "err") + '</span>' +
          '<span class="d">' + (x.dur != null ? x.dur + " мс" : "—") + '</span>' +
          '<span class="p">' + esc(x.prompt) + '</span></button>').join("")
        : '<div class="c75-hnone">пока пусто — отправьте первый запрос</div>') +
    '</div>';
  }
  function pickListHTML(e) {
    const q = ST.probe.pickQ.trim().toLowerCase();
    const models = (e.models || []).filter(m => !q || m.toLowerCase().indexOf(q) >= 0);
    const cur = testCurrent(e);
    return models.length
      ? models.map(m => '<button type="button" class="c75-po' + (m === cur ? " sel" : "") + '" data-c8="tpickv" data-m="' + esc(m) + '">' +
        '<i class="fam">' + famMark(m) + '</i><b>' + esc(m) + '</b></button>').join("")
      : '<div class="c75-hnone">ничего не найдено</div>';
  }
  function pickHTML(e) {
    const q = ST.probe.pickQ.trim().toLowerCase();
    const n = (e.models || []).filter(m => !q || m.toLowerCase().indexOf(q) >= 0).length;
    return '<div class="c75-pick" data-c75-pick>' +
      '<div class="c75-ph"><input class="c75-in" data-c8-pick placeholder="поиск модели из ' + (e.models || []).length + '…" value="' + esc(ST.probe.pickQ) + '" autocomplete="off" spellcheck="false" aria-label="Поиск модели"><span>' + n + '</span></div>' +
      '<div class="c75-pl" data-c75-picklist>' + pickListHTML(e) + '</div></div>';
  }
  function testStateHTML() {
    const p = ST.probe;
    if (p.busy) return '<i class="w"></i>отправка…';
    if (p.out) {
      const k = p.out.kind;
      if (k === "demo") return '<i class="demo"></i>синтетика · ' + (p.out.dur != null ? p.out.dur + " мс" : "локально");
      if (k === "ok") return '<i class="ok"></i>ответ · ' + (p.out.dur != null ? p.out.dur + " мс" : "ok");
      if (k === "err") return '<i class="err"></i>ошибка';
      return '<i class="w"></i>ожидание';
    }
    return liveCore() ? '<i class="ok"></i>живой вызов · API.ChatProbe' : '<i class="demo"></i>синтетика · промпт в ядро не уходил';
  }
  function testHTML(e) {
    const p = ST.probe;
    const m = testCurrent(e);
    return '<div class="c75-test' + (p.open ? " open" : "") + '" data-c75-test>' +
      (p.histOpen ? histHTML() : "") +
      (p.pick ? pickHTML(e) : "") +
      '<div class="c75-th">' +
        '<span class="c75-lab hot">тест-слой</span>' +
        '<button type="button" class="c75-tm" data-c8="tpick" aria-haspopup="listbox" aria-expanded="' + (p.pick ? "true" : "false") + '" title="Выбрать модель из каталога">' +
          '<i class="fam">' + famMark(m) + '</i><b>' + esc(m || "модель не выбрана") + '</b><span>▾</span></button>' +
        '<span class="c75-tstate" data-c75-tstate>' + testStateHTML() + '</span>' +
        '<span class="sp"></span>' +
        '<button type="button" class="c75-link" data-c8="thist" aria-expanded="' + (p.histOpen ? "true" : "false") + '">история ' + p.hist.length + '</button>' +
        '<button type="button" class="c75-link" data-c8="tclear"' + (p.hist.length ? "" : " disabled") + '>очистить</button>' +
        '<button type="button" class="c75-texp" data-c8="ttoggle" aria-expanded="' + (p.open ? "true" : "false") + '" title="' + (p.open ? "Свернуть тест-слой" : "Развернуть тест-слой") + '">' + (p.open ? "⌄" : "⌃") + '</button>' +
      '</div>' +
      '<div class="c75-tbody"' + (p.open ? "" : " hidden") + '>' +
        '<div class="c75-compose">' +
          '<textarea class="c75-ta" data-c8-prompt rows="1" spellcheck="false" aria-label="Запрос к модели" placeholder="запрос к модели · Enter — перенос, Ctrl+Enter — отправить">' + esc(p.prompt) + '</textarea>' +
          '<span class="c75-count" data-c75-count>' + p.prompt.length + '</span>' +
          '<button type="button" class="c75-go" data-c8="tgo"' + (p.busy || !p.prompt.trim() || !m ? " disabled" : "") + '>' + (p.busy ? "…" : "отправить") + '</button>' +
        '</div>' +
        '<div class="c75-res" data-c75-res>' + resHTML() + '</div>' +
      '</div></div>';
  }

  /* ---------- связь ---------- */
  /* ---------- связь: аккордеон в стиле настроек (порт c14) ---------- */
  let c14seq = 0;
  let c14OPEN = null;
  const c14opts = w => Array.prototype.slice.call(w.querySelectorAll("[role=option]"));
  function c14dd(key, value, items, o) {
    o = o || {};
    const list = items.map(it => (typeof it === "string" ? { v: it, t: it } : it));
    const cur = list.filter(i => i.v === value)[0] || list[0];
    const uid = ++c14seq;
    return '<div class="x14-dd' + (o.cls ? " " + o.cls : "") + '" data-c8-dd="' + esc(key) + '"' + (o.dis ? ' data-dis="1"' : "") + '>' +
      '<button type="button" class="x14-ddb" role="combobox" aria-haspopup="listbox" aria-expanded="false"' +
        ' aria-controls="x14-ddm-' + uid + '"' +
        (o.aria ? ' aria-label="' + esc(o.aria) + '"' : "") + (o.title ? ' title="' + esc(o.title) + '"' : "") +
        (o.dis ? " disabled" : "") + '><span class="v">' + esc(cur ? cur.t : "") + '</span><i class="ch" aria-hidden="true"></i></button>' +
      '<div class="x14-ddm" id="x14-ddm-' + uid + '" role="listbox" hidden>' +
        list.map((it, i) => '<button type="button" role="option" id="x14-ddo-' + uid + "-" + i + '" class="x14-ddo' +
          (it.v === value ? " on" : "") + '" data-v="' + esc(it.v) + '" aria-selected="' + (it.v === value) + '">' + esc(it.t) + '</button>').join("") +
      '</div></div>';
  }
  function c14ddActive(w, i) {
    const opts = c14opts(w);
    if (!opts.length) return;
    const n = Math.max(0, Math.min(opts.length - 1, i));
    opts.forEach((o, k) => o.classList.toggle("act", k === n));
    w._ddi = n;
    const b = w.querySelector(".x14-ddb");
    if (b) b.setAttribute("aria-activedescendant", opts[n].id);
    try { opts[n].scrollIntoView({ block: "nearest" }); } catch (x) {}
  }
  function c14ddClose(w, back) {
    if (!w) return;
    w.classList.remove("on", "up");
    const b = w.querySelector(".x14-ddb"), m = w.querySelector(".x14-ddm");
    if (b) b.setAttribute("aria-expanded", "false");
    if (m) m.hidden = true;
    if (c14OPEN === w) c14OPEN = null;
    if (back && b) { try { b.focus({ preventScroll: true }); } catch (x) { try { b.focus(); } catch (x2) {} } }
  }
  function c14ddShow(w) {
    if (!w || w.getAttribute("data-dis")) return;
    if (c14OPEN && c14OPEN !== w) c14ddClose(c14OPEN, false);
    const m = w.querySelector(".x14-ddm"), b = w.querySelector(".x14-ddb");
    if (!m || !b || b.disabled) return;
    m.hidden = false;
    w.classList.add("on");
    c14OPEN = w;
    b.setAttribute("aria-expanded", "true");
    const br = b.getBoundingClientRect();
    const box = w.closest(".c8-scr"), boxTop = box ? box.getBoundingClientRect().top : 0;
    const lim = box ? box.getBoundingClientRect().bottom : innerHeight;
    const ph = m.offsetHeight || 150;
    w.classList.toggle("up", br.bottom + ph + 8 > lim && br.top - ph - 8 > boxTop);
    const cur = c14opts(w).findIndex(o => o.classList.contains("on"));
    c14ddActive(w, cur < 0 ? 0 : cur);
  }
  function c14ddPick(w) {
    const opts = c14opts(w), o = opts[w._ddi == null ? 0 : w._ddi];
    if (!o) return;
    const v = o.getAttribute("data-v"), key = w.getAttribute("data-c8-dd") || "", e = CUR;
    if (e && key.indexOf("reason:") === 0) {
      e.model_reasoning = e.model_reasoning || {};
      e.model_reasoning[key.slice(7)] = v;
    } else if (e && key.indexOf("tf:") === 0) {
      e.transport = e.transport || {};
      const n = parseInt(v, 10);
      if (Number.isFinite(n)) e.transport[key.slice(3)] = n;
    } else if (e && key.indexOf("f:") === 0) {
      storeField(e, key.slice(2), v);
    }
    const b = w.querySelector(".x14-ddb .v");
    if (b) b.textContent = o.textContent;
    opts.forEach(x => { x.classList.toggle("on", x === o); x.setAttribute("aria-selected", String(x === o)); });
    c14paint(document.querySelector("#provDetail [data-c8card]"), e);
    c14ddClose(w, true);
  }
  function c14step(key, value, o) {
    o = o || {};
    const min = o.min == null ? 1 : o.min, max = o.max == null ? 3600 : o.max;
    const v = Number.isFinite(+value) ? +value : min;
    return '<div class="x14-stp' + (o.cls ? " " + o.cls : "") + '" data-c8-stp="' + esc(key) + '" data-min="' + min +
      '" data-max="' + max + '" data-step="' + (o.step || 1) + '">' +
      '<button type="button" class="x14-stb" data-c8="step-" aria-label="Уменьшить" tabindex="-1">−</button>' +
      '<span class="x14-stw"><input class="x14-sti" type="number" inputmode="numeric" value="' + v +
        '" aria-label="' + esc(o.aria || key) + '"' + (o.dis ? " disabled" : "") + '>' +
        (o.unit ? '<i class="x14-stu">' + esc(o.unit) + '</i>' : "") + '</span>' +
      '<button type="button" class="x14-stb" data-c8="step+" aria-label="Увеличить" tabindex="-1">+</button></div>';
  }
  function c14stepWrite(key, n) {
    const e = CUR;
    if (!e) return;
    if (key.indexOf("tf:") === 0) { e.transport = e.transport || {}; e.transport[key.slice(3)] = n; return; }
    if (key.indexOf("f:") === 0) storeField(e, key.slice(2), String(n));
  }
  function c14stepApply(w, n, root, snap) {
    if (!w) return;
    const min = +(w.getAttribute("data-min") || 1), max = +(w.getAttribute("data-max") || 3600);
    const d = +(w.getAttribute("data-step") || 1);
    const inp = w.querySelector(".x14-sti");
    let v = Number.isFinite(+n) ? Math.round(+n) : min;
    v = Math.max(min, Math.min(max, snap === false ? v : Math.round(v / d) * d));
    if (inp && String(v) !== inp.value) inp.value = String(v);
    c14stepWrite(w.getAttribute("data-c8-stp"), v);
    c14paint(root || document.querySelector("#provDetail [data-c8card]"), CUR);
  }
  function c14acc(o) {
    const on = ST.acc[o.id] != null ? !!ST.acc[o.id] : true;
    return '<section class="x14-ac' + (on ? " on" : "") + '" data-c8-ac="' + o.id + '">' +
      '<div class="x14-ach">' +
        '<button type="button" class="x14-acht" data-c8="acc" data-ac="' + o.id + '" aria-expanded="' + on + '" aria-controls="x14-acb-' + o.id + '">' +
          '<span class="i">' + o.no + '</span>' +
          '<span class="t">' + o.title + '</span>' +
          '<span class="h">' + o.hint + '</span>' +
          '<span class="sp"></span>' +
          (o.sum ? '<span class="sum" data-c8-sum="' + o.id + '">' + esc(o.sum) + '</span>' : "") +
        '</button>' + (o.act || "") +
        '<i class="chev" aria-hidden="true"></i>' +
      '</div>' +
      '<div class="x14-acb" id="x14-acb-' + o.id + '">' + o.body + '</div></section>';
  }
  function c14paint(root, e) {
    if (!root || !e) return;
    const tr = e.transport || {};
    const fan = +tr.fanout || 5, rnd = +tr.rounds || 2;
    root.querySelectorAll("[data-c8-calc]").forEach(el => { el.textContent = String(fan * rnd); });
    root.querySelectorAll("[data-c8-calcn]").forEach(el => { el.textContent = fan + " × " + rnd; });
    const sums = {
      chan: [e.auth_mode || (e.auth_kind === "no-auth" ? "none" : "bearer"), (e.timeout_s || 60) + " с", e.egress_mode || "off"],
      pool: [e.key_swap || "round_robin", "fanout " + fan + "×" + rnd, "кулдаун " + (tr.cooldown_s || 60) + " с"]
    };
    Object.keys(sums).forEach(k => {
      const el = root.querySelector('[data-c8-sum="' + k + '"]');
      if (el) el.textContent = sums[k].join(" · ");
    });
  }
  function c14accToggle(id) {
    ST.acc[id] = !(ST.acc[id] != null ? !!ST.acc[id] : true);
    const root = document.querySelector("#provDetail [data-c8card]");
    if (!root) return;
    const sec = root.querySelector('[data-c8-ac="' + id + '"]');
    if (!sec) return;
    sec.classList.toggle("on", !!ST.acc[id]);
    const b = sec.querySelector("[data-c8='acc']");
    if (b) b.setAttribute("aria-expanded", String(!!ST.acc[id]));
  }
  function c14chk(e) {
    const hc = healthOf(e);
    const ok = !!(hc && hc.ok);
    return '<div class="x14-chk">' +
        '<span class="k">endpoint</span><span class="v" title="' + esc((hc && hc.endpoint) || e.base_url || "—") + '">' + esc((hc && hc.endpoint) || e.base_url || "—") + '</span>' +
        '<span class="k">rtt</span><span class="v">' + (hc && hc.latency_ms != null ? hc.latency_ms + " мс" : "—") + '</span>' +
        '<span class="k">http</span><span class="v">' + (hc && hc.http ? hc.http : "—") + '</span>' +
        '<span class="k">состояние</span><span class="v ' + (hc ? (ok ? "on" : "bad") : "") + '">' + (hc ? (ok ? "ok" : "ошибка") : "нет замера") + '</span>' +
      '</div>' +
      (hc && hc.last_err ? '<div class="c8-note bad">последняя ошибка: ' + esc(hc.last_err) + '</div>' : "") +
      '<div class="c8-note">RTT обновляется фоном; кнопка гоняет замер сразу</div>';
  }
  function c14hints(e) {
    const R = (S.cfg && S.cfg.resilience) || {};
    const cd = R.cooldown || {}, bk = R.breaker || {};
    return '<ul class="x14-ul">' +
        '<li><b>429</b> паркует ключ на ' + (cd.base_seconds || 15) + 'с ×2 до ' + (cd.max_seconds || 300) + 'с — остальные ключи пула работают.</li>' +
        '<li><b>5xx/таймаут</b> — повтор по следующему ключу и, если включён egress, по следующему выходу.</li>' +
        '<li><b>брейкер</b> откроется после ' + (bk.max_failures || 5) + ' сбоев подряд и вернётся сам через ' + (bk.recovery_seconds || 60) + 'с.</li>' +
        '<li><b>' + esc(e.egress_mode || "off") + '</b> — ' + (e.egress_mode === "off" ? "запрос идёт напрямую, без прокси-пула." : "запрос идёт через пул egress ядра.") + '</li>' +
      '</ul>';
  }
  function c14conn(e) {
    const isKL = e.auth_kind === "no-auth";
    const tr = e.transport || {};
    const fan = +tr.fanout || 5, rnd = +tr.rounds || 2;
    const hc = healthOf(e);
    const R = (S.cfg && S.cfg.resilience) || {};
    const bk = R.breaker || {};

    const fBase = '<div class="c8-f wide"><span class="lbl">base_url</span>' +
      '<input class="c8-in" data-c8-f="base_url" value="' + esc(e.base_url || "") + '" placeholder="https://api.example.com" spellcheck="false" aria-label="base_url"></div>';
    const fAuth = '<div class="c8-f"><span class="lbl">auth mode</span>' +
      c14dd("f:auth_mode", e.auth_mode || (isKL ? "none" : "bearer"), AUTH, { dis: isKL, title: isKL ? "keyless — подпись не нужна" : "Способ авторизации", aria: "auth mode" }) + '</div>';
    const fUA = '<div class="c8-f"><span class="lbl">user-agent</span>' +
      '<input class="c8-in" data-c8-f="user_agent" value="' + esc(e.user_agent || "") + '" placeholder="opencode/1.18.99" spellcheck="false" aria-label="user-agent"></div>';
    const fTo = '<div class="c8-f"><span class="lbl">таймаут</span>' +
      c14step("f:timeout_s", e.timeout_s || 60, { min: 1, max: 3600, step: 5, unit: "с", aria: "Таймаут, секунд" }) + '</div>';
    const fEm = '<div class="c8-f"><span class="lbl">egress mode</span>' +
      c14dd("f:egress_mode", e.egress_mode || "off", EGRESS, { title: "Как выходит запрос", aria: "egress mode" }) + '</div>';
    const fEu = '<div class="c8-f wide"><span class="lbl">egress url</span>' +
      '<input class="c8-in" data-c8-f="egress" value="' + esc(e.egress || "") + '" placeholder="http://127.0.0.1:8118 — пусто: решает транспорт" spellcheck="false" aria-label="egress url"></div>';

    const fSwap = '<div class="c8-f"><span class="lbl">свап ключей</span>' +
      c14dd("f:key_swap", e.key_swap || "round_robin", SWAP, { title: "Как выбирается ключ", aria: "свап ключей" }) + '</div>';
    const fCd = '<div class="c8-f"><span class="lbl">кулдаун egress</span>' +
      c14step("tf:cooldown_s", +tr.cooldown_s || 60, { min: 10, max: 3600, step: 10, unit: "с", aria: "Кулдаун egress, секунд" }) + '</div>';
    const fFan = '<div class="c8-f"><span class="lbl">fanout × rounds</span><span class="x14-duo2">' +
      c14step("tf:fanout", fan, { min: 1, max: 10, aria: "fanout" }) +
      '<i class="x14-x" aria-hidden="true">×</i>' +
      c14step("tf:rounds", rnd, { min: 1, max: 5, aria: "rounds" }) + '</span></div>';
    const fCalc = '<div class="c8-f"><span class="lbl">попыток на 429</span>' +
      '<span class="x14-calc" title="fanout × rounds"><b data-c8-calc>' + (fan * rnd) + '</b><i data-c8-calcn>' + fan + " × " + rnd + '</i></span></div>';

    const hcSum = hc ? (hc.ok ? "ok" : "ошибка") + (hc.latency_ms != null ? " · " + hc.latency_ms + " мс" : "") + (hc.http ? " · http " + hc.http : "") : "нет замера";
    const acSum = (e.auth_mode || (isKL ? "none" : "bearer")) + " · " + (e.timeout_s || 60) + " с · " + (e.egress_mode || "off");
    const poolSum = (e.key_swap || "round_robin") + " · fanout " + fan + "×" + rnd + " · кулдаун " + (+tr.cooldown_s || 60) + " с";
    const failSum = (e.egress_mode || "off") + " · брейкер " + (bk.max_failures || 5) + " сбоев / " + (bk.recovery_seconds || 60) + " с";

    return '<div class="c8-conn x14">' +
      c14acc({ id: "chan", no: "01", title: "канал", hint: "base_url · auth · ua · таймаут", sum: acSum,
        body: '<div class="c8-form">' + fBase + fAuth + fUA + fTo + fEm + fEu + '</div>' }) +
      c14acc({ id: "pool", no: "02", title: "транспорт и пул", hint: "ротация ключей · fanout · кулдаун egress", sum: poolSum,
        body: '<div class="c8-form">' + fSwap + fCd + fFan + fCalc + '</div>' }) +
      c14acc({ id: "check", no: "03", title: "проверка связи", hint: "healthcheck шлюза", sum: hcSum,
        act: '<button type="button" class="c8-b" data-c8="check">▶ healthcheck</button>',
        body: c14chk(e) }) +
      c14acc({ id: "fail", no: "04", title: "что происходит при сбое", hint: "поведение ядра", sum: failSum,
        body: c14hints(e) }) +
      '<div class="x14-foot"><span class="c8-hint">правки на месте · «Сохранить» — в ядро</span><span class="sp"></span>' +
        '<button type="button" class="c8-b" data-c8="copyurl">⧉ base_url</button>' +
        '<button type="button" class="c8-b prim" data-c8="save">Сохранить</button></div></div>';
  }

  /* ---------- каркас ---------- */
  function screensHTML(e) {
    return [0, 1, 2, 3].map(i => '<div class="c8-scr' + (ST.screen === i ? " on" : "") + '" id="c8-scr-' + i +
      '" data-c8-scr="' + i + '" role="tabpanel" aria-label="' + SCREENS[i] + '">' +
      [sumHTML(e), keysHTML(e), modelsHTML(e), c14conn(e)][i] + '</div>').join("");
  }

  function render(e, force) {
    const host = document.getElementById("provDetail");
    if (!host) return;
    if (!e) { CUR = null; host.innerHTML = '<div class="c8"><div class="c8-empty">провайдер не выбран</div></div>'; return; }
    if (ST.prov !== e.name) {
      ST.prov = e.name; ST.screen = 0; ST.add = ""; ST.acc = {};
      ST.kq = ""; ST.kf = "all"; ST.ko = null;
      ST.mq = ""; ST.mo = null; ST.sort = "calls"; ST.dir = -1;
      ST.probe = freshProbe(e);
    }
    const ae = document.activeElement;
    if (!force && host.querySelector(".c8-dd.open")) return; // не закрывать открытое меню фоновым обновлением
    if (!force && ae && host.contains(ae) && (ae.hasAttribute("data-c8dd") || /^(INPUT|SELECT|TEXTAREA)$/.test(ae.tagName))) return; // не рвать ввод и клавиатуру на фоновом обновлении
    try {
      const meta = document.getElementById("prov-meta");
      if (meta) {
        if (meta._c8saved == null) meta._c8saved = meta.textContent;
        const kt = keyTotal(e), md = (e.models || []).length;
        meta.textContent = (e.name === "__GLOBAL__" ? "глобальные ключи" : e.name) + " · " +
          (md ? md + " " + pl(md, "модель", "модели", "моделей") : "wildcard") + " · " +
          (kt ? kt + " " + pl(kt, "ключ", "ключа", "ключей") : "ключей нет");
      }
    } catch (xM) {}
    CUR = e;
    host.innerHTML = '<div class="c8" data-c8card>' + headHTML(e) + navHTML(e) +
      '<div class="c8-body">' + screensHTML(e) + '</div></div>';
    const root = host.querySelector("[data-c8card]");
    if (!root) return;
    wire(root, e);
    if (ST.add) {
      const inp = root.querySelector("[data-c8-add]");
      if (inp) { try { inp.focus({ preventScroll: true }); } catch (x) { try { inp.focus(); } catch (x2) {} } }
    }
  }

  /* ---------- правка на месте ---------- */
  function storeField(e, f, v) {
    if (f === "timeout_s") { const n = parseInt(v, 10); e.timeout_s = Number.isFinite(n) ? Math.max(1, Math.min(3600, n)) : 0; return; }
    e[f] = v;
  }
  function addKey(e, raw) {
    const v = String(raw || "").trim();
    if (!v) { toast("Пустое значение — ключ не добавлен", "warn"); return; }
    const k = {
      name: "key-" + (v.length > 6 ? v.slice(-4) : v), key: v, models: [],
      quota_tokens: 0, quota_reqs: 0, budget_usd: 0,
      usage_tokens: 0, usage_reqs: 0, usage_usd: 0, token_pct: 0, req_pct: 0, usd_pct: 0
    };
    if (!S.kbp) S.kbp = {};
    const listName = e.name === "__GLOBAL__" ? "глобальные" : e.name;
    S.kbp[listName] = (S.kbp[listName] || []).concat([k]);
    e.keys = (e.keys || 0) + 1;
    e.keys_healthy = (e.keys_healthy || 0) + 1;
    e.keys_pct = e.keys ? e.keys_healthy / e.keys : -1;
    if (!liveCore()) toast("ключ добавлен на экран (демо: в ядро не пишется)", "ok");
    else {
      try {
        API.AddProviderKeys(e.name, [v]).then(r => toast(r && r.message ? r.message : "ключ добавлен", r && r.ok ? "ok" : "err"))
          .catch(x => toast("Ключ: " + x, "err"));
      } catch (x) { toast("Ключ: " + x, "err"); }
    }
  }
  function addModel(e, raw) {
    const v = String(raw || "").trim();
    if (!v) { toast("Пустое имя модели", "warn"); return; }
    e.models = e.models || [];
    if (e.models.indexOf(v) >= 0) { toast("Модель уже в каталоге", "warn"); return; }
    e.models = e.models.concat([v]);
    try { if (typeof customAdd === "function") customAdd(e.name, v); } catch (x) {}
    toast("модель «" + v + "» в каталоге" + (isDemo() ? " (демо)" : ""), "ok");
  }
  function delKey(e, i) {
    const list = poolKeys(e).slice();
    if (!list[i]) return;
    const gone = list.splice(i, 1)[0];
    if (S.kbp) {
      if (e.name === "__GLOBAL__") S.kbp["глобальные"] = list;
      else S.kbp[e.name] = list;
    }
    e.keys = Math.max(0, (e.keys || 0) - 1);
    e.keys_healthy = Math.min(e.keys_healthy || 0, e.keys);
    e.keys_pct = e.keys ? (e.keys_healthy || 0) / e.keys : -1;
    toast("ключ «" + (gone.name || mask(gone.key)) + "» убран с экрана", "warn");
  }
  function toggleModel(e, m, kind) {
    const field = kind === "hid" ? "hidden_models" : "disabled_models";
    const arr = new Set(e[field] || []);
    if (arr.has(m)) arr.delete(m); else arr.add(m);
    e[field] = Array.from(arr);
  }
  function paintKeys(root, e) {
    const list = root.querySelector("[data-c75-klist]");
    const keys = poolKeys(e);
    const pack = keysListHTML(e);
    if (list) list.innerHTML = pack.html;
    const ch = root.querySelector("[data-c75-kchips]");
    if (ch) ch.innerHTML = kChips(pack.c);
    const cnt = root.querySelector("[data-c75-kcnt]");
    if (cnt) cnt.textContent = "показано " + pack.shown + " из " + keys.length;
    const clr = root.querySelector('[data-c8="kclear"]');
    if (clr) clr.hidden = !ST.kq.trim();
  }
  function paintModels(root, e) {
    const list = root.querySelector("[data-c75-mlist]");
    const pack = modelsListHTML(e);
    if (list) list.innerHTML = pack.html;
    const c = root.querySelector("[data-c75-mcnt]");
    if (c) c.textContent = pack.shown + " / " + pack.total;
    const st = root.querySelector("[data-c75-mstat]");
    if (st) st.innerHTML = mstatText(e);
    const clr = root.querySelector('[data-c8="mclear"]');
    if (clr) clr.hidden = !ST.mq.trim();
    const sortb = root.querySelector('[data-c8="msort"]');
    if (sortb) sortb.textContent = sortLabel();
  }
  function paintTest(root, e) {
    const p = ST.probe;
    const m = testCurrent(e);
    const layer = root.querySelector("[data-c75-test]");
    if (layer) layer.classList.toggle("open", p.open);
    const lbody = root.querySelector(".c75-tbody");
    if (lbody) lbody.hidden = !p.open;
    const tb = root.querySelector('[data-c8="ttoggle"]');
    if (tb) { tb.setAttribute("aria-expanded", String(p.open)); tb.textContent = p.open ? "⌄" : "⌃"; }
    const tm = root.querySelector('[data-c8="tpick"] b');
    if (tm) tm.textContent = m || "модель не выбрана";
    const fm = root.querySelector('[data-c8="tpick"] .fam');
    if (fm) fm.textContent = famMark(m);
    const st = root.querySelector("[data-c75-tstate]");
    if (st) {
      st.className = "c75-tstate " + (p.busy ? "busy" : p.out ? p.out.kind : "idle");
      st.innerHTML = testStateHTML();
    }
    const rs = root.querySelector("[data-c75-res]");
    if (rs) rs.innerHTML = resHTML();
    const hist = root.querySelector("[data-c75-hist]");
    if (hist) hist.outerHTML = histHTML();
    const cnt = root.querySelector("[data-c75-count]");
    if (cnt) cnt.textContent = String(p.prompt.length);
    const go = root.querySelector('[data-c8="tgo"]');
    if (go) { go.disabled = p.busy || !p.prompt.trim() || !m; go.textContent = p.busy ? "…" : "отправить"; }
    const clear = root.querySelector('[data-c8="tclear"]');
    if (clear) clear.disabled = !p.hist.length;
    const hb = root.querySelector('[data-c8="thist"]');
    if (hb) hb.textContent = "история " + p.hist.length;
    root.querySelectorAll("[data-c75-mr]").forEach(row => row.classList.toggle("armed", row.getAttribute("data-c75-mr") === m));
  }
  function paintPick(root, e) {
    const layer = root.querySelector("[data-c75-test]");
    if (!layer) return;
    let el = layer.querySelector("[data-c75-pick]");
    if (!ST.probe.pick) { if (el) el.remove(); return; }
    if (!el) {
      el = document.createElement("div");
      layer.insertBefore(el, layer.firstChild);
    }
    el.outerHTML = pickHTML(e);
    const inp = layer.querySelector("[data-c8-pick]");
    if (inp) { try { inp.focus({ preventScroll: true }); } catch (x) { try { inp.focus(); } catch (x2) {} } }
  }
  function paintPickList(root, e) {
    const list = root.querySelector("[data-c75-picklist]");
    if (list) list.innerHTML = pickListHTML(e);
    const ph = root.querySelector(".c75-ph span");
    if (ph) {
      const q = ST.probe.pickQ.trim().toLowerCase();
      ph.textContent = String((e.models || []).filter(m => !q || m.toLowerCase().indexOf(q) >= 0).length);
    }
  }

  /* ---------- дропдаун: открытие, выбор, клавиатура ---------- */
  function closeDD(d) {
    if (!d) return;
    const back = d.contains(document.activeElement) && document.activeElement !== d;
    d.classList.remove("open");
    d.setAttribute("aria-expanded", "false");
    const m = d.querySelector(".c8-dd-m");
    if (m) { m.hidden = true; m.style.left = m.style.top = m.style.width = ""; }
    d.querySelectorAll(".c8-dd-o.cur").forEach(x => x.classList.remove("cur"));
    if (back) { try { d.focus({ preventScroll: true }); } catch (x) {} }
  }
  function openDD(d) {
    if (!d || d.hasAttribute("data-dis")) return;
    document.querySelectorAll("#provDetail .c8-dd.open").forEach(x => { if (x !== d) closeDD(x); });
    const m = d.querySelector(".c8-dd-m");
    if (!m) return;
    d.classList.add("open"); d.setAttribute("aria-expanded", "true");
    m.hidden = false;
    const r = d.getBoundingClientRect();
    m.style.width = Math.max(96, Math.round(r.width)) + "px";
    const mh = Math.min(240, m.offsetHeight || 160);
    const up = r.bottom + 4 + mh > innerHeight - 6 && r.top > mh + 6;
    m.style.left = Math.round(r.left) + "px";
    m.style.top = Math.round(up ? r.top - mh - 4 : r.bottom + 4) + "px";
    const sel = m.querySelector(".c8-dd-o.sel") || m.querySelector(".c8-dd-o");
    if (sel) { sel.classList.add("cur"); try { sel.scrollIntoView({ block: "nearest" }); } catch (x) {} }
  }
  function ddPick(d, v) {
    const e = S.prov;
    if (!e || !d) return;
    const kind = d.getAttribute("data-kind");
    if (kind === "field") storeField(e, d.getAttribute("data-f"), v);
    let lab = null;
    d.querySelectorAll(".c8-dd-o").forEach(o => {
      const on = o.getAttribute("data-v") === v;
      o.classList.toggle("sel", on);
      o.setAttribute("aria-selected", String(on));
      o.classList.remove("cur");
      if (on) lab = o.textContent;
    });
    const pv = d.querySelector(".c8-dd-v");
    if (pv && lab != null) pv.textContent = lab;
    closeDD(d);
  }
  function go(i) {
    ST.screen = ((i % SCREENS.length) + SCREENS.length) % SCREENS.length;
    const root = document.querySelector("#provDetail [data-c8card]");
    if (!root) return;
    root.querySelectorAll("[data-c8-scr]").forEach(s => {
      const on = +s.getAttribute("data-c8-scr") === ST.screen;
      s.classList.toggle("on", on);
      if (on) {
        s.scrollTop = 0;
        s.querySelectorAll(".c75-kbody,.c75-mbody").forEach(b => { b.scrollTop = 0; });
      }
    });
    root.querySelectorAll("[data-c8='nav']").forEach(b => b.setAttribute("aria-selected", String(+b.getAttribute("data-sc") === ST.screen)));
  }
  function syncModels(e) {
    if (!liveCore()) { toast("демо: каталог моделей не опрашивается", "warn"); return; }
    toast("Синхронизация «" + e.name + "» из /models…", "ok");
    try {
      API.SyncModels(e.name).then(r => {
        toast(r && r.message ? r.message : "синхронизировано", r && r.ok ? "ok" : "err");
        try { loadProviders(); } catch (x) {}
      }).catch(x => toast("Синхронизация: " + x, "err"));
    } catch (x) { toast("Синхронизация: " + x, "err"); }
  }
  function runCheck(e) {
    if (!liveCore()) { toast("демо: healthcheck не опрашивает ядро", "warn"); return; }
    toast("healthcheck «" + e.name + "»…", "ok");
    try {
      API.HealthCheck().then(h => {
        S.health = h || [];
        toast("healthcheck: замеры обновлены", "ok");
        render(S.prov, true);
      }).catch(x => toast("healthcheck: " + x, "err"));
    } catch (x) { toast("healthcheck: " + x, "err"); }
  }
  function saveConn(e) {
    if (!liveCore()) { toast("демо: правки живут на экране, в ядро не пишутся", "warn"); return; }
    const patch = { base_url: e.base_url || "", user_agent: e.user_agent || "", egress: e.egress || "", egress_mode: e.egress_mode || "off" };
    if (e.timeout_s) patch.timeout_s = e.timeout_s;
    if (e.auth_mode && e.auth_kind !== "no-auth") patch.auth_mode = e.auth_mode;
    try {
      API.UpdateProvider(e.name, patch).then(r => {
        toast(r && r.message ? r.message : "сохранено", r && r.ok ? "ok" : "err");
        if (r && r.ok) { try { loadProviders(); } catch (x) {} }
      }).catch(x => toast("Сохранение: " + x, "err"));
    } catch (x) { toast("Сохранение: " + x, "err"); }
  }
  function commitAdd() {
    const inp = document.querySelector("[data-c8-add]");
    if (!inp) return;
    const v = inp.value, e = S.prov;
    if (!e) return;
    if (ST.add === "key") addKey(e, v); else addModel(e, v);
    ST.add = "";
    render(e, true);
  }
  function sendProbe(root, e) {
    const m = testCurrent(e);
    const prompt = String(ST.probe.prompt || "").trim();
    if (!m) { toast("Сначала выберите модель", "warn"); return; }
    if (!prompt) { toast("Пустой запрос — нечего отправлять", "warn"); return; }
    if (ST.probe.busy) return;
    ST.probe.busy = true;
    ST.probe.histOpen = false;
    ST.probe.out = { kind: "wait", m: m, dur: null, text: "запрос отправлен…" };
    paintTest(root, e);
    const t0 = performance.now();
    const put = (kind, html, dur) => {
      const ms = dur != null ? dur : Math.round(performance.now() - t0);
      ST.probe.busy = false;
      ST.probe.out = { kind: kind, m: m, prompt: prompt, text: html, dur: ms };
      ST.probe.hist.unshift({ hhmmss: nowHMS(), m: m, prompt: prompt, kind: kind, dur: ms, text: html });
      if (ST.probe.hist.length > 20) ST.probe.hist.length = 20;
      paintTest(root, e);
    };
    const live = liveCore() && typeof API !== "undefined" && typeof API.ChatProbe === "function";
    if (!live) {
      // честная синтетика: локальная пауза, ядро не тронуто
      setTimeout(() => {
        put("demo", '<code>эхо промпта: «' + esc(prompt.slice(0, 110)) + (prompt.length > 110 ? "…" : "") +
          '» · ответ не генерировался, запрос в ядро не уходил</code>');
      }, 340);
      return;
    }
    try {
      Promise.resolve(API.ChatProbe(m, prompt)).then(r => {
        const ok = r && !r.error && (!r.status || +r.status < 400);
        const body = r && (r.text || r.body || r.content);
        const bits = [];
        if (r && r.status) bits.push("http " + r.status);
        if (r && r.latency_ms != null) bits.push(Math.round(r.latency_ms) + " мс");
        if (r && r.error) bits.push(String(r.error));
        const html = (bits.length ? '<span class="c75-meta">' + esc(bits.join(" · ")) + '</span>' : "") +
          '<code>' + esc(String(body || "ответ без тела").slice(0, 600)) + '</code>';
        put(ok ? "ok" : "err", html, r && r.latency_ms != null ? Math.round(r.latency_ms) : null);
      }).catch(x => put("err", esc(String((x && x.message) || x))));
    } catch (x) {
      put("err", esc("сбой вызова: " + x));
    }
  }
  function armTest(root, e, m) {
    if (!m) return;
    ST.probe.model = m;
    ST.probe.pick = false;
    ST.probe.open = true;
    if (!String(ST.probe.prompt || "").trim()) ST.probe.prompt = "Проверка " + m + ": ответь одним словом — сколько будет 2+2?";
    paintTest(root, e);
    paintModels(root, e);
    const ta = root.querySelector("[data-c8-prompt]");
    if (ta) {
      if (!ta.value.trim()) ta.value = ST.probe.prompt;
      const cnt = root.querySelector("[data-c75-count]");
      if (cnt) cnt.textContent = String(ST.probe.prompt.length);
      try { ta.focus({ preventScroll: true }); } catch (x) { try { ta.focus(); } catch (x2) {} }
    }
  }

  /* ---------- проводка ---------- */

  /* ---------- проводка портированной «Связи» ---------- */
  function wire14(root, e) {
    root.addEventListener("click", ev => {
      const t = ev.target;
      if (!t || !t.closest) return;
      const opt = t.closest(".x14-ddo");
      if (opt) {
        ev.preventDefault();
        const w = opt.closest(".x14-dd");
        if (w) { w._ddi = c14opts(w).indexOf(opt); c14ddPick(w); }
        return;
      }
      const dtr = t.closest(".x14-ddb");
      if (dtr) {
        ev.preventDefault();
        const w = dtr.closest(".x14-dd");
        if (w) { if (w.classList.contains("on")) c14ddClose(w, true); else c14ddShow(w); }
        return;
      }
      const chev = t.closest(".x14-ach .chev");
      if (chev) {
        ev.preventDefault();
        const sec = chev.closest("[data-c8-ac]");
        if (sec) c14accToggle(sec.getAttribute("data-c8-ac"));
        return;
      }
      const b = t.closest("[data-c8]");
      const act = b && b.getAttribute("data-c8");
      if (!act) return;
      if (act === "acc") { ev.preventDefault(); c14accToggle(b.getAttribute("data-ac")); return; }
      if (act === "step-" || act === "step+") {
        ev.preventDefault();
        const w = b.closest(".x14-stp");
        const inp = w && w.querySelector(".x14-sti");
        const cur = inp ? parseInt(inp.value, 10) : 0;
        const d = w ? +(w.getAttribute("data-step") || 1) : 1;
        c14stepApply(w, (Number.isFinite(cur) ? cur : 0) + (act === "step+" ? d : -d), root, true);
        return;
      }
    });
    root.addEventListener("input", ev => {
      const t = ev.target;
      if (!t || !t.classList || !t.classList.contains("x14-sti")) return;
      if (String(t.value).trim() === "") return;
      const w = t.closest(".x14-stp");
      if (w) c14stepApply(w, parseInt(t.value, 10), root, false);
    });
    root.addEventListener("blur", ev => {
      const t = ev.target;
      if (!t || !t.classList || !t.classList.contains("x14-sti")) return;
      const w = t.closest(".x14-stp");
      if (w) c14stepApply(w, parseInt(t.value, 10), root, true);
    }, true);
    root.addEventListener("keydown", ev => {
      const t = ev.target;
      if (!t || !t.matches) return;
      const w = t.closest(".x14-dd");
      if (w && w === c14OPEN) {
        const opts = c14opts(w), i = w._ddi == null ? 0 : w._ddi;
        if (ev.key === "ArrowDown") { ev.preventDefault(); c14ddActive(w, i + 1); return; }
        if (ev.key === "ArrowUp") { ev.preventDefault(); c14ddActive(w, i - 1); return; }
        if (ev.key === "Home") { ev.preventDefault(); c14ddActive(w, 0); return; }
        if (ev.key === "End") { ev.preventDefault(); c14ddActive(w, opts.length - 1); return; }
        if (ev.key === "Enter" || ev.key === " ") { ev.preventDefault(); c14ddPick(w); return; }
        if (ev.key === "Escape") { ev.preventDefault(); ev.stopPropagation(); c14ddClose(w, true); return; }
        if (ev.key === "Tab") { c14ddClose(w, false); return; }
      }
      const db = t.closest(".x14-ddb");
      if (db && (ev.key === "Enter" || ev.key === " " || ev.key === "ArrowDown" || ev.key === "ArrowUp")) {
        ev.preventDefault();
        const d = db.closest(".x14-dd");
        c14ddShow(d);
        if (ev.key === "ArrowUp" && d) c14ddActive(d, c14opts(d).length - 1);
      }
    });
  }

  function wire(root, e) {
    wire14(root, e);
    root.addEventListener("click", ev => {
      const t = ev.target;
      if (!t || !t.closest) return;
      const ddHit = t.closest("[data-c8dd]");
      if (ddHit) {
        const opt = t.closest(".c8-dd-o");
        if (opt) { ev.preventDefault(); ddPick(ddHit, opt.getAttribute("data-v")); return; }
        if (t.closest(".c8-dd-m")) return;
        ev.preventDefault();
        if (ddHit.classList.contains("open")) closeDD(ddHit); else openDD(ddHit);
        return;
      }
      const b = t.closest("[data-c8]");
      const act = b && b.getAttribute("data-c8");
      if (!act) return;
      if (act === "nav") { ev.preventDefault(); go(+b.getAttribute("data-sc")); return; }
      if (act === "back") {
        ev.preventDefault();
        try { const m = document.getElementById("prov-meta"); if (m && m._c8saved != null) m.textContent = m._c8saved; } catch (xM) {}
        try { backProv(); } catch (x) {}
        return;
      }
      if (act === "test") {
        ev.preventDefault();
        try { testProvider({ name: e.name, model: testModel(e) }); }
        catch (x) { toast("Тест: " + ((x && x.message) || x), "err"); }
        return;
      }
      if (act === "url" || act === "copyurl") { ev.preventDefault(); try { copyTo(e.base_url || ""); } catch (x) {} return; }
      if (act === "addkey") { ev.preventDefault(); ST.add = "key"; ST.screen = 1; render(e, true); return; }
      if (act === "addmodel") { ev.preventDefault(); ST.add = "model"; ST.screen = 2; render(e, true); return; }
      if (act === "addgo") { ev.preventDefault(); commitAdd(); return; }
      if (act === "addcancel") { ev.preventDefault(); ST.add = ""; render(e, true); return; }
      if (act === "kcopy") {
        ev.preventDefault();
        const k = poolKeys(e)[+b.getAttribute("data-i")];
        if (k) try { copyTo(k.key || k.name || ""); } catch (x) {}
        return;
      }
      if (act === "kexp") {
        ev.preventDefault();
        const i = +b.getAttribute("data-i");
        ST.ko = ST.ko === i ? null : i;
        paintKeys(root, e);
        const row = root.querySelector('[data-c75-kr="' + ST.ko + '"]');
        if (row) row.scrollIntoView({ block: "nearest" });
        return;
      }
      if (act === "ktgl") {
        ev.preventDefault();
        const k = poolKeys(e)[+b.getAttribute("data-i")];
        if (!k) return;
        k.enabled = k.enabled === false;
        toast(k.enabled ? "ключ возвращён в пул" + (isDemo() ? " (демо)" : "") : "ключ выведен из пула" + (isDemo() ? " (демо)" : ""), k.enabled ? "ok" : "warn");
        paintKeys(root, e);
        return;
      }
      if (act === "kdel") { ev.preventDefault(); delKey(e, +b.getAttribute("data-i")); ST.ko = null; render(e, true); return; }
      if (act === "kf") { ev.preventDefault(); ST.kf = b.getAttribute("data-v"); ST.ko = null; paintKeys(root, e); return; }
      if (act === "kclear" || act === "kreset") {
        ev.preventDefault();
        ST.kq = ""; ST.kf = "all"; ST.ko = null;
        const inp = root.querySelector('[data-c8-filter="k"]');
        if (inp) inp.value = "";
        paintKeys(root, e);
        return;
      }
      if (act === "mexp") {
        ev.preventDefault();
        const m = b.getAttribute("data-m");
        ST.mo = ST.mo === m ? null : m;
        paintModels(root, e);
        const row = root.querySelector('[data-c75-mr="' + CSS.escape(m) + '"]');
        if (row) row.scrollIntoView({ block: "nearest" });
        return;
      }
      if (act === "mtgl") { ev.preventDefault(); toggleModel(e, b.getAttribute("data-m"), "off"); paintModels(root, e); return; }
      if (act === "meye") { ev.preventDefault(); toggleModel(e, b.getAttribute("data-m"), "hid"); paintModels(root, e); return; }
      if (act === "mreason") {
        ev.preventDefault();
        const m = b.getAttribute("data-m");
        e.model_reasoning = e.model_reasoning || {};
        e.model_reasoning[m] = b.getAttribute("data-v");
        paintModels(root, e);
        return;
      }
      if (act === "mtest") { ev.preventDefault(); armTest(root, e, b.getAttribute("data-m")); return; }
      if (act === "msync") { ev.preventDefault(); syncModels(e); return; }
      if (act === "msort") {
        ev.preventDefault();
        const i = Math.max(0, SORTS.findIndex(s => s[0] === ST.sort));
        const next = SORTS[(i + 1) % SORTS.length][0];
        ST.sort = next;
        ST.dir = next === "name" ? 1 : -1;
        paintModels(root, e);
        return;
      }
      if (act === "mclear") {
        ev.preventDefault();
        ST.mq = "";
        const inp = root.querySelector('[data-c8-filter="m"]');
        if (inp) inp.value = "";
        paintModels(root, e);
        return;
      }
      if (act === "tpick") {
        ev.preventDefault();
        ST.probe.pick = !ST.probe.pick;
        ST.probe.pickQ = "";
        ST.probe.histOpen = false;
        paintPick(root, e);
        return;
      }
      if (act === "tpickv") {
        ev.preventDefault();
        ST.probe.model = b.getAttribute("data-m");
        ST.probe.pick = false;
        ST.probe.pickQ = "";
        paintPick(root, e);
        paintTest(root, e);
        return;
      }
      if (act === "tgo") { ev.preventDefault(); sendProbe(root, e); return; }
      if (act === "ttoggle") {
        ev.preventDefault();
        ST.probe.open = !ST.probe.open;
        const layer = root.querySelector("[data-c75-test]");
        const body = root.querySelector(".c75-tbody");
        if (layer) layer.classList.toggle("open", ST.probe.open);
        if (body) body.hidden = !ST.probe.open;
        b.setAttribute("aria-expanded", String(ST.probe.open));
        b.textContent = ST.probe.open ? "⌄" : "⌃";
        if (!ST.probe.open) { ST.probe.pick = false; ST.probe.histOpen = false; paintPick(root, e); }
        return;
      }
      if (act === "thist") {
        ev.preventDefault();
        ST.probe.histOpen = !ST.probe.histOpen;
        ST.probe.pick = false;
        paintPick(root, e);
        const layer = root.querySelector("[data-c75-test]");
        let h = root.querySelector("[data-c75-hist]");
        if (ST.probe.histOpen) {
          if (!h && layer) { const d = document.createElement("div"); d.innerHTML = histHTML(); layer.insertBefore(d.firstChild, layer.firstChild); }
        } else if (h) h.remove();
        b.setAttribute("aria-expanded", String(ST.probe.histOpen));
        return;
      }
      if (act === "tclear") {
        ev.preventDefault();
        ST.probe.hist = [];
        paintTest(root, e);
        return;
      }
      if (act === "hload") {
        ev.preventDefault();
        const rec = ST.probe.hist[+b.getAttribute("data-i")];
        if (!rec) return;
        ST.probe.prompt = rec.prompt;
        ST.probe.model = rec.m;
        ST.probe.out = { kind: rec.kind, m: rec.m, prompt: rec.prompt, text: rec.text, dur: rec.dur };
        ST.probe.histOpen = false;
        const h = root.querySelector("[data-c75-hist]");
        if (h) h.remove();
        const ta = root.querySelector("[data-c8-prompt]");
        if (ta) { ta.value = rec.prompt; try { ta.focus({ preventScroll: true }); } catch (x) {} }
        const cnt = root.querySelector("[data-c75-count]");
        if (cnt) cnt.textContent = String(rec.prompt.length);
        paintTest(root, e);
        return;
      }
      if (act === "check") { ev.preventDefault(); runCheck(e); return; }
      if (act === "save") { ev.preventDefault(); saveConn(e); return; }
      if (act === "step") {
        ev.preventDefault();
        const box = b.closest(".c8-step"), inp = box && box.querySelector("input");
        if (!inp) return;
        const step = +b.getAttribute("data-step") || 1, mn = +b.getAttribute("data-min"), mx = +b.getAttribute("data-max");
        let nv = (parseFloat(inp.value) || 0) + (+b.getAttribute("data-d") || 0) * step;
        if (Number.isFinite(mn)) nv = Math.max(mn, nv);
        if (Number.isFinite(mx)) nv = Math.min(mx, nv);
        nv = Math.round(nv * 100) / 100;
        inp.value = String(nv);
        inp.dispatchEvent(new Event("input", { bubbles: true }));
        return;
      }
    });

    root.addEventListener("input", ev => {
      const t = ev.target;
      if (!t || !t.getAttribute) return;
      const flt = t.getAttribute("data-c8-filter");
      if (flt === "k") {
        ST.kq = t.value; ST.ko = null; paintKeys(root, e);
        return;
      }
      if (flt === "m") {
        ST.mq = t.value; ST.mo = null; paintModels(root, e);
        return;
      }
      if (t.hasAttribute("data-c8-prompt")) {
        ST.probe.prompt = t.value;
        const cnt = root.querySelector("[data-c75-count]");
        if (cnt) cnt.textContent = String(t.value.length);
        const go = root.querySelector('[data-c8="tgo"]');
        if (go) go.disabled = ST.probe.busy || !t.value.trim() || !testCurrent(e);
        t.style.height = "auto";
        t.style.height = Math.min(64, Math.max(24, t.scrollHeight)) + "px";
        return;
      }
      if (t.hasAttribute("data-c8-pick")) { ST.probe.pickQ = t.value; paintPickList(root, e); return; }
      const al = t.getAttribute("data-c8-alias");
      if (al != null) { const k = poolKeys(e)[+al]; if (k) k.name = t.value; return; }
      const tf = t.getAttribute("data-c8-tf");
      if (tf) {
        let n0 = parseFloat(t.value);
        if (Number.isFinite(n0)) {
          const mn = parseFloat(t.getAttribute("min")), mx = parseFloat(t.getAttribute("max"));
          if (Number.isFinite(mn)) n0 = Math.max(mn, n0);
          if (Number.isFinite(mx)) n0 = Math.min(mx, n0);
          e.transport = e.transport || {};
          e.transport[tf] = Math.round(n0 * 100) / 100;
        }
        const calc = root.querySelector("[data-c8-calc='attempts']");
        if (calc) { const tr = e.transport || {}; calc.value = String((+tr.fanout || 0) * (+tr.rounds || 0)); }
        return;
      }
      const f = t.getAttribute("data-c8-f");
      if (f) storeField(e, f, t.value);
    });

    root.addEventListener("change", ev => {
      const t = ev.target;
      if (!t || !t.getAttribute) return;
      const f = t.getAttribute("data-c8-f");
      if (f) storeField(e, f, t.value);
    });


    root.addEventListener("keydown", ev => {
      const t = ev.target;
      if (!t || !t.matches) return;
      const d = t.closest ? t.closest("[data-c8dd]") : null;
      if (d) {
        const opts = Array.from(d.querySelectorAll(".c8-dd-o"));
        const open = d.classList.contains("open");
        const cur = d.querySelector(".c8-dd-o.cur");
        if (ev.key === "Escape") {
          if (open) { ev.preventDefault(); ev.stopPropagation(); closeDD(d); }
          return;
        }
        if (ev.key === "Tab") { closeDD(d); return; }
        if (ev.key === "ArrowDown" || ev.key === "ArrowUp") {
          ev.preventDefault();
          if (!open) { openDD(d); return; }
          let i = cur ? opts.indexOf(cur) : -1;
          i = ev.key === "ArrowDown" ? Math.min(opts.length - 1, i + 1) : Math.max(0, i - 1);
          if (cur) cur.classList.remove("cur");
          if (opts[i]) { opts[i].classList.add("cur"); try { opts[i].scrollIntoView({ block: "nearest" }); } catch (x) {} }
          return;
        }
        if (ev.key === "Enter" || ev.key === " ") {
          ev.preventDefault();
          if (!open) { openDD(d); return; }
          const o = cur || d.querySelector(".c8-dd-o.sel") || opts[0];
          if (o) ddPick(d, o.getAttribute("data-v"));
          return;
        }
        return;
      }
      if (t.matches("[data-c8-pick]")) {
        const opts = Array.from(root.querySelectorAll("[data-c75-picklist] .c75-po"));
        let cur = root.querySelector("[data-c75-picklist] .c75-po.cur");
        if (ev.key === "ArrowDown" || ev.key === "ArrowUp") {
          ev.preventDefault();
          const i = cur ? opts.indexOf(cur) : (ev.key === "ArrowDown" ? -1 : opts.length);
          const j = ev.key === "ArrowDown" ? Math.min(opts.length - 1, i + 1) : Math.max(0, i - 1);
          if (cur) cur.classList.remove("cur");
          cur = opts[j] || null;
          if (cur) { cur.classList.add("cur"); try { cur.scrollIntoView({ block: "nearest" }); } catch (x) {} }
          return;
        }
        if (ev.key === "Enter") {
          ev.preventDefault();
          const pick = cur || opts[0];
          if (pick) {
            ST.probe.model = pick.getAttribute("data-m");
            ST.probe.pick = false; ST.probe.pickQ = "";
            paintPick(root, e); paintTest(root, e);
          }
          return;
        }
        if (ev.key === "Escape") { ev.preventDefault(); ST.probe.pick = false; paintPick(root, e); return; }
        return;
      }
      if (t.matches("[data-c8-prompt]")) {
        if ((ev.ctrlKey || ev.metaKey) && ev.key === "Enter") { ev.preventDefault(); sendProbe(root, e); return; }
        if (ev.key === "Escape" && (ST.probe.pick || ST.probe.histOpen)) {
          ev.preventDefault();
          ST.probe.pick = false; ST.probe.histOpen = false;
          paintPick(root, e);
          const h = root.querySelector("[data-c75-hist]");
          if (h) h.remove();
          return;
        }
      }
      if (ev.key === "Enter" && t.matches("[data-c8-add]")) { ev.preventDefault(); commitAdd(); return; }
      if (ev.key === "Escape" && t.matches("[data-c8-add]")) { ev.preventDefault(); ST.add = ""; render(e, true); return; }
    });
  }

  /* ---------- меню закрывается вне плашки, при скролле и ресайзе ---------- */
  document.addEventListener("pointerdown", ev => {
    const t = ev.target;
    const inside = t && t.closest ? t.closest("#provDetail [data-c8dd]") : null;
    document.querySelectorAll("#provDetail .c8-dd.open").forEach(d => { if (d !== inside) closeDD(d); });
    const root = document.querySelector("#provDetail [data-c8card]");
    if (!root || !t || !t.closest) return;
    let changed = false;
    if (ST.probe.pick && !t.closest("[data-c75-pick]") && !t.closest('[data-c8="tpick"]')) { ST.probe.pick = false; changed = true; }
    if (ST.probe.histOpen && !t.closest("[data-c75-hist]") && !t.closest('[data-c8="thist"]')) {
      ST.probe.histOpen = false;
      const h = root.querySelector("[data-c75-hist]");
      if (h) h.remove();
    }
    if (changed) paintPick(root, S.prov);
  }, true);
  document.addEventListener("scroll", () => {
    document.querySelectorAll("#provDetail .c8-dd.open").forEach(closeDD);
  }, true);
  window.addEventListener("resize", () => {
    document.querySelectorAll("#provDetail .c8-dd.open").forEach(closeDD);
  });

  /* ---------- ?card=1 ---------- */
  function autoCard() {
    let want = false, scr = -1;
    try {
      const q = new URLSearchParams(location.search);
      want = q.get("card") === "1";
      const v = q.get("scr");
      if (v !== null && v !== "") scr = parseInt(v, 10);
    } catch (x) { return; }
    if (!want) return;
    const applyScr = () => {
      if (!(scr >= 0 && scr <= 3)) return true;
      const b = document.querySelector('#provDetail .c8-nav button[data-sc="' + scr + '"]');
      if (!b) return false;
      b.click();
      return true;
    };
    let tries = 0;
    (function tick() {
      try {
        if (document.querySelector("#provDetail [data-c8card]")) { applyScr(); return; }
        if (typeof S !== "undefined" && S.end && S.end.length) {
          const first = S.end.filter(x => !x.catalog)[0] || S.end[0];
          if (S.tab !== "providers" && typeof show === "function") show("providers");
          if (first && typeof openProv === "function") openProv(first.name);
          if (document.querySelector("#provDetail [data-c8card]")) { applyScr(); return; }
        }
      } catch (x) {}
      if (++tries > 50) return;
      setTimeout(tick, 90);
    })();
  }
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", () => setTimeout(autoCard, 0));
  else setTimeout(autoCard, 0);

  window.M1Card = { render: render, go: go, state: ST };
})();
