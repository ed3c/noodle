"""Explicit continuation of an unstarted group; never reopen a downstream fallback."""
import shutil
from pathlib import Path

from owner_contract import OwnerCall, OwnerResult, tree_ref
from soodles_bridge import decode, read_ref, reference, require, save


def prepare_group_continuation(config, adapters, bridge, gate, group, schedule):
    require(not gate.state['blocked'] and not gate.state['paid_blocked']
            and all(row['state'] in {'settled', 'refused'} for row in gate.state['calls'].values()),
            'original owner or billing is still unresolved')
    selected = [item for item in adapters if item[0].group == group]
    require(len(selected) == 1, 'continuation group is outside the original experiment')
    route, bundle, adapter = selected[0]
    require(route.provider == 'pi', 'this continuation is limited to the original unstarted Pi group')
    namespace = schedule.Namespace(adapter.spec.name, adapter.substrate.name, 'agg', 1, None, '')
    expected = OwnerCall(route.group, namespace.run_id(bundle.task_id, 'base1'), 'solver', route.provider,
        route.model, {'task_id': bundle.task_id, 'prompt': bundle.prompt, 'source': bundle.source,
        'harness': tree_ref(adapter.base_harness()), 'effort': route.effort, 'timeout_s': route.timeout_s},
        route.paid_reservation_usd)
    rows = list(gate.state['calls'].values())
    group_rows = [row for row in rows if row['call']['group'] == group]
    require(len(group_rows) <= 1, 'group already has downstream native calls')
    native_root = Path(config['output_root']) / group
    empty_call = bridge.root / expected.fingerprint()
    if group_rows:
        row = group_rows[0]
        require(row['call'] == expected.__dict__ and row['input_sha256'] == expected.fingerprint()
                and row['state'] == 'refused' and row.get('error') == 'task template belongs to another call route'
                and empty_call.is_dir() and not any(empty_call.iterdir()),
                'original baseline refusal has owner evidence or different inputs')
        tree_ref(empty_call)
        summary = decode(read_ref(reference(namespace.task_dir(native_root, bundle.task_id) / 'summary.json')))
        require(summary.get('status') == 'base-failed' and summary.get('task_id') == bundle.task_id
                and summary.get('k') == 1 and not summary.get('modes') and not summary.get('selections')
                and len(summary.get('base', [])) == 1
                and summary['base'][0]['run_id'] == expected.run_id,
                'native group is not the original failed first baseline')
    else:
        require(not native_root.exists() and not empty_call.exists(), 'unstarted group has unexplained native evidence')
    protected, prior_groups = [], []
    for other_route, other_bundle, other in adapters:
        if other_route.group == group:
            continue
        root = Path(config['output_root']) / other_route.group
        other_ns = schedule.Namespace(other.spec.name, other.substrate.name, 'agg', 1, None, '')
        summary_ref = reference(other_ns.task_dir(root, other_bundle.task_id) / 'summary.json')
        summary = decode(read_ref(summary_ref))
        require(summary.get('status') == 'ok' and summary.get('selections', {}).get('seq', {}).get('ok'),
                'prior native group has not finished its original selection')
        protected.append(tree_ref(root))
        previous = [row for row in rows if row['call']['group'] == other_route.group]
        settled = [row for row in previous if row['state'] == 'settled']
        solvers = sum(row['call']['role'] == 'solver' for row in settled)
        proposers = sum(row['call']['role'] == 'proposer' for row in settled)
        prior_groups.append({'group': other_route.group, 'summary': summary_ref,
            'settled_solvers': solvers, 'settled_proposers': proposers,
            'complete_requested_owner_calls': solvers == 4 and proposers == 3,
            'refused_calls': [row['call']['run_id'] for row in previous if row['state'] == 'refused']})
    for row in rows:
        if row['state'] == 'settled':
            gate.verify_result(OwnerResult(**row['result']), OwnerCall(**row['call']))
    bridge.restore_templates(rows)
    bridge.template_for(expected)
    archive = Path(config['output_root']) / ('unstarted-group-before-' + group)
    archive.mkdir()
    shutil.copy2(gate.journal, archive / 'prior-journal.json')
    proof = {'kind': 'unstarted-group-first-dispatch', 'call_sha256': expected.fingerprint(),
        'prior_journal': reference(archive / 'prior-journal.json'), 'protected_native': protected,
        'prior_groups': prior_groups, 'original_namespace': str(namespace), 'group': group}
    if group_rows:
        shutil.copytree(native_root, archive / 'native')
        proof['saved_native'] = tree_ref(archive / 'native')
        proof['original_native'] = tree_ref(native_root)
        # Preserve the exact empty directory; normal bridge execution can now create it once.
        empty_call.rename(archive / 'empty-call')
        proof['saved_empty_call'] = tree_ref(archive / 'empty-call')
    proof_ref = save(archive / 'continuation.json', proof)
    if group_rows:
        gate.authorize_first_dispatch(expected, proof_ref)
    return selected, proof_ref, protected
