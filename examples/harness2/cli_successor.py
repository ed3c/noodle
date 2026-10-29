"""Explicit transport upgrade for one already captured original owner call."""


def verify_cli_successor(call, proof):
    from soodles_bridge import PinnedCLI, canonical, decode, read_ref, require

    packet = decode(read_ref(proof['cli_successor']))
    require(set(packet) == {'schema', 'kind', 'call_sha256', 'request', 'original_intent',
                            'predecessor_config', 'successor_config', 'failure'},
            'CLI successor packet fields differ')
    require(packet['schema'] == 1 and packet['kind'] == 'captured-original-call-cli-successor'
            and packet['call_sha256'] == call.fingerprint()
            and packet['request'] == proof['request']
            and packet['original_intent'] == proof['original_intent']
            and proof.get('retained_result') is not None
            and proof.get('admission_continuation') is None,
            'CLI successor is not the same captured original call')
    failure = decode(read_ref(packet['failure']))
    require(failure.get('owner') == 'soodles.loop' and failure.get('status') == 'refused'
            and failure.get('effects') == [], 'CLI successor has no preserved outer refusal')
    old = decode(read_ref(packet['predecessor_config']))
    new = decode(read_ref(packet['successor_config']))
    old_cli, new_cli = old.pop('cli'), new.pop('cli')
    require(old == new, 'CLI successor changed the original experiment')
    for name in ('python', 'environment', 'timeout_s'):
        require(old_cli[name] == new_cli[name], 'CLI successor changed ' + name)
    require(old_cli['source_commit'] != new_cli['source_commit'], 'CLI successor source is unchanged')
    for cli in (old_cli, new_cli):
        PinnedCLI(**cli).verify()
    intent = decode(read_ref(proof['original_intent']))
    require(all(canonical(intent.get(name)) == canonical(old_cli[name])
                for name in ('source_commit', 'python', 'sources'))
            and intent['argv'][:3] == [old_cli['python']['path'], '-B', old_cli['entrypoint']['path']],
            'CLI successor predecessor differs from the original transport')
    return new_cli
