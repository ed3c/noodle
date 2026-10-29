import { createRequire } from 'node:module';
import { createHash } from 'node:crypto';
import { readFile, writeFile, mkdir } from 'node:fs/promises';
import { resolve, join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { platform, release } from 'node:os';
import { inspectVisual, observationStatus, VISUAL_PROFILE } from './drawing-visual.mjs';
import { collectDrawingFacts, checkDrawingFacts, expandDefinitionRows } from './drawing-facts.mjs';

const hash = bytes => createHash('sha256').update(bytes).digest('hex');
const require = createRequire(new URL('../../e2e/ui/package.json', import.meta.url));

export function sourceCoverage(observation, sourceFacts) {
  const values = sourceFacts?.values || {}, findings = [];
  const rows = expandDefinitionRows(observation.tables.flatMap(t => t.rows || []));
  const definitions = new Map(rows.map(row => [row.code, row]));
  const proven = new Map();
  const anchored = (code, visiting = new Set()) => {
    if (proven.has(code)) return proven.get(code);
    if (Object.hasOwn(values, code) && values[code] != null) return true;
    const row = definitions.get(code);
    if (!row || visiting.has(code)) return false;
    if (/^\[?TBD\]?$/i.test(row.value.trim())) return true;
    const expressions = row.value.split('=');
    if (expressions.length > 1 && expressions[0].trim() === code) expressions.shift();
    const dependencies = [...new Set(expressions.flatMap(part => [...part.matchAll(/\b[A-Z][A-Z0-9]*\b/g)]
      .map(m => m[0]).filter(c => definitions.has(c))))];
    const path = new Set([...visiting, code]);
    const result = dependencies.length > 0 && dependencies.every(c => anchored(c, path));
    proven.set(code, result); return result;
  };
  for (const row of rows) {
    if (/^\[?TBD\]?$/i.test(row.value.trim())) continue;
    if (!anchored(row.code)) {
      findings.push({ status: 'UNKNOWN', kind: 'source-value-unverified', code: row.code, location: row.location });
    }
  }
  return { status: observationStatus(findings), findings };
}

export async function verifyDrawing({ browser, html, outputDir, sourceFacts = {}, viewports = VISUAL_PROFILE.viewports, progress }) {
  await mkdir(outputDir, { recursive: false });
  const evidence = [];
  const save = async (name, bytes) => {
    const path = join(outputDir, name);
    await writeFile(path, bytes, { flag: 'wx' });
    const ref = { path: resolve(path), sha256: hash(bytes) }; evidence.push(ref); return ref;
  };
  const artifact = await save('drawing.html', html);
  const oracle = await save('source-facts.json', JSON.stringify(sourceFacts, null, 2));
  const engine = [];
  for (const name of ['drawing-quality.mjs', 'drawing-visual.mjs', 'drawing-facts.mjs']) {
    const url = new URL(name, import.meta.url), bytes = await readFile(url);
    engine.push({ path: url.pathname, sha256: hash(bytes) });
  }
  const results = [];
  for (const viewport of viewports) {
    const contexts = [], errors = [], requests = [];
    try {
      const options = { viewport, deviceScaleFactor: 1, serviceWorkers: 'block', offline: true };
      const build = await browser.newContext(options); contexts.push(build);
      await build.route('**/*', route => { requests.push(route.request().url()); return route.abort(); });
      await build.routeWebSocket('**/*', socket => { requests.push(socket.url()); socket.close(); });
      const producer = await build.newPage();
      producer.on('pageerror', e => errors.push({ status: 'FAIL', kind: 'page-error', error: e.message }));
      await producer.setContent(html, { waitUntil: 'load', timeout: 15000 });
      await producer.addStyleTag({ content: `*{font-family:${VISUAL_PROFILE.fontFamily}!important;animation:none!important;transition:none!important;caret-color:transparent!important}` });
      await producer.evaluate(async () => { await document.fonts.ready; await new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r))); });
      const builtPixels = await producer.screenshot({ fullPage: true, animations: 'disabled' });
      const snapshot = await producer.content();
      await save(`${viewport.width}-rendered.html`, snapshot);
      // Generated SVG survives serialization. Author code cannot run during measurement.
      const context = await browser.newContext({ ...options, bypassCSP: true, javaScriptEnabled: false }); contexts.push(context);
      const page = await context.newPage(), decoder = await context.newPage();
      await page.setContent(snapshot, { waitUntil: 'load', timeout: 15000 });
      const observed = await page.evaluate(collectDrawingFacts);
      const facts = checkDrawingFacts(observed, sourceFacts);
      const coverage = sourceCoverage(observed, sourceFacts);
      facts.findings.push(...coverage.findings);
      facts.status = observationStatus([{ status: facts.status }, coverage]);
      const measuredPixels = await page.screenshot({ fullPage: true, animations: 'disabled' });
      if (hash(measuredPixels) !== hash(builtPixels)) {
        errors.push({ status: 'UNKNOWN', kind: 'render-snapshot-mismatch' });
        await save(`${viewport.width}-before-snapshot.png`, builtPixels);
      }
      const visual = await inspectVisual(page, decoder, {
        save: (name, bytes) => save(`${viewport.width}-${name}`, bytes), progress,
      });
      await save(`${viewport.width}-sheet.png`, await page.screenshot({ fullPage: true, animations: 'disabled' }));
      const environmentFindings = [
        ...errors,
        ...requests.map(url => ({ status: 'UNKNOWN', kind: 'external-dependency', url })),
      ];
      results.push({ viewport, facts, visual, environmentFindings,
        status: observationStatus([{ status: facts.status }, { status: visual.status }, ...environmentFindings]) });
    } catch (error) {
      results.push({ viewport, status: 'UNKNOWN', error: error.message });
    } finally { for (const context of contexts.reverse()) await context.close(); }
  }
  const report = { schema_version: 1, status: observationStatus(results), artifact, oracle, engine,
    environment: { browser: browser.version(), platform: platform(), release: release(), profile: VISUAL_PROFILE },
    results, evidence, author_code_during_measurement: false, manual_override_allowed: false };
  const reportRef = await save('report.json', JSON.stringify(report, null, 2));
  return { report, reportRef };
}

if (process.argv[1] && import.meta.url === pathToFileURL(resolve(process.argv[1])).href) {
  const [, , input, output, source] = process.argv;
  if (!input || !output) throw new Error('usage: node drawing-quality.mjs INPUT.html NEW_OUTPUT_DIRECTORY [SOURCE_FACTS.json]');
  const { chromium } = require('@playwright/test');
  const browser = await chromium.launch({ headless: true });
  try {
    const { report, reportRef } = await verifyDrawing({ browser, html: await readFile(input, 'utf8'), outputDir: resolve(output),
      sourceFacts: source ? JSON.parse(await readFile(source, 'utf8')) : {},
      progress: value => process.stderr.write(`${JSON.stringify(value)}\n`) });
    process.stdout.write(`${JSON.stringify({ status: report.status, report: reportRef })}\n`);
    process.exitCode = report.status === 'PASS' ? 0 : report.status === 'FAIL' ? 1 : 2;
  } finally { await browser.close(); }
}
