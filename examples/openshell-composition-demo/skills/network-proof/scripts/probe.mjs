import { createHash } from 'node:crypto';
import { open, readFile } from 'node:fs/promises';
import https from 'node:https';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

export const DEADLINE_MS = 10_000;
export const MAX_BYTES = 65_536;
const stages = { component: ['example.com', 'example.org'], drawing: ['example.org', 'example.com'] };
const sha = (bytes) => createHash('sha256').update(bytes).digest('hex');

export function probeHost(host, request = https.request) {
  if (!['example.com', 'example.org'].includes(host)) throw new Error('probe host is not fixed');
  return new Promise((resolve) => {
    let finished = false;
    let req;
    const started = new Date().toISOString();
    const finish = (result) => {
      if (finished) return;
      finished = true;
      clearTimeout(timer);
      resolve({ host, method: 'GET', path: '/', started_at: started, ...result });
    };
    const timer = setTimeout(() => {
      finish({ outcome: 'UNKNOWN', error_code: 'DEADLINE_EXCEEDED' });
      req?.destroy();
    }, DEADLINE_MS);
    try {
      req = request({ hostname: host, port: 443, path: '/', method: 'GET', agent: false }, (response) => {
        let bytes = 0;
        const hash = createHash('sha256');
        response.on('data', (chunk) => {
          bytes += chunk.length;
          if (bytes > MAX_BYTES) {
            finish({ outcome: 'UNKNOWN', error_code: 'BODY_LIMIT_EXCEEDED', status_code: response.statusCode });
            req.destroy();
          } else { hash.update(chunk); }
        });
        response.on('end', () => finish({ outcome: 'HTTP_RESPONSE', status_code: response.statusCode, bytes, body_sha256: hash.digest('hex'), redirect_followed: false }));
        response.on('error', (error) => finish({ outcome: 'UNKNOWN', error_code: String(error.code ?? 'RESPONSE_ERROR') }));
        response.on('aborted', () => finish({ outcome: 'UNKNOWN', error_code: 'RESPONSE_ABORTED' }));
      });
      req.on('error', (error) => finish({ outcome: 'UNKNOWN', error_code: String(error.code ?? 'REQUEST_ERROR') }));
      req.end();
    } catch (error) { finish({ outcome: 'UNKNOWN', error_code: String(error.code ?? 'REQUEST_ERROR') }); }
  });
}

async function writeExclusive(file, value) {
  const handle = await open(file, 'wx', 0o600);
  try { await handle.writeFile(JSON.stringify(value, null, 2) + '\n'); await handle.sync(); }
  finally { await handle.close(); }
  const parent = await open(path.dirname(file), 'r');
  try { await parent.sync(); } finally { await parent.close(); }
}

export async function runProbe(stage, directory, probe = probeHost) {
  if (!Object.hasOwn(stages, stage)) throw new Error('stage is not component or drawing');
  const scriptSHA = sha(await readFile(fileURLToPath(import.meta.url)));
  const reportPath = path.join(directory, `network-proof-${stage}.json`);
  const intentPath = path.join(directory, `network-proof-${stage}.intent.json`);
  const identity = { schema: 1, stage, script_sha256: scriptSHA, expected_allowed_host: stages[stage][0], expected_denied_host: stages[stage][1] };
  let report;
  try { report = JSON.parse(await readFile(reportPath, 'utf8')); }
  catch (error) { if (error.code !== 'ENOENT') throw error; }
  if (report) {
    const intent = JSON.parse(await readFile(intentPath, 'utf8'));
    if (Object.entries(identity).some(([key, value]) => report[key] !== value || intent[key] !== value)
      || report.policy_verdict !== 'REQUIRES_NATIVE_OWNER_EVIDENCE' || report.observations?.length !== 2
      || report.observations.some((item, index) => item.host !== stages[stage][index])) {
      throw new Error('existing network report differs from the fixed probe');
    }
    return report;
  }
  await writeExclusive(intentPath, identity);
  const observations = [];
  for (const host of stages[stage]) observations.push(await probe(host));
  report = { ...identity, policy_verdict: 'REQUIRES_NATIVE_OWNER_EVIDENCE', observations };
  await writeExclusive(reportPath, report);
  return report;
}

if (process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    if (process.argv.length !== 3) throw new Error('one fixed stage argument is required');
    console.log(JSON.stringify(await runProbe(process.argv[2], process.cwd()), null, 2));
  } catch (error) {
    console.error(JSON.stringify({ outcome: 'UNKNOWN', error: error.message, retry_allowed: false }));
    process.exitCode = 1;
  }
}
