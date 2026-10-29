import { after, before, test } from 'node:test';
import assert from 'node:assert/strict';
import { createRequire } from 'node:module';
import { mkdtemp, readFile, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { verifyDrawing, sourceCoverage } from './drawing-quality.mjs';

const require = createRequire(new URL('../../e2e/ui/package.json', import.meta.url));
const { chromium } = require('@playwright/test');
let browser, root;
before(async () => { browser = await chromium.launch(); root = await mkdtemp(join(tmpdir(), 'drawing-quality-')); });
after(async () => { await browser?.close(); await rm(root, { recursive: true, force: true }); });
const html = `<style>table{border-collapse:collapse}td,th{padding:8px;border:1px solid}svg{display:block}</style><p>Units</p><p>px</p>
<table><tr><th>Code</th><th>Variable</th><th>Value</th></tr><tr><td>W1</td><td>width</td><td>480</td></tr></table>
<svg width="200" height="70" id="sheet"></svg><script>document.getElementById('sheet').innerHTML='<text x="20" y="30">W1</text>'</script>`;

test('end-to-end generated SVG is frozen and verified with author JavaScript disabled', async () => {
  const outputDir = join(root, 'good');
  const { report, reportRef } = await verifyDrawing({ browser, html, outputDir,
    sourceFacts: { values: { W1: '480 px' }, defaultUnit: 'px' } });
  assert.equal(report.status, 'PASS', JSON.stringify(report.results));
  assert.equal(report.author_code_during_measurement, false);
  assert.equal(report.results.length, 2);
  assert.match(await readFile(join(outputDir, '390-rendered.html'), 'utf8'), /<text x="20" y="30">W1<\/text>/);
  assert.equal(JSON.parse(await readFile(reportRef.path, 'utf8')).artifact.sha256, report.artifact.sha256);
  await assert.rejects(verifyDrawing({ browser, html, outputDir }), /EEXIST/);
});

test('self-consistent values cannot pass without independent source evidence', async () => {
  const { report } = await verifyDrawing({ browser, html, outputDir: join(root, 'no-source'), viewports: [{ width: 600, height: 500 }] });
  assert.equal(report.status, 'UNKNOWN');
  assert.ok(report.results[0].facts.findings.some(f => f.kind === 'source-value-unverified'));
});

test('a known contradiction beats an unverified source value', async () => {
  const { report } = await verifyDrawing({ browser, html: html.replace('>480<', '>481<'), outputDir: join(root, 'wrong'),
    sourceFacts: { values: { W1: '480 px' }, defaultUnit: 'px' }, viewports: [{ width: 600, height: 500 }] });
  assert.equal(report.status, 'FAIL');
});

test('derived quantities need source-backed leaves; TBD is distinct from a checker unknown', () => {
  const observation = { tables: [{ rows: [{ code: 'S1', value: '12' }, { code: 'H1', value: '2*S1 = 24' }, { code: 'V1', value: '[TBD]' }] }] };
  assert.equal(sourceCoverage(observation, { values: { S1: '12 px' } }).status, 'PASS');
  assert.equal(sourceCoverage(observation, {}).status, 'UNKNOWN');
});

test('self assertions and mutually circular equalities cannot replace source evidence', () => {
  const self = { tables: [{ rows: [{ code:'W1', value:'W1 = 480 px' }] }] };
  assert.equal(sourceCoverage(self, {}).status, 'UNKNOWN');
  const cycle = { tables: [{ rows: [{ code:'W1', value:'W2 = 480 px' }, { code:'W2', value:'W1 = 480 px' }] }] };
  assert.equal(sourceCoverage(cycle, {}).status, 'UNKNOWN');
  assert.equal(sourceCoverage(cycle, {values:{W1:'480 px'}}).status, 'PASS');
});
