// Browser observations, rather than element rectangles, decide visual failures.
export const VISUAL_PROFILE = Object.freeze({
  viewports: [{ width: 1800, height: 1200 }, { width: 390, height: 844 }],
  deviceScaleFactor: 1,
  fontFamily: 'Arial, sans-serif',
  inkCoverage: 128,
  contributionDelta: 8,
  minimumContributionRatio: 0.8,
});

export function observationStatus(findings) {
  if (findings.some(f => f.status === 'FAIL')) return 'FAIL';
  return findings.some(f => f.status === 'UNKNOWN') ? 'UNKNOWN' : 'PASS';
}

function collectText() {
  const elements = [];
  const findings = [];
  const rect = r => ({ x: r.x + scrollX, y: r.y + scrollY, width: r.width, height: r.height });
  const bounds = el => {
    if (el.namespaceURI === 'http://www.w3.org/2000/svg') return el.getBoundingClientRect();
    const range = document.createRange(); range.selectNodeContents(el); return range.getBoundingClientRect();
  };
  const walker = document.createTreeWalker(document.body, NodeFilter.SHOW_TEXT);
  for (let node; (node = walker.nextNode());) {
    if (!node.textContent.trim()) continue;
    const el = node.parentElement;
    if (el.closest('script,style,noscript,template,defs,clipPath,pattern,marker,mask')) continue;
    const style = getComputedStyle(el);
    if (style.display === 'none' || style.visibility !== 'visible') continue;
    const range = document.createRange(); range.selectNodeContents(node);
    if (!range.getClientRects().length) continue;
    if (elements.includes(el)) continue;
    if ([...el.children].some(child => child.textContent.trim())) {
      findings.push({ status: 'UNKNOWN', kind: 'mixed-text-node', text: el.textContent });
      continue;
    }
    let hidden = false;
    for (let ancestor = el; ancestor; ancestor = ancestor.parentElement) {
      const cs = getComputedStyle(ancestor);
      if (Number(cs.opacity) === 0 || cs.display === 'none') hidden = true;
      if (cs.position === 'fixed' || cs.position === 'sticky' || cs.filter !== 'none' ||
          cs.mixBlendMode !== 'normal' || cs.maskImage !== 'none') {
        findings.push({ status: 'UNKNOWN', kind: 'unsupported-compositing', text: el.textContent });
        hidden = true;
        break;
      }
    }
    if (!hidden) elements.push(el);
  }
  for (const el of document.querySelectorAll('canvas,img,video,iframe,object,embed,foreignObject,textPath')) {
    findings.push({ status: 'UNKNOWN', kind: 'unsupported-paint-content', tag: el.tagName });
  }
  for (const el of document.querySelectorAll('body *')) {
    for (const pseudo of ['::before', '::after']) {
      const content = getComputedStyle(el, pseudo).content;
      if (content && !['none', 'normal', '""'].includes(content)) {
        findings.push({ status: 'UNKNOWN', kind: 'pseudo-content', content });
      }
    }
  }
  const textCount = elements.length;
  const paints = [];
  const paintBounds = el => {
    const r = el.getBoundingClientRect(), cs = getComputedStyle(el), m = el.getScreenCTM?.();
    const scale = m ? Math.hypot(m.a, m.b, m.c, m.d) : 1;
    const stroke = cs.stroke === 'none' ? 0 : Number.parseFloat(cs.strokeWidth) || 0;
    const padding = stroke * scale * Math.max(.5, cs.strokeLinejoin === 'miter' ? Number(cs.strokeMiterlimit) / 2 : .5);
    let px = padding, py = padding;
    if (r.height === 0 && r.width > 0) { px = cs.strokeLinecap === 'butt' ? 0 : stroke * scale / 2; py = stroke * scale / 2; }
    if (r.width === 0 && r.height > 0) { py = cs.strokeLinecap === 'butt' ? 0 : stroke * scale / 2; px = stroke * scale / 2; }
    for (const name of ['markerStart', 'markerMid', 'markerEnd']) {
      const id = cs[name]?.match(/#([^"')]+)["']?\)$/)?.[1];
      const marker = id && document.getElementById(id);
      if (!marker || marker.localName !== 'marker') continue;
      const width = marker.markerWidth.baseVal.value, height = marker.markerHeight.baseVal.value;
      const view = marker.viewBox.baseVal;
      const rx = marker.refX.baseVal.value * (view.width ? width / view.width : 1);
      const ry = marker.refY.baseVal.value * (view.height ? height / view.height : 1);
      const units = marker.getAttribute('markerUnits') === 'userSpaceOnUse' ? 1 : stroke;
      const bound = (Math.hypot(width,height) + Math.hypot(rx,ry)) * units * scale;
      px = Math.max(px, bound); py = Math.max(py, bound);
      if (getComputedStyle(marker).overflow === 'visible') {
        findings.push({ status: 'UNKNOWN', kind: 'unbounded-marker-paint', marker: id });
      }
    }
    return { x:r.x-px,y:r.y-py,left:r.left-px,top:r.top-py,
      right:r.right+px,bottom:r.bottom+py,width:r.width+2*px,height:r.height+2*py };
  };
  for (const el of document.querySelectorAll('svg path,svg rect,svg line,svg circle,svg ellipse,svg polygon,svg polyline,svg use')) {
    if (el.closest('defs,clipPath,pattern,marker,mask') || getComputedStyle(el).visibility !== 'visible') continue;
    const box = rect(paintBounds(el));
    let risky = false;
    for (let p = el; p; p = p.parentElement) {
      const cs = getComputedStyle(p), bound = rect(p.getBoundingClientRect());
      if (cs.clipPath !== 'none' || cs.clip !== 'auto') risky = true;
      if (['hidden', 'clip'].includes(cs.overflowX) &&
          (box.x < bound.x || box.x + box.width > bound.x + bound.width)) risky = true;
      if (['hidden', 'clip'].includes(cs.overflowY) &&
          (box.y < bound.y || box.y + box.height > bound.y + bound.height)) risky = true;
    }
    if (risky && (box.width || box.height)) {
      paints.push({ id: elements.length, tag: el.tagName, box }); elements.push(el);
    }
  }
  globalThis.__drawingTextElements = elements;
  globalThis.__drawingBounds = (el, id) => id < textCount ? bounds(el) : paintBounds(el);
  return { findings, texts: elements.map((el, id) => ({
    id, text: el.textContent.trim(), tag: el.tagName,
    box: rect(bounds(el)),
  })).filter(text => !paints.some(p => p.id === text.id)), paints };
}

async function pixels(decoder, png) {
  return decoder.evaluate(async base64 => {
    const bytes = Uint8Array.from(atob(base64), c => c.charCodeAt(0));
    const bitmap = await createImageBitmap(new Blob([bytes], { type: 'image/png' }));
    const canvas = new OffscreenCanvas(bitmap.width, bitmap.height);
    const ctx = canvas.getContext('2d'); ctx.drawImage(bitmap, 0, 0);
    return { width: bitmap.width, height: bitmap.height,
      data: Array.from(ctx.getImageData(0, 0, bitmap.width, bitmap.height).data) };
  }, png.toString('base64'));
}

function alphaMask(picture) {
  const mask = new Uint8Array(picture.width * picture.height);
  for (let i = 0; i < mask.length; i++) mask[i] = glyphCoverage(picture, i) >= VISUAL_PROFILE.inkCoverage;
  return mask;
}

function glyphCoverage(picture, i) {
  return 255 - Math.min(picture.data[i * 4], picture.data[i * 4 + 1], picture.data[i * 4 + 2]);
}

function boxOverlap(a, b) {
  return a.x < b.x + b.width && b.x < a.x + a.width &&
    a.y < b.y + b.height && b.y < a.y + a.height;
}

function countIntersection(a, b) {
  const x0 = Math.max(a.box.x, b.box.x), y0 = Math.max(a.box.y, b.box.y);
  const x1 = Math.min(a.box.x + a.width, b.box.x + b.width);
  const y1 = Math.min(a.box.y + a.height, b.box.y + b.height);
  let count = 0;
  for (let y = y0; y < y1; y++) for (let x = x0; x < x1; x++) {
    if (a.mask[(y - a.box.y) * a.width + x - a.box.x] &&
        b.mask[(y - b.box.y) * b.width + x - b.box.x]) count++;
  }
  return count;
}

// Only user-scrollable ancestors may reveal content. overflow:hidden is a clip.
async function reveal(page, id) {
  return page.evaluate(id => {
    const el = globalThis.__drawingTextElements[id];
    for (let p = el.parentElement; p; p = p.parentElement) {
      const cs = getComputedStyle(p), r = globalThis.__drawingBounds(el, id), pr = p.getBoundingClientRect();
      if (['auto', 'scroll'].includes(cs.overflowX)) p.scrollLeft += r.left - pr.left - 8;
      if (['auto', 'scroll'].includes(cs.overflowY)) p.scrollTop += r.top - pr.top - 8;
    }
    const r = globalThis.__drawingBounds(el, id);
    window.scrollTo(Math.max(0, r.left + scrollX - 8), Math.max(0, r.top + scrollY - 8));
    const now = globalThis.__drawingBounds(el, id);
    return { x: now.left + scrollX, y: now.top + scrollY, width: now.width, height: now.height };
  }, id);
}

async function textPicture(page, decoder, id, clip, unclip, paint = false) {
  await page.evaluate(({ id, unclip, paint }) => {
    globalThis.__drawingResourceStyles = [...document.querySelectorAll('defs,defs *,clipPath,clipPath *,pattern,pattern *,marker,marker *,mask,mask *')]
      .map(el => [el, el.getAttribute('style'), getComputedStyle(el).visibility]);
    const style = document.createElement('style'); style.id = '__drawing_mask_style';
    style.textContent = `*{visibility:hidden!important;background:transparent!important;
      border-color:transparent!important;box-shadow:none!important;outline:none!important;${paint ? '' : 'opacity:1!important'}}
      html,body{background:${paint ? 'transparent' : 'white'}!important}`;
    document.head.append(style);
    for (const [el, , visibility] of globalThis.__drawingResourceStyles) el.style.setProperty('visibility', visibility, 'important');
    globalThis.__drawingClipStyles = [];
    if (unclip) for (const ancestor of document.querySelectorAll('*')) {
      const cs = getComputedStyle(ancestor);
      const props = {};
      if (['hidden', 'clip'].includes(cs.overflowX)) props['overflow-x'] = 'visible';
      if (['hidden', 'clip'].includes(cs.overflowY)) props['overflow-y'] = 'visible';
      if (cs.clip !== 'auto') props.clip = 'auto';
      if (cs.clipPath !== 'none') props['clip-path'] = 'none';
      if (!Object.keys(props).length) continue;
      globalThis.__drawingClipStyles.push([ancestor, ancestor.getAttribute('style')]);
      for (const [key, value] of Object.entries(props)) ancestor.style.setProperty(key, value, 'important');
    }
    const el = globalThis.__drawingTextElements[id];
    el.__drawingOldStyle = el.getAttribute('style');
    const props = paint ? { visibility: 'visible' } : { visibility: 'visible', color: 'black', fill: 'black',
      stroke: 'none', 'text-shadow': 'none', 'text-decoration': 'none' };
    for (const [name, value] of Object.entries(props)) el.style.setProperty(name, value, 'important');
  }, { id, unclip, paint });
  try {
    const current = await page.evaluate(id => {
      const r = globalThis.__drawingBounds(globalThis.__drawingTextElements[id], id);
      return { x: r.x + scrollX, y: r.y + scrollY, width: r.width, height: r.height };
    }, id);
    const png = await page.screenshot({ clip, fullPage: true, animations: 'disabled', omitBackground: paint });
    return { picture: await pixels(decoder, png), box: current, png };
  } finally {
    await page.evaluate(id => {
      document.getElementById('__drawing_mask_style').remove();
      const el = globalThis.__drawingTextElements[id];
      if (el.__drawingOldStyle === null) el.removeAttribute('style');
      else el.setAttribute('style', el.__drawingOldStyle);
      for (const [ancestor, old] of globalThis.__drawingClipStyles) {
        if (old === null) ancestor.removeAttribute('style'); else ancestor.setAttribute('style', old);
      }
      for (const [resource, old] of globalThis.__drawingResourceStyles) {
        if (old === null) resource.removeAttribute('style'); else resource.setAttribute('style', old);
      }
    }, id);
  }
}

export async function inspectVisual(page, decoder, { save, progress = () => {} } = {}) {
  const observation = await page.evaluate(collectText);
  const findings = [...observation.findings];
  const masks = [];
  if (!observation.texts.length) findings.push({ status: 'UNKNOWN', kind: 'no-rendered-text' });
  for (const text of observation.texts) {
    const box = await reveal(page, text.id);
    if (box.x < 0 || box.y < 0 || box.width > 8000 || box.height > 8000) {
      findings.push({ status: 'UNKNOWN', kind: 'text-outside-capture-range', text }); continue;
    }
    const clip = { x: Math.max(0, Math.floor(box.x) - 3), y: Math.max(0, Math.floor(box.y) - 3),
      width: Math.ceil(box.width) + 6, height: Math.ceil(box.height) + 6 };
    const actualPng = await page.screenshot({ clip, fullPage: true, animations: 'disabled' });
    const actual = await pixels(decoder, actualPng);
    const oldStyle = await page.evaluate(id => {
      const el = globalThis.__drawingTextElements[id], old = el.getAttribute('style');
      el.style.setProperty('color', 'transparent', 'important');
      el.style.setProperty('fill', 'transparent', 'important');
      el.style.setProperty('text-shadow', 'none', 'important');
      return old;
    }, text.id);
    let without;
    try {
      without = await pixels(decoder, await page.screenshot({ clip, fullPage: true, animations: 'disabled' }));
    } finally {
      await page.evaluate(({ id, oldStyle }) => {
        const el = globalThis.__drawingTextElements[id];
        if (oldStyle === null) el.removeAttribute('style'); else el.setAttribute('style', oldStyle);
      }, { id: text.id, oldStyle });
    }
    const visible = await textPicture(page, decoder, text.id, clip, false);
    const expected = await textPicture(page, decoder, text.id, clip, true);
    if (Object.keys(box).some(k => Math.abs(box[k] - expected.box[k]) > 0.01) ||
        expected.picture.width !== actual.width || expected.picture.height !== actual.height) {
      findings.push({ status: 'UNKNOWN', kind: 'measurement-changed-layout', text }); continue;
    }
    const mask = alphaMask(visible.picture), complete = alphaMask(expected.picture);
    let ink = 0, clipped = 0, obscured = 0, maxContribution = 0;
    const contributions = new Float32Array(complete.length);
    for (let i = 0; i < complete.length; i++) {
      if (complete[i]) ink++;
      if (complete[i] && !mask[i]) clipped++;
      if (!mask[i]) continue;
      let delta = 0;
      for (let c = 0; c < 3; c++) delta = Math.max(delta, Math.abs(actual.data[i * 4 + c] - without.data[i * 4 + c]));
      contributions[i] = delta / glyphCoverage(visible.picture, i);
      maxContribution = Math.max(maxContribution, contributions[i]);
    }
    for (let i = 0; i < mask.length; i++) if (mask[i] &&
      (maxContribution * 255 < VISUAL_PROFILE.contributionDelta ||
       contributions[i] < maxContribution * VISUAL_PROFILE.minimumContributionRatio)) obscured++;
    if (!ink) findings.push({ status: 'UNKNOWN', kind: 'no-measurable-glyph-ink', text });
    if (clipped) findings.push({ status: 'FAIL', kind: 'clipped-text', text, pixels: clipped });
    if (obscured) findings.push({ status: 'FAIL', kind: 'obscured-text', text, pixels: obscured });
    if ((clipped || obscured) && save) {
      await save(`text-${text.id}.png`, actualPng);
      await save(`text-${text.id}-expected.png`, expected.png);
    }
    masks.push({ id: text.id, mask, width: visible.picture.width, height: visible.picture.height,
      box: { x: Math.floor(text.box.x) - (Math.floor(box.x) - clip.x),
        y: Math.floor(text.box.y) - (Math.floor(box.y) - clip.y) } });
    if ((text.id + 1) % 25 === 0) progress({ measured: text.id + 1, total: observation.texts.length });
  }
  for (let i = 0; i < masks.length; i++) for (let j = i + 1; j < masks.length; j++) {
    const a = masks[i], b = masks[j];
    if (!boxOverlap({ ...a.box, width: a.width, height: a.height }, { ...b.box, width: b.width, height: b.height })) continue;
    const pixels = countIntersection(a, b);
    if (pixels) findings.push({ status: 'FAIL', kind: 'text-collision',
      texts: [observation.texts[a.id], observation.texts[b.id]], pixels });
  }
  for (const paint of observation.paints) {
    const box = await reveal(page, paint.id);
    if (box.x < 0 || box.y < 0 || box.width > 8000 || box.height > 8000) {
      findings.push({ status: 'UNKNOWN', kind: 'paint-outside-capture-range', paint }); continue;
    }
    const clip = { x: Math.max(0, Math.floor(box.x) - 3), y: Math.max(0, Math.floor(box.y) - 3),
      width: Math.ceil(box.width) + 6, height: Math.ceil(box.height) + 6 };
    const visible = await textPicture(page, decoder, paint.id, clip, false, true);
    const complete = await textPicture(page, decoder, paint.id, clip, true, true);
    if (Object.keys(box).some(k => Math.abs(box[k] - complete.box[k]) > 0.01) ||
        complete.picture.width !== visible.picture.width || complete.picture.height !== visible.picture.height) {
      findings.push({ status: 'UNKNOWN', kind: 'paint-measurement-changed-layout', paint }); continue;
    }
    const painted = (p, i) => p.data[i * 4 + 3] >= VISUAL_PROFILE.contributionDelta;
    let clipped = 0, ink = 0;
    for (let i = 0; i < complete.picture.width * complete.picture.height; i++) {
      if (painted(complete.picture, i)) ink++;
      if (painted(complete.picture, i) && !painted(visible.picture, i)) clipped++;
    }
    if (!ink) findings.push({ status: 'UNKNOWN', kind: 'no-measurable-drawing-ink', paint });
    if (clipped) {
      findings.push({ status: 'FAIL', kind: 'clipped-drawing-ink', paint, pixels: clipped });
      if (save) { await save(`paint-${paint.id}.png`, visible.png); await save(`paint-${paint.id}-expected.png`, complete.png); }
    }
  }
  await page.evaluate(() => { for (const p of document.querySelectorAll('*')) { p.scrollLeft = 0; p.scrollTop = 0; } window.scrollTo(0, 0); });
  return { status: observationStatus(findings), findings, texts: observation.texts,
    measuredTexts: masks.length, profile: VISUAL_PROFILE };
}
