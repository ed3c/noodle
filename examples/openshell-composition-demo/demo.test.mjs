import test from 'node:test';
import assert from 'node:assert/strict';
import { EventEmitter } from 'node:events';
import { mkdtemp, readFile, writeFile, rm } from 'node:fs/promises';
import os from 'node:os';
import path from 'node:path';
import { checkpoint, WAIT_MS } from './skills/component-author/scripts/checkpoint.mjs';
import { probeHost, runProbe, MAX_BYTES, DEADLINE_MS } from './skills/network-proof/scripts/probe.mjs';

async function fixture(t) {
  const directory = await mkdtemp(path.join(os.tmpdir(), 'composition-demo-'));
  t.after(() => rm(directory, { recursive: true, force: true }));
  return directory;
}

test('checkpoint persists partial evidence, refuses same attempt, and observes a distinct attempt', async (t) => {
  const directory = await fixture(t);
  const caseFile = path.join(directory, 'case.json');
  await writeFile(caseFile, JSON.stringify({ neutral_name: 'public fixture' }));
  const partial = '<div data-demo-state="incomplete">unfinished public fixture</div>';
  await writeFile(path.join(directory, 'component.partial.html'), partial);
  const reports = [];
  const options = { directory, caseFile, sessionDirectory: '/sandbox/noodle-sessions/first', report: (item) => reports.push(item), wait: async (ms) => {
    assert.equal(ms, WAIT_MS);
    const saved = JSON.parse(await readFile(path.join(directory, 'progress-marker.json'), 'utf8'));
    assert.equal(saved.original_session_directory, options.sessionDirectory);
    assert.equal(await readFile(path.join(directory, 'component.partial.html'), 'utf8'), partial);
    assert.equal(reports[0].event, 'checkpoint_wait_started');
  } };
  assert.equal(await checkpoint(options), 75);
  assert.equal(reports.at(-1).native_stop_proven, false);
  await assert.rejects(checkpoint(options), /original session cannot continue/);
  const marker = await readFile(path.join(directory, 'progress-marker.json'), 'utf8');
  assert.equal(await checkpoint({ ...options, sessionDirectory: '/sandbox/noodle-sessions/second', wait: () => assert.fail('new attempt waited again') }), 0);
  assert.equal(await readFile(path.join(directory, 'progress-marker.json'), 'utf8'), marker);
  await writeFile(path.join(directory, 'component.partial.html'), partial + 'changed');
  await assert.rejects(checkpoint({ ...options, sessionDirectory: '/sandbox/noodle-sessions/third' }), /changed/);
});

test('checkpoint refuses missing unique identity before touching files', async () => {
  await assert.rejects(checkpoint({ sessionDirectory: '/sandbox/session' }), /unique guest session/);
});

function transport(action, calls) {
  return (options, onResponse) => {
    calls.push(options);
    const req = new EventEmitter();
    req.destroy = () => {};
    req.end = () => queueMicrotask(() => action(req, onResponse));
    return req;
  };
}

test('probe uses exact credential-free GET without following redirect', async () => {
  const calls = [];
  const result = await probeHost('example.com', transport((_req, onResponse) => {
    const response = new EventEmitter();
    response.statusCode = 302;
    response.headers = { location: 'https://never-follow.invalid/' };
    onResponse(response);
    response.emit('data', Buffer.from('redirect'));
    response.emit('end');
  }, calls));
  assert.deepEqual(calls, [{ hostname: 'example.com', port: 443, path: '/', method: 'GET', agent: false }]);
  assert.equal(result.outcome, 'HTTP_RESPONSE');
  assert.equal(result.status_code, 302);
  assert.equal(result.redirect_followed, false);
  assert.throws(() => probeHost('arbitrary.invalid'), /not fixed/);
});

test('reset and body limit remain unknown, never policy denial', async () => {
  const reset = await probeHost('example.org', transport((req) => req.emit('error', { code: 'ECONNRESET' }), []));
  assert.equal(reset.outcome, 'UNKNOWN');
  assert.equal(reset.error_code, 'ECONNRESET');
  const large = await probeHost('example.com', transport((_req, onResponse) => {
    const response = new EventEmitter(); response.statusCode = 200;
    onResponse(response); response.emit('data', Buffer.alloc(MAX_BYTES + 1));
  }, []));
  assert.equal(large.outcome, 'UNKNOWN');
  assert.equal(large.error_code, 'BODY_LIMIT_EXCEEDED');
});

test('absolute deadline covers a request with no socket progress', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'] });
  const pending = probeHost('example.com', transport(() => {}, []));
  t.mock.timers.tick(DEADLINE_MS);
  assert.equal((await pending).error_code, 'DEADLINE_EXCEEDED');
});

test('each stage probes once per fixed host and reuses only its finished report', async (t) => {
  for (const stage of ['component', 'drawing']) {
    const directory = await fixture(t); const hosts = [];
    const probe = async (host) => { hosts.push(host); return { host, outcome: 'UNKNOWN', error_code: 'fixture' }; };
    const first = await runProbe(stage, directory, probe);
    assert.deepEqual(hosts, stage === 'component' ? ['example.com', 'example.org'] : ['example.org', 'example.com']);
    assert.equal(first.policy_verdict, 'REQUIRES_NATIVE_OWNER_EVIDENCE');
    assert.deepEqual(await runProbe(stage, directory, () => assert.fail('replayed request')), first);
    assert.equal(hosts.length, 2);
  }
});

test('interrupted intent refuses replay instead of guessing missing network effects', async (t) => {
  const directory = await fixture(t); let calls = 0;
  await assert.rejects(runProbe('component', directory, async () => { calls++; throw new Error('interrupted fixture'); }), /interrupted/);
  await assert.rejects(runProbe('component', directory, () => assert.fail('replayed request')), /EEXIST/);
  assert.equal(calls, 1);
});
