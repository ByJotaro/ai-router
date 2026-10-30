/* ================= КОМБО — canvas-редактор каскада =================
   Вкладка «Комбо» в визуальном языке темы «БД» (mono) главной:
   графит, единственный акцент — тёмно-красный, моно-типографика,
   инженерная плотность. Граф каскада рисуется на <canvas>: ноды,
   связи-стрелки (порядок + условие), перетаскивание, выделение,
   добавление/удаление шагов, параметры в инспекторе, симуляция
   прохода и журнал с фактическим треком combo_path.

   Семантика шагов/условий = бэкенд (CallCombo/callComboGraph):
   шаги идут по порядку объявления, условие маршрутизирует по
   классу ошибки предыдущего шага (quota/auth/network/
   gate_terminal/pool_exhausted + «*»), to — только вперёд,
   stop — обрыв, quota-only шаг входит только при классе квота.

   API-мост с панелью (shell.js):
     window.ComboCanvas.open(id) — редактирование пресета
     window.ComboCanvas.openNew() — новый пресет
     window.ComboCanvas.close()
   Цвета/шрифты — из токенов темы (--canvas/--panel-2/--rule/--ink/
   --ink2/--amber/--accent-dim), тема отслеживается по <body data-theme>.
   ================= */
(function () {
  'use strict';

  var KINDS = ['start', 'step', 'condition', 'finish'];
  var CLASSES = ['quota', 'auth', 'network', 'gate_terminal', 'pool_exhausted'];
  var CLASS_LBL = {
    quota: 'квота · 429/5xx', auth: 'авторизация · 401/403',
    network: 'сеть · транспорт', gate_terminal: 'гейт · аккаунт/биллинг',
    pool_exhausted: 'пул · брейкер/исчерпан'
  };
  var CLASS_SHORT = { quota: 'квота', auth: 'авторизация', network: 'сеть', gate_terminal: 'гейт', pool_exhausted: 'пул' };
  var KIND_TITLE = { start: 'старт', step: 'шаг', condition: 'условие', finish: 'финиш' };
  var KIND_HINT = {
    start: 'модель входа · подставляется в тело',
    step: 'провайдер + модель · попытка',
    condition: 'маршрут по классу ошибки',
    finish: 'явный обрыв каскада'
  };
  var NODE_W = { start: 188, step: 208, condition: 200, finish: 152 };
  var NODE_H = { start: 52, step: 58, condition: 54, finish: 44 };
  var GAP = 30;

  var st = {
    open: false, presetId: 0, name: '', model: '',
    nodes: [], sel: null, sim: null, simAt: null, simAll: null,
    logs: [], logFilterCombo: true, logSel: null, logBusy: false, logTimer: null, logSeq: 0
  };

  /* ---------- утилиты ---------- */
  function $(s, r) { return (r || document).querySelector(s); }
  function el(tag, cls, txt) { var e = document.createElement(tag); if (cls) e.className = cls; if (txt != null) e.textContent = txt; return e; }
  function esc(s) { return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) { return ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' })[c]; }); }
  function uid() { return 'n' + Math.random().toString(36).slice(2, 8); }
  function state() { try { if (typeof S !== 'undefined' && S) return S; } catch (e) {} return (typeof window !== 'undefined' && window.S) || null; }
  function isDemo() { try { if (typeof DEMO !== 'undefined' && DEMO) return true; } catch (e) {} return (typeof window !== 'undefined' && !!window.DEMO); }
  function api() { try { if (typeof API !== 'undefined' && API) return API; } catch (e) {} return (typeof window !== 'undefined' && window.API) || null; }
  function providers() { var s = state(); return (s && s.end) || []; }
  function provModels(name) {
    var p = providers().find(function (x) { return x.name === name; });
    return (p && p.models) || [];
  }
  function toast(msg, kind) { if (window.toast) window.toast(msg, kind === 'err' ? 'err' : 'ok'); }

  /* ---------- модель ---------- */
  function node(kind, extra) {
    var n = { id: uid(), kind: kind };
    if (extra) Object.keys(extra).forEach(function (k) { n[k] = extra[k]; });
    return n;
  }
  function demoNodes() {
    return [
      node('start', { model: 'gpt-4o', y: 0 }),
      node('step', { provider: 'openai', model: 'gpt-4o' }),
      node('step', { provider: 'anthropic', model: 'claude-3-5-sonnet' }),
      node('condition', { on: ['quota'] }),
      node('step', { provider: 'google', model: 'gemini-2.5-pro', q: true }),
      node('condition', { on: ['auth', 'network'], stop: true }),
      node('finish', {})
    ];
  }
  function stepsOf() {
    var steps = [];
    st.nodes.forEach(function (n) {
      var s = { id: n.id, kind: n.kind };
      if (n.kind === 'start') { s.provider = ''; s.model = n.model || ''; }
      if (n.kind === 'step') { s.provider = n.provider || ''; s.model = n.model || ''; if (n.q) s.quota_only = true; }
      if (n.kind === 'condition') { s.on = (n.on && n.on.length) ? n.on.slice() : ['*']; if (n.stop) s.stop = true; if (n.to) s.to = n.to; }
      if (n.kind === 'finish') { s.provider = ''; s.model = ''; }
      steps.push(s);
    });
    return steps;
  }
  function nodesFromPreset(p) {
    var nodes = [];
    (p.steps || []).forEach(function (s, i) {
      var kind = s.kind || 'step';
      var n = { id: s.id || ('n' + (i + 1)), kind: kind };
      if (kind === 'start') n.model = s.model || '';
      if (kind === 'step') { n.provider = s.provider || ''; n.model = s.model || ''; n.q = !!(s.quota_only || (s.params && s.params.quota_only)); }
      if (kind === 'condition') {
        n.on = (s.on && s.on.length) ? s.on.slice() : (s.params && s.params.quota_only ? ['quota'] : ['*']);
        n.stop = !!s.stop; n.to = s.to || '';
      }
      nodes.push(n);
    });
    if (!nodes.length) nodes = demoNodes();
    return nodes;
  }
  function titleOf(n) {
    if (!n) return '';
    if (n.kind === 'step') return (n.provider || '—') + (n.model ? ' / ' + n.model : '');
    if (n.kind === 'start') return 'старт · ' + (n.model || 'модель входа');
    if (n.kind === 'condition') return 'условие · ' + ((n.on || ['*']).join('/'));
    if (n.kind === 'finish') return 'финиш';
    return n.kind;
  }
  function sizeOf(n) { return { w: NODE_W[n.kind] || 200, h: NODE_H[n.kind] || 52 }; }
  function bounds() {
    if (!st.nodes.length) return { x: 0, y: 0, w: 1, h: 1 };
    var x0 = Infinity, y0 = Infinity, x1 = -Infinity, y1 = -Infinity;
    st.nodes.forEach(function (n) {
      var s = sizeOf(n);
      x0 = Math.min(x0, n.x); y0 = Math.min(y0, n.y);
      x1 = Math.max(x1, n.x + s.w); y1 = Math.max(y1, n.y + s.h);
    });
    return { x: x0, y: y0, w: Math.max(1, x1 - x0), h: Math.max(1, y1 - y0) };
  }
  // вертикальный поток: порядок нод = порядок попыток
  function autoLayout() {
    var y = 26;
    st.nodes.forEach(function (n) {
      var s = sizeOf(n);
      n.w = s.w; n.h = s.h;
      n.x = 150; n.y = y;
      y += s.h + GAP;
    });
  }
  function placeAt(kind, wx, wy) {
    var s = sizeOf(kind);
    return { id: uid(), kind: kind, x: Math.max(6, wx - s.w / 2), y: Math.max(6, wy - s.h / 2), w: s.w, h: s.h };
  }
  function normalize(n) { var s = sizeOf(n); n.w = s.w; n.h = s.h; if (typeof n.x !== 'number' || typeof n.y !== 'number') { n.x = 150; n.y = 26; } return n; }

  /* ---------- граф: рёбра = функция порядка и to ---------- */
  function deriveEdges() {
    var out = [];
    for (var i = 0; i < st.nodes.length - 1; i++) {
      var a = st.nodes[i];
      if (a.kind === 'finish') break;
      var nxt = st.nodes[i + 1];
      if (a.kind === 'condition' && a.to && st.nodes.some(function (m) { return m.id === a.to; })) out.push({ from: a.id, to: a.to, cond: true });
      else out.push({ from: a.id, to: nxt.id });
    }
    return out;
  }

  /* ---------- симуляция (зеркалит callComboGraph 1:1) ---------- */
  function classNames(cls) {
    if (cls === 'gate_terminal') return ['gate_terminal', 'auth'];
    if (cls === 'pool_exhausted') return ['pool_exhausted', 'quota'];
    return [cls];
  }
  function classMatch(on, cls) {
    if (!on || !on.length) return false;
    var got = classNames(cls);
    return on.some(function (w) {
      var x = String(w == null ? '' : w).toLowerCase().trim();
      if (!x || x === '*' || x === 'any') return true;
      return got.indexOf(x) >= 0;
    });
  }
  function stepOn(n) { return n.kind === 'step' && n.q ? ['quota'] : []; }
  function runFailAt(failIdx, failCls) {
    var path = [], tl = [], winner = null, lastErr = null, stopped = false, skipped = [], ms = 0, visits = 0;
    var maxVisits = 8 * st.nodes.length + 8, pos = failIdx;
    var first = st.nodes[failIdx];
    if (first) { path.push(first.id); lastErr = failCls; tl.push({ cls: 'fail', s: 'шаг «' + titleOf(first) + '» — отказ (' + (CLASS_SHORT[failCls] || failCls) + ')' }); }
    pos = failIdx + 1;
    while (pos < st.nodes.length) {
      if (++visits > maxVisits) { tl.push({ cls: 'stop', s: 'граф зациклен — прервано после ' + maxVisits + ' посещений' }); break; }
      var n = st.nodes[pos], idx = pos; pos++;
      if (n.kind === 'start') { path.push(n.id); tl.push({ cls: '', s: 'старт · вход ' + (n.model || '—') }); continue; }
      if (n.kind === 'finish') { path.push(n.id); tl.push({ cls: 'stop', s: 'финиш: каскад завершён — вердикт = ошибка последнего шага' + (lastErr ? ' (' + lastErr + ')' : '') }); break; }
      if (n.kind === 'condition') {
        path.push(n.id);
        if (lastErr === null) { tl.push({ cls: '', s: 'условие пропущено: отказов в цепочке нет' }); continue; }
        if (!classMatch(n.on, lastErr)) { tl.push({ cls: '', s: 'условие не совпало (' + lastErr + ') → дальше по порядку' }); continue; }
        if (n.stop) { stopped = true; tl.push({ cls: 'stop', s: 'условие совпало (' + lastErr + ') → СТОП каскада' }); break; }
        if (n.to) {
          var ti = st.nodes.findIndex(function (m) { return m.id === n.to; });
          if (ti < 0) tl.push({ cls: '', s: 'условие: цель «' + n.to + '» висячая → дальше по порядку' });
          else if (ti <= idx) tl.push({ cls: '', s: 'условие: цель «' + n.to + '» назад (цикл) → дальше по порядку' });
          else { tl.push({ cls: '', s: 'условие совпало (' + lastErr + ') → прыжок на «' + titleOf(st.nodes[ti]) + '»' }); pos = ti; }
          continue;
        }
        tl.push({ cls: '', s: 'условие совпало (' + lastErr + ') → дальше по порядку' });
        continue;
      }
      var on = stepOn(n);
      if (lastErr !== null && on.length && !classMatch(on, lastErr)) {
        path.push(n.id); skipped.push(n.id);
        tl.push({ cls: 'skip', s: 'шаг «' + titleOf(n) + '» пропущен: on:[' + on.join(',') + '] ≠ ' + lastErr });
        continue;
      }
      var dt = Math.round(140 + Math.random() * 860); ms += dt;
      path.push(n.id);
      tl.push({ cls: 'ok', s: 'шаг «' + titleOf(n) + '» — ' + (lastErr === null ? 'успех' : 'замена после отказа (' + lastErr + ')'), ms: dt });
      winner = n.id; lastErr = null;
      break;
    }
    var verdict;
    if (winner) verdict = 'проход завершён: победитель «' + titleOf(st.nodes.find(function (n) { return n.id === winner; })) + '» · итого ' + ms + ' мс';
    else if (stopped) verdict = 'СТОП по условию · победителя нет · ошибка клиенту (' + (lastErr || failCls) + ')';
    else if (skipped.length) verdict = 'ТУПИК: оставшиеся шаги пропущены по on-гейту';
    else verdict = 'финиш: победителя нет · ошибка последнего шага (' + (lastErr || '—') + ')';
    return { path: path, tl: tl, winner: winner, skipped: skipped, cls: failCls, ms: ms, stopped: stopped, verdict: verdict };
  }
  function firstStepIdx() { for (var i = 0; i < st.nodes.length; i++) if (st.nodes[i].kind === 'step') return i; return -1; }
  function simulate(cls) { return runFailAt(firstStepIdx(), cls); }
  function simulateAt(stepIdx, cls) {
    var s = runFailAt(stepIdx, cls);
    var notes = [];
    if (s.skipped.length) notes.push('пропуск quota-only: ' + s.skipped.length);
    if (s.winner) return { txt: '→ ' + titleOf(st.nodes.find(function (n) { return n.id === s.winner; })), cls: 'go', note: notes.join(' · ') || 'успех замены', sim: s };
    if (s.stopped) return { txt: '■ стоп', cls: 'stop', note: (notes.length ? notes.join(' · ') + ' · ' : '') + 'условие обрывает', sim: s };
    return { txt: '▸ финиш', cls: 'stop', note: (notes.length ? notes.join(' · ') + ' · ' : '') + 'победителя нет', sim: s };
  }
  function runAll() {
    var res = { go: 0, stop: 0 };
    st.nodes.forEach(function (n, idx) {
      if (n.kind !== 'step') return;
      CLASSES.forEach(function (c) { if (simulateAt(idx, c).cls === 'go') res.go++; else res.stop++; });
    });
    st.simAll = res;
    return res;
  }

  /* ================= канвас ================= */
  var cv = null, ctx = null, stage = null, cam = { k: 1, tx: 0, ty: 0 };
  var P = null, ro = null, pat = null;
  var FALLBACK = {
    canvas: '#121214', panel: '#1a1a1e', 'panel-2': '#212125', rule: '#303036',
    ink: '#f7f7f7', ink2: '#b5b5b5', amber: '#c22a31', 'accent-dim': '#330d10', chart: '#c22a31'
  };
  var MONO = "'JetBrains Mono','Cascadia Mono','SF Mono',Consolas,monospace";

  function rgba(v, a) {
    if (!v) return 'rgba(0,0,0,' + (a == null ? 1 : a) + ')';
    v = String(v).trim();
    if (v[0] === '#') {
      var h = v.slice(1);
      if (h.length === 3) h = h[0] + h[0] + h[1] + h[1] + h[2] + h[2];
      var n = parseInt(h, 16);
      return 'rgba(' + ((n >> 16) & 255) + ',' + ((n >> 8) & 255) + ',' + (n & 255) + ',' + (a == null ? 1 : a) + ')';
    }
    if (v.indexOf('rgb') === 0) {
      var m = v.match(/[\d.]+/g);
      if (m && m.length >= 3) return 'rgba(' + m[0] + ',' + m[1] + ',' + m[2] + ',' + (a == null ? (m[3] == null ? 1 : m[3]) : a) + ')';
    }
    return v;
  }
  function resolvePalette() {
    var body = document.body, cs;
    try { cs = getComputedStyle(body); } catch (e) { cs = null; }
    function g(name, fb) { var v = cs && cs.getPropertyValue(name); v = v && v.trim(); return v || fb; }
    var raw = {
      canvas: g('--canvas', FALLBACK.canvas),
      panel2: g('--panel-2', FALLBACK['panel-2']),
      ink: g('--ink', FALLBACK.ink), ink2: g('--ink2', FALLBACK.ink2),
      amber: g('--amber', FALLBACK.amber),
      accentDim: g('--accent-dim', FALLBACK['accent-dim']),
      font: (cs && cs.getPropertyValue('--mono') || '').trim() || MONO
    };
    P = {
      bg: rgba(raw.canvas), hatch: rgba(raw.ink, .045),
      node: rgba(raw.panel2), nodeEdgeHot: rgba(raw.ink2),
      ink: rgba(raw.ink), ink2: rgba(raw.ink2), faint: rgba(raw.ink2, .55),
      amber: rgba(raw.amber), accentDim: rgba(raw.accentDim),
      edge: rgba(raw.ink2, .8),
      fontLbl: '600 9px ' + raw.font,
      fontB: 'bold 11.5px ' + raw.font, fontS: '10px ' + raw.font
    };
    pat = null;
  }
  function setCam(k, tx, ty) {
    cam.k = Math.max(.3, Math.min(2.2, k)); cam.tx = tx; cam.ty = ty;
  }
  function worldPt(cx, cy) {
    var r = cv.getBoundingClientRect();
    return { x: (cx - r.left - cam.tx) / cam.k, y: (cy - r.top - cam.ty) / cam.k };
  }
  function nodeAt(w) {
    for (var i = st.nodes.length - 1; i >= 0; i--) {
      var n = st.nodes[i];
      if (w.x >= n.x && w.x <= n.x + n.w && w.y >= n.y && w.y <= n.y + n.h) return n;
    }
    return null;
  }
  function fit(pad) {
    var r = cv.getBoundingClientRect(); if (!r.width || !r.height) return;
    var b = bounds();
    var k = Math.min((r.width - (pad || 56)) / b.w, (r.height - (pad || 56)) / b.h);
    k = Math.max(.3, Math.min(1.15, k));
    setCam(k, (r.width - b.w * k) / 2 - b.x * k, (r.height - b.h * k) / 2 - b.y * k);
    draw(); zoomLbl();
  }
  function zoomAt(f, cx, cy) {
    var r = cv.getBoundingClientRect();
    var wx = (cx - r.left - cam.tx) / cam.k, wy = (cy - r.top - cam.ty) / cam.k;
    var k = Math.max(.3, Math.min(2.2, cam.k * f));
    setCam(k, cx - r.left - wx * k, cy - r.top - wy * k);
    draw(); zoomLbl();
  }
  function zoomLbl() { var z = $('#cbc-zoom'); if (z) z.textContent = Math.round(cam.k * 100) + '%'; }
  function zoomCenter(f) {
    if (!cv) return;
    var r = cv.getBoundingClientRect();
    zoomAt(f, r.left + r.width / 2, r.top + r.height / 2);
  }
  function makePattern() {
    var pc = document.createElement('canvas'); pc.width = pc.height = 26;
    var p = pc.getContext('2d');
    p.strokeStyle = P.hatch; p.lineWidth = 1;
    p.beginPath(); p.moveTo(0, 26.5); p.lineTo(26.5, 0); p.stroke();
    pat = ctx.createPattern(pc, 'repeat');
  }
  function resize() {
    if (!cv || !stage) return;
    var r = stage.getBoundingClientRect();
    var dpr = Math.min(2.5, window.devicePixelRatio || 1);
    cv.width = Math.max(2, Math.round(r.width * dpr));
    cv.height = Math.max(2, Math.round(r.height * dpr));
    cv.style.width = r.width + 'px'; cv.style.height = r.height + 'px';
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    pat = null;
    draw();
  }
  function trunc(txt, maxW) {
    if (ctx.measureText(txt).width <= maxW) return txt;
    var s = txt;
    while (s.length > 1 && ctx.measureText(s + '…').width > maxW) s = s.slice(0, -1);
    return s + '…';
  }
  function arrow(x, y, dx, dy, col) {
    var L = 7, W = 3.2;
    ctx.save(); ctx.translate(x, y); ctx.rotate(Math.atan2(dy, dx));
    ctx.fillStyle = col; ctx.beginPath();
    ctx.moveTo(0, 0); ctx.lineTo(-L, -W); ctx.lineTo(-L, W); ctx.closePath(); ctx.fill();
    ctx.restore();
  }
  function bez(p1, c1, c2, p2, col, w, dash) {
    ctx.save();
    ctx.strokeStyle = col; ctx.lineWidth = w;
    if (dash) ctx.setLineDash(dash);
    ctx.beginPath(); ctx.moveTo(p1.x, p1.y); ctx.bezierCurveTo(c1.x, c1.y, c2.x, c2.y, p2.x, p2.y); ctx.stroke();
    ctx.restore();
    var t = .92, mt = 1 - t;
    var px = mt * mt * mt * p1.x + 3 * mt * mt * t * c1.x + 3 * mt * t * t * c2.x + t * t * t * p2.x;
    var py = mt * mt * mt * p1.y + 3 * mt * mt * t * c1.y + 3 * mt * t * t * c2.y + t * t * t * p2.y;
    arrow(p2.x, p2.y, p2.x - px, p2.y - py, col);
  }
  function draw() {
    if (!ctx || !P) return;
    var r = cv.getBoundingClientRect();
    ctx.clearRect(0, 0, r.width, r.height);
    ctx.fillStyle = P.bg; ctx.fillRect(0, 0, r.width, r.height);
    if (!pat) makePattern();
    if (pat) { ctx.fillStyle = pat; ctx.fillRect(0, 0, r.width, r.height); }
    ctx.save();
    ctx.translate(cam.tx, cam.ty); ctx.scale(cam.k, cam.k);
    var sim = st.sim, edges = deriveEdges();
    // рёбра
    edges.forEach(function (e) {
      var a = st.nodes.find(function (n) { return n.id === e.from; });
      var b = st.nodes.find(function (n) { return n.id === e.to; });
      if (!a || !b) return;
      var on = !sim || (sim.path.indexOf(e.from) >= 0 && sim.path.indexOf(e.to) >= 0);
      var col = e.cond ? (on ? P.amber : rgba(P.amber, .28)) : (on ? P.edge : rgba(P.edge, .22));
      var dash = e.cond ? [5, 4] : null;
      var p1, p2, c1, c2;
      if (e.cond) {
        p1 = { x: a.x + a.w, y: a.y + a.h / 2 };
        p2 = { x: b.x, y: b.y + b.h / 2 };
        var mx = (p1.x + p2.x) / 2;
        c1 = { x: mx + 26, y: p1.y }; c2 = { x: mx - 26, y: p2.y };
      } else {
        p1 = { x: a.x + a.w / 2, y: a.y + a.h };
        p2 = { x: b.x + b.w / 2, y: b.y };
        c1 = { x: p1.x, y: p1.y + 26 }; c2 = { x: p2.x, y: p2.y - 26 };
      }
      bez(p1, c1, c2, p2, col, on ? 1.6 : 1, dash);
    });
    // ноды
    st.nodes.forEach(function (n, i) {
      var inPath = !sim || sim.path.indexOf(n.id) >= 0;
      var win = sim && sim.winner === n.id;
      var skip = sim && sim.skipped.indexOf(n.id) >= 0;
      var sel = st.sel === n.id;
      var alpha = inPath ? 1 : .38;
      ctx.save(); ctx.globalAlpha = alpha;
      // тень выделения
      if (sel) {
        ctx.fillStyle = rgba(P.amber, .12);
        rr(n.x - 3.5, n.y - 3.5, n.w + 7, n.h + 7, 3); ctx.fill();
      }
      // корпус
      ctx.fillStyle = win ? P.accentDim : P.node;
      if (n.kind === 'finish') {
        ctx.strokeStyle = win ? P.amber : (sel ? P.amber : P.nodeEdge);
        ctx.setLineDash([4, 3]);
      } else {
        ctx.strokeStyle = win ? P.amber : (sel ? P.amber : (n.kind === 'condition' ? P.amber : P.nodeEdgeHot));
      }
      ctx.lineWidth = win || sel || n.kind === 'condition' ? 1.5 : 1;
      rr(n.x, n.y, n.w, n.h, 3); ctx.fill(); ctx.stroke();
      ctx.setLineDash([]);
      // маркер kind
      var mk = 9, mx0 = n.x + 9, my0 = n.y + 9;
      ctx.strokeStyle = sel || win ? P.amber : P.ink2;
      ctx.fillStyle = sel || win ? P.amber : (n.kind === 'condition' ? P.amber : P.ink);
      ctx.lineWidth = 1;
      ctx.strokeRect(mx0, my0, mk, mk);
      if (n.kind !== 'finish') ctx.fillRect(mx0 + 2, my0 + 2, mk - 4, mk - 4);
      // индекс
      ctx.font = P.fontLbl; ctx.fillStyle = P.ink2; ctx.textBaseline = 'middle';
      ctx.textAlign = 'left';
      ctx.fillText(String(i + 1), n.x + n.w - 14, n.y + 14);
      // kind-ярлык
      var lbl = n.kind === 'start' ? 'СТАРТ' : n.kind === 'step' ? 'ШАГ' : n.kind === 'condition' ? 'УСЛОВИЕ' : 'ФИНИШ';
      ctx.font = P.fontLbl;
      ctx.fillStyle = n.kind === 'condition' ? P.amber : P.faint;
      ctx.fillText(lbl, mx0 + mk + 7, my0 + mk / 2);
      // заголовок
      var main = '', sub = '';
      if (n.kind === 'start') { main = n.model || 'модель входа'; sub = 'входящая модель'; }
      if (n.kind === 'step') { main = n.provider || '—'; sub = (n.q ? 'quota-only · ' : '') + (n.model || 'модель не выбрана'); }
      if (n.kind === 'condition') {
        main = 'on: ' + ((n.on || ['*']).map(function (c) { return CLASS_SHORT[c] || c; }).join(' · ') || 'любой');
        sub = n.stop ? '■ СТОП при совпадении' : (n.to ? 'прыжок на «' + titleOf(st.nodes.find(function (m) { return m.id === n.to; }) || { kind: 'step', provider: '?' }) + '»' : 'дальше по порядку');
      }
      if (n.kind === 'finish') { main = 'каскад завершён'; sub = 'вердикт = ошибка последнего шага'; }
      ctx.font = P.fontB; ctx.fillStyle = P.ink;
      ctx.fillText(trunc(main, n.w - 34), n.x + 12, n.y + n.h / 2 - 6);
      ctx.font = P.fontS; ctx.fillStyle = P.ink2;
      ctx.fillText(trunc(sub, n.w - 34), n.x + 12, n.y + n.h / 2 + 9);
      // знаки симуляции
      if (win) {
        ctx.font = P.fontLbl; ctx.fillStyle = P.amber;
        ctx.fillText('победитель', n.x + n.w - 62, n.y + 14);
      } else if (skip) {
        ctx.font = P.fontLbl; ctx.fillStyle = P.faint;
        ctx.fillText('пропущен', n.x + n.w - 58, n.y + 14);
      }
      ctx.restore();
    });
    ctx.restore();
  }
  function rr(x, y, w, h, r) {
    ctx.beginPath();
    ctx.moveTo(x + r, y);
    ctx.arcTo(x + w, y, x + w, y + h, r);
    ctx.arcTo(x + w, y + h, x, y + h, r);
    ctx.arcTo(x, y + h, x, y, r);
    ctx.arcTo(x, y, x + w, y, r);
    ctx.closePath();
  }

  /* ---------- взаимодействие с канвасом ---------- */
  function wireCanvas() {
    var drag = null;
    cv.addEventListener('pointerdown', function (e) {
      if (e.button === 2) return;
      var w = worldPt(e.clientX, e.clientY);
      var hit = nodeAt(w);
      cv.focus({ preventScroll: true });
      if (hit) {
        st.sel = hit.id; renderInsp(); crumb();
        drag = { mode: 'node', id: hit.id, ox: w.x - hit.x, oy: w.y - hit.y, moved: false };
      } else {
        drag = { mode: 'pan', x: e.clientX, y: e.clientY, tx: cam.tx, ty: cam.ty, moved: false };
      }
      try { cv.setPointerCapture(e.pointerId); } catch (err) {}
      draw();
    });
    cv.addEventListener('pointermove', function (e) {
      if (!drag) return;
      if (drag.mode === 'node') {
        var n = st.nodes.find(function (x) { return x.id === drag.id; });
        if (!n) return;
        var w = worldPt(e.clientX, e.clientY);
        n.x = w.x - drag.ox; n.y = w.y - drag.oy;
        drag.moved = true;
      } else {
        cam.tx = drag.tx + (e.clientX - drag.x);
        cam.ty = drag.ty + (e.clientY - drag.y);
        if (Math.abs(e.clientX - drag.x) + Math.abs(e.clientY - drag.y) > 3) drag.moved = true;
      }
      draw();
    });
    var end = function (e) {
      if (!drag) return;
      if (drag.mode === 'pan' && !drag.moved) {
        if (st.sel) { st.sel = null; renderInsp(); crumb(); draw(); }
      }
      drag = null;
      try { if (e && e.pointerId != null) cv.releasePointerCapture(e.pointerId); } catch (err) {}
    };
    cv.addEventListener('pointerup', end);
    cv.addEventListener('pointercancel', end);
    cv.addEventListener('wheel', function (e) {
      e.preventDefault();
      zoomAt(e.deltaY < 0 ? 1.12 : 1 / 1.12, e.clientX, e.clientY);
    }, { passive: false });
    cv.addEventListener('contextmenu', function (e) {
      e.preventDefault();
      var w = worldPt(e.clientX, e.clientY);
      var hit = nodeAt(w);
      if (hit) {
        st.sel = hit.id; renderInsp(); crumb(); draw();
        var i = st.nodes.indexOf(hit);
        openMenu(e.clientX, e.clientY, [
          { label: '+ шаг выше', fn: function () { addNode('step', i); } },
          { label: '+ шаг ниже', fn: function () { addNode('step', i + 1); } },
          '-',
          { label: 'Дублировать', fn: function () { var c = JSON.parse(JSON.stringify(hit)); normalize(c); c.id = uid(); c.x += 26; c.y += 26; st.nodes.splice(i + 1, 0, c); st.sim = null; draw(); renderInsp(); crumb(); } },
          { label: 'Поднять', fn: function () { moveNode(hit.id, -1); } },
          { label: 'Опустить', fn: function () { moveNode(hit.id, 1); } },
          '-',
          { label: 'Удалить', danger: true, kbd: 'Del', fn: function () { removeNode(hit.id); } }
        ]);
        return;
      }
      var at = st.nodes.length;
      for (var k = 0; k < st.nodes.length; k++) {
        if (w.y < st.nodes[k].y + st.nodes[k].h / 2) { at = k; break; }
      }
      var items = [{ label: 'Блок здесь (клик)' }, '-'];
      KINDS.forEach(function (knd) {
        items.push({ label: '+ ' + KIND_TITLE[knd], fn: function () { addNodeAt(knd, at, w.x, w.y); } });
      });
      items.push('-');
      items.push({ label: 'Выстроить по порядку', fn: function () { autoLayout(); fit(); } });
      items.push({ label: 'Демо-граф', fn: function () { st.nodes = demoNodes(); st.sel = null; st.sim = null; autoLayout(); fit(); renderInsp(); crumb(); } });
      openMenu(e.clientX, e.clientY, items);
    });
  }

  /* ---------- разметка панели редактора ---------- */
  function shell() {
    var cats = KINDS.map(function (k) {
      return '<button type="button" class="cbc-kind" data-add="' + k + '" title="Добавить блок: ' + KIND_TITLE[k] + ' — ' + KIND_HINT[k] + '">' +
        '<i></i><span>' + KIND_TITLE[k] + '<small>' + KIND_HINT[k] + '</small></span></button>';
    }).join('');
    var simBtns = CLASSES.map(function (c) {
      return '<button type="button" class="btn sm" data-sim="' + c + '" title="Симулировать отказ первого шага классом «' + CLASS_LBL[c] + '»">' + CLASS_SHORT[c] + '</button>';
    }).join('');
    return '<div class="cbc cbc-full">' +
      '<div class="cbc-bar">' +
      '<div class="cbc-title"><span class="nm" id="cbc-nm"></span><span class="id" id="cbc-id"></span></div>' +
      '<span class="spacer"></span>' +
      '<button type="button" class="btn sm ghost" data-act="add" title="Добавить блок в конец каскада">+ Добавить ▾</button>' +
      '<input class="input" id="cbc-name" placeholder="имя пресета" autocomplete="off" aria-label="Имя пресета">' +
      '<span class="cbc-err" id="cbc-err" aria-live="polite"></span>' +
      '<button type="button" class="btn sm primary" data-act="save" id="cbc-save">Создать</button>' +
      '<button type="button" class="btn sm ghost" data-act="close">Закрыть</button>' +
      '</div>' +
      '<div class="cbc-main cbc-main-full">' +
      '<div class="cbc-pane">' +
      '<div class="cbc-stage" id="cbc-stage">' +
      '<canvas class="cbc-cv" id="cbc-cv" tabindex="0" aria-label="Граф комбо-каскада"></canvas>' +
      '<div class="cbc-crumb" id="cbc-crumb"></div>' +
      '<div class="cbc-hudl">' +
      '<span class="cbc-cls" data-c="quota" title="шаг">шаг</span>' +
      '<span class="cbc-cls" data-c="auth" title="условие">условие</span>' +
      '<span class="cbc-cls" data-c="gate_terminal" title="путь симуляции">путь</span>' +
      '<span class="cbc-cls" data-c="pool_exhausted" title="победитель">победитель</span>' +
      '</div>' +
      '<div class="cbc-hud"><span class="cbc-zoom" id="cbc-zoom">100%</span>' +
      '<button type="button" class="btn sm ghost" data-act="zoom-out" title="Уменьшить (−)">−</button>' +
      '<button type="button" class="btn sm ghost" data-act="zoom-in" title="Увеличить (+)">+</button>' +
      '<button type="button" class="btn sm ghost" data-act="fit" title="Вписать в экран (0)">⤢</button>' +
      '</div></div></div>' +
      '<div class="cbc-pane cbc-insp">' +
      '<div class="cbc-h">Инспектор <span class="cbc-hsub" id="cbc-insp-sub"></span></div>' +
      '<div class="cbc-b" id="cbc-insp"></div>' +
      '</div>' +
      '</div>' +
      '<div class="cbc-bottom">' +
      '<div class="cbc-pane">' +
      '<div class="cbc-h">Симуляция <span class="cbc-hsub">отказ шага → фактический путь</span></div>' +
      '<div class="cbc-b" id="cbc-sim">' +
      '<div class="cbc-simrow">' + simBtns +
      '<button type="button" class="btn sm ghost" data-sim="">сброс</button><span class="spacer"></span>' +
      '<button type="button" class="btn sm" id="cbc-runall">матрица</button>' +
      '<button type="button" class="btn sm" id="cbc-test">протестировать</button></div>' +
      '<div class="cbc-mx" id="cbc-mx"></div>' +
      '<div class="cbc-verdict" id="cbc-verdict"></div>' +
      '<div class="cbc-tl" id="cbc-tl"></div>' +
      '</div></div>' +
      '<div class="cbc-pane">' +
      '<div class="cbc-h">Журнал <span class="cbc-hsub">combo_path</span>' +
      '<label class="cbc-chk" style="margin-left:auto" title="Показывать только комбо-запросы"><input type="checkbox" id="cbc-lcombo" checked><span>только комбо</span></label>' +
      '<button type="button" class="btn sm ghost" data-act="log-refresh" title="Обновить журнал">обновить</button></div>' +
      '<div class="cbc-jbody"><div class="cbc-b tight" id="cbc-logs"></div><div class="cbc-jdet" id="cbc-det"></div></div>' +
      '</div>' +
      '</div></div>';
  }

  function crumb() {
    var c = $('#cbc-crumb'); if (!c) return;
    var steps = st.nodes.filter(function (n) { return n.kind === 'step'; }).length;
    var conds = st.nodes.filter(function (n) { return n.kind === 'condition'; }).length;
    var selN = selNode();
    if (selN) {
      c.classList.add('on');
      var idx = st.nodes.indexOf(selN) + 1;
      c.innerHTML = '<b>' + esc(titleOf(selN)) + '</b> · ' + idx + '/' + st.nodes.length;
    } else {
      c.classList.remove('on');
      c.innerHTML = 'шагов: <b>' + steps + '</b> · условий: <b>' + conds + '</b> · рёбер: <b>' + deriveEdges().length + '</b>';
    }
  }

  /* ---------- инспектор ---------- */
  function selNode() { return st.nodes.find(function (n) { return n.id === st.sel; }) || null; }
  function renderInsp() {
    var box = $('#cbc-insp'); if (!box) return;
    box.innerHTML = '';
    var sub = $('#cbc-insp-sub');
    var n = selNode();
    if (sub) sub.textContent = n ? (KIND_TITLE[n.kind] || '') : 'блок не выбран';
    if (!n) {
      box.appendChild(el('div', 'cbc-hint', 'Выберите блок на холсте — свойства появятся здесь. ПКМ по холсту — добавить блок в место клика, по блоку — действия.'));
    } else {
      var nt = el('div', 'cbc-nodet');
      nt.appendChild(el('span', 'k', KIND_TITLE[n.kind]));
      nt.appendChild(el('span', 'v', titleOf(n)));
      box.appendChild(nt);
      if (n.kind === 'start') {
        var f = el('div', 'cbc-f');
        f.appendChild(el('label', null, 'Модель входа'));
        var inp = el('input', 'input'); inp.value = n.model || '';
        inp.addEventListener('input', function () { n.model = inp.value.trim(); draw(); nt.querySelector('.v').textContent = titleOf(n); });
        f.appendChild(inp); box.appendChild(f);
      }
      if (n.kind === 'step') {
        var f1 = el('div', 'cbc-f'); f1.appendChild(el('label', null, 'Провайдер'));
        var sel = el('select', 'input');
        providers().forEach(function (p) { var o = el('option', null, p.name); o.value = p.name; if (p.name === n.provider) o.selected = true; sel.appendChild(o); });
        sel.addEventListener('change', function () {
          n.provider = sel.value; n.model = (provModels(sel.value)[0]) || '';
          st.sim = null; renderInsp(); crumb(); draw();
        });
        f1.appendChild(sel); box.appendChild(f1);
        var f2 = el('div', 'cbc-f'); f2.appendChild(el('label', null, 'Модель'));
        var ms = el('select', 'input');
        var list = provModels(n.provider);
        if (!list.length) { var o0 = el('option', null, 'любая'); o0.value = ''; ms.appendChild(o0); }
        list.forEach(function (m) { var o = el('option', null, m); o.value = m; if (m === n.model) o.selected = true; ms.appendChild(o); });
        ms.addEventListener('change', function () { n.model = ms.value; draw(); nt.querySelector('.v').textContent = titleOf(n); });
        f2.appendChild(ms); box.appendChild(f2);
        var f3 = el('div', 'cbc-f');
        var lab = el('label', 'cbc-chk'); var cb = el('input'); cb.type = 'checkbox'; cb.checked = !!n.q;
        cb.addEventListener('change', function () { n.q = cb.checked; st.sim = null; renderInsp(); draw(); });
        lab.appendChild(cb); lab.appendChild(el('span', null, 'quota-only: шаг входит только при отказе класса «квота»'));
        f3.appendChild(lab); box.appendChild(f3);
      }
      if (n.kind === 'condition') {
        var f4 = el('div', 'cbc-f'); f4.appendChild(el('label', null, 'Классы ошибки (on)'));
        var pick = el('div', 'cbc-pick');
        CLASSES.concat(['*']).forEach(function (c) {
          var b = el('button', 'btn sm' + ((n.on || []).indexOf(c) >= 0 ? ' primary' : ''), CLASS_SHORT[c] || 'любой');
          b.type = 'button';
          b.addEventListener('click', function () {
            var on = n.on || (n.on = []);
            var i = on.indexOf(c);
            if (i >= 0) { if (!on.length) return; on.splice(i, 1); } else on.push(c);
            if (!on.length) on.push('*');
            st.sim = null; renderInsp(); crumb(); draw();
          });
          pick.appendChild(b);
        });
        f4.appendChild(pick); box.appendChild(f4);
        var f5 = el('div', 'cbc-f'); f5.appendChild(el('label', null, 'Цель (to) — только вперёд'));
        var ts = el('select', 'input');
        var opt0 = el('option', null, '— по порядку —'); opt0.value = ''; if (!n.to) opt0.selected = true; ts.appendChild(opt0);
        var myIdx = st.nodes.indexOf(n);
        st.nodes.forEach(function (m, mi) {
          if (mi <= myIdx || m.kind === 'start') return;
          var o = el('option', null, titleOf(m)); o.value = m.id; if (n.to === m.id) o.selected = true; ts.appendChild(o);
        });
        ts.addEventListener('change', function () { n.to = ts.value; st.sim = null; renderInsp(); crumb(); draw(); });
        f5.appendChild(ts); box.appendChild(f5);
        var f6 = el('div', 'cbc-f');
        var lab2 = el('label', 'cbc-chk'); var cb2 = el('input'); cb2.type = 'checkbox'; cb2.checked = !!n.stop;
        cb2.addEventListener('change', function () { n.stop = cb2.checked; st.sim = null; renderInsp(); draw(); });
        lab2.appendChild(cb2); lab2.appendChild(el('span', null, 'стоп: при совпадении оборвать каскад и отдать ошибку клиенту'));
        f6.appendChild(lab2); box.appendChild(f6);
      }
      if (n.kind === 'finish') box.appendChild(el('div', 'cbc-hint', 'Финиш завершает каскад: если обход дошёл — клиенту возвращается ошибка последнего шага.'));
      var acts = el('div', 'cbc-acts');
      var up = el('button', 'btn sm', '↑ выше'); up.type = 'button';
      up.addEventListener('click', function () { moveNode(n.id, -1); });
      var dn = el('button', 'btn sm', '↓ ниже'); dn.type = 'button';
      dn.addEventListener('click', function () { moveNode(n.id, 1); });
      var dup = el('button', 'btn sm', 'Дублировать'); dup.type = 'button';
      dup.addEventListener('click', function () {
        var i = st.nodes.indexOf(n);
        var c = normalize(JSON.parse(JSON.stringify(n))); c.id = uid(); c.x += 26; c.y += 26;
        st.nodes.splice(i + 1, 0, c); st.sim = null; st.sel = c.id; renderInsp(); crumb(); draw();
      });
      var del = el('button', 'btn sm danger', 'Удалить'); del.type = 'button';
      del.addEventListener('click', function () { removeNode(n.id); });
      acts.appendChild(up); acts.appendChild(dn); acts.appendChild(dup); acts.appendChild(del);
      box.appendChild(acts);
    }
    // пресет
    var steps = st.nodes.filter(function (x) { return x.kind === 'step'; }).length;
    var conds = st.nodes.filter(function (x) { return x.kind === 'condition'; }).length;
    var stats = el('div', 'cbc-stats');
    [['шагов', steps], ['условий', conds], ['посещений', 8 * st.nodes.length + 8]].forEach(function (p) {
      var s = el('div', 'cbc-stat'); s.appendChild(el('b', null, String(p[1]))); s.appendChild(el('span', null, p[0]));
      stats.appendChild(s);
    });
    box.appendChild(stats);
    var mod = el('div', 'cbc-f');
    mod.appendChild(el('label', null, 'Модель пресета (по умолчанию — из старта)'));
    var mi = el('input', 'input'); mi.value = st.model || '';
    mi.addEventListener('input', function () { st.model = mi.value.trim(); });
    mod.appendChild(mi); box.appendChild(mod);
    box.appendChild(el('div', 'cbc-hint', 'Порядок блоков = порядок попыток. Условие маршрутизирует по классу ошибки предыдущего шага.'));
  }

  /* ---------- симуляция: матрица + таймлайн ---------- */
  function renderSim() {
    var mx = $('#cbc-mx'), verdict = $('#cbc-verdict'), tl = $('#cbc-tl');
    if (tl) tl.innerHTML = '';
    if (verdict) verdict.innerHTML = '';
    if (!mx) return;
    mx.innerHTML = '';
    var rows = st.nodes.map(function (n, i) { return { n: n, i: i }; }).filter(function (r) { return r.n.kind === 'step'; });
    if (!rows.length) { mx.appendChild(el('div', 'cbc-hint', 'шагов нет — матрица пуста')); return; }
    mx.style.gridTemplateColumns = 'minmax(92px,auto) repeat(' + CLASSES.length + ',minmax(64px,1fr))';
    mx.appendChild(el('div', 'cbc-mh', 'шаг ↓ / класс →'));
    CLASSES.forEach(function (c) { mx.appendChild(el('div', 'cbc-mh', CLASS_SHORT[c])); });
    rows.forEach(function (r) {
      mx.appendChild(el('div', 'cbc-mr', (r.i + 1) + ' · ' + (r.n.provider || '—')));
      CLASSES.forEach(function (c) {
        var v = simulateAt(r.i, c);
        var on = st.sim && st.simAt && st.simAt.i === r.i && st.simAt.c === c;
        var cell = el('div', 'cbc-mc ' + v.cls + (on ? ' on' : ''), v.txt);
        if (v.note) cell.title = v.note;
        cell.dataset.mx = r.i + ':' + c;
        mx.appendChild(cell);
      });
    });
    if (st.sim) {
      st.sim.tl.forEach(function (l) {
        var ln = el('div', 'l' + (l.cls === 'ok' ? ' ok' : l.cls === 'stop' ? ' stop' : l.cls === 'skip' ? ' skip' : l.cls === 'fail' ? ' stop' : ''));
        ln.appendChild(el('span', null, l.s + (l.ms ? ' · ' + l.ms + ' мс' : '')));
        tl.appendChild(ln);
      });
      verdict.innerHTML = (function () {
        var i = st.sim.verdict.indexOf(':');
        if (i < 0) return '<b>' + esc(st.sim.verdict) + '</b>';
        return '<b>' + esc(st.sim.verdict.slice(0, i)) + '</b>:' + esc(st.sim.verdict.slice(i + 1));
      })();
      verdict.classList.toggle('bad', !st.sim.winner);
    } else if (verdict) {
      verdict.textContent = 'выберите класс отказа — холст покажет фактический путь, клетки матрицы кликабельны';
    }
  }

  /* ---------- журнал запросов ---------- */
  function logRow(r) {
    var d = el('button', 'cbc-lrow' + (st.logSel === r.id ? ' sel' : ''));
    d.type = 'button'; d.dataset.id = r.id;
    var t = (r.ts || '').replace('T', ' ').slice(11, 19);
    d.appendChild(el('span', 't', t || '—'));
    d.appendChild(el('span', 'm', r.combo_name ? r.combo_name : (r.model || '—')));
    d.appendChild(el('span', 'w', r.provider || r.combo_step_id || '—'));
    var s = el('span', 's' + (r.status === 'ok' ? ' ok' : ' err'), String(r.status || '—') + (r.http_status ? ' ' + r.http_status : ''));
    d.appendChild(s);
    d.appendChild(el('span', 'tag', r.duration_ms != null ? r.duration_ms + ' мс' : '—'));
    if (r.combo_name) d.appendChild(el('span', 'tag', 'комбо'));
    if (r.combo_path && r.combo_path.length) d.appendChild(el('span', 'tag path', 'путь ' + r.combo_path.length));
    d.addEventListener('click', function () { selectLog(r); });
    return d;
  }
  function fmtStep(label) {
    var parts = String(label || '').split(':');
    var d = el('span', 'cbc-st');
    if (parts[0]) d.appendChild(el('i', null, parts[0]));
    var who = parts[1] || '';
    var what = parts.slice(2).join(':') || '';
    if (who) d.appendChild(el('b', null, who));
    if (what) {
      var cls = 'w';
      if (/fail/.test(what)) cls = 'fail';
      else if (/skip/.test(what)) cls = 'skip';
      else if (/win/.test(what)) cls = 'win';
      else if (/stop|finish/.test(what)) cls = 'stop';
      else if (/cond/.test(what)) cls = 'cond';
      d.appendChild(el('em', cls, what));
    }
    return d;
  }
  function selectLog(r) {
    st.logSel = r.id;
    var box = $('#cbc-det');
    var rowsEl = $('#cbc-logs');
    if (rowsEl) [].forEach.call(rowsEl.querySelectorAll('.cbc-lrow'), function (x) { x.classList.toggle('sel', +x.dataset.id === r.id); });
    if (!box) return;
    box.innerHTML = '';
    var meta = el('div', 'cbc-detmeta');
    meta.innerHTML = '<span>время <b>' + esc((r.ts || '').replace('T', ' ').slice(0, 19)) + '</b></span>' +
      '<span>rid <b>' + esc(r.rid || '—') + '</b></span>' +
      '<span>модель <b>' + esc(r.model || '—') + '</b></span>' +
      '<span>провайдер <b>' + esc(r.provider || '—') + '</b></span>' +
      '<span>статус <b>' + esc(String(r.status || '—') + (r.http_status ? ' / ' + r.http_status : '')) + '</b></span>' +
      '<span>длит. <b>' + (r.duration_ms || 0) + ' мс</b></span>';
    box.appendChild(meta);
    if (r.combo_name) {
      var cb = el('div', 'cbc-detmeta');
      cb.innerHTML = '<span>комбо <b>' + esc(r.combo_name) + '</b></span>' + (r.combo_step_id ? '<span>победитель <b>' + esc(r.combo_step_id) + '</b></span>' : '');
      box.appendChild(cb);
    }
    var trace = el('div', 'cbc-tr');
    if (r.combo_path && r.combo_path.length) r.combo_path.forEach(function (lbl) { trace.appendChild(fmtStep(lbl)); });
    else if (r.combo_name) trace.appendChild(el('div', 'cbc-hint', 'легаси-линейный каскад: трек обхода не пишется, известен только победитель ' + (r.combo_step_id || '—')));
    else trace.appendChild(el('div', 'cbc-hint', 'не комбо-запрос: цепочка не задействована'));
    box.appendChild(trace);
    if (r.error) {
      var er = el('div', 'cbc-warn'); er.textContent = r.error; box.appendChild(er);
    }
    // подсветить фактический путь на холсте, если это наш пресет
    if (r.combo_name && st.name && r.combo_name === st.name && r.combo_path) { st.sim = pathToSim(r.combo_path); draw(); renderSim(); }
  }
  function pathToSim(path) {
    var ids = [], winner = null, skipped = [];
    path.forEach(function (lbl) {
      var m = /([^:]+):([^:]+):(.+)/.exec(lbl);
      if (!m) return;
      var who = m[2], what = m[3];
      var nd = st.nodes.find(function (n) { return n.id === who; }) || st.nodes.find(function (n) { return (n.provider || '') === who; });
      if (!nd) return;
      if (/win/.test(what)) { winner = nd.id; ids.push(nd.id); }
      else if (/skip/.test(what)) { skipped.push(nd.id); ids.push(nd.id); }
      else if (!/dangling/.test(what)) ids.push(nd.id);
    });
    return { path: ids, tl: [], winner: winner, skipped: skipped, verdict: 'фактический путь из журнала' };
  }
  function refreshLogs() {
    if (!st.open || st.logBusy) return;
    var my = ++st.logSeq;
    var A = api();
    if (isDemo()) {
      st.logBusy = true;
      var all = ((state() || {}).logs) || [];
      var rows0 = st.logFilterCombo ? all.filter(function (r) { return r.combo_name; }) : all;
      st.logs = rows0; st.logBusy = false;
      renderLogs();
      return;
    }
    if (!A || !A.Logs) { renderLogs(); return; }
    st.logBusy = true;
    Promise.resolve(A.Logs(400)).then(function (all) {
      if (my !== st.logSeq || !st.open) return;
      var rows = st.logFilterCombo ? (all || []).filter(function (r) { return r.combo_name; }) : (all || []);
      st.logs = rows;
      renderLogs();
    }).catch(function () { }).finally(function () { st.logBusy = false; });
  }
  function renderLogs() {
    var tbl = $('#cbc-logs'); if (!tbl) return;
    tbl.innerHTML = '';
    if (!st.logs.length) {
      tbl.appendChild(el('div', 'cbc-hint', isDemo() ? 'демо-режим: комбо-запросов в выборке нет' :
        (st.logFilterCombo ? 'комбо-запросов пока нет — снимите фильтр или отправьте запрос' : 'журнал пуст')));
      return;
    }
    st.logs.slice(0, 60).forEach(function (r) { tbl.appendChild(logRow(r)); });
  }

  /* ---------- операции ---------- */
  function addNode(kind, atIdx) {
    var n;
    if (atIdx == null) {
      var last = st.nodes[st.nodes.length - 1];
      var cy = last ? last.y + last.h + GAP + (NODE_H[kind] || 52) / 2 : 60;
      n = placeAt(kind, 150 + (NODE_W[kind] || 200) / 2, cy);
    } else {
      n = placeAt(kind, 150 + (NODE_W[kind] || 200) / 2, 100);
      st.nodes.splice(Math.max(0, Math.min(st.nodes.length, atIdx)), 0, n);
      autoLayout();
    }
    if (kind === 'step') { var ps = providers(); if (ps.length) { n.provider = ps[0].name; n.model = (ps[0].models || [])[0] || ''; } }
    if (kind === 'condition') n.on = ['quota'];
    if (kind === 'start') n.model = (st.nodes[0] && st.nodes[0].model) || 'gpt-4o';
    if (atIdx == null) st.nodes.push(n);
    st.sel = n.id; st.sim = null;
    renderInsp(); renderSim(); crumb(); draw();
    return n;
  }
  function addNodeAt(kind, atIdx, wx, wy) {
    var n = placeAt(kind, wx, wy);
    if (kind === 'step') { var ps = providers(); if (ps.length) { n.provider = ps[0].name; n.model = (ps[0].models || [])[0] || ''; } }
    if (kind === 'condition') n.on = ['quota'];
    if (kind === 'start') n.model = (st.nodes[0] && st.nodes[0].model) || 'gpt-4o';
    st.nodes.splice(Math.max(0, Math.min(st.nodes.length, atIdx)), 0, n);
    st.sel = n.id; st.sim = null;
    renderInsp(); renderSim(); crumb(); draw();
  }
  function removeNode(id) {
    st.nodes = st.nodes.filter(function (x) { return x.id !== id; });
    if (st.sel === id) st.sel = null;
    st.sim = null;
    renderInsp(); renderSim(); crumb(); draw();
  }
  function moveNode(id, dir) {
    var i = st.nodes.findIndex(function (x) { return x.id === id; });
    var j = i + dir;
    if (i < 0 || j < 0 || j >= st.nodes.length) return;
    var t = st.nodes[i]; st.nodes[i] = st.nodes[j]; st.nodes[j] = t;
    st.sim = null;
    renderInsp(); renderSim(); crumb(); draw();
  }

  /* ---------- контекстное меню ---------- */
  var menuEl = null;
  function closeMenu() { if (menuEl) { menuEl.remove(); menuEl = null; } }
  function openMenu(x, y, items) {
    closeMenu();
    var m = el('div', 'cbc-menu');
    items.forEach(function (it) {
      if (it === '-') { m.appendChild(el('div', 'cbc-ms')); return; }
      if (it.label && !it.fn) { m.appendChild(el('div', 'cbc-mt', it.label)); return; }
      var b = el('button', 'cbc-mi' + (it.danger ? ' danger' : ''), it.label);
      b.type = 'button';
      if (it.kbd) { var k = el('kbd'); k.textContent = it.kbd; b.appendChild(k); }
      b.addEventListener('click', function () { closeMenu(); it.fn(); });
      m.appendChild(b);
    });
    document.body.appendChild(m);
    var r = m.getBoundingClientRect();
    m.style.left = Math.min(x, window.innerWidth - r.width - 8) + 'px';
    m.style.top = Math.min(y, window.innerHeight - r.height - 8) + 'px';
    menuEl = m;
  }

  /* ---------- сохранение ---------- */
  function sanitizeForSave() {
    // чистим висячие/назад-ссылки условий — иначе бэкенд отвергнет сохранение
    st.nodes.forEach(function (n, i) {
      if (n.kind !== 'condition' || !n.to) return;
      var ti = st.nodes.findIndex(function (m) { return m.id === n.to; });
      if (ti < 0 || ti <= i || st.nodes[ti].kind === 'start') { n.to = ''; }
    });
  }
  async function save() {
    sanitizeForSave();
    var err = $('#cbc-err');
    var name = (($('#cbc-name') || {}).value || '').trim();
    if (err) err.textContent = name ? '' : 'имя не может быть пустым';
    if (!name) return;
    if (!st.nodes.some(function (n) { return n.kind === 'step' && (n.provider || '').trim(); })) {
      err.textContent = 'нужен хотя бы один шаг с провайдером'; return;
    }
    st.name = name;
    var steps = stepsOf();
    var model = st.model || (st.nodes[0] && st.nodes[0].model) || '';
    var A = api();
    try {
      if (isDemo()) {
        var s0 = state();
        if (st.presetId && s0) {
          var c = (s0.combos || []).find(function (x) { return +x.id === st.presetId; });
          if (c) { c.name = name; c.model = model; c.steps = steps; }
        } else if (s0) {
          var maxId = (s0.combos || []).reduce(function (a, c) { return Math.max(a, +c.id || 0); }, 0);
          s0.combos = [{ id: maxId + 1, name: name, model: model, steps: steps }].concat(s0.combos || []);
          st.presetId = maxId + 1;
        }
        toast('Демо: пресет «' + name + '» сохранён (без БД)', 'ok');
        close(); if (window.renderCombos) window.renderCombos();
        return;
      }
      if (st.presetId) {
        await A.ComboUpdate(st.presetId, name, model, steps);
        toast('Пресет «' + name + '» обновлён', 'ok');
      } else {
        var id = await A.ComboSave(name, model, steps);
        st.presetId = id;
        toast('Комбо «' + name + '» создано', 'ok');
      }
      if (window.loadCombos) window.loadCombos();
      refreshLogs();
    } catch (e) {
      if (err) err.textContent = (e && e.message) || String(e);
      toast('Сохранение: ' + ((e && e.message) || e), 'err');
    }
  }

  /* ---------- тест пресета (реальный пинг → запись в журнал) ---------- */
  async function testPreset() {
    if (isDemo()) { toast('Демо: живой тест выключен — используйте симуляцию', 'err'); return; }
    var A = api();
    var btn = $('#cbc-test');
    if (btn) { btn.disabled = true; btn.textContent = 'проверяю…'; }
    try {
      var steps = stepsOf();
      var model = st.model || (st.nodes[0] && st.nodes[0].model) || '';
      var tr = await A.ComboTest(st.presetId || 0, steps, model);
      if (tr && tr.error) toast('Тест: ' + tr.error, 'err');
      else if (tr) toast('Тест: победитель ' + (tr.combo_step || tr.provider || '—') + ' · ' + (tr.latency_ms || 0) + ' мс', 'ok');
      setTimeout(refreshLogs, 400);
    } catch (e) {
      toast('Тест: ' + ((e && e.message) || e), 'err');
    } finally {
      if (btn) { btn.disabled = false; btn.textContent = 'протестировать'; }
    }
  }

  /* ---------- привязки панели ---------- */
  function bind() {
    var root = $('#combo-editor');
    if (!root) return;
    root.addEventListener('click', function (e) {
      var t = e.target;
      var add = t.closest ? t.closest('[data-add]') : null;
      if (add) { addNode(add.dataset.add); return; }
      var sim = t.closest ? t.closest('[data-sim]') : null;
      if (sim) {
        st.sim = sim.dataset.sim ? simulate(sim.dataset.sim) : null;
        st.simAt = sim.dataset.sim ? { i: firstStepIdx(), c: sim.dataset.sim } : null;
        draw(); renderSim(); crumb();
        return;
      }
      var mx = t.closest ? t.closest('[data-mx]') : null;
      if (mx) {
        var pr = mx.dataset.mx.split(':');
        st.sim = runFailAt(+pr[0], pr[1]);
        st.simAt = { i: +pr[0], c: pr[1] };
        draw(); renderSim();
        return;
      }
      var act = t.closest ? t.closest('[data-act]') : null;
      if (act) {
        var a = act.dataset.act;
        if (a === 'save') { save(); return; }
        if (a === 'close') { close(); return; }
        if (a === 'add') {
          var r = act.getBoundingClientRect();
          openMenu(r.left, r.bottom + 4, KINDS.map(function (k) {
            return { label: '+ ' + KIND_TITLE[k], fn: function () { addNode(k); } };
          }));
          return;
        }
        if (a === 'zoom-in' || a === 'zoom-out') { zoomCenter(a === 'zoom-in' ? 1.18 : 1 / 1.18); return; }
        if (a === 'fit') { fit(); return; }
        if (a === 'log-refresh') { refreshLogs(); return; }
      }
    });
    var lcc = $('#cbc-lcombo');
    if (lcc) lcc.addEventListener('change', function () { st.logFilterCombo = lcc.checked; refreshLogs(); });
    var ra = $('#cbc-runall');
    if (ra) ra.addEventListener('click', function () {
      var r = runAll();
      var v = $('#cbc-verdict');
      if (v) { v.textContent = 'матрица: сценариев ' + (r.go + r.stop) + ' · с победителем ' + r.go + ' · без победителя ' + r.stop; v.classList.remove('bad'); }
      renderSim();
    });
    var tt = $('#cbc-test'); if (tt) tt.addEventListener('click', testPreset);
    var nm = $('#cbc-name'); if (nm) nm.value = st.name || '';
    var nm2 = $('#cbc-nm'), id2 = $('#cbc-id'), sv = $('#cbc-save');
    document.addEventListener('keydown', onKey);
    st.onResize = debounce(resize, 120);
    window.addEventListener('resize', st.onResize);
    if ('ResizeObserver' in window && stage) { ro = new ResizeObserver(debounce(resize, 60)); ro.observe(stage); }
    if ('MutationObserver' in window) {
      st.mo = new MutationObserver(function () { resolvePalette(); pat = null; draw(); });
      st.mo.observe(document.body, { attributes: true, attributeFilter: ['data-theme'] });
    }
    wireCanvas();
    refreshLogs();
    st.logTimer = setInterval(refreshLogs, 15000);
    var det0 = $('#cbc-det');
    if (det0 && !det0.innerHTML) det0.appendChild(el('div', 'cbc-hint', 'выберите строку журнала — покажу фактический трек обхода (combo_path)'));
    // fullscreen: список и топ прячем ПОСЛЕ первого замера холста — иначе
    // ensureFit увидит нулевую геометрию скрытой панели и уйдёт в rAF-цикл.
    function hideList() {
      var listH = document.getElementById('combo-list'), topH = document.querySelector('#panel-combos .combo-top');
      st.hidden = [];
      if (listH) { listH.style.display = 'none'; st.hidden.push(listH); }
      if (topH) { topH.style.display = 'none'; st.hidden.push(topH); }
      resize(); fit(); draw();
    }
    // холст мог смонтироваться в скрытой панели (например ?newcombo=1) — ждём реальных размеров
    (function ensureFit(tries) {
      var r = stage.getBoundingClientRect();
      if (r.width > 4 && r.height > 4) { resize(); fit(); hideList(); renderInsp(); renderSim(); crumb(); draw(); return; }
      if (tries > 0) requestAnimationFrame(function () { ensureFit(tries - 1); });
    })(40);
    // синхронизация тулбара
    if (nm2) nm2.textContent = st.presetId ? 'Редактор: ' + (st.name || 'пресет') : 'Новый пресет';
    if (id2) id2.textContent = st.presetId ? '#' + st.presetId : 'черновик';
    if (sv) sv.textContent = st.presetId ? 'Сохранить' : 'Создать';
  }
  function debounce(fn, ms) { var t; return function () { var a = arguments, c = this; clearTimeout(t); t = setTimeout(function () { fn.apply(c, a); }, ms); }; }
  function onKey(e) {
    if (!st.open) return;
    var t = e.target;
    if (t && (t.tagName === 'INPUT' || t.tagName === 'SELECT' || t.tagName === 'TEXTAREA')) return;
    if (e.key === 'Escape') { if (menuEl) { closeMenu(); return; } if (st.open) { close(); return; } return; }
    if ((e.key === 'Delete' || e.key === 'Backspace') && st.sel) { removeNode(st.sel); e.preventDefault(); return; }
    if (e.key === '+' || e.key === '=') { zoomCenter(1.18); return; }
    if (e.key === '-' || e.key === '_') { zoomCenter(1 / 1.18); return; }
    if (e.key === '0') { fit(); }
  }

  /* ---------- открытие/закрытие ---------- */
  function openEdit(id) {
    var host = $('#combo-editor'); if (!host) return;
    var p = null;
    if (id) { var s0 = state(); p = ((s0 && s0.combos) || []).find(function (x) { return +x.id === +id; }); }
    st.open = true; st.presetId = p ? +p.id : 0; st.name = p ? p.name : '';
    st.nodes = p ? nodesFromPreset(p) : demoNodes();
    // список и топ прячем в hideList() после первого замера холста (bind/ensureFit)
    closeMenu();
    host.innerHTML = shell();
    cv = $('#cbc-cv'); stage = $('#cbc-stage');
    ctx = cv ? cv.getContext('2d') : null;
    resolvePalette();
    autoLayout();
    bind();
    resize();
    try { host.scrollIntoView({ block: 'nearest' }); } catch (e) {}
  }
  function close() {
    // закрытие fullscreen: вернуть список и топ
    (st.hidden || []).forEach(function (h) { h.style.display = ''; });
    st.hidden = [];
    st.open = false; st.sel = null; st.sim = null;
    if (st.logTimer) { clearInterval(st.logTimer); st.logTimer = null; }
    if (ro) { try { ro.disconnect(); } catch (e) {} ro = null; }
    if (st.mo) { try { st.mo.disconnect(); } catch (e) {} st.mo = null; }
    if (st.onResize) { window.removeEventListener('resize', st.onResize); st.onResize = null; }
    closeMenu();
    var host = $('#combo-editor');
    if (host) host.innerHTML = '';
    document.removeEventListener('keydown', onKey);
  }

  window.ComboCanvas = {
    open: openEdit,
    openNew: function () { openEdit(0); },
    close: close,
    fit: fit,
    _st: st
  };
})();
