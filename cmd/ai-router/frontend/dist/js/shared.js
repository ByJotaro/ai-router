/* ==================================================================
   MODAL (lегкий)
   ================================================================== */
function modal(title, bodyHtml, opts) {
  const o = opts || {};
  let m = $("#modal"); if (m) m.remove();
  m = document.createElement("div"); m.id = "modal";
  let foot = "";
  if (o.ok) {
    foot = '<div class="card-foot"><button class="btn" id="md-cancel">Отмена</button><button class="btn primary" id="md-ok">' + esc(o.ok) + '</button></div>';
  }
  m.setAttribute("role", "dialog"); m.setAttribute("aria-modal", "true"); m.setAttribute("aria-label", title);
  m.innerHTML = '<div class="ov"></div><div class="md' + (o.wide ? ' md--wide' : '') + '"><div class="md-head"><h3>' + esc(title) + '</h3><span class="spacer"></span><button class="icon-btn modal-x" aria-label="Закрыть (Esc)" title="Закрыть (Esc)">✕</button></div><div class="md-body">' + bodyHtml + foot + '</div></div>';
  document.body.appendChild(m);
  try { enhanceSelects(m); } catch (eDDE) {}
  const prevFocus = document.activeElement, prevOverflow = document.body.style.overflow;
  document.body.style.overflow = "hidden";
  const firstInput = m.querySelector(".md-body input, .md-body select, #md-ok, .modal-x");
  if (firstInput && firstInput.focus) firstInput.focus();
  const escH = e => { if (e.key === "Escape") { e.stopPropagation(); close(); } };
  document.addEventListener("keydown", escH, { capture: true });
  // P3 LogsModal: Tab-трап — фокус циклит внутри модалки (a11y), снимается в close()
  const trapH = e => {
    if (e.key !== "Tab") return;
    const f = Array.from(m.querySelectorAll('button, [href], input, select, textarea, [tabindex]:not([tabindex="-1"])')).filter(x => !x.disabled && x.offsetParent !== null);
    if (!f.length) { e.preventDefault(); return; }
    const first = f[0], last = f[f.length - 1];
    if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
    else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
  };
  m.addEventListener("keydown", trapH);
  function close() { document.removeEventListener("keydown", escH, { capture: true }); m.removeEventListener("keydown", trapH); document.body.style.overflow = prevOverflow; m.remove(); if (prevFocus && prevFocus.focus) prevFocus.focus(); }
  m.querySelector(".ov").addEventListener("click", close);
  m.querySelector(".modal-x").addEventListener("click", close);
  const cx = $("#md-cancel", m); if (cx) cx.addEventListener("click", close);
  const ok = $("#md-ok", m);
  if (ok) ok.addEventListener("click", () => {
    if (o.onOk) {
      const r = o.onOk({ el: m, close });
      // Promise/асинхронный onOk сам закрывает модалку при успехе (или не закрывает при ошибке)
      if (r !== false && !(r && typeof r.then === "function")) close();
    }
  });
  return { el: m, close };
}

/* ==================================================================
   PROVIDERS
   ================================================================== */
let sparkProviders = {};
let sparkTokens = {};
