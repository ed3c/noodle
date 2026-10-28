"""Explicit entry for a bounded native experiment over an accepted frozen source."""
import sys
import time
from dataclasses import dataclass
from pathlib import Path, PurePosixPath

from openrouter_billing import OpenRouterBilling, ReadOnlyOpenRouter
from owner_contract import SharedGate, tree_ref
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
    candidate_ref = inner.get('candidate') or selection.get('candidate')
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


def run_experiment(config: dict, review_callback):
    """Run actual pinned k=1/seq; Session supplies original Soodles semantic review.

    Calling this function authorizes its configured owner calls. Importing/preparing
    public bundles performs no model or Host operations.
    """
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
    output.mkdir(parents=True, exist_ok=False)
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
                                poll_s=config['poll_s'])
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
    for route, bundle, adapter in adapters:
        group_started = time.monotonic()
        result = run_native_task(adapter, bundle.task_id,
                                 experiment_id=config['experiment_id'] + '/' + route.group)
        groups.append({'group': route.group, 'result': result, 'stage_a_source': bundle.provenance,
                       'optimization_elapsed_s': time.monotonic() - group_started})
        save(output / (route.group + '-result.json'), groups[-1])
        require(not gate.state['blocked'] and not gate.state['paid_blocked'], 'original owner or paid gate is blocked')
        require(not any(row['provider'] == 'pi' and row.get('result') and row['result']['cost_usd'] is None
                        for row in gate.state['calls'].values()), 'original paid bill needs same-call reconciliation')
    return save(output / 'experiment.json', {'schema_version': 1, 'groups': groups,
        'owner_journal': reference(gate.journal), 'optimization_elapsed_s': time.monotonic() - started,
        'whole_workflow_cost_usd': None, 'whole_workflow_wall_s': None,
        'stage_a_accounting': 'combine original Stage-A receipts; unknown remains null',
        'hidden_evaluation': 'not performed', 'adoption_authorized': False})
