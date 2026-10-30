import test from 'node:test';
import assert from 'node:assert/strict';
import {createHash} from 'node:crypto';
import {spawnSync} from 'node:child_process';
import {cpSync, linkSync, mkdtempSync, readFileSync, readdirSync, rmSync, statSync, symlinkSync, writeFileSync} from 'node:fs';
import {tmpdir} from 'node:os';
import {join} from 'node:path';
import {fileURLToPath} from 'node:url';

const entry = fileURLToPath(new URL('../scripts/render.mjs', import.meta.url));
const diagram = 'stateDiagram-v2\n  [*] --> 外環\n  外環 --> 內環: 原准入\n  內環 --> 交付: 獨立驗收\n';

function fixture(t, source = diagram) {
  const directory = mkdtempSync(join(tmpdir(), 'pele-render-'));
  t.after(() => rmSync(directory, {recursive: true, force: true}));
  const input = join(directory, 'chart.mmd');
  const output = join(directory, 'chart.svg');
  writeFileSync(input, source);
  return {directory, input, output};
}

function run(input, output, ...args) {
  return spawnSync(process.execPath, [entry, input, output, ...args], {encoding: 'utf8'});
}

test('real Pele renders a Chinese statechart and reports the saved SVG', t => {
  const {input, output} = fixture(t);
  const result = run(input, output);
  assert.equal(result.status, 0, result.stderr);
  const report = JSON.parse(result.stdout);
  const svg = readFileSync(output, 'utf8');
  assert.match(svg, /^<svg\b/);
  for (const label of ['外環', '內環', '交付', '獨立驗收']) assert.ok(svg.includes(label));
  assert.equal(report.renderer, 'pele');
  assert.equal(report.version, '1.9.0');
  assert.ok(report.width > 0 && report.height > 0);
  assert.equal(report.sha256, createHash('sha256').update(svg).digest('hex'));
  assert.equal(readFileSync(input, 'utf8'), diagram);
});

test('statechart broken-rule classes and explicit theme CSS reach the SVG', t => {
  const {directory, input, output} = fixture(t, 'stateDiagram-v2\n  A --> B: unknown replay\n  class B breaks\n  classDef breaks fill:#ffecee,stroke:#e72e3b\n');
  const theme = join(directory, 'theme.css');
  const css = '.pele { --pele-bg: #123456; } .pele .breaks rect { stroke-width: 4px !important; }';
  writeFileSync(theme, css);
  const result = run(input, output, '--css', theme);
  assert.equal(result.status, 0, result.stderr);
  const svg = readFileSync(output, 'utf8');
  assert.match(svg, /class="[^"]*\bbreaks\b/);
  assert.ok(svg.includes(css));
  assert.ok(svg.includes('#ffecee'));
});

test('repeating the same request leaves the SVG and its modification time unchanged', t => {
  const {directory, input, output} = fixture(t);
  assert.equal(run(input, output).status, 0);
  const before = {bytes: readFileSync(output), modified: statSync(output).mtimeMs};
  const result = run(input, output);
  assert.equal(result.status, 0, result.stderr);
  assert.equal(JSON.parse(result.stdout).changed, false);
  assert.deepEqual(readFileSync(output), before.bytes);
  assert.equal(statSync(output).mtimeMs, before.modified);
  assert.deepEqual(readdirSync(directory).sort(), ['chart.mmd', 'chart.svg']);
});

test('changed diagrams replace the previous SVG without leaving temporary files', t => {
  const {directory, input, output} = fixture(t);
  assert.equal(run(input, output).status, 0);
  writeFileSync(input, 'flowchart LR\n A[New candidate] --> B[Independent test]\n');
  const result = run(input, output);
  assert.equal(result.status, 0, result.stderr);
  assert.equal(JSON.parse(result.stdout).changed, true);
  assert.ok(readFileSync(output, 'utf8').includes('New candidate'));
  assert.deepEqual(readdirSync(directory).sort(), ['chart.mmd', 'chart.svg']);
});

test('an invalid diagram preserves an existing SVG and returns the Pele error', t => {
  const {directory, input, output} = fixture(t, 'this is not a Mermaid diagram');
  const previous = '<svg xmlns="http://www.w3.org/2000/svg"><text>Original</text></svg>';
  writeFileSync(output, previous);
  const result = run(input, output);
  assert.equal(result.status, 1);
  const error = JSON.parse(result.stderr);
  assert.equal(error.status, 'refused');
  assert.equal(error.renderer, 'pele');
  assert.notEqual(error.code, 'input_or_output');
  assert.equal(readFileSync(output, 'utf8'), previous);
  assert.deepEqual(readdirSync(directory).sort(), ['chart.mmd', 'chart.svg']);
});

test('output cannot replace the source through the same path or a hard link', t => {
  const {directory, input} = fixture(t);
  const hardLink = join(directory, 'linked.svg');
  linkSync(input, hardLink);
  for (const output of [input, hardLink]) {
    const result = run(input, output);
    assert.equal(result.status, 1);
    assert.match(JSON.parse(result.stderr).message, /diagram input/);
  }
  assert.equal(readFileSync(input, 'utf8'), diagram);
});

test('symlink outputs do not replace another file', t => {
  const {directory, input, output} = fixture(t);
  const original = join(directory, 'original.svg');
  writeFileSync(original, 'retained');
  symlinkSync(original, output);
  const result = run(input, output);
  assert.equal(result.status, 1);
  assert.equal(readFileSync(original, 'utf8'), 'retained');
});

test('a dangling symlink output is preserved rather than replaced', t => {
  const {directory, input, output} = fixture(t);
  symlinkSync(join(directory, 'missing.svg'), output);
  const result = run(input, output);
  assert.equal(result.status, 1);
  assert.match(JSON.parse(result.stderr).message, /regular file/);
});

test('CSS text cannot close its SVG style element', t => {
  const {directory, input, output} = fixture(t);
  const theme = join(directory, 'theme.css');
  writeFileSync(theme, '/* ]]> </style><script>not markup</script> */');
  const result = run(input, output, '--css', theme);
  assert.equal(result.status, 0, result.stderr);
  const svg = readFileSync(output, 'utf8');
  assert.ok(svg.includes(']]]]><![CDATA[>'));
  assert.match(svg, /<style><!\[CDATA\[/);
});

test('vendored renderer and license retain the recorded upstream bytes', () => {
  const manifest = JSON.parse(readFileSync(new URL('../source.json', import.meta.url), 'utf8'));
  for (const file of manifest.files) {
    const bytes = readFileSync(new URL('../' + file.path, import.meta.url));
    assert.equal(bytes.length, file.bytes);
    assert.equal(createHash('sha256').update(bytes).digest('hex'), file.sha256);
  }
});

test('a relocated skill renders without host npm module resolution', t => {
  const {directory, input, output} = fixture(t);
  const relocated = join(directory, 'transported-skill');
  cpSync(fileURLToPath(new URL('..', import.meta.url)), relocated, {recursive: true});
  const result = spawnSync(process.execPath, [join(relocated, 'scripts/render.mjs'), input, output], {
    cwd: directory, env: {PATH: '', HOME: directory}, encoding: 'utf8',
  });
  assert.equal(result.status, 0, result.stderr);
  assert.ok(readFileSync(output, 'utf8').includes('原准入'));
});
