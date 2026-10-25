/* catalog-scroll-probe.js
 * Paste into the Firefox DevTools console (F12) on the Jad page, with the
 * catalog dialog open. Read-only: it measures, it changes nothing.
 *
 * If DevTools refuses the paste, type "allow pasting" and press Enter first.
 */
(async () => {
  const CANDIDATES = ['dialog[open]', '[role="dialog"]', 'div[popover]:popover-open'];
  const pick = () => {
    for (const sel of CANDIDATES) {
      const els = [...document.querySelectorAll(sel)].filter((el) => {
        const r = el.getBoundingClientRect();
        return r.width > 300 && r.height > 200 && getComputedStyle(el).visibility !== 'hidden';
      });
      if (els.length) return { el: els[els.length - 1], sel };
    }
    return null;
  };

  const px = (v) => parseFloat(v) || 0;
  const canScroll = (cs) => /(auto|scroll|overlay)/.test(cs.overflowX + ' ' + cs.overflowY);

  // A scrollbar that takes up space is (offset - client - borders). Borders are
  // NOT a scrollbar, so they must be subtracted or every bordered box lies.
  const measure = (el) => {
    const cs = getComputedStyle(el);
    const bx = px(cs.borderLeftWidth) + px(cs.borderRightWidth);
    const by = px(cs.borderTopWidth) + px(cs.borderBottomWidth);
    const r = el.getBoundingClientRect();
    const overX = el.scrollWidth - el.clientWidth;
    const overY = el.scrollHeight - el.clientHeight;
    // Sub-pixel rounding reports 1px on flat boxes; a real bar is far wider.
    const barX = Math.max(0, el.offsetWidth - el.clientWidth - bx) >= 2 ? Math.round(el.offsetWidth - el.clientWidth - bx) : 0;
    const barY = Math.max(0, el.offsetHeight - el.clientHeight - by) >= 2 ? Math.round(el.offsetHeight - el.clientHeight - by) : 0;
    const scrollable = canScroll(cs);
    // "visible" overflow spills out and clips nothing, so it is never a clip.
    const clipsX = /(hidden|clip)/.test(cs.overflowX) && overX > 1;
    const clipsY = /(hidden|clip)/.test(cs.overflowY) && overY > 1;
    return {
      box: Math.round(r.width) + 'x' + Math.round(r.height),
      overflow: cs.overflowX + '/' + cs.overflowY,
      overX: Math.max(0, overX),
      overY: Math.max(0, overY),
      // The only thing a user calls "a scrollbar":
      barX, barY,
      // Content that is cut off with no way to scroll to it.
      clipped: clipsX || clipsY,
      // 1px is the whole bug: a sub-pixel overflow still paints a bar on some machines.
      scrolls: scrollable && (overX > 0.5 || overY > 0.5),
    };
  };

  // Is this machine painting classic (space-taking) scrollbars at all?
  const classic = (() => {
    const d = document.createElement('div');
    d.style.cssText = 'position:fixed;top:0;left:0;width:100px;height:100px;overflow:scroll;visibility:hidden;pointer-events:none';
    document.documentElement.appendChild(d);
    const w = d.offsetWidth - d.clientWidth;
    d.remove();
    return w;
  })();

  const hit = pick();
  if (!hit) {
    console.log('%cNo dialog found.%c Open Catalog (the button in the composer toolbar), then run this again.',
      'color:#e9af08;font-weight:bold', 'color:inherit');
    return;
  }

  const surface = measure(hit.el);
  const rows = [{ what: 'DIALOG SURFACE (' + hit.sel + ')', ...surface }];
  hit.el.querySelectorAll('*').forEach((el) => {
    const r = el.getBoundingClientRect();
    if (r.width < 40 || r.height < 24) return;
    const m = measure(el);
    if (!m.scrolls && !m.barX && !m.barY && !m.clipped) return; // only containers that actually scroll, bar, or cut content
    const tag = el.tagName.toLowerCase()
      + (el.getAttribute('role') ? '[role=' + el.getAttribute('role') + ']' : '')
      + (el.dataset.part ? '[data-part=' + el.dataset.part + ']' : '');
    rows.push({ what: '  ' + tag.slice(0, 48), ...m });
  });

  // Who is sticking out? Names the element responsible for a sub-pixel overflow,
  // which is the usual reason a surface with overflow:auto sprouts a scrollbar.
  const dr = hit.el.getBoundingClientRect();
  const ds = getComputedStyle(hit.el);
  // The content area ends inside the borders; comparing to the border box hides a 1px spill.
  const innerRight = dr.right - (parseFloat(ds.borderRightWidth) || 0);
  const innerBottom = dr.bottom - (parseFloat(ds.borderBottomWidth) || 0);
  const overhang = [];
  hit.el.querySelectorAll('*').forEach((el) => {
    const r = el.getBoundingClientRect();
    if (!r.width || !r.height) return;
    const right = r.right - innerRight;
    const bottom = r.bottom - innerBottom;
    if (right > 0.5 || bottom > 0.5) {
      overhang.push({
        what: (el.tagName.toLowerCase()
          + (el.dataset.part ? '[data-part=' + el.dataset.part + ']' : '')
          + (el.className ? '.' + String(el.className).split(/\s+/).slice(0, 2).join('.') : '')).slice(0, 56),
        overRight: +right.toFixed(2),
        overBottom: +bottom.toFixed(2),
        box: Math.round(r.width) + 'x' + Math.round(r.height),
      });
    }
  });
  overhang.sort((a, b) => Math.max(b.overRight, b.overBottom) - Math.max(a.overRight, a.overBottom));

  console.log('%cJad catalog scrollbar probe', 'font-weight:bold;font-size:13px;color:#88bcff');
  console.log('Firefox ' + (navigator.userAgent.match(/Firefox\/[\d.]+/) || ['?'])[0]
    + ' \u00B7 window ' + innerWidth + 'x' + innerHeight + ' \u00B7 DPR ' + devicePixelRatio);
  console.table(rows);

  const surfaceHasBar = surface.barX > 0 || surface.barY > 0;
  const surfaceScrolls = surface.scrolls;

  if (surfaceHasBar) {
    console.log('%cVERDICT: the dialog surface paints a real, space-taking scrollbar (' + surface.barX + 'x' + surface.barY + 'px).',
      'color:#E3193B;font-weight:bold');
    console.log('That is the bug. The numbers above say which axis.');
  } else if (surfaceScrolls) {
    console.log('%cVERDICT: content overflows the surface with ' + surface.overflow + ' overflow, so it scrolls',
      'color:#E9AF08;font-weight:bold');
    console.log('but takes no space here (overlay bars, classic = ' + classic + 'px).');
    console.log('On a machine with classic bars this same overflow WOULD show a bar, so it is still a defect:');
    console.log('overflowX/Y = ' + surface.overX + 'x' + surface.overY + 'px too big.');
  } else if (surface.clipped) {
    console.log('%cVERDICT: surface is clean of scrollbars, but content is CLIPPED (' + surface.overX + 'x' + surface.overY
      + 'px hidden by overflow:' + surface.overflow + ').', 'color:#E9AF08;font-weight:bold');
  } else if (rows.length > 1) {
    console.log('%cVERDICT: the surface is clean. The scrolling boxes listed are INSIDE it \u2014 that is by design.',
      'color:#0D8626;font-weight:bold');
  } else {
    console.log('%cVERDICT: no scrolling box anywhere in the dialog. Nothing to report.', 'color:#0D8626;font-weight:bold');
  }

  if (overhang.length) {
    console.log('%cSticking out of the surface (top 6) — this is what makes it scroll:', 'color:#E9AF08');
    console.table(overhang.slice(0, 6));
  } else {
    console.log('Nothing sticks out past the surface edges.');
  }

  console.log(classic > 0
    ? '%cThis browser uses CLASSIC scrollbars (' + classic + 'px), so any 1px overflow is visible.'
    : '%cThis browser uses OVERLAY scrollbars (0px): overflow can exist without a visible bar.',
    'color:#6a6a6a');

  try {
    const keys = Object.keys(localStorage).filter((k) => /catalog/i.test(k));
    if (keys.length) console.log('stored size:', Object.fromEntries(keys.map((k) => [k, localStorage.getItem(k)])));
  } catch {}

  window.__jadScrollProbe = () => {
    const h = pick();
    if (!h) return 'dialog closed';
    const m = measure(h.el);
    console.log('surface', m.box, 'overflow', m.overflow, 'over', m.overX + 'x' + m.overY, 'bars', m.barX + 'x' + m.barY);
    return m;
  };
  console.log('%cResize the window, then run  __jadScrollProbe()  to compare.', 'color:#6a6a6a');
})();
