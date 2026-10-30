import {createHash, randomUUID} from 'node:crypto';
import {closeSync, existsSync, fsyncSync, lstatSync, openSync, readFileSync, realpathSync, renameSync, statSync, unlinkSync, writeFileSync} from 'node:fs';
import {basename, dirname, join, resolve} from 'node:path';
import {PeleError, render} from '../vendor/pele.mjs';

const usage = 'Usage: node scripts/render.mjs INPUT.mmd OUTPUT.svg [--css THEME.css]';
const args = process.argv.slice(2);

try {
  if (args.length !== 2 && !(args.length === 4 && args[2] === '--css')) {
    throw new Error(usage);
  }
  const input = realpathSync(args[0]);
  const output = join(realpathSync(dirname(resolve(args[1]))), basename(args[1]));
  const original = statSync(input);
  if (!original.isFile() || original.size > 4_000_000) {
    throw new Error('diagram input is not a bounded regular file');
  }
  let current;
  let target;
  try {
    target = lstatSync(output);
  } catch (error) {
    if (error.code !== 'ENOENT') throw error;
  }
  if (target) {
    if (!target.isFile() || target.isSymbolicLink()) {
      throw new Error('SVG output is not a regular file');
    }
    if (target.dev === original.dev && target.ino === original.ino) {
      throw new Error('SVG output refers to the diagram input');
    }
    current = readFileSync(output);
  }
  const source = readFileSync(input, 'utf8');
  const result = render(source, {limit: 1_000_000, outputLimit: 8_000_000, maxEdges: 10_000});
  let svg = result.svg;
  if (args[3]) {
    if (statSync(args[3]).size > 8_000_000) throw new Error('theme CSS exceeds the output limit');
    const css = readFileSync(args[3], 'utf8').replaceAll(']]>', ']]]]><![CDATA[>');
    svg = svg.replace(/(<svg\b[^>]*>)/, (_, opening) => `${opening}<style><![CDATA[${css}]]></style>`);
  }
  const bytes = Buffer.from(svg);
  if (bytes.length > 8_000_000) {
    throw new Error('themed SVG exceeds the output limit');
  }
  const changed = !current?.equals(bytes);
  if (changed) {
    const temporary = join(dirname(output), `.pele-${randomUUID()}.tmp`);
    let descriptor;
    try {
      descriptor = openSync(temporary, 'wx', 0o600);
      writeFileSync(descriptor, bytes);
      fsyncSync(descriptor);
      closeSync(descriptor);
      descriptor = undefined;
      renameSync(temporary, output);
    } finally {
      if (descriptor !== undefined) closeSync(descriptor);
      if (existsSync(temporary)) unlinkSync(temporary);
    }
  }
  console.log(JSON.stringify({renderer: 'pele', version: '1.9.0', output, changed,
    width: result.width, height: result.height, sha256: createHash('sha256').update(bytes).digest('hex')}));
} catch (error) {
  console.error(JSON.stringify({status: 'refused', renderer: 'pele',
    code: error instanceof PeleError ? error.code : 'input_or_output', message: error.message}));
  process.exitCode = 1;
}
