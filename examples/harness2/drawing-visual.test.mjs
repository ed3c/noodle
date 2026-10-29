import { after, before, test } from 'node:test';
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';
import { inspectVisual, VISUAL_PROFILE } from './drawing-visual.mjs';

const require = createRequire(new URL('../../e2e/ui/package.json', import.meta.url));
const { chromium } = require('@playwright/test');
let browser;
before(async () => { browser = await chromium.launch({ headless: true }); });
after(async () => { await browser?.close(); });

async function inspect(html, width = 600) {
  const context = await browser.newContext({ viewport: { width, height: 500 }, deviceScaleFactor: 1 });
  try {
    const page = await context.newPage(), decoder = await context.newPage();
    await page.setContent(`<style>body{margin:0;background:white;color:black;font:20px Arial}svg{display:block}*{animation:none!important;transition:none!important}</style>${html}`);
    await page.evaluate(() => document.fonts.ready);
    return await inspectVisual(page, decoder);
  } finally { await context.close(); }
}

test('non-overlapping text and normal table borders pass', async () => {
  const r = await inspect('<table style="border-collapse:collapse"><tr><td style="border:1px solid;padding:8px">W1</td><td style="border:1px solid;padding:8px">480</td></tr></table><footer>REFERENCE</footer>');
  assert.equal(r.status, 'PASS', JSON.stringify(r.findings));
});

test('HTML header collision fails from glyph intersections', async () => {
  const r = await inspect('<div style="position:relative;height:60px"><span style="position:absolute;left:10px;top:10px">HEIGHT FORMULA</span><span style="position:absolute;left:70px;top:10px">R1 16</span></div>');
  assert.equal(r.status, 'FAIL');
  assert.ok(r.findings.some(f => f.kind === 'text-collision'));
});

test('text overflowing its own HTML cell still collides with its neighbour', async () => {
  const r = await inspect('<div style="display:flex"><div style="width:40px;flex:none;white-space:nowrap">LONG FORMULA</div><div style="width:80px">18</div></div>');
  assert.ok(r.findings.some(f => f.kind === 'text-collision'), JSON.stringify(r.findings));
});

test('small text and gray text do not become rasterization false positives', async () => {
  const r = await inspect('<p style="font-size:9px">SLOTS ARE ARCHITECTURE PLACEHOLDERS. HEADER BODY AND FOOTER REGIONS FILL CW.</p><p style="color:#777;opacity:.5">Reference text</p>');
  assert.equal(r.status, 'PASS', JSON.stringify(r.findings));
});

test('intersecting text rectangles without glyph intersection pass', async () => {
  const r = await inspect('<svg width="500" height="100"><text x="10" y="40" style="white-space:pre">I                    I</text><text x="60" y="40">O</text></svg>');
  assert.equal(r.status, 'PASS', JSON.stringify(r.findings));
});

test('a drawing line crossing text fails; a real white knockout passes', async () => {
  const html = stroke => `<svg width="500" height="100"><path d="M0 31H250" stroke="black"/><text x="20" y="40" ${stroke}>DIMENSION</text></svg>`;
  const bad = await inspect(html(''));
  assert.ok(bad.findings.some(f => f.kind === 'obscured-text'), JSON.stringify(bad.findings));
  const good = await inspect(html('stroke="white" stroke-width="5" paint-order="stroke"'));
  assert.equal(good.status, 'PASS', JSON.stringify(good.findings));
});

test('opaque foreground shape hiding text fails', async () => {
  const r = await inspect('<svg width="500" height="100"><text x="20" y="40">DIMENSION</text><rect x="20" y="20" width="120" height="30" fill="white"/></svg>');
  assert.ok(r.findings.some(f => f.kind === 'obscured-text'));
});

test('overflow hidden clipping fails while accessible horizontal scroll passes', async () => {
  const html = overflow => `<div style="width:140px;overflow-x:${overflow}"><svg width="450" height="80"><text x="125" y="40">WIDTH</text></svg></div>`;
  const bad = await inspect(html('hidden'));
  assert.ok(bad.findings.some(f => f.kind === 'clipped-text'), JSON.stringify(bad.findings));
  const good = await inspect(html('auto'), 390);
  assert.equal(good.status, 'PASS', JSON.stringify(good.findings));
});

test('rotated labels measure actual glyphs', async () => {
  const r = await inspect('<svg width="500" height="200"><text transform="translate(50 150) rotate(-90)">HEIGHT</text></svg>');
  assert.equal(r.status, 'PASS', JSON.stringify(r.findings));
});

test('clipped drawing lines are defects even without clipped text', async () => {
  const r = await inspect('<div style="width:100px;overflow:hidden"><svg width="400" height="100"><path d="M50 30H180" stroke="black"/></svg></div><p>REFERENCE</p>');
  assert.ok(r.findings.some(f => f.kind === 'clipped-drawing-ink'), JSON.stringify(r.findings));
});

test('clip paths and pattern resources retain their real visibility in masks', async () => {
  const html = (fill,width) => `<p>REFERENCE</p><svg width="300" height="100"><defs><pattern id="p" width="8" height="8" patternUnits="userSpaceOnUse"><rect width="8" height="8"/></pattern><clipPath id="c"><rect width="${width}" height="100"/></clipPath></defs><rect x="20" y="20" width="200" height="40" fill="${fill}" clip-path="url(#c)"/></svg>`;
  const bad = await inspect(html('url(#p)',80));
  assert.ok(bad.findings.some(f => f.kind==='clipped-drawing-ink'),JSON.stringify(bad.findings));
  const good = await inspect(html('black',280));
  assert.equal(good.status,'PASS',JSON.stringify(good.findings));
});

test('stroke extending outside the SVG viewport cannot be missed by geometric bounds', async () => {
  const r = await inspect('<p>REFERENCE</p><svg width="200" height="100"><path d="M10 2H190" stroke="black" stroke-width="12"/></svg>');
  assert.ok(r.findings.some(f => f.kind==='clipped-drawing-ink'),JSON.stringify(r.findings));
});

test('clipped pale and white drawing ink is detected independently of background colour', async () => {
  for (const fill of ['white', '#eee', 'rgba(0,0,0,.1)']) {
    const r = await inspect(`<p>REFERENCE</p><svg width="300" height="100" style="background:black"><defs><clipPath id="c"><rect width="80" height="100"/></clipPath></defs><rect x="20" y="20" width="200" height="40" fill="${fill}" clip-path="url(#c)"/></svg>`);
    assert.ok(r.findings.some(f => f.kind === 'clipped-drawing-ink'), JSON.stringify(r.findings));
  }
});

test('unsupported canvas and absent rendered text cannot pass', async () => {
  assert.equal((await inspect('<canvas></canvas>')).status, 'UNKNOWN');
  assert.equal(VISUAL_PROFILE.viewports.length, 2);
});
