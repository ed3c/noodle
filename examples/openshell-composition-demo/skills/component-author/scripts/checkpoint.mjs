import { createHash } from 'node:crypto';
import { open, readFile, lstat } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

export const WAIT_MS = 180_000;
const sha = (bytes) => createHash('sha256').update(bytes).digest('hex');

async function regular(file) {
  const info = await lstat(file);
  if (!info.isFile() || info.size === 0 || info.size > 2 * 1024 * 1024) {
    throw new Error('checkpoint input is empty, oversized, or not a file');
  }
  return readFile(file);
}

export async function checkpoint({ directory, caseFile, sessionDirectory, wait, report }) {
  if (!/^\/sandbox\/noodle-sessions\/[^/]+$/.test(sessionDirectory ?? '')) {
    throw new Error('original unique guest session directory is unavailable');
  }
  const partialPath = path.join(directory, 'component.partial.html');
  const markerPath = path.join(directory, 'progress-marker.json');
  const caseBytes = await regular(caseFile);
  JSON.parse(caseBytes.toString('utf8'));
  const partial = await regular(partialPath);
  if (!partial.toString('utf8').includes('data-demo-state="incomplete"')) {
    throw new Error('partial component lacks its explicit incomplete label');
  }
  const expected = {
    schema: 1, kind: 'composition-demo-checkpoint',
    case_sha256: sha(caseBytes), partial_sha256: sha(partial),
  };
  let prior;
  try { prior = JSON.parse(await readFile(markerPath, 'utf8')); }
  catch (error) { if (error.code !== 'ENOENT') throw error; }
  if (prior) {
    if (Object.entries(expected).some(([key, value]) => prior[key] !== value)
      || !/^\/sandbox\/noodle-sessions\/[^/]+$/.test(prior.original_session_directory ?? '')) {
      throw new Error('saved checkpoint case, partial component, or identity changed');
    }
    if (prior.original_session_directory === sessionDirectory) {
      throw new Error('this original session cannot continue past its interruption checkpoint');
    }
    report({ event: 'saved_checkpoint_observed', ...expected, original_session_directory: prior.original_session_directory, current_session_directory: sessionDirectory });
    return 0;
  }
  const file = await open(markerPath, 'wx', 0o600);
  try {
    await file.writeFile(JSON.stringify({ ...expected, original_session_directory: sessionDirectory }) + '\n');
    await file.sync();
  } finally { await file.close(); }
  const parent = await open(directory, 'r');
  try { await parent.sync(); } finally { await parent.close(); }
  report({ event: 'checkpoint_wait_started', wait_ms: WAIT_MS, ...expected, original_session_directory: sessionDirectory });
  await wait(WAIT_MS);
  report({ event: 'checkpoint_wait_elapsed', status: 'incomplete', native_stop_proven: false });
  return 75;
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    if (process.argv.length !== 3) throw new Error('one exact materialized case path is required');
    process.exitCode = await checkpoint({
      directory: process.cwd(), caseFile: process.argv[2], sessionDirectory: process.env.NOODLE_SESSION_DIR,
      wait: (ms) => new Promise((resolve) => setTimeout(resolve, ms)),
      report: (value) => console.log(JSON.stringify(value)),
    });
  } catch (error) {
    console.error(JSON.stringify({ event: 'checkpoint_refused', error: error.message }));
    process.exitCode = 1;
  }
}
