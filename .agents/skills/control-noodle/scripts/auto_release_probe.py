#!/usr/bin/env python3
"""Drive one original Host task against a private native Codex/MCP fixture."""
import argparse
import datetime
import fcntl
import hashlib
import http.server
import json
import os
from pathlib import Path
import shlex
import socket
import subprocess
import sys
import threading
import time
import uuid


def save(path, value):
    path.write_text(json.dumps(value, ensure_ascii=False, indent=2) + '\n')
    path.chmod(0o600)


def wait(check, reason, seconds=90):
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        result = check()
        if result:
            return result
        time.sleep(.1)
    raise RuntimeError(reason)


def tool_selection(tools, namespace=None):
    for tool in tools:
        if tool.get('type') == 'namespace':
            result = tool_selection(tool.get('tools', []), tool.get('name'))
            if result:
                return result
        name = tool.get('name', '')
        if tool.get('type') == 'function' and 'release_probe' in (str(namespace) + name) and 'ping' in name:
            return namespace, name
    return None


def control(runtime, evidence, action, value=None):
    intent = evidence / ('control-' + action + '.json')
    if intent.exists():
        original = json.loads(intent.read_bytes())
        if original.get('action') != action or original.get('value') != value:
            raise RuntimeError('Original control intent changed')
        return original  # Readback only; never append an existing intent again.
    command = {'id': uuid.uuid4().hex, 'action': action}
    if value is not None:
        command['value'] = value
    with (runtime / 'control.lock').open('a+b') as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        save(intent, command)
        with (runtime / 'control.ndjson').open('ab') as stream:
            stream.write((json.dumps(command) + '\n').encode())
            stream.flush()
            os.fsync(stream.fileno())
    return command


def process_absent(pid):
    return subprocess.run(['/bin/ps', '-p', str(pid)], capture_output=True).returncode != 0


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--binary', type=Path, required=True)
    parser.add_argument('--host-root', type=Path, required=True)
    parser.add_argument('--codex', type=Path, required=True)
    parser.add_argument('--evidence', type=Path, required=True)
    args = parser.parse_args()
    binary, host, codex, evidence = (p.resolve() for p in (args.binary, args.host_root, args.codex, args.evidence))
    evidence.mkdir(mode=0o700, parents=True, exist_ok=False)
    home, project, tools, skills = (evidence / n for n in ('profile', 'project', 'bin', 'skills'))
    for path in (home, project, tools, skills):
        path.mkdir()
    env = {'HOME': str(home), 'CODEX_HOME': str(home), 'PATH': str(tools) + ':/usr/bin:/bin:/usr/sbin:/sbin',
           'NOODLE_HOST_DIR': str(host), 'LANG': 'en_US.UTF-8'}

    def cli(*argv, check=True):
        result = subprocess.run([str(binary), *argv], env=env, cwd=project, capture_output=True, text=True, timeout=40)
        with (evidence / 'commands.ndjson').open('a') as stream:
            stream.write(json.dumps({'argv': [str(binary), *argv], 'exit': result.returncode,
                                     'stdout': result.stdout, 'stderr': result.stderr}) + '\n')
        if check and result.returncode:
            raise RuntimeError(result.stdout + result.stderr)
        return json.loads(result.stdout)

    before = cli('host', 'resources')
    digest = hashlib.sha256(binary.read_bytes()).hexdigest()
    if before.get('binary_sha256') != digest or before.get('rolling_host_version') != 1:
        raise RuntimeError('Selected binary does not own this rolling Host')
    pool_before = json.loads((host / 'pool.json').read_bytes())
    save(evidence / 'before.json', {'resources': before, 'pool': pool_before, 'binary_sha256': digest,
                                  'codex_sha256': hashlib.sha256(codex.read_bytes()).hexdigest()})
    seen = []
    mcp_log = evidence / 'mcp-generations.ndjson'

    class Provider(http.server.BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_POST(self):
            body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
            seen.append(body)
            if len(seen) > 4:
                self.send_error(400, 'Bounded verification request limit reached')
                return
            save(evidence / 'provider-requests.json', seen)
            witness = mcp_log.read_text() if mcp_log.exists() else ''
            selected = tool_selection(body.get('tools', []))
            if not any(json.loads(line).get('event') == 'ping' for line in witness.splitlines()):
                if len(seen) > 1:
                    self.send_error(400, 'Original MCP call did not execute; inspect the first tool outcome')
                    return
                if selected is None:
                    self.send_error(400, 'Private MCP ping not exposed in native tool declarations')
                    return
                namespace, name = selected
                item = {'type': 'function_call', 'call_id': 'control-noodle-private-ping', 'name': name, 'arguments': '{}'}
                if namespace:
                    item['namespace'] = namespace
            else:
                item = {'type': 'message', 'role': 'assistant', 'id': 'release-complete',
                        'content': [{'type': 'output_text', 'text': 'CONTROL_NOODLE_AUTO_RELEASE_COMPLETE'}]}
            self.send_response(200)
            self.send_header('Content-Type', 'text/event-stream')
            self.end_headers()
            events = [{'type': 'response.created', 'response': {'id': 'private-response'}},
                      {'type': 'response.output_item.done', 'item': item},
                      {'type': 'response.completed', 'response': {'id': 'private-response',
                       'usage': {'input_tokens': 0, 'output_tokens': 0, 'total_tokens': 0}}}]
            for event in events:
                self.wfile.write(('data: ' + json.dumps(event) + '\n\n').encode())
            self.wfile.flush()

    provider = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Provider)
    threading.Thread(target=provider.serve_forever, daemon=True).start()
    port = provider.server_port
    seatbelt = f'(version 1)(allow default)(deny network*)(allow network-outbound (remote ip "localhost:{port}"))'
    (tools / 'codex').write_text('#!/bin/sh\nexec /usr/bin/sandbox-exec -p ' + shlex.quote(seatbelt) + ' ' + shlex.quote(str(codex)) + ' "$@"\n')
    (tools / 'codex').chmod(0o700)
    fixture = Path(__file__).with_name('mcp_release_fixture.py').resolve()
    profile = f'''thread_unload_delay_secs = 0
model_provider = "offline"
model = "offline"
approval_policy = "never"
[model_providers.offline]
name = "offline"
base_url = "http://127.0.0.1:{port}/v1"
wire_api = "responses"
[mcp_servers.release_probe]
command = {json.dumps(sys.executable)}
args = [{json.dumps(str(fixture))}, {json.dumps(str(mcp_log))}]
enabled_tools = ["ping"]
[mcp_servers.release_probe.tools.ping]
approval_mode = "approve"
'''
    (home / 'config.toml').write_text(profile)
    order = 'control-noodle-auto-release'
    (skills / 'release-probe').mkdir()
    (skills / 'release-probe/SKILL.md').write_text('---\nname: release-probe\ndescription: Private native MCP release verification.\nschedule: Only the admitted verification order.\n---\nCall release_probe ping and return its sentinel.\n')
    (project / '.noodle.toml').write_text(f'''mode = "manual"
[server]
enabled = false
[routing.defaults]
provider = "codex"
model = "offline"
[skills]
paths = [{json.dumps(str(skills))}]
[agents.codex]
path = {json.dumps(str(tools))}
require_typed_outcome = false
[runtime]
default = "codex-app-server"
[runtime.process]
admitted_orders = ["{order}"]
''')
    (project / '.gitignore').write_text('.noodle/\n.worktrees/\n')
    for argv in (['init', '-b', 'fixture'], ['config', 'user.name', 'Control Noodle'],
                 ['config', 'user.email', 'control-noodle@invalid'], ['add', '.'], ['commit', '-m', 'Create isolated verification source']):
        subprocess.run(['/usr/bin/git', *argv], env=env, cwd=project, capture_output=True, check=True)
    request = evidence / 'host-request.json'
    native = None
    try:
        native = cli('--project-dir', str(project), 'host', 'attach', '--mode', 'manual', '--cleanup-completed', '--request-file', str(request))
        save(evidence / 'admitted.json', native)
        runtime = project / '.noodle'
        snapshot = json.loads((runtime / 'state.snapshot.json').read_bytes())
        proposal = {'initial_revision': snapshot['order_revision'], 'orders': [{'id': order, 'stages': [
            {'do': 'release-probe', 'with': 'codex', 'model': 'offline', 'runtime': 'codex-app-server',
             'prompt': 'Verify the private release_probe MCP ping and complete this bounded release probe.'}]}]}
        save(evidence / 'original-proposal.json', proposal)
        with (runtime / 'orders-next.json').open('x') as stream:
            json.dump(proposal, stream)
        wait(lambda: order in json.loads((runtime / 'state.snapshot.json').read_bytes())['state']['orders'], 'Original proposal not admitted', 20)
        command = control(runtime, evidence, 'mode', 'supervised')
        print(json.dumps({'stage': 'native_worker_running', 'request_id': native['request_id'], 'host': native['host_instance_id']}), flush=True)

        def released():
            rows = list((runtime / 'sessions').glob('*/native.json'))
            if len(rows) != 1:
                return None
            record = json.loads(rows[0].read_bytes())
            return (rows[0], record) if record.get('state') == 'released' else None

        path, record = wait(released, 'Native worker did not automatically release', 120)
        save(evidence / 'native-released.json', record)
        if record.get('terminal') != 'completed' or record.get('server_exit') != 0 or not record.get('unsubscribe_ack') or record.get('unload_readback', {}).get('data') != []:
            raise RuntimeError('Native release chain is incomplete')
        witness = [json.loads(line) for line in mcp_log.read_text().splitlines()]
        if not any(r['event'] == 'ping' for r in witness):
            raise RuntimeError('MCP was started but not used')
        generations = {r['pid'] for r in witness if r['event'] == 'started'}
        observed = {r['pid'] for r in record.get('observed_children', [])}
        if not generations or not generations <= observed:
            raise RuntimeError('MCP generation is absent from native descendant observation')
        exits = [{'pid': pid, 'process_absent': process_absent(pid)} for pid in sorted(generations)]
        save(evidence / 'mcp-exit-observation.json', {'observed_generations': exits,
              'native_observed_children': record.get('observed_children', [])})
        if not all(row['process_absent'] for row in exits):
            raise RuntimeError('Original MCP process still exists')
        # File-only observation cannot drive Host cleanup on behalf of its timer.
        def detached():
            pool = json.loads((host / 'pool.json').read_bytes())
            owner = pool['owners'][native['request_id']]
            return owner if owner['current'].get('context_state') == 'detached' else None
        owner = wait(detached, 'Completed context did not automatically detach', 30)
        if owner['current'].get('terminal_cause') != 'auto-completed':
            raise RuntimeError('Context ended without the original automatic completion cause')
        after = cli('host', 'resources')
        pool_after = json.loads((host / 'pool.json').read_bytes())
        if pool_after['active'] != pool_before['active'] or any(pool_after['owners'].get(key) != value for key, value in pool_before['owners'].items()):
            raise RuntimeError('Drive changed an original Host/task owner')
        if after['resources']['active_allocations'] != before['resources']['active_allocations']:
            raise RuntimeError('Original shared allocation baseline not restored')
        if after['contexts'] != before['contexts'] or any(after['resources'][key] != before['resources'][key]
                for key in ('active_workers', 'active_residents', 'owned_allocations', 'unresolved_residents')):
            raise RuntimeError('Original shared Host resource baseline changed')
        if any(after['resources']['shared_quota'][key] != before['resources']['shared_quota'][key]
               for key in ('allocations', 'residents')):
            raise RuntimeError('Original shared quota baseline changed')
        result = {'status': 'PASS', 'host_instance_id': after['host_instance_id'], 'native_record': str(path.resolve()),
                  'thread_id': record['thread_id'], 'mcp_generations': sorted(generations), 'native_owner': owner,
                  'resources_after': after, 'original_owners_unchanged': True, 'external_network_denied': True,
                  'evidence_survives_cleanup': True, 'authorizes_landing': False}
        save(evidence / 'result.json', result)
        print(json.dumps({'stage': 'passed', 'evidence': str(evidence), 'mcp_generations': sorted(generations)}), flush=True)
    except BaseException as error:
        save(evidence / 'failure.json', {'type': type(error).__name__, 'message': str(error),
                                       'original_request': str(request), 'cleanup': 'Original task readback required; no PID/name kill or effect replay'})
        if native is not None:
            cleanup = {'status': 'pending', 'native_request': str(request)}
            try:
                current = cli('--project-dir', str(project), 'host', 'status', '--request-file', str(request))
                if current.get('context_state') == 'active':
                    control(runtime, evidence, 'pause')
                    control(runtime, evidence, 'stop-all')
                    wait(released, 'Owned native release remains unresolved during cleanup', 30)
                    wait(lambda: not json.loads((runtime / 'status.json').read_bytes()).get('active'), 'Original completion watcher has not settled', 15)
                    current = cli('--project-dir', str(project), 'host', 'detach', '--request-file', str(request),
                                  '--context-id', current['context_id'], '--instance-id', current['host_instance_id'])
                cleanup = {'status': 'detached' if current.get('context_state') == 'detached' else 'pending', 'readback': current}
            except BaseException as cleanup_error:
                cleanup['problem'] = str(cleanup_error)
            save(evidence / 'failed-drive-cleanup.json', cleanup)
        raise
    finally:
        provider.shutdown()
        provider.server_close()


if __name__ == '__main__':
    main()
