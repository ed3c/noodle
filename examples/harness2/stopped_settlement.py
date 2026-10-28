"""Settle one retained call through an explicit stopped reader source migration.

This module is also cherry-pickable onto the frozen training bridge: it never
constructs RequestTemplate or adds defaults to an original serialized binding.
"""
import json
import os
import subprocess
from dataclasses import asdict, replace
from pathlib import Path

from owner_contract import OwnerResult, tree_ref
from soodles_bridge import (
    decode,
    host_identity,
    read_ref,
    reference,
    require,
    retained_snapshot,
    save,
)


def same_file(ref):
    require(reference(Path(ref['path'])) == ref, 'stopped settlement source changed')


def reader_command(migration, migration_ref, request, output):
    reader = migration['reader']
    same_file(reader['python'])
    for ref in reader['sources']:
        same_file(ref)
    entries = [ref for ref in reader['sources'] if Path(ref['path']).name == 'task_stopped_integration.py']
    require(len(entries) == 1, 'explicit stopped reader entrypoint is missing or ambiguous')
    return [reader['python']['path'], '-B', entries[0]['path'], 'inspect', '--request-file', request['path'],
            '--migration', migration_ref['path'], migration_ref['sha256'], '--output', str(output)]


def verify_stopped_reconciliation(call, proof):
    require(proof.get('kind') == 'stopped-original-call-integration'
            and proof.get('call_sha256') == call.fingerprint()
            and decode(read_ref(proof['call'])) == asdict(call), 'stopped settlement call differs')
    binding = decode(read_ref(proof['binding']))
    require(binding['call_sha256'] == call.fingerprint(), 'stopped original binding differs')
    for ref in binding['inputs']:
        same_file(ref)
    require(tree_ref(proof['retained_result']['path']) == proof['retained_result'], 'retained original result changed')
    migration = decode(read_ref(proof['migration']))
    observed = decode(read_ref(proof['fresh_observation']))
    require(migration.get('kind') == 'stopped-original-task-integration'
            and migration.get('request') == proof['request']
            and observed.get('kind') == 'stopped_original_task_integration'
            and observed.get('owner') == 'soodles.loop' and observed.get('request') == proof['request']
            and observed.get('status') == 'observed' and observed.get('effects') == []
            and observed.get('authorizes_landing') is False
            and observed.get('execution_time_scope') == 'historical_original_verified_capture'
            and observed.get('integration_time_scope') == 'current_stopped_native_reconciliation'
            and observed['sources']['migration'] == proof['migration']
            and observed['sources']['reconciliation'] == migration['reconciliation']
            and observed['sources']['closure'] == migration['closure'],
            'explicit stopped integration readback differs')
    for ref in observed['sources'].values():
        same_file(ref)
    native = decode(read_ref(migration['reconciliation']))
    require(native.get('status') == 'reconciled' and native.get('owner') == 'Noodle stopped local merge'
            and native.get('candidate') == migration['candidate']
            and native.get('effects') == ['local_canonical_reconciliation'],
            'stopped native reconciliation is not complete')
    capture = decode(read_ref(observed['sources']['original_capture']))
    expected_host = host_identity(capture)
    inner = observed['inner']
    require(host_identity(observed) == expected_host
            and inner.get('native_host_time_scope') == 'historical_identity_original_owner_closed'
            and inner['native_host'].get('context_state') == 'closed'
            and inner.get('candidate') == migration['candidate'] == capture['inner']['candidate'],
            'stopped original candidate or historical Host identity differs')
    intent = decode(read_ref(proof['reader_intent']))
    receipt = decode(read_ref(proof['reader_receipt']))
    command = reader_command(migration, proof['migration'], proof['request'], proof['fresh_observation']['path'])
    require(intent == {'argv': command, 'migration': proof['migration'], 'request': proof['request'],
                       'reader': migration['reader']}
            and receipt.get('exit_code') == 0
            and decode(read_ref(receipt['stdout'])) == proof['fresh_observation'],
            'stopped reader transport does not bind the observed output')
    same_file(receipt['stderr'])
    return expected_host


def settle_stopped_call(bridge, gate, call, migration_ref, *, billing_receipt=None):
    """Read the same retained result; no prepare/enter/review/model/optimizer calls.

Session first performs original integrated acceptance through the explicit new
reader. This function only rechecks it and settles the existing unknown row.
"""
    migration = decode(read_ref(migration_ref))
    directory = bridge.root / call.fingerprint()
    call_ref = reference(directory / 'call.json')
    require(decode(read_ref(call_ref)) == asdict(call), 'original retained call differs')
    binding_ref = reference(directory / 'binding.json')
    binding = decode(read_ref(binding_ref))
    matches = [value for value in bridge.templates if decode(json.dumps(asdict(value))) == binding['template']]
    require(len(matches) == 1 and binding['call_sha256'] == call.fingerprint(),
            'original training template changed; no defaults or migration shim allowed')
    request = reference(directory / 'request.json')
    result_dir = directory / 'result'
    frozen = retained_snapshot(call, matches[0], request, result_dir)
    require(migration['request'] == request and migration['candidate'] == frozen[0],
            'migration is not bound to this retained candidate')
    key = json.dumps([call.group, call.run_id])
    row = gate.state['calls'].get(key)
    require(row and row['state'] == 'unknown' and row['input_sha256'] == call.fingerprint(),
            'original call is not awaiting explicit reconciliation')
    evidence = directory / ('stopped-settlement-' + migration_ref['sha256'])
    evidence.mkdir()
    output = evidence / 'readback.json'
    command = reader_command(migration, migration_ref, request, output)
    intent = save(evidence / 'intent.json', {'argv': command, 'migration': migration_ref,
                  'request': request, 'reader': migration['reader']})
    # The exact new entrypoint can only inspect stopped integration. No Host
    # locator or provider credentials are passed into this terminal reader.
    environment = {key: os.environ[key] for key in ('PATH', 'SYSTEMROOT', 'WINDIR') if key in os.environ}
    environment['PYTHONDONTWRITEBYTECODE'] = '1'
    with (evidence / 'stdout.json').open('xb') as out, (evidence / 'stderr.log').open('xb') as err:
        run = subprocess.run(command, cwd=Path(command[2]).parent, env=environment,
                             stdout=out, stderr=err, timeout=120, check=False)
    receipt = save(evidence / 'receipt.json', {'exit_code': run.returncode,
                   'stdout': reference(evidence / 'stdout.json'), 'stderr': reference(evidence / 'stderr.log')})
    require(run.returncode == 0, 'stopped reader did not produce a validated integration observation')
    observation_ref = reference(output)
    proof = {'kind': 'stopped-original-call-integration', 'call_sha256': call.fingerprint(),
        'call': call_ref, 'binding': binding_ref, 'request': request, 'migration': migration_ref,
        'fresh_observation': observation_ref, 'retained_result': tree_ref(result_dir),
        'reader_intent': intent, 'reader_receipt': receipt}
    verify_stopped_reconciliation(call, proof)
    observed = decode(read_ref(observation_ref))
    inner = observed['inner']
    progress = inner.get('review_progress') or {}
    selection = progress.get('review') or {}
    require(inner.get('owner') == 'issue-execution.local-task' and inner.get('acceptance') == 'PASS'
            and progress.get('status') == 'locally_integrated' and selection.get('decision') == 'approve'
            and selection.get('candidate') == frozen[0], 'original integrated acceptance is incomplete')
    settled = save(evidence / 'settled.json', {'call_sha256': call.fingerprint(), 'request': request,
        'candidate': frozen[0], 'original_review': observation_ref, 'result': tree_ref(result_dir),
        'cost_usd': None, 'wall_s': None, 'billing_authority': 'unresolved', 'hidden_acceptance': 'not evaluated'})
    result = OwnerResult(str(result_dir), tree_ref(result_dir)['sha256'], frozen[1], frozen[2],
                         settled['path'], settled['sha256'], True, None, None)
    if billing_receipt is not None:
        from openrouter_billing import verify_settlement
        bill = verify_settlement(call, result, billing_receipt)
        result = replace(result, cost_usd=bill['cost_usd'], billing_receipt=billing_receipt)
    proof.update(result_receipt=settled, completed_observation=observation_ref)
    recovery = save(evidence / 'reconciliation.json', proof)
    gate.reconcile_original_call(call, result, recovery)
    return result, recovery
