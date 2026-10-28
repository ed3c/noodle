"""Read-only proof for one original, effect-free Host capacity refusal.

This module never invokes Soodles or Noodle. The human/operator performs the
original native continuation and supplies immutable evidence of that action.
"""
from pathlib import Path
import re


def verify_admission_continuation(call, proof, refused, original_intent, original_receipt):
    from soodles_bridge import decode, host_identity, original_inner, read_ref, reference, require

    evidence = decode(read_ref(proof['admission_continuation']))
    require(evidence.get('schema') == 1
            and evidence.get('kind') == 'original-capacity-admission-continuation'
            and evidence.get('call_sha256') == call.fingerprint()
            and evidence.get('request') == proof['request']
            and evidence.get('original_observation') == proof['original_observation'],
            'capacity continuation does not bind the original failed call')
    require(original_receipt.get('exit_code') == 2
            and refused.get('owner') == 'soodles.loop' and refused.get('status') == 'refused'
            and refused.get('problem') == {'field': 'task.host.attach', 'value': 'host context limit reached (16)'}
            and refused.get('effects') == [] and refused.get('authorizes_landing') is False
            and refused.get('next') == {'kind': 'input', 'owner': 'Noodle', 'required': ['original_request_readback']}
            and 'inner' not in refused and proof.get('retained_result') is None,
            'original operation is not the exact effect-free capacity refusal')
    read_ref(original_receipt['stderr'])
    body = decode(read_ref(proof['request']))
    template = decode(read_ref(proof['binding']))['template']
    require(body['repository'] == template['repository'] and body['noodle'] == template['noodle']['path']
            and body['profile'] == template['profile']['path'], 'continued task differs from its original template')
    read_ref(template['noodle'])
    read_ref(template['profile'])
    before = decode(read_ref(evidence['request_before']))
    after = decode(read_ref(evidence['request_after']))
    native_path = str(Path(body['output']) / 'admission' / 'host-request.json')
    config_path = str(Path(body['output']) / 'admission' / 'noodle.toml')
    expected_keys = {'schema', 'request_id', 'operation', 'project_dir', 'config_path', 'config_sha256',
                     'mode', 'binary_sha256', 'environment_sha256', 'status', 'host_dir', 'cleanup_completed'}
    require(set(before) == set(after) == expected_keys and before['status'] == 'busy'
            and after == {**before, 'status': 'accepted'}
            and before['schema'] == 1 and before['operation'] == 'attach'
            and before['mode'] == 'manual' and before['cleanup_completed'] is True
            and before['project_dir'] == body['repository'] and before['config_path'] == config_path
            and evidence.get('native_request_path') == native_path
            and all(before[key] for key in ('request_id', 'environment_sha256', 'host_dir'))
            and re.fullmatch(r'[a-f0-9]{64}', before['environment_sha256']),
            'original native request changed beyond busy-to-accepted status')
    require(decode(read_ref(reference(Path(native_path)))) == after,
            'continued native request is not the original current accepted binding')
    require(before['binary_sha256'] == template['noodle']['sha256'],
            'continued native binary differs from the original call template')
    # These are the same original public sources, not a replacement launcher or policy.
    read_ref({'path': body['noodle'], 'sha256': before['binary_sha256']})
    read_ref({'path': config_path, 'sha256': before['config_sha256']})

    def native_packet(prefix, argv):
        intent = decode(read_ref(evidence[prefix + '_intent']))
        receipt = decode(read_ref(evidence[prefix + '_receipt']))
        stdout = evidence[prefix + '_stdout']
        stderr = evidence[prefix + '_stderr']
        value = decode(read_ref(stdout))
        read_ref(stderr)
        require(intent == {'argv': argv} and receipt.get('exit_code') == 0
                and receipt.get('stdout_sha256') == stdout['sha256']
                and receipt.get('stderr_sha256') == stderr['sha256'],
                'native capacity continuation transport differs')
        return value

    resources = native_packet('resources_before', [body['noodle'], 'host', 'resources'])
    native = native_packet('native_attach', [body['noodle'], '--project-dir', body['repository'],
        'host', 'attach', '--request-file', native_path, '--mode', 'manual', '--config-file',
        config_path, '--cleanup-completed'])
    require(resources.get('schema') == 1 and resources.get('protocol') == 'noodle-host-v1'
            and type(resources.get('contexts')) is int and 0 <= resources['contexts'] < 16
            and resources.get('host_instance_id') and resources.get('host_pid')
            and native.get('host_instance_id') == resources['host_instance_id']
            and native.get('host_pid') == resources['host_pid']
            and native.get('schema') == 1 and native.get('protocol') == 'noodle-host-v1'
            and native.get('status') == 'ok' and native.get('context_state') == 'active'
            and native.get('mode') == 'manual' and native.get('cleanup_completed') is True
            and native.get('admission_refusal') == '' and native.get('context_id')
            and native.get('binary_path') == body['noodle']
            and all(native.get(key) == before[key] for key in
                    ('request_id', 'project_dir', 'config_path', 'config_sha256', 'binary_sha256', 'host_dir')),
            'native continuation did not preserve the original Host and request')
    admission = decode(read_ref(evidence['admission_observation']))
    admission_intent = decode(read_ref(evidence['admission_intent']))
    admission_receipt = decode(read_ref(evidence['admission_receipt']))
    read_ref(admission_receipt['stderr'])
    require(admission_intent == original_intent
            and admission_intent.get('request') == proof['request']
            and admission_intent.get('argv', [])[-4:] == ['loop', 'enter', '--request-file', proof['request']['path']]
            and admission_receipt.get('exit_code') == 0
            and admission_receipt.get('stdout') == evidence['admission_observation'],
            'successful initial admission does not use the original Soodles operation')
    inner = original_inner(admission, proof['request'])
    require('owner' not in inner and admission.get('status') == 'pending'
            and host_identity(admission) == host_identity({'inner': {'native_host': native}}),
            'continued admission is not the original pre-dispatch pending state')
    return admission
