#!/usr/bin/env python3
"""Observe the original Host selection. No launch, admission or control writes."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--binary', type=Path, required=True)
    parser.add_argument('--host-root', type=Path, required=True)
    parser.add_argument('--request-file', type=Path)
    args = parser.parse_args()
    root, binary = args.host_root.resolve(), args.binary.resolve()
    if not root.is_dir() or not binary.is_file():
        raise RuntimeError('Original Host root or executable is unavailable')
    env = dict(os.environ, NOODLE_HOST_DIR=str(root))
    actions = []

    def read(*argv):
        command = [str(binary), *argv]
        run = subprocess.run(command, env=env, capture_output=True, text=True, timeout=15)
        actions.append({'argv': command, 'exit': run.returncode, 'stdout': run.stdout, 'stderr': run.stderr})
        if run.returncode:
            raise RuntimeError(run.stderr.strip() or run.stdout)
        return json.loads(run.stdout)

    # The original CLI validates pool custody or performs its legacy readback.
    selection = read('host', 'switch', '--status')
    observed = read('host', 'resources')
    if observed.get('protocol') != 'noodle-host-v1' or not observed.get('host_instance_id') or not observed.get('host_pid'):
        raise RuntimeError('Original Host identity is unavailable')
    digest = hashlib.sha256(binary.read_bytes()).hexdigest()
    rolling = 'generations' in selection
    if rolling:
        selected = selection['generations'][selection['active']]
        if selected['id'] != observed['host_instance_id'] or selected['binary_sha256'] != digest or observed.get('binary_sha256') != digest:
            raise RuntimeError('Executable differs from selected Host; inspect original native selection')
    elif selection.get('native_resources', {}).get('host_instance_id') != observed['host_instance_id'] or selection.get('native_resources', {}).get('host_pid') != observed['host_pid']:
        raise RuntimeError('Legacy Host identity changed during readback')
    result = {'status': 'identity_observed', 'kind': 'rolling' if rolling else 'legacy',
              'host_root': str(root), 'native_selection': selection, 'native_resources': observed,
              'caller_binary_sha256': digest, 'actions': actions,
              'limits': ['Identity is not admission readiness.',
                         'Legacy readback may not identify its executable before first admission.',
                         'Native reads may maintain directory permissions and metadata locks.']}
    if args.request_file:
        request = json.loads(args.request_file.read_bytes())
        result['task'] = read('--project-dir', request['project_dir'], 'host', 'status', '--request-file', str(args.request_file.resolve()))
    print(json.dumps(result, ensure_ascii=False, indent=2))


if __name__ == '__main__':
    try:
        main()
    except (OSError, ValueError, KeyError, RuntimeError, subprocess.TimeoutExpired) as error:
        print(json.dumps({'status': 'refused', 'problem': str(error)}, ensure_ascii=False))
        sys.exit(1)
