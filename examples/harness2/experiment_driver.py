"""Explicit entry for a bounded native experiment over an accepted frozen source."""
import shutil
import sys
import tempfile
import time
from dataclasses import dataclass
from pathlib import Path, PurePosixPath

from openrouter_billing import OpenRouterBilling, ReadOnlyOpenRouter
from owner_contract import OwnerCall, OwnerResult, SharedGate, money, tree_ref
from soodles_bridge import (
    MODELS,
    PinnedCLI,
    RequestTemplate,
    SoodlesOwnerBridge,
    decode,
    read_ref,
    reference,
    require,
    save,
    selected_candidate,
)


def public_destination(name):
    path = PurePosixPath(name)
    require(name and not path.is_absolute() and path.as_posix() == name and '..' not in path.parts
            and '\\' not in name, 'public bundle destination is invalid')
    parts = [part.casefold() for part in path.parts]
    require(not any(part in {'.git', '.ssh', '.gnupg', '.aws', '.env'} or part.startswith('.env.') for part in parts)
            and parts[-1] not in {'auth.json', 'credentials', 'credentials.json', 'credentials.toml'}
            and not parts[-1].endswith(('.pem', '.key')), 'public bundle contains a private destination')
    return path


def copy_public(ref, destination):
    # Validate the original path before reading bytes, not only the new alias.
    public_destination(Path(ref['path']).as_posix().lstrip('/'))
    raw = read_ref(ref)
    destination.parent.mkdir(parents=True, exist_ok=True)
    with destination.open('xb') as stream:
        stream.write(raw)
    destination.chmod(Path(ref['path']).stat().st_mode & 0o777)


def public_tree(ref):
    root = Path(ref['path'])
    public_destination(root.as_posix().lstrip('/'))
    for path in root.rglob('*'):
        public_destination(path.as_posix().lstrip('/'))
        require(path.resolve() == path, 'public source traverses a link')
    require(tree_ref(root) == ref, 'public source tree differs')
    return ref


@dataclass(frozen=True)
class FrozenAcceptedSource:
    task_id: str
    prompt: str
    source: dict
    provenance: dict


def prepare_public_task_bundle(*, destination: Path, task_id: str, prompt: str,
                               artifacts: dict, measurements: dict, owner_acceptance: dict,
                               provenance: tuple[dict, ...]):
    """Freeze explicit accepted Stage-A artifacts and measured geometry, not hidden answers.

    artifacts maps a public destination to {source: FileRef, candidate_path: str}.
    measurements maps a public destination to an original measurement FileRef.
    Their semantic sufficiency remains the original acceptance/measurement owner's job.
    """
    accepted = decode(read_ref(owner_acceptance))
    inner = accepted.get('inner') or {}
    progress = inner.get('review_progress') or {}
    selection = progress.get('review') or {}
    candidate_ref = selected_candidate(inner)
    require(accepted.get('owner') == 'soodles.loop' and inner.get('owner') == 'issue-execution.local-task'
            and progress.get('status') == 'locally_integrated' and inner.get('acceptance') == 'PASS'
            and selection.get('decision') == 'approve' and selection.get('candidate') == candidate_ref,
            'Stage A has no original candidate-bound integrated acceptance')
    candidate = decode(read_ref(candidate_ref))
    require(task_id and prompt and artifacts and measurements, 'frozen task source or measured geometry is missing')
    require(not set(artifacts).intersection(measurements), 'public artifact and measurement destinations collide')
    for ref in provenance:
        read_ref(ref)
    destination = Path(destination)
    destination.mkdir(parents=True, exist_ok=False)
    source = destination / 'source'
    source.mkdir()
    names = set()
    for name, item in artifacts.items():
        public_destination(name)
        require(name.casefold() not in names, 'public bundle names collide')
        names.add(name.casefold())
        expected = candidate['files'].get(item['candidate_path'])
        require(expected is not None and expected['sha256'] == item['source']['sha256'],
                'public artifact differs from the accepted candidate')
        copy_public(item['source'], source / name)
        require(bool((source / name).stat().st_mode & 0o111) == expected['executable'],
                'accepted artifact mode differs')
    for name, ref in measurements.items():
        public_destination(name)
        require(name.casefold() not in names, 'public bundle names collide')
        names.add(name.casefold())
        copy_public(ref, source / name)
    manifest = save(destination / 'provenance.json', {'schema_version': 1, 'owner_acceptance': owner_acceptance,
        'candidate': candidate_ref, 'candidate_head': candidate['head'], 'artifacts': artifacts,
        'measurements': measurements, 'provenance': provenance, 'source': tree_ref(source),
        'stage_a_regenerated_per_solver': False, 'scope': 'explicit public artifacts and measurements'})
    bundle = FrozenAcceptedSource(task_id, prompt, tree_ref(source), manifest)
    return save(destination / 'task.json', bundle.__dict__)


def prepare_editable_harness(*, destination: Path, agents: dict, skill_bases: dict):
    """Build only the native editable text surface; fixed assets stay in skill_bases."""
    destination = Path(destination)
    destination.mkdir(parents=True, exist_ok=False)
    copy_public(agents, destination / 'AGENTS.md')
    for name, base in skill_bases.items():
        public_destination(name)
        require('/' not in name, 'fixed skill base alias differs')
        public_tree(base)
        copy_public(reference(Path(base['path']) / 'SKILL.md'), destination / 'skills' / name / 'SKILL.md')
    return tree_ref(destination)


def resume_experiment(config: dict, review_callback, call_ref: dict, fresh_observation: dict, *, admission_continuation=None):
    """Reconcile this original call, then continue the same native namespaces."""
    return _run_experiment(config, review_callback, call_ref=call_ref, fresh_observation=fresh_observation,
                           admission_continuation=admission_continuation)


def continue_unstarted_group(config: dict, review_callback, group: str):
    """After the prior driver exits, run only an original group with no owner effects."""
    return _run_experiment(config, review_callback, continue_group=group)


def run_experiment(config: dict, review_callback):
    return _run_experiment(config, review_callback)


def _run_experiment(config: dict, review_callback, *, call_ref=None, fresh_observation=None, continue_group=None, admission_continuation=None):
    """Run actual pinned k=1/seq; Session supplies original Soodles semantic review.

    Calling this function authorizes its configured owner calls. Importing/preparing
    public bundles performs no model or Host operations.
    """
    require(admission_continuation is None or (call_ref is not None and fresh_observation is not None),
            "capacity continuation requires the same original call and fresh observation")
    import reproduce
    pin = config['upstream']
    receipt = decode(read_ref(pin['prepared_receipt']))
    upstream = Path(pin['path'])
    require(receipt['source_pin'] == reproduce.PIN
            and reproduce.fingerprint(upstream) == receipt['source_tree_sha256'], 'original upstream source differs')
    sys.path.insert(0, str(upstream))
    from harness2.core import schedule
    from native_adapter import NoodleAdapter, Route, Task, run_native_task
    require(Path(schedule.__file__).resolve().is_relative_to(upstream.resolve()),
            'imported native core differs from the pinned source')

    output = Path(config['output_root'])
    reopening = fresh_observation is not None or continue_group is not None
    if not reopening:
        output.mkdir(parents=True, exist_ok=False)
    else:
        require(output.is_dir() and (output / 'owner-journal.json').is_file(), 'original experiment is missing')
    billing_config = config.get('billing')
    require(billing_config is not None, 'two-group execution has no original paid-key supplier')
    billing = None
    if billing_config is not None:
        client = ReadOnlyOpenRouter(Path(billing_config['environment_file']),
            key_name=billing_config['key_name'], timeout_s=billing_config['timeout_s'])
        billing = OpenRouterBilling(client, initial_usage_usd=config['initial_paid_spent_usd'],
            exclusive_key=billing_config['exclusive_key'], observations=billing_config['observations'],
            interval_s=billing_config['interval_s'])
    cli = PinnedCLI(**config['cli'])
    templates = [RequestTemplate(**value) for value in config['templates']]
    bridge = SoodlesOwnerBridge(cli, output / 'owner', templates, review=review_callback, billing=billing,
                                poll_s=config['poll_s'], reopen=reopening)
    gate = SharedGate(output / 'owner-journal.json', bridge,
                      initial_paid_spent_usd=config['initial_paid_spent_usd'])
    groups, adapters, started = [], [], time.monotonic()
    require(len(config['groups']) == 2 and {g['route']['provider'] for g in config['groups']} == {'codex', 'pi'}
            and len({g['route']['group'] for g in config['groups']}) == 2,
            'experiment requires exactly the two authorized model groups')
    for group in config['groups']:
        route = Route(**group['route'])
        require(route.model == MODELS[route.provider] and route.effort == 'low', 'experiment model selection differs')
        bundle = FrozenAcceptedSource(**decode(read_ref(group['task_bundle'])))
        public_tree(bundle.source)
        provenance = decode(read_ref(bundle.provenance))
        require(provenance['source'] == bundle.source and provenance['stage_a_regenerated_per_solver'] is False,
                'accepted source provenance differs from the evaluated source')
        read_ref(provenance['owner_acceptance'])
        read_ref(provenance['candidate'])
        public_tree(group['baseline'])
        adapter = NoodleAdapter(route=route, gate=gate,
            tasks={bundle.task_id: Task(bundle.prompt, Path(bundle.source['path']))},
            base=Path(group['baseline']['path']), runs_root=output / route.group,
            always_on_max=group['always_on_max'])
        adapters.append((route, bundle, adapter))
    cli.verify()
    reconciliation_id = None
    continuation_ref, protected = None, []
    if continue_group is not None:
        from group_continuation import prepare_group_continuation
        adapters, continuation_ref, protected = prepare_group_continuation(
            config, adapters, bridge, gate, continue_group, schedule)
        reconciliation_id = 'first-dispatch-' + continue_group
    if fresh_observation is not None:
        call = OwnerCall(**decode(read_ref(call_ref)))
        unresolved = [row for row in gate.state['calls'].values() if row['state'] in {'pending', 'unknown'}]
        require(len(unresolved) == 1 and unresolved[0]['state'] == 'unknown'
                and unresolved[0]['call'] == call.__dict__, 'resume differs from the sole unresolved original call')
        rows = ordered_calls(gate.state)
        require(rows[-1] is unresolved[0], 'original call ordering differs')
        for row in rows:
            previous = OwnerCall(**row['call'])
            if row['state'] == 'settled':
                gate.verify_result(OwnerResult(**row['result']), previous)
            elif row is not unresolved[0]:
                require(row['state'] == 'refused', 'prior original owner is unresolved')
        bridge.restore_templates(rows, call)
        archive = archive_reconciliation(output, call_ref, [route.group for route, _, _ in adapters],
                                         gate.journal, fresh_observation)
        reconciliation_id = archive.name.removeprefix('reconciliation-native-before-')
        if billing is not None:
            known = [money(row['result']['cost_usd']) for row in rows if row['provider'] == 'pi'
                     and row['state'] == 'settled' and row['result']['cost_usd'] is not None]
            require(not any(row['provider'] == 'pi' and row['state'] == 'settled' and row['result']['cost_usd'] is None
                            for row in rows), 'prior paid call still needs same-call billing settlement')
            billing.expected_usage = money(config['initial_paid_spent_usd']) + sum(known)
        result, proof = bridge.reconcile_call(call, fresh_observation, admission_continuation=admission_continuation)
        gate.reconcile_original_call(call, result, proof)
    for route, bundle, adapter in adapters:
        group_started = time.monotonic()
        result = run_native_task(adapter, bundle.task_id,
                                 experiment_id=config['experiment_id'] + '/' + route.group)
        groups.append({'group': route.group, 'result': result, 'stage_a_source': bundle.provenance,
                       'driver_segment_elapsed_s': time.monotonic() - group_started,
                       'optimization_elapsed_s': journal_elapsed(gate.state, route.group)})
        suffix = '-reconciled-' + reconciliation_id + '-result.json' if reconciliation_id else '-result.json'
        save(output / (route.group + suffix), groups[-1])
        require(not gate.state['blocked'] and not gate.state['paid_blocked'], 'original owner or paid gate is blocked')
        require(not any(row['provider'] == 'pi' and row.get('result') and row['result']['cost_usd'] is None
                        for row in gate.state['calls'].values()), 'original paid bill needs same-call reconciliation')
    for original in protected:
        require(tree_ref(original['path']) == original, 'prior native group changed during continuation')
    report_name = 'experiment-continuation-' + continue_group + '.json' if continue_group else 'experiment.json'
    return save(output / report_name, {'schema_version': 1, 'groups': groups,
        'continuation': continuation_ref,
        'owner_journal': reference(gate.journal), 'optimization_elapsed_s': journal_elapsed(gate.state),
        'driver_segment_elapsed_s': time.monotonic() - started,
        'whole_workflow_cost_usd': None, 'whole_workflow_wall_s': None,
        'stage_a_accounting': 'combine original Stage-A receipts; unknown remains null',
        'hidden_evaluation': 'not performed', 'adoption_authorized': False})


def archive_reconciliation(output, call_ref, groups, journal, fresh_observation):
    """Preserve each explicit readback's inputs, including earlier failed readbacks."""
    call = OwnerCall(**decode(read_ref(call_ref)))
    archive = Path(tempfile.mkdtemp(prefix='reconciliation-native-before-' + call.fingerprint() + '-', dir=output))
    archived = []
    for group in groups:
        native_root = output / group
        if native_root.is_dir():
            shutil.copytree(native_root, archive / group)
            archived.append({'original': tree_ref(native_root), 'saved': tree_ref(archive / group)})
    for old_result in output.glob('*result.json'):
        shutil.copy2(old_result, archive / old_result.name)
    save(archive / 'source.json', {'native_trees': archived, 'original_journal': reference(journal),
        'original_call': call_ref, 'fresh_observation': fresh_observation})
    return archive


def ordered_calls(state):
    # Canonical JSON sorts object keys; persisted map order is not dispatch order.
    rows = sorted(state['calls'].values(), key=lambda row: row['started_unix_s'])
    require(len({row['started_unix_s'] for row in rows}) == len(rows), 'original call chronology is ambiguous')
    return rows


def journal_elapsed(state, group=None):
    """Original call start through final settlement includes review/repair downtime."""
    rows = [row for row in ordered_calls(state) if group is None or row['call']['group'] == group]
    if not rows or any(row['state'] not in {'settled', 'refused'} for row in rows):
        return None
    # A retained old row may lack a terminal clock; never replace that with zero.
    if rows[-1].get('settled_unix_s') is None:
        return None
    return rows[-1]['settled_unix_s'] - min(row['started_unix_s'] for row in rows)
