async function loadProviders() {
  const grid = $("#prov-grid");
  if (DEMO) { $("#prov-loading").style.display = "none"; $("#prov-empty").style.display = "none"; $("#cnt-prov").textContent = (S.end||[]).length; renderProvGrid(); return; }
  const _d = DEMO;
  // Авто/фоновые обновления не должны мигать скелетоном и сбрасывать карточку:
  // скелетон показываем только на первой отрисовке (сетка ещё пустая).
  const first = !grid.innerHTML;
  // Повторный визит: старые карточки остаются видимыми, пока летит свежий запрос (SWR).
  if (first) { $("#prov-loading").style.display = "block"; grid.style.display = "none"; }
  else { $("#prov-loading").style.display = "none"; grid.style.display = "grid"; }
  $("#prov-empty").style.display = "none";
  try {
    const _prs = await Promise.allSettled([
      API.Endpoints(), API.Breakers(), API.Keys(), API.KeysByProvider(),
    ]);
    const _pv = i => (_prs[i] && _prs[i].status === "fulfilled") ? _prs[i].value : null;
    const end = _pv(0) || S.end || [], brk = _pv(1), keys = _pv(2), kbp = _pv(3);
    if (!_pv(0) && !S.end.length) throw new Error("шлюз недоступен — проверьте, что ядро запущено");
    if (DEMO !== _d) return;
    S.end = end || []; if (brk) S.breakers = brk; if (keys) S.keys = keys; if (kbp) S.kbp = kbp;
    S.health = S.health || []; S.logs = S.logs || [];
    $("#cnt-prov").textContent = S.end.length;
  } catch (e) {
    if (DEMO !== _d) return;
    toast("Провайдеры: " + (e && e.message || e), "err");
    $("#prov-loading").style.display = "none";
    if (first) grid.innerHTML = errHint(e);
    grid.style.display = "grid"; $("#prov-empty").style.display = "none";
    return;
  }
  $("#prov-loading").style.display = "none";
  if (first) grid.style.display = "grid";
  renderProvGrid();
  // Спарклайны токенов догоняют фоном (Logs 400 строк): перерисовываем только canvas.
  API.Logs(400).then(lg => {
    if (DEMO !== _d) return;
    S.logs = lg || []; buildSparkMap();
    if (S.tab !== "providers") return; // tab-guard: фон не трогает чужую табу
    if (window.contentScrollBusy) return; // scroll-guard: пока скроллят — drawSparks пропускаем
    drawSparks();
  }).catch(() => {});
  // RTT догоняет фоном: тихий HealthCheck обновляет только чипы латентности, сетку не перерисовываем.
  API.HealthCheck().then(h2 => {
    if (DEMO !== _d) return;
    S.health = h2 || [];
    if (S.tab !== "providers") return; // tab-guard: чипы правит только своя таба
    if (window.contentScrollBusy) return; // scroll-guard: пока скроллят — фон не перерисовываем
    drawSparks();
    (S.end || []).forEach(e => {
      const hc = (S.health || []).find(x => x.name === e.name);
      const lat = (hc && hc.ok && hc.latency_ms != null) ? hc.latency_ms + " мс" : null;
      const card = grid.querySelector('[data-p="' + CSS.escape(e.name) + '"]');
      if (!card) return;
      const chip = card.querySelector('.chip[data-lat]');
      if (chip) {
        // патч текстом без перестройки DOM: чип всегда в сетке, RTT не прыгает; класс порога тоже освежаем
        const lv = (hc && hc.ok && hc.latency_ms != null) ? hc.latency_ms : null;
        const nc = "chip" + (lv == null ? "" : lv <= 500 ? "" : lv <= 1000 ? " warn" : " hot");
        if (chip.className !== nc) chip.className = nc;
        let dot = chip.firstElementChild;
        if (!dot || !dot.classList.contains("dot")) { chip.textContent = ""; dot = document.createElement("span"); dot.className = "dot"; chip.appendChild(dot); }
        let tn = dot.nextSibling;
        if (!tn || tn.nodeType !== 3) { tn = document.createTextNode(""); chip.appendChild(tn); }
        tn.textContent = lat || "—";
      }
    });
  }).catch(() => {});
  // Не сбрасываем открытую деталь: перепривязываем к свежим данным и перерисовываем её (только своя таба).
  if (S.tab === "providers" && S.prov && $("#provDetail").style.display !== "none") {
    const fresh = S.end.find(x => x.name === S.prov.name);
    if (fresh) S.prov = fresh;
    let _pdBusy = false;
    try {
      if (S._pdFlyUntil && Date.now() < S._pdFlyUntil + 200) _pdBusy = true;
      if (!_pdBusy) { const _ap = document.querySelector("#pd-uni .pd-pane.on") || document.getElementById("pd-pane-" + (S._pdTab || "keys")); if (_ap && _ap.scrollHeight > _ap.clientHeight + 4 && _ap.scrollTop > 2 && _ap.scrollTop + _ap.clientHeight < _ap.scrollHeight - 2) _pdBusy = true; }
    } catch (ePB) {}
    if (!_pdBusy) renderProvDetail();
  }
}
function buildSparkMap() {
  const counts = {}; // provider -> {day:count}
  const sums = {};   // provider -> {day:токены}
  (S.logs || []).forEach(l => {
    if (!l.provider) return;
    const day = (l.ts || "").slice(0, 10); if (!day) return;
    counts[l.provider] = counts[l.provider] || {};
    sums[l.provider] = sums[l.provider] || {};
    counts[l.provider][day] = (counts[l.provider][day] || 0) + 1;
    sums[l.provider][day] = (sums[l.provider][day] || 0) + (l.prompt || 0) + (l.completion || 0);
  });
  const out = {}, outTok = {};
  Object.keys(counts).forEach(p => {
    const days = Object.keys(counts[p]).sort();
    const d = days.map(x => counts[p][x]);
    const td = days.map(x => sums[p][x] || 0);
    // Хвост ненулевых: ведущие нули добивки режем — иначе Catmull-Rom тянет кривую в ноль слева.
    let _d = d.slice(-14), _td = td.slice(-14);
    while (_d.length && _d[0] === 0 && _td[0] === 0) { _d.shift(); _td.shift(); }
    out[p] = _d.length ? _d : [0]; outTok[p] = _td.length ? _td : [0];
  });
  sparkProviders = out;
  sparkTokens = outTok;
}
function normQ(s) { return String(s == null ? "" : s).normalize("NFC").toLowerCase().replace(/ё/g, "е").trim(); }
// Single-source предикаты состояния провайдера (вкладка «Провайдеры»).
// homeKPIs isLive (~1910) шире сознательно (transport_ready/egress-фолбэк) — не трогать.
// Каталог (catalog:true, keys_pct -1, ненастроенные) ни в один фильтр не входит: все предикаты требуют !e.catalog.
 function provOnline(e) { return !e.catalog && e.breaker === "closed" && e.keys_pct >= 0 && (e.keys_healthy || 0) > 0; }
 function provOffline(e) { return !e.catalog && e.breaker !== "half_open" && (e.breaker === "open" || e.keys_pct < 0 || (e.keys_healthy || 0) === 0); }
function provBreaker(e) { return !e.catalog && e.breaker !== "closed"; }
function paintProvFilterCounts() {
  const end = S.end || [];
  const n = { all: 0, online: 0, offline: 0, breaker: 0 };
  end.forEach(e => { if (e.catalog) return; n.all++; if (provOnline(e)) n.online++; if (provOffline(e)) n.offline++; if (provBreaker(e)) n.breaker++; });
  document.querySelectorAll("#prov-filters .fc").forEach(s => { const k = s.getAttribute("data-fc"); if (k && n[k] != null) s.textContent = n[k]; });
  // aria-pressed всегда синхронен S.pFilter (покрывает и сброс через #prov-clear без правок чужого кода)
  document.querySelectorAll("#prov-filters .btn").forEach(x => x.setAttribute("aria-pressed", String(x.dataset.f === (S.pFilter || "all"))));
  // clear-кнопка поиска видна только при непустом поле (покрывает и сброс через #prov-clear)
  try { const si = document.getElementById("prov-search"), px = document.getElementById("prov-x"); if (px) px.hidden = !((si && si.value) || S.pSearch); } catch (e) {}
}
function provFilter(e) {
  const q = normQ(S.pSearch), f = S.pFilter, sort = S.pSort;
  let list = S.end.slice();
  if (q) list = list.filter(e => normQ(e.name).includes(q) || (e.models || []).some(m => normQ(m).includes(q)) || normQ(e.type).includes(q) || normQ(e.tier).includes(q));
  if (f === "online") list = list.filter(provOnline);
  else if (f === "offline") list = list.filter(provOffline);
  else if (f === "breaker") list = list.filter(provBreaker);
  const latMap = new Map((S.health || []).map(x => [x.name, x.latency_ms]));
  const byLat = e => latMap.get(e.name) ?? 99999;
  const byName = (a, b) => a.name.localeCompare(b.name, "ru", { sensitivity: "base", numeric: true });
  list.sort((a, b) => {
    switch (sort) {
      case "healthy": return ((b.keys_healthy || 0) - (a.keys_healthy || 0)) || byName(a, b);
      case "keys": return ((b.keys || 0) - (a.keys || 0)) || byName(a, b);
      case "latency": return (byLat(a) - byLat(b)) || byName(a, b);
      case "tier": return ((a.tier || "").localeCompare(b.tier || "", "ru", { sensitivity: "base", numeric: true })) || byName(a, b);
      default: return byName(a, b);
    }
  });
  return list;
}
/* P-providers scroll-guard (GuiImplProviders): флаг скролла .content на window по контракту с H, снимается дебаунсом 300мс; свои строки, autoRefreshTick не трогаем */
window.contentScrollBusy = false; let _provScrollT = null;
function provScrollKick() { try { window.contentScrollBusy = true; } catch (eB) {} try { if (_provScrollT) clearTimeout(_provScrollT); } catch (e) {} _provScrollT = setTimeout(() => { try { window.contentScrollBusy = false; } catch (e2) {} }, 300); }
function renderProvGrid() {
  const grid = $("#prov-grid"), empty = $("#prov-empty");
  const list = provFilter();
  const totCfg = S.end.filter(e => !e.catalog).length;
  const res = $("#prov-result"); if (res) res.innerHTML = (S.pSearch || S.pFilter !== "all") ? ("Показано " + list.filter(e => !e.catalog).length + " из " + totCfg + ' <button class="btn btn--pill sm ghost" id="prov-clear">Сбросить</button>') : "";
  const pcb = document.getElementById("prov-clear"); if (pcb) pcb.addEventListener("click", () => { S.pSearch = ""; const si = document.getElementById("prov-search"); if (si) si.value = ""; S.pFilter = "all"; document.querySelectorAll("#prov-filters .btn").forEach(x => setCls(x, x.dataset.f === "all", "active")); renderProvGrid(); });
  let html = "";
  // Группировка: активные (настроенные) провайдеры — полными карточками сверху,
  // каталог (не настроены: нет ключей/моделей/запросов) — свёрнутым блоком внизу.
  const act = list.filter(e => !e.catalog), cat = list.filter(e => e.catalog);
  const latMap = new Map((S.health || []).filter(x => x.ok).map(x => [x.name, x.latency_ms]));
  act.forEach(e => {
    const latV = latMap.get(e.name);
    const lat = latV != null ? latV + " мс" : null;
    const st = e.breaker === "open" ? "err" : e.breaker === "half_open" ? "warn" : (e.keys_pct < 0 ? "off" : (e.keys_healthy === 0 ? "warn" : "on"));
    const stL = e.breaker === "open" ? "Брейкер" : e.breaker === "half_open" ? "Проба · брейкер" : e.keys_pct < 0 ? "нет ключей" : (e.keys_healthy === 0 ? "Пауза" : "В сети");
    const healthy = e.keys_pct < 0 ? 0 : Math.round((e.keys_pct || 0) * 100);
    const kvc = (S.kbp && S.kbp[e.name] && S.kbp[e.name].length) || 0;
    const keysChip = '<span class="chip"><span class="dot"></span>' + e.keys + ' ' + plural(e.keys, "ключ", "ключа", "ключей") + '</span>';
    const liveChip = (e.keys_healthy || 0) > 0 ? '<span class="chip"><span class="dot" style="background:var(--ok)"></span>' + (e.keys_healthy || 0) + " " + plural(e.keys_healthy || 0, "живой", "живых", "живых") + '</span>' : '<span class="chip"><span class="dot"></span>0 живых</span>';
    const latCls = latV == null ? "" : latV <= 500 ? "" : latV <= 1000 ? " warn" : " hot";
    const latChip = '<span class="chip' + latCls + '" data-lat title="RTT HealthCheck: прочерк — нет свежего замера"><span class="dot"></span>' + (lat ? esc(lat) : "—") + '</span>';
    const virtChip = '<span class="chip accent" title="виртуальных ключей"><span class="dot"></span>' + kvc + ' врт</span>';
    html += '<div class="card hover-card prov-card" data-p="' + esc(e.name) + '" data-st="' + st + '" role="button" tabindex="0">' +
      '<div class="gc-top"><div class="gc-ico">' + provIcon(e) + '</div>' +
      '<div class="gc-tit"><div class="gc-name" title="' + esc(e.name) + '">' + esc(e.name) + '</div><div class="gc-sub" title="' + esc((e.type || "") + " · " + (e.tier || "—")) + '">' + esc(e.type || "") + ' · ' + esc(e.tier || "—") + '</div></div>' +
      '<span class="badge ' + st + '" title="' + esc(badgeTip(e)) + '"><span class="dot"></span>' + esc(stL) + '</span></div>' +
      ((e.models && e.models.length) ? '<div class="gc-mods gc-mods--one" title="' + esc(e.models.join(", ")) + '">' + e.models.slice(0, 3).map(m => { const off = (e.disabled_models||[]).includes(m), hid = (e.hidden_models||[]).includes(m); return '<span class="model-pill' + (off?' off':'') + (hid?' hid':'') + '" title="' + esc(off ? m + ' — выключена (404)' : hid ? m + ' — скрыта' : m) + '">' + (off?'⛔ ':'') + esc(m) + '</span>'; }).join("") + (e.models.length > 3 ? '<span class="model-pill more' + (((e.models||[]).slice(3).some(m => (e.disabled_models||[]).includes(m))) ? ' has-off' : '') + '" title="ещё ' + (e.models.length-3) + '">' + '+' + (e.models.length - 3) + '</span>' : '') + '</div>' : '<div class="gc-mods gc-mods--one"><span class="model-pill wc' + (((e.disabled_models||[]).length) ? ' has-off' : '') + '" title="wildcard' + (((e.disabled_models||[]).length) ? ' · кроме выкл: ' + (e.disabled_models||[]).length : '') + '">любая модель</span></div>') +
      '<div class="gc-meta gc-meta--one">' + keysChip + liveChip + latChip + virtChip + '</div>' +
      '<div class="gc-tok"><span class="tok-lbl">токены</span><canvas data-token="' + esc(e.name) + '" width="0" height="0"></canvas></div>' +
      '<div class="gc-health" data-health="' + healthy + '"><div class="progress thin"><div class="bar" data-pct="' + healthy + '" style="width:' + healthy + '%"></div></div>' +
      (e.keys_pct >= 0 ? '<span class="gc-pct" title="живых ключей вне бэкоффа: ' + (e.keys_healthy || 0) + ' из ' + (e.keys || 0) + '">' + healthy + '%</span>' : '<span class="gc-pct" title="ключей нет">—</span>') + '</div></div>';
  });
  // Прочие (каталог): свёрнутый список «Не настроено (N)» — при поиске фильтруется тем же запросом.
  let catShow = cat;
  if (S.pSearch) { const qc = normQ(S.pSearch); catShow = cat.filter(e => normQ(e.name).includes(qc) || ((e.models||[]).some(m => normQ(m).includes(qc)))); }
  const _catWasOpen = !!document.querySelector('details.prov-catalog[open]');
  if (catShow.length) {
    html += '<details class="prov-catalog"' + (_catWasOpen ? ' open' : '') + '><summary><span class="arr">▸</span>Не настроено (' + catShow.length + ')<span class="spacer"></span><span class="hint">известные провайдеры без ключей и запросов</span></summary><div class="prov-cat-list">';
    catShow.forEach(e => {
      const hint = e.auth_hint || (e.base_url || "").replace(/^https?:\/\//, "");
      html += '<button type="button" class="prov-cat-item" data-p="' + esc(e.name) + '" title="Открыть карточку провайдера">' +
        '<span class="nm">' + esc(e.name) + '</span>' + (hint ? '<span class="hint">' + esc(hint) + '</span>' : '') + '</button>';
    });
    html += '</div></details>';
  }
  const glob = (S.pFilter !== "all" || S.pSearch) ? [] : ((S.kbp && S.kbp["глобальные"]) || []);
  if (glob.length) {
    html += '<div class="card hover-card prov-card" data-name="__GLOBAL__" role="button" tabindex="0">' +
      '<div class="gc-top"><div class="pc-ico">⌘</div><div class="gc-tit"><div class="gc-name">Глобальные ключи</div><div class="gc-sub">входящие · без привязки к моделям</div></div></div>' +
      '<div class="gc-meta"><span class="chip">' + glob.length + ' ' + plural(glob.length, "ключ", "ключа", "ключей") + '</span></div>' +
      '<div class="state" style="padding:10px 12px;margin-top:10px"><div class="hint">входящие ключи без allowlist · второй фактор не включён</div></div></div>';
  }
  // P-providers: скип при том же порядке + save/restore скролла .content (свои строки)
  const _ordKey = list.map(e => [e.name, e.keys, e.keys_healthy, e.breaker, e.keys_pct, (e.models||[]).length, (e.disabled_models||[]).length, (e.hidden_models||[]).length, e.tier, e.type].join(":")).join("|") + "#" + (S.pSort || "") + "#" + (S.pFilter || "") + "#" + glob.length + "#" + ((S.kbp && S.kbp["глобальные"] || []).length);
  if (grid._provOrd === _ordKey) { requestAnimationFrame(() => { drawSparks(); drawBars(); }); try { paintProvFilterCounts(); } catch (ePC) {} return; }
  grid._provOrd = _ordKey;
  let _sc = null; try { const _c = document.querySelector(".content"); if (_c) _sc = _c.scrollTop; } catch (eSC) {}
  grid.innerHTML = html;
  try { const _c2 = document.querySelector(".content"); if (_c2 && _sc != null) _c2.scrollTop = _sc; } catch (eRS) {}
  grid.style.display = "grid";
  if (!list.length && !glob.length) {
    if (!S.end.length) { grid.innerHTML = '<div class="card section"><div class="card-head"><h3>Подключите первого провайдера</h3></div><div class="hint">1 — + Провайдер · 2 — base_url и ключ · 3 — ↻ синк · 4 — ▶ Тест</div><div style="margin-top:10px;display:flex;gap:8px"><button class="btn btn--pill primary sm" id="empty-add">+ Провайдер</button></div></div>'; const ea = document.getElementById("empty-add"); if (ea) ea.addEventListener("click", () => addProviderForm()); empty.style.display = "none"; }
    else { empty.style.display = "flex"; try { const et = empty.querySelector(".title"); if (et && S.pSearch) et.textContent = 'Ничего не найдено по «' + S.pSearch + '»'; } catch (e) {} }
  } else empty.style.display = "none";
  // Бейдж «рабочих/всего» в заголовке табы: живой = есть ключи и брейкер не открыт
  const lb = $("#cnt-prov-live");
  if (lb) {
    const tot = S.end.filter(e => !e.catalog).length;
    const okN = S.end.filter(provOnline).length;
    lb.textContent = okN + "/" + tot;
    lb.style.display = "";
    lb.title = "Рабочих провайдеров: " + okN + " из " + tot;
  }
  requestAnimationFrame(() => { drawSparks(); drawBars(); });
  try { paintProvFilterCounts(); } catch (e) {}
}
function drawSparks() {
  if (document.hidden) return;
  if (window.contentScrollBusy && S.tab === "providers") return;
  const Tk = sparkTokens || {};
  const cont = document.querySelector(".content");
  const cH = cont ? cont.clientHeight : innerHeight;
  const all = $$("#prov-grid canvas[data-token]");
  const vis = all.filter(c => { try { const r = c.getBoundingClientRect(), cr = cont ? cont.getBoundingClientRect() : null; const top = cr ? r.top - cr.top : r.top, bot = top + r.height; return bot >= -40 && top <= cH + 40; } catch (eV) { return true; } });
  vis.forEach(c => { sparkSmooth(c, Tk[c.getAttribute("data-token")] || []); });
}
// Один аккуратный мини-график потребления токенов: гладкая кривая (Catmull-Rom →
// кубические Безье, контрольные Y зажаты соседями — без «колючек»), мягкая
// заливка, клип по канвасу и отступы, чтобы линия и маркер не выходили.
// Оси/деления/метки не рисуем; без данных — нейтральное «нет данных».
function sparkSmooth(cv, data, col) {
  const { ctx, w, h } = box(cv, 24);
  ctx.save(); ctx.beginPath(); ctx.rect(0, 0, w, h); ctx.clip();
  const c = col || E.accent;
  if (!data || !data.length) { ctx.font = "10px " + E.mono; ctx.fillStyle = E.muted; ctx.textAlign = "center"; ctx.fillText("нет данных", w / 2, h / 2 + 3); ctx.restore(); return; }
  if (data.length === 1) { ctx.font = "10px " + E.mono; ctx.fillStyle = E.muted; ctx.textAlign = "center"; ctx.fillText(compact(data[0]) + " ток. · мало данных", w / 2, h / 2 + 3); ctx.restore(); return; }
  let mx = Math.max.apply(null, data), mn = Math.min.apply(null, data), rng = mx - mn;
  const flat = !rng, allZero = (mx === 0);
  const pad = 3, hi = h - pad, N = data.length;
  const X = i => (w - 2) * i / (N - 1);
  const Y = v => flat ? (allZero ? hi : h / 2) : Math.max(pad, Math.min(hi, pad + hi * (1 - (v - mn) / rng)));
  const pts = []; for (let i = 0; i < N; i++) pts.push([X(i), Y(data[i])]);
  const Clamp = (v, a, b) => v < a ? a : v > b ? b : v;
  ctx.beginPath(); ctx.moveTo(pts[0][0], pts[0][1]);
  for (let i = 0; i < N - 1; i++) {
    const p0 = pts[i === 0 ? 0 : i - 1], p1 = pts[i], p2 = pts[i + 1], p3 = pts[i + 2 >= N ? N - 1 : i + 2];
    const k = 0.55, lo = Math.min(p1[1], p2[1]), hp = Math.max(p1[1], p2[1]);
    const c1x = p1[0] + (p2[0] - p0[0]) * k, c1y = Clamp(p1[1] + (p2[1] - p0[1]) * k, lo, hp);
    const c2x = p2[0] - (p3[0] - p1[0]) * k, c2y = Clamp(p2[1] - (p3[1] - p1[1]) * k, lo, hp);
    ctx.bezierCurveTo(c1x, c1y, c2x, c2y, p2[0], p2[1]);
  }
  ctx.strokeStyle = c; ctx.lineWidth = 1.6; ctx.lineJoin = "round"; ctx.lineCap = "round"; ctx.stroke();
  ctx.lineTo(pts[N - 1][0], h); ctx.lineTo(pts[0][0], h); ctx.closePath();
  const g = ctx.createLinearGradient(0, pad, 0, h); g.addColorStop(0, rawA(c, .16)); g.addColorStop(1, rawA(c, 0));
  ctx.fillStyle = g; ctx.fill();
  ctx.beginPath(); ctx.arc(pts[N - 1][0], pts[N - 1][1], 2, 0, 6.283); ctx.fillStyle = c; ctx.fill();
  ctx.restore();
}
function drawBars() {
  if (document.hidden) return;
  if (window.contentScrollBusy && S.tab === "providers") return;
  $$("#prov-grid .progress .bar[data-pct]").forEach(b => { const p = Math.max(0, Math.min(100, +b.getAttribute("data-pct") || 0)); const w = p + "%"; if (b.style.width !== w) b.style.width = w; const nc = "bar " + (p >= 70 ? "" : p >= 40 ? "warn" : "danger"); if (b.className !== nc) b.className = nc; });
}

// Живые отсчёты кулдаунов ключей: один тик в секунду, без перерисовки деталей.
(function pdCdTick() {
  setInterval(() => {
    document.querySelectorAll(".pd-cd-live[data-cd]").forEach(el => {
      let v = +el.getAttribute("data-cd") || 0;
      v -= 1;
      if (v <= 0) { el.textContent = "0:00"; return; }
      el.setAttribute("data-cd", String(v));
      el.textContent = fmtMmSs(v);
    });
  }, 1000);
})();

function openProv(name) {
  const e = S.end.find(x => x.name === name);
  S.prov = e || { name, type: "", base_url: "", models: [], tier: "", egress: "", egress_mode: "off", timeout_s: 0, keys_healthy: 0, breaker: "closed", keys_pct: -1 };
  $("#provList").style.display = "none"; $("#provDetail").style.display = "block";
  try { document.querySelector(".content").classList.add("prov-open"); } catch (eLock) {}
  try { const sc = document.querySelector(".content"); if (sc) sc.scrollTop = 0; } catch (e) {}
  renderProvDetail();
}
function backProv() { S.prov = null; $("#provDetail").style.display = "none"; $("#provList").style.display = "block";
  try { document.querySelector(".content").classList.remove("prov-open"); } catch (eUn) {}
  try { const sc = document.querySelector(".content"); if (sc) sc.scrollTop = 0; } catch (e) {} }
function badgeTip(e) {
  const parts = [];
  if (e.breaker === "open") parts.push("трафик идёт в обход, вернётся сам");
  else if (e.breaker === "half_open") parts.push("пробные запросы, проверяется восстановление");
  if (e.last_err) parts.push("причина: " + e.last_err);
  if (e.breaker_until_s) parts.push("возврат через " + fmtMmSs(e.breaker_until_s));
  return parts.join(" · ") || "все каналы работают";
}
function testModelName(e) {
  const en = (e.models || []).filter(m => !(e.disabled_models || []).includes(m) && !(e.hidden_models || []).includes(m));
  return en[0] || (e.models || [])[0] || "модель не выбрана";
}
function fmtMmSs(s) { s = Math.max(0, +s || 0); const m = Math.floor(s / 60), r = s % 60; return m + ":" + String(r).padStart(2, "0"); }
// Синк-тост с префиксом операции и провайдера: ok = изменилось, warn = ok но «ничего не изменено», err = провал.
function syncToast(op, r, changeRe, ms) {
  const msg = (r && r.message) || "ok";
  const changed = (changeRe || /добавлено|удалено/).test(msg);
  const nm = (S.prov && S.prov.name) || "";
  toast(op + " «" + nm + "»: " + msg + (changed ? "" : " — ничего не изменено"), !r || !r.ok ? "err" : (changed ? "ok" : "warn"), ms);
}
// ---- Синтез волны «деталка провайдера»: лента состояния (v6) + машина брейкера (v1/v6) ----
// Состояния честные и только из живых полей: нет ключей / все в бэкоффе /
// брейкер открыт / полуоткрытие / всё в порядке. Лента сворачивается в одну
// строку, когда всё хорошо — пустого места на экране нет.
function pdState(e, keys) {
  const b = (S.breakers || []).find(x => x.provider === e.name);
  const brk = (b && b.state) || (e.breaker || "closed");
  if (brk === "open") return { id: "breaker_open", lvl: "err", n: "1" };
  if (brk === "half_open") return { id: "proba", lvl: "warn", n: "2" };
  const total = (e.keys || 0), live = (e.keys_healthy || 0);
  if (e.keys_pct < 0 && !total) return { id: "no_keys", lvl: "warn", n: "1" };
  if (total > 0 && live === 0) return { id: "all_backoff", lvl: "warn", n: "2" };
  return { id: "healthy", lvl: "ok", n: "3" };
}
function pdBandHTML(e, keys) {
  const s = pdState(e, keys);
  const b = (S.breakers || []).find(x => x.provider === e.name) || {};
  const until = Math.max(0, +(b.opened_until_s || e.breaker_until_s || 0));
  const fails = b.failures != null ? b.failures : (b.consecutive_failures != null ? b.consecutive_failures : (e.breaker_failures || 0));
  const err = b.last_error || e.last_err || "";
  let head = "", body = "", act = "";
  if (s.id === "breaker_open") {
    head = "Брейкер открыт — запросы уходят в обход";
    body = "Провайдер исключён из маршрута на " + fmtMmSs(until) + ". Причина: " + (err || "серия отказов") + ".";
    act = "Ничего не трогайте — вернётся сам. Повторяется — смотрите Журналы и ключи.";
  } else if (s.id === "proba") {
    head = "Идут пробные запросы";
    body = "Полуоткрытие: " + fmtMmSs(until) + " до следующей проверки. Сбоев подряд: " + fails + ".";
    act = "Не жмите «Тест» пачкой — собьёте пробы.";
  } else if (s.id === "no_keys") {
    head = "Ключи не настроены";
    body = "Добавьте ключ провайдера — без них маршрут через него невозможен.";
    act = "Кнопка «+ Ключ» ниже или Настройки → ключи.";
  } else if (s.id === "all_backoff") {
    head = "Все ключи в бэкоффе";
    body = "Живых ключей нет. Ожидание: " + fmtMmSs(until || 0) + (err ? " · причина: " + err : "") + ".";
    act = "Дождитесь снятия кулдауна или добавьте ключ.";
  } else {
    head = "Все каналы в норме";
    body = "Брейкер закрыт, ключи живые.";
    act = "";
  }
  const healthy = s.id === "healthy";
  return '<div class="pd-band st-' + s.id + '" role="status">' +
    '<div class="pd-band-k"><span class="pd-band-n">шаг ' + s.n + ' · 3</span><span class="pd-band-h">' + esc(head) + '</span></div>' +
    '<div class="pd-band-b">' + (healthy ? '' : '<div class="pd-band-x">' + esc(body) + '</div><div class="pd-band-a">' + esc(act) + '</div>') + '</div>' +
    (healthy ? '' : '<div class="pd-band-track"><span class="on"></span><span class="' + (s.n === "2" ? "on" : "") + '"></span><span></span></div>') +
    '</div>';
}
function pdBreakerHTML(e) {
  const b = (S.breakers || []).find(x => x.provider === e.name) || {};
  const st = b.state || e.breaker || "closed";
  const until = Math.max(0, +(b.opened_until_s || e.breaker_until_s || 0));
  const fails = b.failures != null ? b.failures : (b.consecutive_failures != null ? b.consecutive_failures : (e.breaker_failures || 0));
  const err = b.last_error || e.last_err || "";
  const node = (id, lb, on) => '<span class="pd-brk-node' + (on ? " on" : "") + ' ' + id + '">' + lb + '</span>';
  const flow = node("closed", "закрыт", st === "closed") + '<i>→</i>' + node("open", "открыт", st === "open") + '<i>→</i>' + node("half", "полуоткрытие", st === "half_open") + '<i>↩</i>' + node("closed", "закрыт", false);
  if (st === "closed") {
    // Здоровый провайдер: лента уже сказала «все каналы в норме» — машина
    // состояний одной строкой, без повторного «закрыт».
    return '<div class="card section pd-brk st-closed compact"><div class="pd-brk-inline">' +
      '<span class="pd-brk-lbl">Брейкер</span>' + flow +
      '<span class="hint">сбоев подряд: ' + fails + (err ? ' · последняя: <code>' + esc(err) + '</code>' : "") + ' · порог и окно — в Настройки → Resilience</span></div></div>';
  }
  return '<div class="card section pd-brk st-' + st + '"><div class="card-head"><h3>Брейкер</h3>' +
    '<span class="badge ' + (st === "open" ? "err" : st === "half_open" ? "warn" : "ok") + '"><span class="dot"></span>' +
    (st === "open" ? "открыт" : st === "half_open" ? "полуоткрытие" : "закрыт") + '</span><span class="spacer"></span>' +
    (st === "closed" ? "" : '<span class="hint">возврат через <b class="pd-brk-cd" data-cd="' + until + '">' + fmtMmSs(until) + '</b></span>') + '</div>' +
    '<div class="pd-brk-flow">' + flow + '</div>' +
    '<div class="pd-brk-facts"><span>сбоев подряд: <b>' + fails + '</b></span>' +
    (err ? '<span>причина: <code>' + esc(err) + '</code></span>' : '') +
    '<span class="hint">порог и окно — в Настройки → Resilience</span></div>' +
    (st === "open" ? '<div class="hint row-err">Трафик идёт в обход провайдера. Вернётся автоматически по таймеру.</div>' :
     st === "half_open" ? '<div class="hint row-warn">Идут пробные запросы. Не гоняйте «Тест» пачками — пробы прерываются.</div>' :
     '<div class="hint">Норма: каскад использует провайдера.</div>') + '</div>';
}
function renderProvDetail() {
  const e = S.prov;
  if (!e) return;
  // сохранить скролл до сноса DOM (автотик пересоздаёт паны)
  let _svU = 0; const _svP = {};
  try { const _u = document.getElementById("pd-uni"); if (_u) _svU = _u.scrollLeft; } catch (e2) {}
  try { document.querySelectorAll("#pd-uni .pd-pane").forEach(pn => { _svP[pn.id] = pn.scrollTop; }); } catch (e2) {}
  $("#pd-name").textContent = e.name === "__GLOBAL__" ? "Глобальные ключи" : e.name;
  const keys = S.kbp ? (S.kbp[e.name] || (e.name === "__GLOBAL__" ? S.kbp["глобальные"] || [] : [])) : [];
  const hero = $("#pd-hero");
  // Чистка хвоста: renderProvDetail() зовётся повторно (автотик/loadProviders), старые секции удаляем.
  hero.parentElement.querySelectorAll(':scope > #pd-models, :scope > #pd-poolkeys, :scope > #pd-keys, :scope > #pd-settings, :scope > .pd-settings-wrap, :scope > .pd-tabs, :scope > #pd-panes, :scope > #pd-uni, :scope > #pd-secdrop').forEach(n => n.remove());
  // лента/брейкер живут внутри keysPane — снимаются вместе с ним (innerHTML перезаписывается в renderProvDetail ниже)
  window._pdScrollSave = { u: _svU, p: _svP, prov: (S.prov && S.prov.name) || "" };
  const hc = S.health.find(x => x.name === e.name);
  const lat = hc ? hc.latency_ms + " мс" : "—";
  const st = e.breaker === "open" ? "err" : e.breaker === "half_open" ? "warn" : (e.keys_pct < 0 ? "off" : (e.keys_healthy === 0 ? "warn" : "on"));
  const stL = e.breaker === "open" ? "Брейкер" : e.breaker === "half_open" ? "Проба" : e.keys_pct < 0 ? "нет ключей" : (e.keys_healthy === 0 ? "все ключи в бэкоффе" : "В сети");
  const isGlob = e.name === "__GLOBAL__";
  // Донат #pd-donut живёт только в блоке 2б (renderProvKeys); hero чистый, quotaCard удалён.
  const inModels = (e.models && e.models.length) ? e.models.length + ' моделей' : 'wildcard';
  const allowTxt = (e.models && e.models.length) ? esc(e.models.slice(0,3).join(", ")) + (e.models.length > 3 ? ' +ещё ' + (e.models.length-3) : '') : 'любая модель';
  hero.innerHTML = '';
  hero.style.display = "none";
  const summaryHTML =
    '<div class="card section summary-card"><div class="card-head"><h3>Сводка</h3><span class="badge ' + st + '" title="' + esc(badgeTip(e)) + '"><span class="dot"></span>' + esc(stL) + '</span><span class="spacer"></span>' + (isGlob ? '' : '<button class="btn sm primary" id="pd-test" title="Живой тест выбранной модели: ' + esc(testModelName(e)) + '">▶ Тест</button><button class="btn sm" id="pd-sync" title="Добавить новые модели из upstream /models, свои не удаляет">↻ Синхронизировать</button>') + '<button class="btn sm" data-copy="' + esc(e.base_url || "") + '" title="Скопировать base_url"' + (isGlob && !e.base_url ? ' style="display:none"' : '') + '>⧉ URL</button></div>' +
    '<div class="epline"><code title="' + esc(e.base_url || "") + '">' + esc(e.base_url || '—') + '</code><span class="spacer"></span><span class="chip' + (hc ? ' hot' : '') + '" title="RTT HealthCheck' + (hc ? ': ' + lat + ', обновляется фоном каждые 30с' : ': нет свежего замера — появится после HealthCheck') + '">RTT ' + (hc ? lat : "—") + '</span><span class="chip' + ((e.keys_healthy || 0) === 0 ? ' hot' : '') + '" title="Исходящие пул-ключи апстрима: здоровые / всего (входящие — строкой ниже)">' + (e.keys_healthy || 0) + '/' + (e.keys || 0) + ' ключей</span></div>' +
    '<div class="epline-sub"><span class="hint">входящие</span><span class="chip" title="Входящий тип провайдера — как шлюз принимает запросы">' + esc(e.type || 'openai-compatible') + '</span><span class="chip" title="Входящие модели: что разрешено запрашивать через шлюз">' + esc(inModels) + '</span><span class="hint" title="Первые модели каталога; полный список — в карточке Модели ниже">' + allowTxt + '</span></div>' + '</div>';
  // Табы детали: один рендер, переключение без потери состояния (display only).
  const tabs = document.createElement("div"); tabs.className = "pd-tabs"; tabs.setAttribute("role","group"); tabs.setAttribute("aria-label", "Разделы провайдера");
  const isKeyless = (e.auth_kind === "no-auth");
  if (S._pdTab === "diag") S._pdTab = "models";
  if (S._pdTab === "conn") S._pdTab = "keys";
  const tabDefs = isKeyless
    ? [["keys","Сводка + подключение"],["models","Модели"],["transport","Транспорт"]]
    : [["keys","Сводка + подключение"],["models","Модели"],["transport","Транспорт"]];
  const cur = S._pdTab || "keys";
  tabs.innerHTML = tabDefs.map(([id,lb]) => '<button class="btn sm' + (cur === id ? " on" : "") + '" data-pdtab="' + id + '"' + (cur === id ? ' aria-current="true"' : "") + '>' + lb + "</button>").join("");
  tabs.style.display = ""; hero.parentElement.appendChild(tabs);
  const panes = document.createElement("div"); panes.id = "pd-panes"; hero.parentElement.appendChild(panes);
  const mkPane = id => { const d = document.createElement("div"); d.className = "pd-pane" + (cur === id ? " on" : ""); d.id = "pd-pane-" + id; panes.appendChild(d); return d; };
  const keysPane = mkPane("keys"), modelsPane = mkPane("models"), transPane = mkPane("transport");
  const secIds = ["keys", "models", "transport"];
  tabs.querySelectorAll("[data-pdtab]").forEach(b => b.addEventListener("click", () => {
    S._pdTab = b.getAttribute("data-pdtab");
    tabs.querySelectorAll("[data-pdtab]").forEach(x => { setCls(x, x === b, "on"); if (x === b) x.setAttribute("aria-current", "true"); else x.removeAttribute("aria-current"); });
    try { window._hGo(S._pdTab); } catch (e) {}
    paintSecDrop();
    try { window._flashIsland && window._flashIsland(); } catch (e) {}
  }));
  // единый скролл ключи→модели→подключение→транспорт: 1 проворот = чуть вниз, 2 тика = следующая секция
  window.paintSecDrop = paintSecDrop;
  function paintSecDrop() {
    const secs = ["keys", "models", "transport"].map(id => document.getElementById("pd-pane-" + id)).filter(Boolean);
    if (!secs.length) return;
    let ix = 0;
    try { ix = Math.max(0, Math.min(secs.length - 1, Math.round(uni.scrollLeft / Math.max(1, uni.clientWidth)))); }
    catch (e) { let best = 1e12; secs.forEach((pn, i) => { const r = pn.getBoundingClientRect(); const d = Math.abs(r.left - innerWidth / 2); if (d < best) { best = d; ix = i; } }); }
    const y = 0;
    const names = ["Сводка", "Модели", "Транспорт"];
    const mapIx = secs.map(pn => ["keys", "models", "transport"].indexOf(pn.id.replace("pd-pane-", "")));
    const cur2 = document.querySelector("#pd-secdrop [data-scur]"); if (cur2) cur2.textContent = names[mapIx[ix]] || "";
    const sides = document.querySelectorAll("#pd-secdrop [data-sside]");
    if (sides[0]) sides[0].textContent = ix > 0 ? (names[mapIx[ix - 1]] || "") : "";
    if (sides[1]) sides[1].textContent = ix < secs.length - 1 ? (names[mapIx[ix + 1]] || "") : "";
    void y;
    // инит/скролл: красим только каплю, таб не трогаем — иначе сносит ?pdtab=
  }
  // syncTabs удалён: таб красится один раз при создании по S._pdTab, скролл его не трогает
  // 1 тик = 1 секция: счётчиков нет
  if (!window._secScrollBound) { window._secScrollBound = true; window.addEventListener("scroll", () => { paintSecDrop(); }, { passive: true }); }

  const uni = document.createElement("div"); uni.className = "uniscroll"; uni.id = "pd-uni";
  // Островок: вспышка только при смене секции; uni-scroll не мигает
  const flashIsland = () => { const sd = document.getElementById("pd-secdrop"); if (sd) { sd.classList.remove("on"); void sd.offsetWidth; sd.classList.add("on"); clearTimeout(window._sdT); window._sdT = setTimeout(() => sd.classList.remove("on"), 1400); } };
  window._flashIsland = flashIsland;
  uni.addEventListener("scroll", () => { try { paintSecDrop(); } catch (e) {} }, { passive: true });
  const secDrop = document.createElement("div"); secDrop.className = "secdrop"; secDrop.id = "pd-secdrop";
  secDrop.innerHTML = '<span class="s-side" data-sside="0" data-secmove="-1"></span><span class="s-cur" data-scur></span><span class="s-side" data-sside="1" data-secmove="1"></span>';
  secDrop.querySelectorAll("[data-secmove]").forEach(sd => sd.addEventListener("click", () => {
    const dir = +sd.getAttribute("data-secmove") || 1;
    try { const nx = Math.max(0, Math.min(uni.children.length - 1, Math.round(uni.scrollLeft / Math.max(1, uni.clientWidth)) + dir)); uni.scrollTo({ left: nx * uni.clientWidth, behavior: "smooth" }); flashIsland(); } catch (e) {}
  }));
  hero.parentElement.insertBefore(uni, panes);
  hero.parentElement.insertBefore(secDrop, hero.parentElement.firstChild);
  try { panes.remove(); } catch (e) {}
  const bandBox = document.createElement("div"); bandBox.id = "pd-band"; bandBox.innerHTML = pdBandHTML(e, keys); keysPane.appendChild(bandBox);
  const brkBox = document.createElement("div"); brkBox.id = "pd-brk"; brkBox.innerHTML = pdBreakerHTML(e); keysPane.appendChild(brkBox);
  const sumBox = document.createElement("div"); sumBox.className = "summary-in-tab"; sumBox.innerHTML = summaryHTML; keysPane.appendChild(sumBox);
  // Подключение — сразу под сводкой в первой вкладке (отдельной вкладки нет).
  const _connFirst = document.createElement("div"); _connFirst.innerHTML = provConnCard(e); while (_connFirst.firstChild) keysPane.appendChild(_connFirst.firstChild); wireProvConn();
  uni.appendChild(keysPane); uni.appendChild(modelsPane); uni.appendChild(transPane);
  [keysPane, modelsPane, transPane].forEach(pn => { pn.classList.add("usec"); pn.classList.add("on"); pn.style.display = "block"; });
  keysPane.dataset.sec = "Сводка"; modelsPane.dataset.sec = "Модели"; transPane.dataset.sec = "Транспорт";
  // Колесо: панель скроллится нативно всегда; свап секции только на её краю
  let hLock = 0;
  uni.addEventListener("wheel", ev => { /* V13: вертикаль — нативный скролл, свапа нет */ }, { passive: true });
  const _sameProv = window._pdNoAnimProv === ((S.prov && S.prov.name) || "");
  const mrow = document.createElement("div"); mrow.id = "pd-models"; if (_sameProv) mrow.classList.add("no-anim"); mrow.innerHTML = provModelsCard(e); modelsPane.appendChild(mrow);
  const strow = document.createElement("div"); strow.id = "pd-mstage"; strow.innerHTML = provModelStageCard(e); modelsPane.appendChild(strow);
  window._pdNoAnimProv = (S.prov && S.prov.name) || "";
  const hGo = id => { try { const el = document.getElementById("pd-pane-" + id); if (el) el.scrollIntoView({ behavior: "smooth", block: "start" }); S._pdTab = id; document.querySelectorAll('#provDetail [data-pdtab]').forEach(x => { const on = x.getAttribute("data-pdtab") === S._pdTab; setCls(x, on, "on"); if (on) x.setAttribute("aria-current", "true"); else x.removeAttribute("aria-current"); }); } catch (e) {} };
  window._hGo = hGo;
  // FIT-UNI: высота карусели по активной вкладке — короткая (опенкод keys)
  // не тянется под высокую (models), пустого места снизу нет. Свап/скролл те же.
  function fitUni(ixArg, holdMax) { /* V13: вертикаль — высота нативна, fit не нужен */ }
  window._fitUni = fitUni;
  let _fitT = null;
  uni.addEventListener("scroll", () => { try { if (_fitT) clearTimeout(_fitT); _fitT = setTimeout(() => { try { fitUni(); } catch (eFS) {} }, 140); } catch (eSB) {} }, { passive: true });
  if (!window._fitUniResize) { window._fitUniResize = true; window.addEventListener("resize", () => { try { if (window._fitUni) window._fitUni(); } catch (eR) {} }, { passive: true }); }
  try { const _sv = window._pdScrollSave, _pn = (S.prov && S.prov.name) || ""; if (_sv && _sv.prov === _pn) { if (_sv.u) uni.scrollLeft = _sv.u; if (_sv.p) Object.keys(_sv.p).forEach(id => { const pn = document.getElementById(id); if (pn && _sv.p[id]) pn.scrollTop = _sv.p[id]; }); } } catch (eRestore) {}
  setTimeout(() => { try { const _s2 = window._pdScrollSave, _p2 = (S.prov && S.prov.name) || ""; if (!(_s2 && _s2.prov === _p2 && _s2.u)) hGo(S._pdTab || "keys"); } catch (eH) { try { hGo(S._pdTab || "keys"); } catch (eH2) {} } paintSecDrop(); try { fitUni(); } catch (eFI) {} try { setTimeout(() => { try { fitUni(); } catch (eFI2) {} }, 500); } catch (eFI3) {} }, 120);
  const sy = $("#pd-sync"); if (sy) sy.addEventListener("click", async () => {
    if (!S.prov || sy.disabled) return;
    if (DEMO || !Go()) { toast("демо: синк недоступен", "warn"); return; }
    sy.disabled = true;
    try { const r = await API.SyncModels(S.prov.name); syncToast("Синк", r); if (r.ok) loadProviders(); }
    catch (e) { toast("Синк: " + (e && e.message || e), "err"); }
    finally { sy.disabled = false; }
  });
    // prune удалён: обновление = SyncModels (новые добавятся, свои сохранятся)
    const pdBan = $("#pd-ban-add"); if (pdBan) pdBan.addEventListener("click", async () => {
      if (!S.prov) return;
      const inp = $("#pd-ban-name"), m = inp ? inp.value.trim() : ""; if (!m) return;
      const cur = ((S.prov.hidden_models || []).slice()); if (!cur.includes(m)) cur.push(m);
      try { const r = await API.UpdateProvider(S.prov.name, {hidden_models: cur}); toast('«' + m + '»: скрыта из каталога — ' + (r.message || 'ok'), r.ok ? 'ok' : 'err'); if (r.ok) { S.prov.hidden_models = cur; loadProviders(); } }
      catch (e) { toast('«' + m + '»: ' + (e && e.message || e), 'err'); }
    });
    const mtglFlip = async (m, hide) => {
      if (!S.prov) return;
      if (DEMO || !Go()) { toast("демо: переключение недоступно", "warn"); return; }
      const fld = hide ? "hidden_models" : "disabled_models";
      const cur = ((S.prov[fld] || []).slice());
      const ix = cur.indexOf(m); if (ix >= 0) cur.splice(ix, 1); else cur.push(m);
      const act = ix >= 0 ? (hide ? 'показана в каталоге' : 'включена') : (hide ? 'скрыта из каталога' : 'отключена');
      try { const patch = {}; patch[fld] = cur; const r = await API.UpdateProvider(S.prov.name, patch); toast('\u00ab' + m + '\u00bb: ' + act + ' \u2014 ' + (r.message || 'ok'), r.ok ? 'ok' : 'err'); if (r.ok) { S.prov[fld] = cur; loadProviders(); } }
      catch (e) { toast('\u00ab' + m + '\u00bb: ' + (e && e.message || e), 'err'); }
    };
    S._mdlSelMode = S._mdlSelMode || false;
    const toggleSel = mm => { S._mdlSel = S._mdlSel || new Set(); const has = S._mdlSel.has(mm); if (has) S._mdlSel.delete(mm); else S._mdlSel.add(mm); document.querySelectorAll('#provDetail [data-chip="' + CSS.escape(mm) + '"]').forEach(x => x.classList.toggle("sel", !has)); paintMbulk(); };
    document.querySelectorAll("#provDetail [data-chip]").forEach(ch => {
      const m = ch.getAttribute("data-chip"); if (!m) return;
      const eye = ch.querySelector("[data-meye]");
      if (eye) eye.addEventListener("click", ev => { ev.stopPropagation(); mtglFlip(m, true); }); // глаз = показать скрытую
      let pressT = 0; // долгий клик = выбрать + включает режим
      ch.addEventListener("pointerdown", () => { pressT = Date.now(); });
      ch.addEventListener("click", ev => {
        if (ev.target && (ev.target.closest("[data-mdot]") || ev.target.closest("[data-meye]"))) { if (ev.target.closest("[data-mdot]")) { ev.stopPropagation(); mtglFlip(m, false); } return; }
        ev.stopPropagation();
        const held = Date.now() - pressT > 550;
        if (S._mdlSelMode || held || ev.ctrlKey || ev.metaKey || ev.altKey) { if (held && !S._mdlSelMode) { S._mdlSelMode = true; paintSelMode(); } toggleSel(m); return; } // выбор
        // Обычный клик без модификаторов: чужое выделение снимается (капля — Solo-выбор).
        if (S._mdlSel && S._mdlSel.size) { S._mdlSel.clear(); document.querySelectorAll('#provDetail [data-chip].sel').forEach(x => x.classList.remove("sel")); try { paintMbulk(); } catch (e) {} }
        // КАПЛЯ: из чипа в нижнюю плашку — дуга с затуханием, полоса дотягивается до краёв
        try {
          const fam = ch.getAttribute("data-fam") || "f-other";
          const col = (typeof FAM_HEX !== "undefined" && FAM_HEX[fam]) || "#e5484d";
          const r0 = ch.getBoundingClientRect();
          const stage0 = document.getElementById("pd-mstage");
          const nmEl = stage0 ? stage0.querySelector(".mdl-stage-head .nm") : null;
          const rl0 = (nmEl || stage0) ? ((nmEl || stage0).getBoundingClientRect()) : null;
          const sx = r0.left + r0.width / 2, sy = r0.top + r0.height / 2;
          // Цель — название модели (.nm), не центр блока: шарик прилетает в имя.
          const tx = rl0 ? rl0.left + rl0.width / 2 : sx, ty = rl0 ? rl0.top + rl0.height / 2 : sy + 120;
          const drop = document.createElement("div"); drop.className = "drop";
          drop.innerHTML = '<span class="d-glow"></span><span class="d-txt">' + esc(m) + '</span>';
          const d0 = 14;
          Object.assign(drop.style, { left: (sx - d0 / 2) + "px", top: (sy - d0 / 2) + "px", width: d0 + "px", height: d0 + "px", background: "radial-gradient(circle," + col + " 0%," + col + "cc 55%," + col + "00 75%)", borderRadius: "50%", opacity: "1" });
          document.body.appendChild(drop);
          drop.getBoundingClientRect();
          const mx = (sx + tx) / 2, my = Math.min(sy, ty) - 46;
          const fly = drop.animate([
            { left: (sx - d0 / 2) + "px", top: (sy - d0 / 2) + "px", width: d0 + "px", height: d0 + "px", borderRadius: "50%", opacity: 1, offset: 0 },
            { left: (mx - 20) + "px", top: (my - 20) + "px", width: "40px", height: "40px", borderRadius: "50%", opacity: .95, offset: .45 },
            { left: (tx - 60) + "px", top: (ty - 14) + "px", width: "120px", height: "28px", borderRadius: "14px", opacity: .85, offset: .75 },
            { left: (tx - 200) + "px", top: ty + "px", width: "400px", height: "3px", borderRadius: "2px", opacity: 0, offset: 1 }
          ], { duration: 620, easing: "cubic-bezier(.2,.8,.25,1)", fill: "forwards" });
          fly.onfinish = () => {
            S._mdlCur = m;
            try {
              const sr = document.getElementById("pd-mstage");
              if (sr && S.prov) { sr.innerHTML = provModelStageCard(S.prov); wireStage(sr); }
              document.querySelectorAll('#provDetail [data-chip]').forEach(x => x.classList.toggle("cur", x.getAttribute("data-chip") === m));
              const card = sr ? sr.querySelector(".mstage-card") : null;
              const sep = sr ? sr.querySelector(".mdrop-sep") : null;
              if (sep) { sep.classList.remove("sweep"); void sep.offsetWidth; sep.classList.add("sweep"); }
              if (card) { card.classList.remove("st-in"); void card.offsetWidth; card.classList.add("st-in"); }
            } catch (e) {}
            try { drop.remove(); } catch (e) {}
          };
          setTimeout(() => { try { if (document.body.contains(drop)) drop.remove(); } catch (e) {} }, 1600);
        } catch (e) { S._mdlCur = m; try { renderProvDetail(); } catch (e2) {} }
      });
    });
    const wireStage = scope => { scope.querySelectorAll(".mdl-stage[data-mrow]").forEach(row => {
      const m = row.getAttribute("data-mrow"); if (!m) return;
      const head = row.querySelector(".mdl-stage-head");
      if (head) head.addEventListener("click", ev => {
        if (ev.target && (ev.target.closest("input,select,button"))) return; // клик по контролам — не схлопывать
        row.classList.toggle("open");
      });
      const hb = row.querySelector("[data-mdl-hide]");
      if (hb) hb.addEventListener("click", () => mtglFlip(m, true));
      const tb = row.querySelector("[data-mdl-test]");
      if (tb) tb.addEventListener("click", () => { try { testProvider(m); } catch (e) {} });
      const rsC0 = row.querySelector("[data-mdl-resp]");
      if (rsC0) { rsC0.addEventListener("click", ev => ev.stopPropagation()); rsC0.addEventListener("change", async () => {
        if (!S.prov || DEMO || !Go()) { toast("демо: недоступно", "warn"); rsC0.checked = !rsC0.checked; return; }
        const cur = ((S.prov.responses_models || []).slice());
        const ix = cur.indexOf(m); if (rsC0.checked && ix < 0) cur.push(m); if (!rsC0.checked && ix >= 0) cur.splice(ix, 1);
        try { const r = await API.UpdateProvider(S.prov.name, { responses_models: cur }); toast(rsC0.checked ? "«" + m + "» → /responses" : "«" + m + "» → chat", r.ok ? "ok" : "err"); if (r.ok) { S.prov.responses_models = cur; loadProviders(); } }
        catch (e) { toast("responses: " + (e && e.message || e), "err"); rsC0.checked = !rsC0.checked; }
      }); }
      const pb = row.querySelector("[data-mdl-probe]");
      if (pb) pb.addEventListener("click", async () => {
        if (!S.prov || DEMO || !Go()) { toast("демо: живой зонд недоступен", "warn"); return; }
        const out = row.querySelector("[data-probe-out]"); if (out) out.innerHTML = '<span class="spin" role="status" aria-label="Проверка модели…"></span> chat…';
        try {
          const r = await API.TestProvider(S.prov.name, m);
          const ok = !r.error && r.status < 400;
          if (ok) { if (out) out.textContent = "chat ok · " + (r.latency_ms || 0) + "мс"; row.classList.remove("mdl-flash"); void row.offsetWidth; row.classList.add("mdl-flash"); return; }
          const already = (S.prov.responses_models || []).includes(m);
          if (already) { if (out) out.textContent = "chat ✗ (" + (r.status || r.error || "?") + ") · уже в responses — ядро само ретраит /responses"; return; }
          if (out) out.innerHTML = 'chat ✗ (' + esc(String(r.status || r.error || "?")).slice(0, 60) + ') · <button class="btn sm primary" data-probe-fix>записать в responses</button>';
          const fx = row.querySelector("[data-probe-fix]");
          if (fx) fx.addEventListener("click", async ev => { ev.stopPropagation();
            const cur = ((S.prov.responses_models || []).slice()); if (!cur.includes(m)) cur.push(m);
            try { const rr = await API.UpdateProvider(S.prov.name, { responses_models: cur }); toast("«" + m + "» → /responses", rr.ok ? "ok" : "err"); if (rr.ok) { S.prov.responses_models = cur; loadProviders(); } }
            catch (e) { toast("responses: " + (e && e.message || e), "err"); }
          });
        } catch (e) { if (out) out.textContent = "зонд: " + (e && e.message || e); }
      });
      const queueReasonSave = (mm) => {
        if (DEMO || !Go() || !S.prov) return;
        clearTimeout(window._rsT);
        window._rsT = setTimeout(async () => {
          const mr0 = Object.assign({}, S.prov.model_reasoning || {}, S._mr || {});
          Object.keys(mr0).forEach(k => { if (!mr0[k] || mr0[k] === "auto") delete mr0[k]; });
          try { const r = await API.UpdateProvider(S.prov.name, { model_reasoning: mr0 }); if (r.ok) S.prov.model_reasoning = mr0; toast("reasoning «" + mm + "» сохранён", r.ok ? "ok" : "err"); }
          catch (e) { toast("reasoning: " + (e && e.message || e), "err"); }
        }, 700);
      };
      const rsl = row.querySelector("[data-rsl]"), lb = row.querySelector("[data-rsl-lb]");
      const paintFill = () => { const f = Math.round((+rsl.value || 0) / 4 * 100) + "%"; if (rsl) rsl.style.setProperty("--fill", f); try { const w = rsl.closest(".kx-wrap"); if (w) w.style.setProperty("--fill", f); } catch (eF) {} };
      if (rsl) {
        paintFill();
        const snapDots = () => { const box = row.querySelector(".kx-dots"); if (!box || !rsl) return; const v = Math.round(+rsl.value || 0); box.querySelectorAll("i").forEach((d, i) => { if (i === v) { d.style.background = "transparent"; d.style.boxShadow = "none"; d.style.opacity = "0"; } else { d.style.background = "rgba(255,255,255,.38)"; d.style.boxShadow = "none"; d.style.opacity = "1"; } }); };
        snapDots();
        rsl.addEventListener("input", () => {
          const LV2 = ["auto","low","high","xhigh","max"];
          const v = LV2[+rsl.value] || "auto";
          S._mr = S._mr || {}; S._mr[m] = v;
          if (lb) lb.textContent = v === "auto" ? ("auto→" + (/x-preview/i.test(m) ? "max" : (/muse-spark|mimo/i.test(m) ? "xhigh" : "auto"))) : v;
          paintFill();
          const cdx = row.querySelector(".kx"); if (cdx) { cdx.className = cdx.className.replace(/lvl\d/, "lvl" + (+rsl.value || 0)); cdx.classList.toggle("kx-lastmax", (v === "max") || (v === "auto" && /x-preview/i.test(m))); }
          rsl.value = LV2.indexOf(v); snapDots();
        });
        rsl.addEventListener("change", () => { rsl.value = Math.round(+rsl.value || 0); snapDots(); });
      }
      if (rsl) { const rs0 = rsl; rs0.addEventListener("change", () => queueReasonSave(m)); }
    });
    };
    wireStage(strow);
    const paintMbulk = () => { const bar = $("#mdl-bulk"); if (!bar) return; const c = (S._mdlSel || new Set()).size; bar.classList.toggle("on", c > 0 || S._mdlSelMode); const cc = $("#mdl-bulk-cnt"); if (cc) cc.textContent = S._mdlSelMode ? ("режим выбора · выбрано: " + c) : (c ? ("выбрано: " + c) : ""); const sv = $("#pd-selmode"); if (sv) sv.classList.toggle("on", !!S._mdlSelMode); };
    window.paintMbulk = paintMbulk;
    window.paintSelMode = () => { const box = $("#pd-models"); if (box) box.classList.toggle("selmode", !!S._mdlSelMode); paintMbulk(); };
    paintMbulk();
    document.querySelectorAll("#provDetail [data-mbulk]").forEach(b => b.addEventListener("click", async () => {
      const act = b.getAttribute("data-mbulk"); const sel = Array.from(S._mdlSel || []);
      if (act === "clear") { S._mdlSel = new Set(); try { renderProvDetail(); } catch (e) {} return; }
      if (!S.prov || DEMO || !Go()) { toast("демо: недоступно", "warn"); return; }
      if (act === "test") {
        if (!sel.length) { toast("выберите модели □ слева", "warn"); return; }
        const w = modal("Тест выбранных (" + sel.length + ")", '<div class="state"><div class="big" aria-hidden="true">↻</div><div class="title">шлём по 1 ping последовательно…</div><div class="hint" id="mb-prog"></div></div>');
        let ok = 0, fail = 0; const fails = [];
        for (let i = 0; i < sel.length; i++) { const mm = sel[i]; const pg = $("#mb-prog", w.el); if (pg) pg.textContent = (i + 1) + "/" + sel.length + " · " + mm;
          try { const r = await API.TestProvider(S.prov.name, mm); if (!r.error && r.status < 400) ok++; else { fail++; fails.push(mm + " (" + (r.status || r.error || "?") + ")"); } }
          catch (e) { fail++; fails.push(mm + " (" + (e && e.message || e) + ")"); } }
        w.close();
        modal("Тест выбранных", '<div class="test-line"><span class="badge on">ok ' + ok + '</span> <span class="badge err">fail ' + fail + '</span></div>' + (fails.length ? '<div class="test-out err">' + esc(fails.join("\n")) + '</div>' : '<div class="hint">все ответили</div>'));
        return;
      }
      const fld = "disabled_models"; const cur = ((S.prov[fld] || []).slice());
      sel.forEach(mm => { const ix = cur.indexOf(mm); if (act === "off" && ix < 0) cur.push(mm); if (act === "on" && ix >= 0) cur.splice(ix, 1); });
      try { const r = await API.UpdateProvider(S.prov.name, { disabled_models: cur }); toast(act === "on" ? "выбранные включены" : "выбранные выключены", r.ok ? "ok" : "err"); if (r.ok) { S.prov.disabled_models = cur; S._mdlSel = new Set(); loadProviders(); } }
      catch (e) { toast("bulk: " + (e && e.message || e), "err"); }
    }));
    const smode = $("#pd-selmode"); if (smode) smode.addEventListener("click", () => { S._mdlSelMode = !S._mdlSelMode; if (!S._mdlSelMode) { S._mdlSel = new Set(); try { renderProvDetail(); } catch (e) {} } paintSelMode(); toast(S._mdlSelMode ? "режим выбора: кликайте модели" : "режим выбора выкл", S._mdlSelMode ? "ok" : undefined); });
    const sall = $("#pd-sel-all"); if (sall) sall.addEventListener("click", () => { if (!S._mdlSelMode) { S._mdlSelMode = true; paintSelMode(); } S._mdlSel = S._mdlSel || new Set(); document.querySelectorAll('#provDetail .mchip[data-chip]').forEach(b => { if (b.style.display === "none") return; const mm = b.getAttribute("data-chip"); S._mdlSel.add(mm); b.classList.add("sel"); }); paintMbulk(); toast("выбрано: " + S._mdlSel.size, "ok"); });
    const mref = $("#pd-models-refresh"); if (mref) mref.addEventListener("click", async () => { if (!S.prov || mref.disabled) return; if (DEMO || !Go()) { toast("демо: синк недоступен", "warn"); return; } mref.disabled = true; try { const r = await API.SyncModels(S.prov.name); syncToast("Обновлено", r, null, 5000); if (r.ok) loadProviders(); } catch (e) { toast("Обновление: " + (e && e.message || e), "err"); } finally { mref.disabled = false; } });
    const cAdd = $("#pd-custom-add"), cGo = $("#pd-custom-go");
    const doCustomAdd = async () => {
      if (!S.prov || !cAdd) return;
      const nm = (cAdd.value || "").trim();
      if (!nm) { toast("Укажите имя модели", "warn"); return; }
      if ((S.prov.models || []).includes(nm)) { toast("Модель уже в списке", "warn"); return; }
      if (DEMO || !Go()) { toast("демо: добавление недоступно", "warn"); return; }
      cGo.disabled = true;
      try {
        const models = (S.prov.models || []).concat([nm]);
        const r = await API.UpdateProvider(S.prov.name, { models });
        if (r && r.ok) { try { customAdd(S.prov.name, nm); } catch (e) {} toast("Модель «" + nm + "» добавлена (кастомная, не убирается при обновлении)", "ok"); loadProviders(); }
        else toast("Добавление: " + ((r && r.message) || "ошибка"), "err");
      } catch (e) { toast("Добавление: " + (e && e.message || e), "err"); }
      finally { cGo.disabled = false; }
    };
    if (cGo) cGo.addEventListener("click", doCustomAdd);
    if (cAdd) cAdd.addEventListener("keydown", e => { if (e.key === "Enter") { e.preventDefault(); doCustomAdd(); } });
document.querySelectorAll("#provDetail [data-mdlsort]").forEach(b => b.addEventListener("click", () => { S._mdlSort = b.getAttribute("data-mdlsort"); try { renderProvDetail(); } catch (e) {} }));
    const mq = $("#pd-model-q"); if (mq) {
      const upd = () => { const s = normQ(mq.value || ""); let vis = 0, tot = 0; document.querySelectorAll("#provDetail .mchip[data-chip]").forEach(b => { const m = normQ(b.getAttribute("data-chip") || ""); tot++; const ok = !s || m.includes(s); b.style.display = ok ? "" : "none"; if (ok) vis++; }); const cc = $("#pd-model-count"); if (cc) cc.textContent = "Показано " + vis + " из " + tot; if (!vis && tot) { if (!$("#pd-model-none")) { const d = document.createElement("div"); d.className = "hint"; d.id = "pd-model-none"; d.textContent = "Ничего не найдено — измените запрос"; mq.closest(".card").appendChild(d); } } else { const nn = $("#pd-model-none"); if (nn) nn.remove(); } };
      mq.addEventListener("input", upd); upd();
    }
    const ao = $("#pd-all-on"); if (ao) ao.addEventListener("click", async () => { if (!S.prov) return; if (DEMO || !Go()) { toast("демо: недоступно", "warn"); return; } try { const r = await API.UpdateProvider(S.prov.name, { disabled_models: [] }); toast("Все модели включены — " + (r.message || "ok"), r.ok ? "ok" : "err"); if (r.ok) { S.prov.disabled_models = []; loadProviders(); } } catch (e) { toast("Всё вкл: " + (e && e.message || e), "err"); } });
    const af = $("#pd-all-off"); if (af) af.addEventListener("click", async () => { if (!S.prov || !(S.prov.models || []).length) return; if (DEMO || !Go()) { toast("демо: недоступно", "warn"); return; } if (!confirm("Выключить ВСЕ " + S.prov.models.length + " моделей у «" + S.prov.name + "»? Маршрутизация остановится.")) return; try { const r = await API.UpdateProvider(S.prov.name, { disabled_models: (S.prov.models || []).slice() }); toast("Все модели выключены — " + (r.message || "ok"), r.ok ? "ok" : "err"); if (r.ok) { S.prov.disabled_models = (S.prov.models || []).slice(); loadProviders(); } } catch (e) { toast("Всё выкл: " + (e && e.message || e), "err"); } });
  // Ключи — первая секция (главное). Keyless: ключи не используются — только подпись, без пустых блоков.
  if (isKeyless) {
    const kn = document.createElement("div"); kn.className = "keyless-note";
    kn.innerHTML = '<b>Ключи не используются</b> — keyless-архитектура: запросы идут пулом через warp egress без API-ключей. Лимитов нет; 429 банит egress на 8ч per-exact-IP. Ротация — в табе Транспорт, диагностика — в табе Диагностика.';
    keysPane.appendChild(kn);
  } else {
    const prow = document.createElement("div"); prow.id = "pd-poolkeys"; keysPane.appendChild(prow);
    const ks = document.createElement("div"); ks.id = "pd-keys"; keysPane.appendChild(ks);
    renderProvKeys(ks, keys);
    if ($("#pd-donut")) redrawDonut();
    renderPoolKeys();
  }
  const srow = document.createElement("div"); srow.id = "pd-settings"; srow.innerHTML = provSettingsCard(e);
  // Подключение живёт в первой вкладке (Сводка): форма base/UA/responses под сводкой, транспорт отдельно.
  const transBox = document.createElement("div");
  transPane.appendChild(transBox);
  while (srow.firstChild) transBox.appendChild(srow.firstChild);
  wireProvSettings();
  try { enhanceSelects(document.getElementById("provDetail")); } catch (eDDP) {}
}
function wireProvSettings() {
  if (!S.prov) return;
  const sv = $("#pd-save-set");
  if (sv) sv.addEventListener("click", async () => {
    if (DEMO || !Go()) { toast("демо: сохранение недоступно", "warn"); return; }
    const name = S.prov.name;
    const ep = {};
    document.querySelectorAll("#provDetail .pd-ep").forEach(s => { ep[s.getAttribute("data-ep")] = s.value; });
    const mr0 = {}; Object.keys(S._mr || {}).forEach(k => { if (S._mr[k] && S._mr[k] !== "auto") mr0[k] = S._mr[k]; });
    const in0 = {
      model_reasoning: mr0,
      key_swap: $("#pd-keyswap").value,
      quota_mode: $("#pd-quota-mode").value,
      timeout_s: Math.max(1, +$("#pd-timeout").value || 60),
      egress: $("#pd-egress").value.trim(),
      error_policy: ep,
      transport_mode: $("#pd-tmode") ? $("#pd-tmode").value : undefined,
      transport_fanout: $("#pd-fanout") ? Math.max(1, +$("#pd-fanout").value || 5) : undefined,
      transport_rounds: $("#pd-rounds") ? Math.max(1, +$("#pd-rounds").value || 2) : undefined,
      transport_cooldown: $("#pd-tcd") ? Math.max(10, +$("#pd-tcd").value || 60) : undefined,
    };
    try {
      const r = await API.UpdateProvider(name, in0);
      toast(r.message || (r.ok ? "сохранено" : "ошибка"), r.ok ? "ok" : "err");
      if (r.ok) loadProviders();
    } catch (e) { toast("Сохранение: " + (e && e.message || e), "err"); }
  });
  // S._mr уже наполнен построчными wire выше (drawer .mdl); инит из сохранённого reasoning.
  S._mr = Object.assign({}, (S.prov && S.prov.model_reasoning) || {}, S._mr || {});
  const rs = $("#pd-reset-def");
  if (rs) rs.addEventListener("click", async () => {
    if (DEMO || !Go()) { toast("демо: сброс недоступен", "warn"); return; }
    const name = S.prov.name;
    try {
      const r = await API.ResetProviderDefaults(name);
      toast(r.message || "сброшено", r.ok ? "ok" : "err");
      if (r.ok) loadProviders();
    } catch (e) { toast("Сброс: " + (e && e.message || e), "err"); }
  });
}
// Подключение: base/type/auth/UA/responses — keyless-пресет прячет auth+keys текстом.
function provConnCard(e) {
  const isKL = (e.auth_kind === "no-auth");
  const sel = (val, opts) => opts.map(k => '<option value="' + k + '"' + (val === k ? " selected" : "") + '>' + k + '</option>').join("");
  return '<div class="card section"><div class="card-head"><h3>Подключение</h3><span class="hint">base_url · auth · UA</span></div>' +
    '<div class="form-grid">' +
    '<div class="field span-all"><label>Base URL</label><input class="input" id="pd-base" value="' + esc(e.base_url || "") + '" placeholder="https://..."></div>' +
    '<div class="field"><label>Auth mode</label><select class="input" id="pd-auth"' + (isKL ? " disabled" : "") + '>' + sel(e.auth_mode || (isKL ? "none" : "bearer"), ["bearer","none","public"]) + '</select>' + (isKL ? '<div class="hint">keyless: auth=none, ключей нет по архитектуре</div>' : '') + '</div>' +
    '<div class="field"><label>User-Agent</label><input class="input" id="pd-ua" value="' + esc(e.user_agent || "") + '" placeholder="opencode/1.18.99"></div>' +
    '</div><div class="card-foot"><button class="btn primary" id="pd-save-conn">Сохранить</button></div></div>';
}
function wireProvConn() {
  if (!S.prov) return;
  const sv = $("#pd-save-conn");
  if (sv) sv.addEventListener("click", async () => {
    if (DEMO || !Go()) { toast("демо: сохранение недоступно", "warn"); return; }
    const in0 = {
      base_url: $("#pd-base").value.trim(),
      user_agent: $("#pd-ua").value.trim(),
    };
    const am = $("#pd-auth"); if (am && !am.disabled) in0.auth_mode = am.value;
    try { const r = await API.UpdateProvider(S.prov.name, in0); toast(r.message || (r.ok ? "сохранено" : "ошибка"), r.ok ? "ok" : "err"); if (r.ok) loadProviders(); }
    catch (e) { toast("Подключение: " + (e && e.message || e), "err"); }
  });
}
 /* удалено: provDiagCard()/wireProvDiag() — употреблений 0 */
function lastModelStatus(m) {
  // Последний вердикт по модели из загруженных логов: 200 -> ok, 429 -> neutral (троттл, не смерть), else err.
  const logs = S.logs || [];
  for (let i = 0; i < logs.length; i++) {
    const l = logs[i];
    if ((l.model || l.requested_model) === m || (l.requested_model || l.model) === m) {
      const st = +l.http_status || 0;
      if (st === 200) return "ok";
      if (st === 429) return "rl";
      if (st) return "err";
    }
  }
  // fallback: tops90 errors (модель ошибалась за 90д, свежести нет — только warn-подсказка)
  return "";
}
function customSet(prov) {
  try { return new Set(JSON.parse(localStorage.getItem("ai-router.custom." + prov) || "[]")); } catch (e) { return new Set(); }
}
function customAdd(prov, name) {
  try {
    const arr = Array.from(customSet(prov)); if (!arr.includes(name)) arr.push(name);
    localStorage.setItem("ai-router.custom." + prov, JSON.stringify(arr));
  } catch (e) {}
}
function mdlFam(m) {
  m = String(m || "").toLowerCase();
  if (/^gpt|o3|o4|codex/.test(m)) return "f-gpt";
  if (/claude/.test(m)) return "f-claude";
  if (/gemini|gemma/.test(m)) return "f-gemini";
  if (/grok/.test(m)) return "f-grok";
  if (/deepseek/.test(m)) return "f-deepseek";
  if (/qwen/.test(m)) return "f-qwen";
  if (/mistral|mixtral/.test(m)) return "f-mistral";
  if (/muse/.test(m)) return "f-muse";
  if (/mimo/.test(m)) return "f-mimo";
  if (/nemotron|laguna|ling-/.test(m)) return "f-nemo";
  return "f-other";
}
const FAM_HEX = {"f-gpt":"#10a37f","f-claude":"#d97757","f-gemini":"#4285f4","f-grok":"#e8e8e8","f-deepseek":"#7c6cf0","f-qwen":"#7b61ff","f-mistral":"#ff7000","f-muse":"#00d4aa","f-mimo":"#ff4d8d","f-nemo":"#76b900","f-other":"#e5484d"};
function provModelRows(e) {
  const mr = e.model_reasoning || {};
  const famMax = m => /x-preview/i.test(m) ? "max" : (/muse-spark|mimo/i.test(m) ? "xhigh" : "auto");
  const LV = ["auto","low","high","xhigh","max"];
  const dis = m => (e.disabled_models||[]).includes(m), hid = m => (e.hidden_models||[]).includes(m);
  const byName = (a, b) => a.m.localeCompare(b.m, "ru", { sensitivity: "base", numeric: true });
  if (S._mdlSort === "iq" || S._mdlSort === "reqs") S._mdlSort = "name";
  const sort = S._mdlSort || "name";
  const rows = (e.models||[]).map(m => {
    const off = dis(m), hd = hid(m);
    const cur = mr[m] || "auto", fm = famMax(m);
    const effIx = Math.max(0, LV.indexOf(cur === "auto" ? (fm === "auto" ? "auto" : fm) : cur));
    return { m, off, hd, cur, fm, effIx, fam: mdlFam(m) };
  });
  if (sort === "status") rows.sort((a, b) => ((a.off ? 1 : 0) - (b.off ? 1 : 0)) || byName(a, b));
  else if (sort === "fam") rows.sort((a, b) => a.fam.localeCompare(b.fam) || byName(a, b));
  else if (sort === "lvl") rows.sort((a, b) => (b.effIx - a.effIx) || byName(a, b));
  else rows.sort(byName);
  S._mdlSel = S._mdlSel || new Set();
  S._mdlCur = S._mdlCur || null;
  if (S._mdlCur && !rows.some(r => r.m === S._mdlCur)) S._mdlCur = null;
  const cur0 = rows.find(r => r.m === S._mdlCur) || rows.find(r => !r.off) || rows[0];
  const disN = rows.filter(r => r.off).length, hidN = rows.filter(r => r.hd && !r.off).length;
  const onN = rows.length - disN - hidN;
  const cnt = '<span class="hint">включено ' + onN + ' из ' + rows.length + (hidN ? ' · скрыто ' + hidN : '') + '</span>';
  const sbtn = (id, lb, t) => '<button class="btn sm' + (sort === id ? " on" : "") + '" data-mdlsort="' + id + '" title="' + esc(t || lb) + '">' + lb + '</button>';
  const _cs = customSet(e.name);
  const chips = rows.map(r => {
    const isResp = (e.responses_models || []).includes(r.m);
    const isCustom = _cs.has(r.m);
    const _ls = lastModelStatus(r.m);
    return '<span class="mchip ' + r.fam + (r.off ? " off" : "") + (r.hd ? " hd" : "") + (isCustom ? " mcustom" : "") + (_ls === "ok" ? " last-ok" : _ls === "err" ? " last-err" : "") + (S._mdlSel.has(r.m) ? " sel" : "") + (S._mdlCur === r.m ? " cur" : "") + '" data-chip="' + esc(r.m) + '" data-fam="' + r.fam + '" title="клик — капля несёт модель в плашку ниже · точка — вкл/выкл">' +
      '<span class="dot" data-mdot title="вкл/выкл маршрутизации"></span>' +
      '<span class="nm">' + esc(r.m) + '</span>' +
      (isResp ? '<span class="r">R</span>' : '') +
      (isCustom ? '<span class="r c" title="кастомная: добавлена вручную, не убирается при обновлении">C</span>' : '') +
      (r.hd ? '<button class="eyebtn" data-meye title="скрыта из каталога — нажмите чтобы показать">скрыта</button>' : '') + '</span>';
  }).join("");
  const chipsWrap = '<div class="mchips mpool">' + chips + '</div>';
  const mselBar = '<div class="pk-bulk" id="mdl-bulk"><span class="cnt" id="mdl-bulk-cnt"></span><button class="btn sm primary" data-mbulk="test">▶ тест</button><button class="btn sm" data-mbulk="on">вкл</button><button class="btn sm" data-mbulk="off">выкл</button><button class="btn sm ghost" data-mbulk="clear">снять выделение</button></div>';
  const poolCard = '<div class="card section"><div class="card-head"><h3>Модели</h3>' + cnt + '<span class="spacer"></span><button class="btn sm" id="pd-models-refresh" title="обновить из /models: новые добавятся, выключенные/скрытые сохранятся">↻ обновить из каталога</button></div>' + mselBar +
    '<div class="hint" style="margin-bottom:8px">клик — капля несёт модель в плашку ниже · ● точка — маршрутизация (выкл → 404, disabled_models) · 👁 глаз — каталог (скрыта, hidden_models) · цвет точки=семейство · R=responses · Выбрать/ctrl/долгий клик — выделение</div>' +
    '<div class="mdl-sort" style="margin-bottom:8px"><input class="input sm" id="pd-model-q" placeholder="Найти модель…" autocomplete="off" style="flex:1;min-width:140px"><span class="hint" id="pd-model-count"></span>' + sbtn("name", "А–Я", "по имени") + sbtn("status", "Статус") + sbtn("fam", "Семья", "по семейству (цвет)") + sbtn("lvl", "Сила", "по силе reasoning") + '<span class="spacer"></span><button class="btn sm" id="pd-selmode" title="режим выделения: клик/ctrl/долгий клик — выбрать">Выбрать</button><button class="btn sm" id="pd-sel-all" title="выделить все видимые">все</button><button class="btn sm" id="pd-all-on">Всё вкл</button><button class="btn sm" id="pd-all-off">Всё выкл</button></div>' +
    '<div class="mpool-wrap">' + chipsWrap + '</div>' +
    '<div class="card-foot" style="display:flex;gap:8px"><input class="input sm" id="pd-custom-add" placeholder="добавить модель… (имя, Enter — добавить)" style="flex:1;min-width:200px"><button class="btn sm" id="pd-custom-go" title="Кастомная модель: сохраняется в списке, не убирается при обновлении">＋ добавить модель</button></div>' +
    '</div>';
  return { rows, cur0, LV, poolCard };
}
function provModelsCard(e) {
  if (!e.models || !e.models.length) {
    const ban = '<div class="ban-row" style="margin-top:8px;display:flex;gap:8px"><input class="input sm" id="pd-ban-name" placeholder="имя модели для запрета"><button class="btn sm danger" id="pd-ban-add" title="добавить имя в hidden_models">запретить</button></div>';
    return '<div class="card section"><div class="card-head"><h3>Модели</h3><span class="hint">wildcard — любая модель</span></div>' +
      '<span class="model-pill wc">любая модель (wildcard)</span>' + ((e.disabled_models||[]).length ? '<div class="hint" style="margin-top:8px">отключены (404, disabled_models):</div><div class="model-list">' + (e.disabled_models||[]).map(m => '<span class="model-pill off" title="' + esc(m) + ' — отключена (404)">' + esc(m) + '</span>').join("") + '</div>' : '') + ((e.hidden_models||[]).length ? '<div class="hint" style="margin-top:8px">скрыты из каталога (hidden_models):</div><div class="model-list">' + (e.hidden_models||[]).map(m => '<span class="model-pill hid" title="' + esc(m) + ' — скрыта из каталога">' + esc(m) + '</span>').join("") + '</div>' : '') + ban + '</div>';
  }
  return provModelRows(e).poolCard;
}
function provModelStageCard(e) {
  if (!e.models || !e.models.length) return '';
  const { cur0, LV } = provModelRows(e);
  const r = cur0; if (!r) return '';
  const lbl = r.cur === "auto" ? ("auto→" + r.fm) : r.cur;
  const pct = Math.round(r.effIx / 4 * 100);
  const isResp = (e.responses_models || []).includes(r.m);
  const fam0 = r.fam || "f-other";
  const hex0 = (typeof FAM_HEX !== "undefined" && FAM_HEX[fam0]) || "#e5484d";
  const stg = (() => {
    return '<div class="mdl-stage st-in" id="mdl-stage" data-mrow="' + esc(r.m) + '">' +
      '<div class="mdl-stage-head"><span class="mchip ' + r.fam + ' cur" style="cursor:default"><span class="dot"></span><span class="nm">' + esc(r.m) + '</span>' + (isResp ? '<span class="r">R</span>' : '') + (customSet(e.name).has(r.m) ? '<span class="r c" title="кастомная: добавлена вручную, не убирается при обновлении">C</span>' : '') + '</span>' +
      '<span class="st ' + (r.off ? "off" : "on") + '">' + (r.off ? "выкл" : r.hd ? "скрыта" : "вкл") + '</span><span class="spacer"></span>' +
      '<button class="btn sm" data-mdl-hide>' + (r.hd ? "👁 показать" : "👁 скрыть") + '</button>' +
      '<button class="btn sm primary" data-mdl-test>▶ тест модели</button></div>' +
      '<div class="mdl-stage-body"><div class="stg-grid">' +
      '<div><div class="kx lvl' + r.effIx + '" data-kx>' +
      
      '<div class="kx-wrap"><input type="range" min="0" max="4" step="1" value="' + r.effIx + '" data-rsl data-dots="5" aria-label="уровень рассуждений ' + esc(r.m) + '"><div class="kx-dots" aria-hidden="true"><i></i><i></i><i></i><i></i><i></i></div></div>' +
      '<div class="kx-lbl"><span class="chip" data-rsl-lb>' + esc(lbl) + '</span></div></div></div>' +
      '<div><div class="hint">семья: ' + esc(r.fm) + ' · клиент > GUI > auto</div>' +
      '<div class="mdl-probe" style="margin:6px 0"><button class="btn sm" data-mdl-probe title="1 ping chat: при ошибке сам допингует responses и предложит записать">◉ авто-responses</button><span class="hint" data-probe-out></span></div>' +
      '<label class="hint" style="display:flex;gap:6px;align-items:center;cursor:pointer"><input type="checkbox" data-mdl-resp' + (isResp ? " checked" : "") + '> responses-модель <span class="hint">(/responses)</span></label></div>' +
      '</div></div></div>';
  })();
  return '<div class="card section mstage-card" data-mstage style="margin-top:0"><div class="card-head"><h3>Настройки модели</h3><span class="hint">продолжение</span></div>' +
    '<div class="mdrop-sep" style="background:linear-gradient(90deg,transparent,' + hex0 + ',transparent)"></div>' +
    '<div class="mstage" style="margin-top:4px">' + stg + '</div></div>';
}
// Карточка «Настройки провайдера»: свап ключей, политика ошибок, таймаут,
// Карточка «Настройки провайдера»: свап ключей, политика ошибок, таймаут,
// egress + Сохранить / Вернуть к дефолтам. Проводка — в wireSettingsProv.
function provSettingsCard(e) {
  const D = (getF(S.cfg, "provider_defaults") || {});
  const ks = e.key_swap || D.key_swap || "round_robin";
  const qm = e.quota_mode || D.quota_mode || "auto";
  const ep = Object.assign({}, D.error_policy || {});
  const SW = ["round_robin", "random", "least_used", "best_latency"];
  const AC = ["none", "retry", "retry_proxy", "retry_token"];
  const ERR = [["401", "неверный/истёкший ключ"], ["429", "лимит запросов"], ["5xx", "сбой сервера"], ["timeout", "таймаут"], ["network", "сеть недоступна"], ["rate_limit", "ограничение скорости"]];
  const sel = (val, opts) => opts.map(k => '<option value="' + k + '"' + (val === k ? " selected" : "") + '>' + k + '</option>').join("");
  return '<div class="pd-settings-wrap span-all">' +
    '<div class="card section"><div class="card-head"><h3>Ротация ключей</h3><span class="hint">как выбирать ключ из пула</span></div>' +
    '<div class="form-grid">' +
    '<div class="field"><label>Методика свапа</label><select class="input" id="pd-keyswap" aria-label="Методика свапа">' + sel(ks, SW) + '</select><div class="hint">least_used · random · round_robin · best_latency</div></div>' +
    '<div class="field"><label>Транспорт</label><select class="input" id="pd-tmode" aria-label="Транспорт">' + sel((e.transport && e.transport.mode) || "off", ["off","pool","warp","iprotate","direct"]) + '</select><div class="hint">pool=пул warp-router (:8787, ротация IP) · warp=warp напрямую (:8118) · off=напрямую с IP сервера</div></div>' +
    '<div class="field"><label>Fanout × Rounds</label><div style="display:flex;gap:6px;flex-wrap:wrap;min-width:0"><input class="input" id="pd-fanout" type="number" min="1" max="10" value="' + ((e.transport && e.transport.fanout) || 5) + '" title="параллельных egress" style="flex:1 1 90px;min-width:0"><input class="input" id="pd-rounds" type="number" min="1" max="5" value="' + ((e.transport && e.transport.rounds) || 2) + '" title="раундов" style="flex:1 1 90px;min-width:0"></div><div class="hint">5×2=10 попыток на 429</div></div>' +
    '<div class="field"><label>Cooldown egress (с)</label><input class="input" id="pd-tcd" type="number" min="10" value="' + ((e.transport && e.transport.cooldown_s) || 60) + '"><div class="hint">429 банит egress на это время</div></div>' +
    '</div></div>' +
    '<div class="card section" style="margin-top:12px"><div class="card-head"><h3>Сеть и лимиты</h3></div>' +
    '<div class="form-grid">' +
    '<div class="field"><label>Режим квоты</label><select class="input" id="pd-quota-mode" title="режим отображения квоты">' + sel(qm, ["auto","usd","tokens","requests"]) + '</select></div>' +
    '<div class="field"><label>Таймаут (сек)</label><input class="input" id="pd-timeout" type="number" min="1" value="' + (e.timeout_s || 60) + '"></div>' +
    '<div class="field span-all"><label>Egress (прокси URL)</label><input class="input" id="pd-egress" value="' + esc(e.egress || "") + '" placeholder="http://127.0.0.1:8118 (пусто = транспорт выше решает)"></div>' +
    '</div></div>' +
    '<div class="card section" style="margin-top:12px"><div class="card-head"><h3>Что делать при ошибке</h3><span class="hint">таблица код → действие, компактно</span></div>' +
    '<div class="errtbl">' + ERR.map(([code, why]) => {
      const v = (e.error_policy && e.error_policy[code]) || ep[code] || "retry";
      return '<div class="erow"><code>' + code + '</code><span class="hint">' + why + '</span><select class="input sm pd-ep" data-ep="' + code + '">' + sel(v, AC) + '</select></div>';
    }).join("") + '</div>' +
    '<div class="card-foot">' +
    '<button class="btn" id="pd-reset-def">Вернуть к дефолтам</button>' +
    '<button class="btn primary" id="pd-save-set">Сохранить</button></div></div></div>';
}
// API-ключи пула провайдера: маскированные, тумблер вкл/выкл, кулдаун.
function renderPoolKeys() {  const c = $("#pd-poolkeys");
  if (!c || !S.prov) return; // keyless: контейнера нет — нечего рисовать
  if (DEMO || !Go()) {
    const mocks = [
      { hash: "d1", masked: "sk-demo…0001", enabled: true, backoff_secs: 0, failures: 0, default: true, proxy: "" },
      { hash: "d2", masked: "sk-demo…0002", enabled: true, backoff_secs: 12, failures: 2, default: false, proxy: "egress" },
      { hash: "d3", masked: "sk-demo…0003", enabled: false, backoff_secs: 0, failures: 0, default: false, proxy: "" },
    ];
    S._keySel = S._keySel || new Set();
    c.innerHTML = '<div class="card section"><div class="card-head"><h3>Исходящие · ' + mocks.length + '</h3><span class="hint">ключи пула: исходящие запросы, кулдаун локальный; галочка=ротация, ⭐=по дефолту (демо)</span><span class="spacer"></span><button class="btn sm ghost" id="pk-sel-all" title="выделить все ключи">выделить все</button></div>' +
      '<div class="pk-bulk" id="pk-bulk"><span class="cnt" id="pk-bulk-cnt"></span><button class="btn sm" data-bulk="on">вкл</button><button class="btn sm" data-bulk="off">выкл</button><button class="btn sm ghost" data-bulk="clear">снять</button></div>' +
      '<div class="card section"><div class="card-head"><h3>Добавить ключи</h3><span class="hint">bulk-вставка: каждый с новой строки (демо — не сохраняет)</span></div><textarea class="input" id="pk-bulk-add" rows="2" placeholder="sk-..." aria-label="Добавить ключи, каждый с новой строки"></textarea><div class="card-foot"><button class="btn sm primary" id="pk-add-go">Добавить</button></div></div>' +
      '<div class="keys-scroll">' + mocks.map(k =>
      '<div class="kv-row' + (k.enabled ? "" : " is-off") + ((k.backoff_secs || 0) > 0 ? " is-cd" : "") + '" data-hash="' + k.hash + '">' +
      '<div class="kv-head" data-kvhead title="нажмите — настройки ключа выдвигаются вниз">' +
      '<input type="checkbox" class="pk-sel" title="выделить ключ" aria-label="Выделить ключ">' +
      '<div class="kv-key mono">' + (k.default ? '<span class="star" title="по дефолту" aria-label="ключ по умолчанию">★</span> ' : '') + esc(k.masked) + '</div>' +
      '<button class="chip pk-hash" data-hash="' + k.hash + '" title="нажмите — скопировать hash ключа">#' + esc(String(k.hash || "").slice(0, 8)) + '</button>' +
      (k.backoff_secs > 0 ? '<span class="badge warn kv-cd-title">кулдаун <b class="pd-cd-live" data-cd="' + k.backoff_secs + '">' + fmtMmSs(k.backoff_secs) + '</b></span>' : (k.failures > 0 ? '<span class="badge off">' + k.failures + ' сбоев</span>' : '<span class="badge on">чист</span>')) +
      '<button class="icon-btn pk-def" title="по дефолту" style="' + (k.default ? "color:var(--warn)" : "") + '">★</button>' +
      '<span class="chev">▸</span></div>' +
      '<div class="kdrawer"><div class="kd-grid">' +
      '<div class="field"><label>Ротация</label><div class="hint">' + (k.enabled ? "в ротации" : "выключен") + ' · демо</div></div>' +
      '<div class="field"><label>Прокси ключа</label><div class="hint">' + esc(k.proxy || "провайдерский") + '</div></div>' +
      '<div class="field"><label>Действия</label><div class="hint">демо: тест/лог недоступны</div></div>' +
      '</div></div></div>').join("") + '</div></div>';
    c.querySelectorAll("[data-kvhead]").forEach(h => h.addEventListener("click", ev => { if (ev.target && ev.target.closest("input,select,button")) return; h.closest(".kv-row").classList.toggle("open"); }));
    c.querySelectorAll(".pk-sel").forEach(x => x.addEventListener("click", ev => ev.stopPropagation()));
    const bulk = $("#pk-bulk"), bcnt = $("#pk-bulk-cnt");
    const paintBulk = () => { const n2 = c.querySelectorAll(".pk-sel:checked").length; bulk.classList.toggle("on", n2 > 0); bcnt.textContent = n2 ? ("выбрано: " + n2) : ""; };
    c.querySelectorAll(".pk-sel").forEach(ch => ch.addEventListener("change", paintBulk));
    const sa = $("#pk-sel-all"); if (sa) sa.addEventListener("click", () => { const boxes = Array.from(c.querySelectorAll(".pk-sel")); const all = boxes.every(x => x.checked); boxes.forEach(x => { x.checked = !all; }); paintBulk(); });
    bulk.querySelectorAll("[data-bulk]").forEach(b => b.addEventListener("click", () => { if (b.getAttribute("data-bulk") === "clear") { c.querySelectorAll(".pk-sel").forEach(x => { x.checked = false; }); paintBulk(); } else toast("демо: bulk не сохраняет", "warn"); }));
    const ago = $("#pk-add-go"); if (ago) ago.addEventListener("click", () => toast("демо: добавление не сохраняет", "warn"));
    return;
  }
  API.PoolKeys(S.prov.name).then(rows => {
    if (S.prov && S.prov.name === "__GLOBAL__") { c.innerHTML = '<div class="card section"><div class="card-head"><h3>Исходящие</h3><span class="hint">ключи пула: исходящие запросы</span></div><div class="state"><div class="hint">у __GLOBAL__ нет пула — исходящие ключи живут на провайдерах</div></div></div>'; return; }
    if (!rows || !rows.length) { c.innerHTML = '<div class="card section"><div class="card-head"><h3>Исходящие</h3><span class="hint">ключи пула: исходящие запросы, кулдаун локальный</span></div><div class="state"><div class="hint">у провайдера нет API-ключей</div></div></div>'; return; }
    // Доступные прокси-каналы для селекта «прокси на ключе».
    const proxies = (S.health || []).filter(h => h.type === "proxy" && h.ok).map(h => h.name);
    const pOpts = '<option value="">провайдерский</option>' + proxies.map(p => '<option value="' + esc(p) + '">' + esc(p) + '</option>').join("");
    S._keySel = S._keySel || new Set();
    c.innerHTML = '<div class="card section"><div class="card-head"><h3>Исходящие · ' + rows.length + '</h3><span class="hint">ключи пула: исходящие запросы, кулдаун локальный; галочка=ротация, ⭐=по дефолту</span><span class="spacer"></span><button class="btn sm ghost" id="pk-sel-all" title="выделить все ключи">выделить все</button><button class="btn sm" id="pk-copy-all" title="скопировать список масок">копировать всё</button></div>' +
      '<div class="pk-bulk" id="pk-bulk"><span class="cnt" id="pk-bulk-cnt"></span><button class="btn sm primary" data-bulk="test-all" title="Прогнать все ключи разом: whoami(models)+пинг, какие ответили/нет и с какой ошибкой">▶ тест всех</button><button class="btn sm" data-bulk="on">вкл</button><button class="btn sm" data-bulk="off">выкл</button><button class="btn sm" data-bulk="proxy">прокси…</button><button class="btn sm ghost" data-bulk="clear">снять</button></div>' +
      '<div class="card section"><div class="card-head"><h3>Добавить ключи</h3><span class="hint">bulk-вставка: каждый с новой строки</span></div><textarea class="input" id="pk-bulk-add" rows="2" placeholder="sk-..." aria-label="Добавить ключи, каждый с новой строки"></textarea><div class="card-foot"><button class="btn sm primary" id="pk-add-go">Добавить</button><button class="btn sm ghost" id="pk-exp">экспорт масок</button></div></div>' +
      '<div class="keys-scroll">' + rows.map(k =>
      '<div class="kv-row' + (k.enabled ? "" : " is-off") + ((k.backoff_secs || 0) > 0 ? " is-cd" : "") + '" data-hash="' + esc(k.hash) + '">' +
      '<div class="kv-head" data-kvhead title="нажмите — настройки ключа выдвигаются вниз">' +
      '<input type="checkbox" class="pk-sel" title="выделить ключ" aria-label="Выделить ключ" ' + (S._keySel.has(k.hash) ? "checked" : "") + '>' +
      '<div class="kv-key mono" data-masked-full="' + esc(k.masked) + '">' + (k.default ? '<span class="star" title="по дефолту" aria-label="ключ по умолчанию">★</span> ' : '') + '<span class="mk">' + esc(k.masked) + '</span></div>' +
      '<button class="chip pk-hash" data-hash="' + esc(k.hash) + '" title="нажмите — скопировать hash ключа">#' + esc(String(k.hash || "").slice(0, 8)) + '</button>' +
      '<button class="icon-btn pk-def" title="по дефолту (приоритетный ключ)" style="' + (k.default ? "color:var(--warn)" : "") + '">★</button>' +
      ((k.backoff_secs || 0) > 0 ? '<span class="badge warn kv-cd-title">кулдаун <b class="pd-cd-live" data-cd="' + esc(k.backoff_secs) + '">' + esc(fmtMmSs(k.backoff_secs)) + '</b></span>' : ((k.failures || 0) > 0 ? '<span class="badge off" title="сбои за сессию">' + esc(k.failures) + ' сбоев</span>' : '<span class="badge on">чист</span>')) +
      '<span class="chev">▸</span></div>' +
      '<div class="kdrawer"><div class="kd-grid">' +
      '<div class="field"><label>Ротация</label><label class="switch" title="использовать ключ при ротации"><input type="checkbox" class="sw pk-en" ' + (k.enabled ? "checked" : "") + '><span class="trk"></span><span class="sw-lbl">' + (k.enabled ? "вкл" : "выкл") + '</span></label></div>' +
      '<div class="field"><label>Прокси ключа</label><select class="input sm pk-proxy" style="width:100%">' + pOpts.replace('value="' + esc(k.proxy || "") + '"', 'value="' + esc(k.proxy || "") + '" selected') + '</select></div>' +
      '<div class="field"><label>Действия</label><div style="display:flex;gap:6px;flex-wrap:wrap"><button class="btn sm pk-test">▶ тест</button><button class="btn sm ghost pk-show">маска</button><button class="btn sm ghost pk-log">лог</button></div>' +
      '<div class="hint" style="margin-top:6px">' + (k.backoff_secs > 0 ? ("кулдаун " + k.backoff_secs + "с") : "кулдаун чист") + ' · сбоев: ' + (k.failures || 0) + '</div></div>' +
      '</div></div></div>').join("") + '</div></div>';
    c.querySelectorAll(".pk-en").forEach(s => {
      s.addEventListener("change", () => {
        const row = s.closest(".kv-row"), hash = row.getAttribute("data-hash");
        API.SetKeyEnabled(S.prov.name, hash, s.checked).then(r => { toast(r.message || "ok", r.ok ? "ok" : "err"); s.nextElementSibling.textContent = s.checked ? "вкл" : "выкл"; }).catch(e => toast("ключ: " + e, "err"));
      });
    });
    c.querySelectorAll(".pk-def").forEach(b => {
      b.addEventListener("click", () => {
        const row = b.closest(".kv-row"), hash = row.getAttribute("data-hash");
        API.SetKeyDefault(S.prov.name, hash).then(r => { toast(r.message || "ok", r.ok ? "ok" : "err"); renderPoolKeys(); }).catch(e => toast("дефолт: " + e, "err"));
      });
    });
    c.querySelectorAll(".pk-show").forEach(b => b.addEventListener("click", () => {
      const el = b.closest(".kv-row").querySelector(".kv-key");
      const mk = el.querySelector(".mk");
      const full = el.getAttribute("data-masked-full"), strict = maskStrict(full);
      const shown = (mk ? mk.textContent : el.textContent) !== strict;
      if (mk) mk.textContent = shown ? strict : full;
      else el.innerHTML = esc(shown ? strict : full);
      b.textContent = shown ? "показать" : "маска";
      b.setAttribute("aria-pressed", String(!shown));
    }));
    const ca = $("#pk-copy-all"); if (ca) ca.addEventListener("click", () => copyTo(rows.map(k => (k.hash + "  " + k.masked)).join("\n")));
    c.querySelectorAll(".pk-hash").forEach(h => h.addEventListener("click", ev => { ev.stopPropagation(); copyTo(h.getAttribute("data-hash") || ""); }));
    c.querySelectorAll(".pk-proxy").forEach(s => {
      if (!s.options.length || (s.options.length === 1 && !s.options[0].value)) s.disabled = true; // пустой список — селект недоступен
      s.addEventListener("change", () => {
        const row = s.closest(".kv-row"), hash = row.getAttribute("data-hash");
        const prev = s.getAttribute("data-prev") || "";
        API.SetKeyProxy(S.prov.name, hash, s.value).then(r => { toast(r.message || "ok", r.ok ? "ok" : "err"); if (r.ok) s.setAttribute("data-prev", s.value); else s.value = prev; }).catch(e => { toast("прокси: " + e, "err"); s.value = prev; });
      });
      s.setAttribute("data-prev", s.value);
    });
    c.querySelectorAll("[data-kvhead]").forEach(h => h.addEventListener("click", ev => {
      if (ev.target && ev.target.closest("input,select,button")) return;
      h.closest(".kv-row").classList.toggle("open");
    }));
    c.querySelectorAll(".pk-sel").forEach(x => x.addEventListener("click", ev => ev.stopPropagation()));
    // Bulk: липкая панель при выделении.
    const bulk = $("#pk-bulk"), bcnt = $("#pk-bulk-cnt");
    const paintBulk = () => {
      const n = c.querySelectorAll(".pk-sel:checked").length;
      if (bulk) bulk.classList.toggle("on", n > 0); if (bcnt) bcnt.textContent = n ? ("выбрано: " + n) : "";
      c.querySelectorAll(".kv-row").forEach(r => { const x = r.querySelector(".pk-sel"); setCls(r, x && x.checked, "sel"); });
    };
    c.querySelectorAll(".pk-sel").forEach(ch => ch.addEventListener("change", () => {
      const h = ch.closest(".kv-row").getAttribute("data-hash");
      if (ch.checked) S._keySel.add(h); else S._keySel.delete(h);
      paintBulk();
    }));
    paintBulk();
    const sa = $("#pk-sel-all"); if (sa) sa.addEventListener("click", () => {
      const boxes = Array.from(c.querySelectorAll(".pk-sel"));
      const all = boxes.every(x => x.checked);
      boxes.forEach(x => { x.checked = !all; const h = x.closest(".kv-row").getAttribute("data-hash"); if (!all) S._keySel.add(h); else S._keySel.delete(h); });
      paintBulk();
    });
    bulk.querySelectorAll("[data-bulk]").forEach(b => b.addEventListener("click", async () => {
      const act = b.getAttribute("data-bulk");
      const hs = Array.from(c.querySelectorAll(".pk-sel:checked")).map(x => x.closest(".kv-row").getAttribute("data-hash"));
      if (act === "clear") { S._keySel.clear(); paintBulk(); return; }
      if (!hs.length) return;
      if (act === "on" || act === "off") {
        const en = act === "on";
        for (const h of hs) { try { await API.SetKeyEnabled(S.prov.name, h, en); } catch (e) {} }
        toast("Ключи " + (en ? "включены" : "выключены") + ": " + hs.length, "ok"); renderPoolKeys(); return;
      }
      if (act === "proxy") {
        const px = prompt("Прокси для " + hs.length + " ключей (пусто = снять):", "");
        if (px === null) return;
        for (const h of hs) { try { await API.SetKeyProxy(S.prov.name, h, px.trim()); } catch (e) {} }
        toast("Прокси обновлён: " + hs.length, "ok"); renderPoolKeys(); return;
      }
      if (act === "test-all") {
        // Тест всех разом: 1 живой пинг (модели стейджа) + построчный отчет по КАЖДОМУ ключу из rows:
        // кто в ротации, кто в кулдауне/сбоях — без N запросов (ключи пула не пингуются по одному, ротация на бэке).
        const w = modal("Тест всех ключей · " + S.prov.name, '<div class="state"><div class="big" aria-hidden="true">↻</div><div class="title">Пингуем…</div><div class="hint">1 живой ping + whoami /models по всем ключам разом</div></div>');
        try {
          const m = (S._mdlCur && (S.prov.models || []).includes(S._mdlCur)) ? S._mdlCur : undefined;
          const [t, sy] = await Promise.allSettled([
            API.Test(S.prov.name + "/" + (m || ((S.prov.models || []).filter(x => !(S.prov.disabled_models || []).includes(x))[0] || ""))),
            API.SyncModels(S.prov.name),
          ]);
          const ping = t.status === "fulfilled" ? t.value : { error: String(t.reason && t.reason.message || t.reason) };
          const who = sy.status === "fulfilled" ? sy.value : { error: String(sy.reason && sy.reason.message || sy.reason) };
          const pok = !ping.error && ping.status < 400;
          const wok = who && who.ok;
          const line = k => {
            const st = !k.enabled ? ["off", "выкл (вне ротации)"] : (k.backoff_secs > 0 ? ["warn", "кулдаун " + k.backoff_secs + "с"] : (k.failures > 0 ? ["warn", "сбоев: " + k.failures] : ["on", "в ротации"]));
            return '<div class="test-line"><code>' + esc(k.masked || k.hash) + '</code> <span class="badge ' + st[0] + '">' + esc(st[1]) + '</span></div>';
          };
          w.close();
          modal("Тест всех · " + S.prov.name + " (" + rows.length + ")", '<div class="' + (pok ? "test-ok" : "test-err") + '">'
            + '<div class="test-line">' + (pok ? '<span class="badge on">пинг ok</span>' : '<span class="badge err">пинг ошибка</span>') + ' · <span class="chip">' + (ping.latency_ms || 0) + ' мс</span>'
            + (wok ? ' · <span class="chip">/models ok</span>' : ' · <span class="chip">/models ошибка</span>') + '</div>'
            + (pok ? "" : '<div class="test-out err">' + esc(ping.error || ("HTTP " + ping.status)) + '</div>')
            + (wok ? "" : '<div class="test-out err">' + esc(who.message || who.error || "/models недоступен") + '</div>')
            + '<div class="hint" style="margin:8px 0 4px">ключи (' + rows.length + '):</div>'
            + rows.map(line).join("") + '</div>');
          toast("Тест всех: пинг " + (pok ? "ok" : "ошибка") + ", /models " + (wok ? "ok" : "ошибка"), pok && wok ? "ok" : "err");
        } catch (e) { try { w.close(); } catch (err) {} toast("Тест всех: " + (e && e.message || e), "err"); }
        return;
      }
    }));
    // Bulk-вставка + экспорт масок.
    const ago = $("#pk-add-go"); if (ago) ago.addEventListener("click", async () => {
      const ta = $("#pk-bulk-add"); const vals = (ta.value || "").split("\n").map(x => x.trim()).filter(Boolean);
      if (!vals.length || !S.prov) return;
      try {
        const r = await API.AddProviderKeys(S.prov.name, vals);
        toast(r.message || "ok", r.ok ? "ok" : "err"); if (r.ok) loadProviders();
      } catch (e) { toast("Добавление: " + (e && e.message || e), "err"); }
    });
    const pkAdd = $("#pk-bulk-add");
    if (pkAdd) pkAdd.addEventListener("keydown", e => { if (e.key === "Enter" && !e.shiftKey) { e.preventDefault(); try { ago.click(); } catch (err) {} } });
    const pexp = $("#pk-exp"); if (pexp) pexp.addEventListener("click", () => copyTo(rows.map(k => k.masked).join("\n")));
    c.querySelectorAll(".pk-test").forEach(b => b.addEventListener("click", () => { try { const m = (S._mdlCur && (S.prov.models || []).includes(S._mdlCur)) ? S._mdlCur : undefined; testProvider(m); } catch (e) { testProvider(); } }));
    c.querySelectorAll(".pk-log").forEach(b => b.addEventListener("click", () => { try { gotoLogsQuery(S.prov.name); } catch (e) {} }));
  }).catch(e => c.innerHTML = '<div class="card section"><div class="card-head"><h3>Исходящие</h3></div><div class="state"><div class="hint">ошибка: ' + esc(e) + '</div></div></div>');
}
function renderProvKeys(c, keys) {
  const pname = S.prov && S.prov.name;
  if (!keys || !keys.length) {
    c.innerHTML = '<div class="card section"><div class="card-head"><h3>Ключи</h3><span class="hint">виртуальные ключи, направленные на провайдера</span></div>' +
      '<div class="state"><div class="big" aria-hidden="true">○</div><div class="title">Нет виртуальных ключей</div><div class="hint">Ключи с allowlist на модели этого провайдера появятся здесь. Соответствие — majority-match по списку моделей, подсчёт локальный; раздел «Настройки → keys».</div></div></div>';
    return;
  }
  const qm = (S.prov && S.prov.quota_mode) || "auto";
  // Backend pct: доля 0..1, -1 = безлимит (quota<=0). null vs -1/ложь различать нельзя —
  // оба означают «лимита нет». dim-fallback поэтому честный: null/false/-1 = unlimited.
  const hasLim = v => v != null && v !== false && isFinite(v) && v >= 0;
  const dim = k => { const o = [["токены", k.token_pct], ["запросы", k.req_pct], ["USD", k.usd_pct]]; const pick = v => hasLim(v) ? v : -1; const live = o.filter(x => hasLim(x[1])); const fb = live.length ? live[0][1] : -1; if (qm === "usd") return [hasLim(k.usd_pct) ? "USD" : "безлимит", hasLim(k.usd_pct) ? pick(k.usd_pct) : -1]; if (qm === "tokens") return [hasLim(k.token_pct) ? "токены" : "безлимит", hasLim(k.token_pct) ? pick(k.token_pct) : -1]; if (qm === "requests") return [hasLim(k.req_pct) ? "запросы" : "безлимит", hasLim(k.req_pct) ? pick(k.req_pct) : -1]; const d = live[0]; return d || ["безлимит", fb]; };
  const worst = k => Math.max(hasLim(k.token_pct) ? k.token_pct : -1, hasLim(k.req_pct) ? k.req_pct : -1, hasLim(k.usd_pct) ? k.usd_pct : -1);
  const worstBadge = k => { const w = worst(k); if (w >= 1) return ' <span class="badge err" title="квота исчерпана">исчерпан</span>'; if (w >= 0.9) return ' <span class="badge warn" title="осталось <10% квоты — худшая из трёх (токены/запросы/USD)">осталось &lt;10%</span>'; return ''; };
  // нет ни одной квоты/данных о расходе → полосу не рисуем, показываем подсказку
  const trackable = ks => ks.some(k => hasLim(k.token_pct) || hasLim(k.req_pct) || hasLim(k.usd_pct));
  S.pdKeys = keys; // источник доната = гейт: входящие ключи 2б
  const hasUsage = keys.some(k => (k.usage_usd || 0) > 0 || (k.budget_usd || 0) > 0);
  c.innerHTML = '<div class="card section"><div class="card-head"><h3>Входящие · ' + keys.length + '</h3><span class="hint">виртуальные ключи: локальный подсчёт, апстрим расход не отдаёт</span></div>' + (hasUsage ? '<div class="pd-donut-row"><canvas id="pd-donut" height="150" width="150" role="img" aria-label="Кольцо расхода квоты"></canvas><div class="pd-donut-cap" id="pd-donut-cap"></div></div>' : '') + (!trackable(keys) ?
    '<div class="state" style="padding:10px"><div class="hint">Расход по квотам не отслеживается (у провайдера нет данных о расходе, подсчёт локальный по ключам ниже). Задайте лимит ключу (✎) — полоса использования появится.</div></div>' : '') +
    '<div class="keys-scroll">' + keys.map(k => {
      const d = dim(k), qb = qBar(d ? d[1] : null);
      const dl = "по " + (d ? d[0] : "квоте") + " · " + qb.label;
      const wv = worst(k);
      return '<div class="kv-row' + (k.revoked ? ' is-rev' : '') + '" data-key="' + esc(k.name) + '">' +
        '<div class="kv-main"><div class="kv-name">' + esc(k.name) +
        (k.revoked ? ' <span class="badge err">отозван</span>' : '') + worstBadge(k) +
        (k.no_log ? ' <span class="badge off">no_log</span>' : '') + '</div>' +
        '<div class="kv-key">' + esc(k.key) + '</div>' +
        ((k.models && k.models.length) ? '<div class="model-list" style="margin-top:6px">' + k.models.map(m => '<span class="model-pill">' + esc(m) + '</span>').join("") + '</div>' : '') +
        '</div>' +
        '<div class="kv-quota"><div class="qlabel"><span>' + esc(d ? d[0] : "квота") + '</span><span>' + esc(dl) + '</span></div>' +
        (d && hasLim(d[1]) ? '<div class="progress" role="progressbar" aria-label="' + esc(d[0]) + '" aria-valuemin="0" aria-valuemax="100" aria-valuenow="' + qb.w + '" aria-valuetext="' + esc(qb.label) + '"><div class="bar ' + qb.cls + '" style="width:' + qb.w + '%"></div></div>' : '<div class="progress" title="безлимит"><div class="bar ' + qb.cls + '" style="width:' + qb.w + '%"></div></div>') +
        '<div class="qlabel" style="margin-top:4px"><span>USD ' + $c(k.usage_usd) + ' / ' + (k.budget_usd > 0 ? $c(k.budget_usd) : "∞") + '</span><span>' + (k.budget_usd > 0 ? 'осталось ' + $c(Math.max(0, k.budget_usd - (k.usage_usd || 0))) : '') + '</span></div>' +
        '<div class="qlabel" style="margin-top:2px"><span>рек. ' + nFmt(k.usage_reqs) + (k.quota_reqs > 0 ? ' / ' + nFmt(k.quota_reqs) : '') + '</span><span>' + (k.quota_reqs > 0 ? 'осталось ' + nFmt(Math.max(0, k.quota_reqs - (k.usage_reqs || 0))) : '') + '</span></div>' +
        '<div class="qlabel" style="margin-top:2px"><span>токены ' + compact(k.usage_tokens || 0) + (k.quota_tokens > 0 ? " / " + compact(k.quota_tokens) : "") + '</span></div></div>' +
        '<div class="kv-actions">' +
        '<button class="btn sm primary act-test" data-k="' + esc(k.name) + '" title="Живой тест ключа">▶ Тест</button>' +
        '<button class="btn sm act-quota" data-k="' + esc(k.name) + '" title="Изменить квоту">✎</button>' +
        (!k.revoked ? '<button class="btn sm danger act-revoke" data-k="' + esc(k.name) + '" title="Отозвать ключ">⛔</button>' : '') +
        '</div></div>';
    }).join("") + '</div></div>';
  // quota-mode живёт в provSettingsCard (#pd-quota-mode + in0); хендлер шапки Ключей удалён.
  c.querySelectorAll(".act-test").forEach(b => b.addEventListener("click", () => testKey(b.getAttribute("data-k"))));
  c.querySelectorAll(".act-revoke").forEach(b => b.addEventListener("click", () => revokeKey(b.getAttribute("data-k"))));
  c.querySelectorAll(".act-quota").forEach(b => b.addEventListener("click", () => editQuota(b.getAttribute("data-k"))));
}
function redrawDonut() {
  const cv = $("#pd-donut");
  if (!cv || !S.prov) return;
  try { if (!cv.offsetParent) return; } catch (eV) {}  // скрытый канвас — не анимируем
  const keys = (S.pdKeys && S.pdKeys.length) ? S.pdKeys : (S.kbp ? (S.kbp[S.prov.name] || (S.prov.name === "__GLOBAL__" ? S.kbp["глобальные"] || [] : [])) : []);
  let used = 0, budget = 0;
  keys.filter(k => !k.revoked).forEach(k => { used += k.usage_usd || 0; budget += k.budget_usd || 0; });
  const ratio = budget > 0 ? used / budget : 0;
  const same = Math.abs(ratio - (S._donutRatio == null ? -1 : S._donutRatio)) < 0.002 && S._donutTheme === S.theme;
  S._donutRatio = ratio; S._donutTheme = S.theme;
  if (!same) donut(cv, ratio, ratio >= 0.9 ? E.danger : ratio >= 0.7 ? E.warn : E.accent, false);
  const cap = $("#pd-donut-cap");
  if (cap) { const ct = 'USD ' + $c(used) + ' / ' + (budget > 0 ? $c(budget) : '∞') + ' · локальный подсчёт по входящим ключам'; if (cap.textContent !== ct) cap.textContent = ct; }
}
function addProviderForm() {
  modal("Добавить openai-совместимый провайдер",
    '<div class="form-grid">' +
    '<div class="field"><label>Имя</label><input class="input" id="ap-name" placeholder="например: openai"></div>' +
    '<div class="field" style="grid-column:1/-1"><label>Ендпоинт (base_url)</label><input class="input" id="ap-base" placeholder="https://api.openai.com/v1"></div>' +
    '<div class="field" style="grid-column:1/-1"><label>API-ключи (каждый на новой строке)</label><textarea class="input" id="ap-keys" aria-label="API-ключи, каждый на новой строке" rows="3" placeholder="sk-..."></textarea>' +
    '<div class="hint">модели и таймаут подтянутся автоматически (GET /models), их можно изменить в карточке провайдера</div></div>' +
    '</div>',
    { ok: "Сохранить", onOk: async (dlg) => {
      const name = $("#ap-name").value.trim(), base = $("#ap-base").value.trim();
      if (!name || !base) { toast("имя и ендпоинт обязательны", "err"); return; }
      const keys = $("#ap-keys").value.split("\n").map(s => s.trim()).filter(Boolean);
      try {
        const r = await API.AddProvider({ name, base_url: base, type: "openai-compatible", api_keys: keys });
        toast(r.message || (r.ok ? "добавлен" : "ошибка"), r.ok ? "ok" : "err");
        if (r.ok) {
          dlg.close(); loadProviders();
          // Авто-подтягивание моделей; если /models недоступен — подсказка.
          try { const s = await API.SyncModels(name); if (s && !s.ok) toast("Модели: " + (s.message || "не подтянулись — ↻ синк в карточке"), "warn"); else if (s) toast("Модели: " + (s.message || "обновлены"), "ok"); loadProviders(); } catch (e) { toast("Модели: " + (e && e.message || e), "warn"); }
        }
      } catch (e) { toast("Добавление: " + (e && e.message || e), "err"); }
    } });
}
async function testKey(name) {
  const m = modal("Тест ключа", '<div class="state"><div class="big" aria-hidden="true">↻</div><div class="title">Тестируем «' + esc(name) + '»…</div></div>');
  try {
    const r = await API.TestKey(name);
    const ok = !r.error && r.status < 400;
    m.close();
    modal("Тест ключа «" + name + "»", '<div class="test-res"><div class="test-line">' + (ok ? '<span class="badge on">ok</span>' : '<span class="badge err">ошибка</span>') + '</div>' +
      '<div class="test-line"><span class="k">модель</span><code>' + esc(r.model) + '</code></div>' +
      '<div class="test-line"><span class="k">провайдер</span><code>' + esc(r.provider || "—") + '</code></div>' +
      '<div class="test-line"><span class="k">латентность</span><code>' + (r.latency_ms || 0) + ' мс</code></div>' +
      '<div class="test-line"><span class="k">токены</span><code>' + esc((() => { try { const _u = JSON.parse(r.usage || "{}"); return (_u.prompt_tokens != null || _u.completion_tokens != null) ? ("in " + nf(_u.prompt_tokens || 0) + " · out " + nf(_u.completion_tokens || 0) + ((_u.total_tokens != null) ? (" · Σ " + nf(_u.total_tokens)) : "")) : (r.usage || "—"); } catch (e) { return (r.usage || "—"); } })()) + '</code></div>' +
        '<div class="test-line"><span class="k">HTTP</span><code>' + (r.status || "—") + '</code></div>' +
      (ok ? '<div class="test-out">' + esc(r.content || "") + '</div>' : '<div class="test-out err">' + esc(r.error || "") + '</div>') + '</div>');
    if (!ok) toast("Ключ «" + name + "»: " + (r.error || "ошибка"), "err");
    else toast("Ключ «" + name + "»: ok, " + (r.latency_ms || 0) + " мс", "ok");
  } catch (e) { m.close(); toast("Тест: " + (e && e.message || e), "err"); }
}
async function revokeKey(name) {
  if (!confirm("Отозвать ключ «" + name + "»? Действие необратимо до пересоздания.")) return;
  try { await API.RevokeKey(name); toast("Ключ «" + name + "» отозван", "ok"); loadProviders(); }
  catch (e) { toast("Отзыв: " + (e && e.message || e), "err"); }
}
function editQuota(name) {
  const row = document.querySelector('.kv-row[data-key="' + CSS.escape(name) + '"]');
  if (!row) return;
  const k = (S.kbp && Object.values(S.kbp).flat().find(x => x.name === name)) || S.keys.find(x => x.name === name);
  if (!k) return;
  row.querySelector(".kv-actions").innerHTML =
    '<div class="qedit"><input class="input sm" data-q="t" type="number" min="0" placeholder="токены" value="' + (k.quota_tokens || 0) + '">' +
    '<input class="input sm" data-q="r" type="number" min="0" placeholder="запросы" value="' + (k.quota_reqs || 0) + '">' +
    '<input class="input sm" data-q="u" type="number" step="0.01" min="0" placeholder="USD ($)" value="' + (((k.budget_usd || 0) / 100).toFixed(2).replace(/\.00$/, "").replace(/(\.\d)0$/, "$1")) + '"></div>' +
    '<div class="kv-actions" style="gap:6px"><button class="btn sm primary q-save">Сохранить</button><button class="btn sm ghost q-cancel">Отмена</button></div>';
  const qsave = row.querySelector(".q-save"), qcancel = row.querySelector(".q-cancel");
  qsave.addEventListener("click", async () => {
    const t = +(row.querySelector('[data-q="t"]').value || 0), r = +(row.querySelector('[data-q="r"]').value || 0), uD = parseFloat(row.querySelector('[data-q="u"]').value || "0") || 0;
    const u = Math.round(uD * 100); // backend хранит центы (keys.go BudgetUSD/quota schema)
    try { await API.SetKeyQuota(name, t, r, u); toast("Квота «" + name + "» обновлена", "ok"); loadProviders(); }
    catch (e) { toast("Квота: " + (e && e.message || e), "err"); }
  });
  qcancel.addEventListener("click", () => loadProviders());
}
 /* удалено: wireProvCards() — legacy-пустышка, клики делегированы в wire() */
