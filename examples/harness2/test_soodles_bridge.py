"""Offline protocol fixtures, never real owner/model completion evidence."""
import copy
import hashlib
import json
import shutil
import subprocess
import sys
import tempfile
import unittest
from dataclasses import replace
from pathlib import Path
from unittest.mock import patch

from owner_contract import OwnerCall, Refused, SharedGate, UnknownEffect, tree_ref
from soodles_bridge import (
    MODELS,
    PinnedCLI,
    RequestTemplate,
    SoodlesOwnerBridge,
    decode,
    original_inner,
    public_trace,
    read_ref,
    reference,
    save,
    snapshot_candidate,
)


class ScriptedCLI:
    """Explicit fake Soodles responses; the real transport is tested separately."""
    def __init__(self, test):
        self.test, self.calls, self.accepted = test, [], False
        self.source_commit = 'f' * 40
        self.python = reference(test.noodle)
        self.sources = [reference(test.skill)]

    def verify(self):
        pass

    def invoke(self, verb, request, evidence, *, capture=None):
        self.calls.append((verb, capture))
        evidence.mkdir()
        if capture:
            value = self.test.completion(request)
        elif self.accepted:
            value = copy.deepcopy(self.test.completed)
            value.pop('capture')
            value['inner'].update(review_progress={'status': 'locally_integrated',
                'review': {'decision': 'approve', 'candidate': value['inner']['candidate']}}, acceptance='PASS')
            if getattr(self, 'published_candidate', None) is not None:
                value['inner']['candidate'] = self.published_candidate
        elif len(self.calls) >= 3:
            value = self.test.completion(request)
            value.pop('capture')
        else:
            value = self.test.initial_pending(request)
        value['inner']['native_host'] = self.test.initial_pending(request)['inner']['native_host']
        ref = save(evidence / 'stdout.json', value)
        save(evidence / 'intent.json', {'request': request, 'argv': ['fixture', 'loop', verb],
            'source_commit': self.source_commit, 'python': self.python, 'sources': self.sources})
        save(evidence / 'receipt.json', {'exit_code': 0, 'stdout': ref})
        return value, ref


class UnknownBilling:
    def begin(self, call, directory):
        directory.mkdir()
        return save(directory / 'before.json', {'fixture_only': True})

    def settle(self, call, before, result, directory):
        directory.mkdir()
        return save(directory / 'settlement.json', {'status': 'unknown', 'cost_usd': None, 'fixture_only': True})


class BridgeTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        self.source = self.root / 'source'
        self.source.mkdir()
        (self.source / 'input.txt').write_text('signed ledger')
        self.harness = self.root / 'harness'
        self.skill = self.harness / 'skills' / 'main' / 'SKILL.md'
        self.skill.parent.mkdir(parents=True)
        self.skill.write_text('Read the original source files.')
        (self.harness / 'AGENTS.md').write_text('Use the evaluated main skill.')
        self.skill_base = self.root / 'immutable-main'
        shutil.copytree(self.skill.parent, self.skill_base)
        self.profile = self.root / 'profile.json'
        self.profile.write_text('{}')
        self.noodle = self.root / 'noodle'
        self.noodle.write_text('fixture, not executable')
        self.repo = self.root / 'app-control-fixture'
        self.repo.mkdir()
        self.template = RequestTemplate('codex', 'solver', str(self.repo), None, (),
                reference(self.noodle), reference(self.profile), ('answer.txt', 'run', 'deleted.txt'),
                ('Include all signed transactions.',), ('answer.txt',), harness_skills=('main',), skill_bases={'main': tree_ref(self.skill_base)})
        self.call = OwnerCall('codex', 'original/run-1', 'solver', 'codex', MODELS['codex'],
                {'prompt': 'Reconcile all records.', 'source': tree_ref(self.source), 'harness': tree_ref(self.harness), 'effort': 'low'})
        self.fixture = self.root / 'capture-fixture'
        self.fixture.mkdir()

    def initial_pending(self, request):
        data = decode(read_ref(request))
        raw = (Path(__file__).parent / 'fixtures/initial-admission-pending.json').read_text()
        for key, value in {'CONTROL_ROOT': data['repository'], 'NOODLE_BINARY': data['noodle'],
                           'REQUEST_FILE': request['path']}.items():
            raw = raw.replace(key, value)
        return decode(raw)

    def request(self):
        directory = self.root / 'original-request'
        directory.mkdir()
        return self.template.prepare(self.call, directory)

    def completion(self, request):
        if hasattr(self, 'completed'):
            return copy.deepcopy(self.completed)
        request_data = decode(Path(request['path']).read_bytes())
        envelope = save(self.fixture / 'envelope.json', {'runtime': 'openshell', 'provider': self.call.provider,
                        'model': self.call.model, 'model_reasoning_effort': 'low',
                        'control_root': self.template.repository, 'profile': self.template.profile,
                        'noodle': self.template.noodle, 'goal': request_data['goal'],
                        'source_refs': [reference(path) for path in request_data['source_refs']]})
        worktree = self.root / 'returned'
        worktree.mkdir()
        (worktree / 'answer.txt').write_text('8\n')
        (worktree / 'run').write_bytes(b'\x00binary\xff')
        (worktree / 'run').chmod(0o755)
        native = {'native_attempt_id': 'fixture-native-distinct', 'command_sha256': 'a' * 64,
                  'candidate_head': 'b' * 40, 'native_exec_exit_code': 0, 'native_phase': 'Stopped',
                  'stop_acknowledged': True, 'main_process_instance_id': 'fixture-original-main', 'sources': {}}
        native_observation = save(self.fixture / 'native.json', {'observation': {'execution': native}})
        candidate = {'task_envelope': envelope, 'order_id': 'fixture-order', 'session_id': 'fixture-session',
                     'attempt_id': 'fixture-order-0-attempt-0', 'head': 'b' * 40, 'worktree': str(worktree),
                     'native_execution': native, 'files': {name: {'sha256': reference(worktree / name)['sha256'],
                     'executable': name == 'run'} for name in ('answer.txt', 'run')}}
        candidate['files']['deleted.txt'] = None
        candidate_ref = save(self.fixture / 'candidate.json', candidate)
        row = ({'type': 'event_msg', 'payload': {'type': 'task_complete', 'last_agent_message': '8'}}
               if self.call.provider == 'codex' else
               {'type': 'message', 'message': {'role': 'assistant',
                    'content': [{'type': 'text', 'text': '8'}], 'stopReason': 'stop'}})
        line = json.dumps(row).encode()
        raw = self.fixture / 'raw.jsonl'
        raw.write_bytes(line + b'\n')
        descriptor = {'provider': self.call.provider, 'raw': reference(raw), 'gaps': [], 'terminal': {'line': 1},
                      'native_completion': native_observation, 'cost': {'status': 'unknown'},
                      'inventory': [{'surface': 'public', 'line': 1,
                                     'line_sha256': hashlib.sha256(line).hexdigest(), 'event_id': 'fixture-event'}]}
        descriptor_ref = save(self.fixture / 'descriptor.json', descriptor)
        capture = {'owner': 'issue-execution.local-task.capture', 'status': 'observed', 'request': request,
                   'task_envelope': envelope, 'gaps': [], 'source_custody': [
                       {'original': envelope, 'saved': envelope},
                       {'original': native_observation, 'saved': native_observation}],
                   'snapshot': save(self.fixture / 'snapshot.json', {'fixture_only': True}),
                   'attempts': [{'status': 'observed', 'gaps': [],
                       'binding': {key: candidate[key] for key in ('order_id', 'session_id', 'attempt_id')},
                       'native_completion': {**native, 'observation': native_observation},
                       'capture': {'root_thread_id': 'fixture-thread', 'threads': [
                           {'thread_id': 'fixture-thread', 'descriptor': descriptor_ref, 'raw': reference(raw)}]}}]}
        capture['observation'] = save(self.fixture / 'capture.json', capture)
        self.completed = {'owner': 'soodles.loop', 'request': request, 'status': 'active', 'capture': capture,
                          'inner': {'owner': 'issue-execution.local-task', 'status': 'active',
                                    'candidate': candidate_ref, 'task_envelope': envelope,
                                    'review_progress': None, 'acceptance': 'UNKNOWN'},
                          'next': {'operation': 'original_task_readback'}}
        return copy.deepcopy(self.completed)

    def test_generated_original_request_and_review_then_settlement(self):
        cli = ScriptedCLI(self)
        reviews = []

        def review(call, observation, ref):
            reviews.append((call, observation, ref))
            cli.accepted = True

        bridge = SoodlesOwnerBridge(cli, self.root / 'bridge', [self.template], review=review, billing=UnknownBilling())
        gate = SharedGate(self.root / 'gate.json', bridge, initial_paid_spent_usd='0.084284887')
        with patch('soodles_bridge.time.sleep'):
            result = gate.execute(self.call)
        self.assertTrue(result.settled)
        self.assertEqual([verb for verb, _ in cli.calls], ['enter', 'enter', 'inspect', 'inspect', 'inspect'])
        self.assertEqual(len(reviews), 1)
        self.assertEqual(result.reply, '8')
        self.assertIsNone(result.cost_usd)
        self.assertIsNone(result.wall_s)
        path = Path(result.run_dir)
        self.assertEqual((path / 'workspace/run').read_bytes(), b'\x00binary\xff')
        self.assertTrue((path / 'workspace/run').stat().st_mode & 0o111)
        self.assertFalse((path / 'workspace/deleted.txt').exists())
        request = decode((path.parent / 'request.json').read_bytes())
        self.assertEqual(request['model'], MODELS['codex'])
        self.assertEqual(request['model_reasoning_effort'], 'low')
        self.assertEqual(request['runtime'], 'openshell')
        self.assertIn(str(path.parent / 'call.json'), request['source_refs'])
        self.assertIn(str(self.source / 'input.txt'), request['source_refs'])
        self.assertTrue(gate.execute(self.call).cached)
        with self.assertRaises(Refused):
            gate.execute(replace(self.call, inputs={**self.call.inputs, 'prompt': 'different'}))
        retained = next((path / 'evidence').glob('[0-9a-f]' * 64))
        retained.write_bytes(b'changed original raw evidence')
        with self.assertRaises(UnknownEffect):
            gate.verify_result(result)

    def test_warning_policy_stays_explicit_in_the_original_task_request(self):
        output = self.root / 'warning-request'
        output.mkdir()
        selected = replace(self.template, resource_policy='openshell-single-warning')
        request = decode(read_ref(selected.prepare(self.call, output)))
        self.assertEqual(request['resource_policy'], selected.resource_policy)
        self.assertEqual(request['model_reasoning_effort'], 'low')
        invalid = replace(self.template, resource_policy='ignore-all-pressure')
        with self.assertRaises(Refused):
            invalid.prepare(self.call, self.root / 'invalid-warning')

    def test_solver_materializes_evaluated_skill_not_a_fixed_baseline(self):
        baseline = reference(self.skill)
        self.skill.write_text('Include every signed transaction; evaluated candidate.')
        changed = replace(self.call, inputs={**self.call.inputs, 'harness': tree_ref(self.harness)})
        output = self.root / 'changed-request'
        output.mkdir()
        ref = self.template.prepare(changed, output)
        request = decode(Path(ref['path']).read_bytes())
        self.assertEqual(request['selected_skill'], str(output / 'skills/main/SKILL.md'))
        self.assertNotEqual(reference(request['selected_skill']), baseline)
        self.assertIn(str(self.harness / 'AGENTS.md'), request['source_refs'])
        self.assertIn(self.call.inputs['prompt'], request['goal'])
        self.assertIn('Evaluated Harness² instructions', request['goal'])
        fixed = replace(self.template, selected_skill=baseline)
        with self.assertRaisesRegex(Refused, 'evaluated harness skill selection'):
            fixed.prepare(changed, output)

    def test_skill_overlay_preserves_complete_immutable_assets(self):
        examples = self.skill_base / 'examples'
        examples.mkdir()
        (examples / 'component.html').write_bytes(b'<svg>exact frozen asset</svg>')
        (self.skill_base / 'LICENSE').write_bytes(b'unchanged license')
        (self.skill_base / 'render').write_bytes(b'#!/bin/sh\nexit 0\n')
        (self.skill_base / 'render').chmod(0o755)
        template = replace(self.template, skill_bases={'main': tree_ref(self.skill_base)})
        self.skill.write_text('Native optimized instruction text.')
        call = replace(self.call, inputs={**self.call.inputs, 'harness': tree_ref(self.harness)})
        output = self.root / 'overlay'
        output.mkdir()
        request = decode(Path(template.prepare(call, output)['path']).read_bytes())
        prepared = Path(request['selected_skill']).parent
        self.assertEqual((prepared / 'SKILL.md').read_text(), self.skill.read_text())
        self.assertEqual((prepared / 'examples/component.html').read_bytes(), (examples / 'component.html').read_bytes())
        self.assertEqual((prepared / 'LICENSE').read_bytes(), b'unchanged license')
        self.assertEqual((prepared / 'render').stat().st_mode & 0o777, 0o755)
        (self.skill_base / '.env').write_text('fixture-secret-must-not-be-read')
        blocked = self.root / 'blocked'
        blocked.mkdir()
        with self.assertRaisesRegex(Refused, 'private path'):
            template.prepare(call, blocked)
        self.assertFalse((blocked / 'skills').exists())

    def test_proposer_returns_only_original_candidate_harness(self):
        context = self.root / 'native-context'
        shutil.copytree(self.harness, context / 'harness')
        self.call = replace(self.call, role='proposer', inputs={'mission': 'Improve the signed ledger skill.',
            'system': 'Edit only the frozen instruction surface.', 'context': tree_ref(context),
            'harvest': 'context/harness', 'effort': 'low'})
        self.template = replace(self.template, role='proposer', selected_skill=reference(self.skill),
            harness_skills=(), skill_bases={}, write_paths=('context/harness',), deliverables=(), harvest='context/harness')
        request = self.request()
        observation = self.completion(request)
        candidate = decode(Path(observation['inner']['candidate']['path']).read_bytes())
        destination = Path(candidate['worktree']) / 'context/harness'
        shutil.copytree(self.harness, destination)
        (destination / 'AGENTS.md').write_text('Improved original candidate instructions.')
        candidate['files'] = {str(path.relative_to(Path(candidate['worktree']))):
            {'sha256': reference(path)['sha256'], 'executable': False}
            for path in destination.rglob('*') if path.is_file()}
        observation['inner']['candidate'] = save(self.fixture / 'improved-candidate.json', candidate)
        result = self.root / 'result'
        result.mkdir()
        _, _, harvest = snapshot_candidate(self.call, self.template, observation, result)
        self.assertEqual((Path(harvest) / 'AGENTS.md').read_text(), 'Improved original candidate instructions.')
        self.assertTrue((Path(harvest) / 'skills/main/SKILL.md').is_file())
        self.assertEqual(decode((result / 'deliverables.json').read_bytes()), {})

    def test_real_initial_admission_shape_requires_original_next_and_request(self):
        request = self.request()
        observed = self.initial_pending(request)
        self.assertNotIn('owner', original_inner(observed, request))
        for field, value in [('owner', 'foreign'), ('operation', 'original_task_dispatch'),
                             ('arguments', {'request_file': '/different'})]:
            changed = copy.deepcopy(observed)
            changed['next'][field] = changed['inner']['next'][field] = value
            with self.assertRaises(Refused):
                original_inner(changed, request)

    def test_initial_pending_recovery_only_reads_same_call_and_preserves_unknown_history(self):
        cli = ScriptedCLI(self)
        def review(*_):
            cli.accepted = True
        bridge = SoodlesOwnerBridge(cli, self.root / 'bridge', [self.template], review=review)
        gate = SharedGate(self.root / 'gate.json', bridge, initial_paid_spent_usd='0.091620363')
        with patch('soodles_bridge.original_inner', side_effect=Refused('old initial shape rejection')):
            with self.assertRaises(UnknownEffect):
                gate.execute(self.call)
        unknown_bytes = gate.journal.read_bytes()
        directory = bridge.root / self.call.fingerprint()
        request = reference(directory / 'request.json')
        fresh, fresh_ref = cli.invoke('inspect', request, self.root / 'fresh-readback')
        self.assertEqual(cli.calls[0][0], 'enter')
        reopened = SoodlesOwnerBridge(cli, bridge.root, [self.template], review=review, reopen=True)
        foreign = copy.deepcopy(fresh)
        foreign['inner']['native_host']['context_id'] = 'different-context'
        foreign_ref = save(self.root / 'foreign-readback.json', foreign)
        with self.assertRaisesRegex(Refused, 'Host identity drifted'):
            reopened.reconcile_call(self.call, foreign_ref)
        self.assertFalse((directory / 'reconciliation-intent.json').exists())
        self.assertEqual(gate.journal.read_bytes(), unknown_bytes)
        with patch('soodles_bridge.time.sleep'):
            result, proof = reopened.reconcile_call(self.call, fresh_ref)
        self.assertEqual([verb for verb, _ in cli.calls].count('enter'), 1)
        self.assertTrue(gate.state['blocked'])
        proof_data = decode(read_ref(proof))
        for name in ('acceptance', 'candidate', 'merge_failure', 'host'):
            completed = decode(read_ref(proof_data['completed_observation']))
            if name == 'acceptance':
                completed['inner']['acceptance'] = 'UNKNOWN'
            elif name == 'candidate':
                completed['inner']['review_progress']['review']['candidate'] = {'path': '/other', 'sha256': 'c' * 64}
            elif name == 'merge_failure':
                completed['inner']['review_progress']['status'] = 'pending'
                completed['inner']['review_progress']['reason'] = 'merge failed'
            else:
                completed['inner']['native_host']['host_instance_id'] = 'replacement-host'
            changed = {**proof_data, 'completed_observation': save(self.root / (name + '-completion.json'), completed)}
            changed_ref = save(self.root / (name + '-recovery.json'), changed)
            with self.assertRaises(Refused):
                gate.reconcile_original_call(self.call, result, changed_ref)
            self.assertTrue(gate.state['blocked'])
            self.assertEqual(gate.journal.read_bytes(), unknown_bytes)
        gate.reconcile_original_call(self.call, result, proof)
        row = next(iter(gate.state['calls'].values()))
        audit = row['reconciliations'][0]
        self.assertEqual(read_ref(audit['prior_journal']), unknown_bytes)
        self.assertEqual(audit['prior_row']['state'], 'unknown')
        self.assertEqual(gate.state['initial_paid_spent_usd'], '0.091620363')
        self.assertFalse(gate.state['blocked'])
        calls = len(cli.calls)
        self.assertTrue(gate.execute(self.call).cached)
        self.assertEqual(len(cli.calls), calls)
        with self.assertRaises(Refused):
            reopened.reconcile_call(self.call, fresh_ref)
        with self.assertRaises(Refused):
            gate.execute(replace(self.call, inputs={**self.call.inputs, 'prompt': 'different'}))

    def test_paid_route_preserves_unknown_billing_and_entire_remaining_hold(self):
        self.call = replace(self.call, group='pi', provider='pi', model=MODELS['pi'],
                            paid_reservation_usd='4.915715113')
        self.template = replace(self.template, group='pi')
        cli = ScriptedCLI(self)

        def review(*_):
            cli.accepted = True

        bridge = SoodlesOwnerBridge(cli, self.root / 'bridge', [self.template], review=review, billing=UnknownBilling())
        gate = SharedGate(self.root / 'gate.json', bridge, initial_paid_spent_usd='0.084284887')
        with patch('soodles_bridge.time.sleep'):
            result = gate.execute(self.call)
        self.assertIsNone(result.cost_usd)
        self.assertEqual(result.reply, '8')
        with self.assertRaisesRegex(Refused, 'remaining authorized budget'):
            gate.execute(replace(self.call, run_id='next-call', paid_reservation_usd='0.01'))
        evidence = decode((Path(result.run_dir) / 'native-evidence.json').read_bytes())
        self.assertEqual(evidence['billing'], {'status': 'unknown'})

    def test_review_pending_without_current_candidate_waits_for_original_approval(self):
        cli = ScriptedCLI(self)
        original_invoke = cli.invoke
        pending = []

        def invoke(*args, **kwargs):
            value, ref = original_invoke(*args, **kwargs)
            if cli.accepted and not pending:
                pending.append(True)
                value['inner']['review_progress']['status'] = 'pending'
                value['inner']['candidate'] = None
                value['inner']['acceptance'] = 'UNKNOWN'
            return value, ref

        def review(*_):
            cli.accepted = True

        bridge = SoodlesOwnerBridge(cli, self.root / 'bridge', [self.template], review=review, billing=UnknownBilling())
        with patch.object(cli, 'invoke', side_effect=invoke), patch('soodles_bridge.time.sleep'):
            result = bridge.execute(self.call)
        self.assertTrue(result.settled)
        self.assertEqual([verb for verb, _ in cli.calls].count('enter'), 2)
        self.assertTrue(pending)

    def test_integrated_without_acceptance_never_settles(self):
        cli = ScriptedCLI(self)
        original_invoke = cli.invoke

        def invoke(*args, **kwargs):
            value, ref = original_invoke(*args, **kwargs)
            if cli.accepted:
                value['inner']['acceptance'] = 'UNKNOWN'
            return value, ref

        def review(*_):
            cli.accepted = True

        bridge = SoodlesOwnerBridge(cli, self.root / 'bridge', [self.template], review=review, billing=UnknownBilling())
        with patch.object(cli, 'invoke', side_effect=invoke), patch('soodles_bridge.time.sleep'):
            with self.assertRaises(UnknownEffect):
                bridge.execute(self.call)
        self.assertFalse((self.root / 'bridge' / self.call.fingerprint() / 'settled.json').exists())

    def test_lost_enter_closes_gate_without_retry_or_other_route(self):
        cli = ScriptedCLI(self)
        bridge = SoodlesOwnerBridge(cli, self.root / 'bridge', [self.template], review=lambda *_: None)
        gate = SharedGate(self.root / 'gate.json', bridge, initial_paid_spent_usd='0.084284887')
        with patch.object(cli, 'invoke', side_effect=TimeoutError('lost original response')) as invoke:
            with self.assertRaises(UnknownEffect):
                gate.execute(self.call)
            with self.assertRaises(UnknownEffect):
                gate.execute(replace(self.call, group='pi', provider='pi', model=MODELS['pi']))
            self.assertEqual(invoke.call_count, 1)

    def test_completed_proposer_reconciliation_uses_snapshot_after_original_worktree_removed(
        self,
    ):
        prior = BridgeTests()
        prior.setUp()
        self.addCleanup(prior.doCleanups)
        prior_cli = ScriptedCLI(prior)

        def accept_prior(*_):
            prior_cli.accepted = True

        prior_bridge = SoodlesOwnerBridge(
            prior_cli, prior.root / "bridge", [prior.template], review=accept_prior
        )
        gate = SharedGate(
            prior.root / "gate.json", prior_bridge, initial_paid_spent_usd="0.091620363"
        )
        with patch("soodles_bridge.time.sleep"):
            baseline = gate.execute(prior.call)
        context = self.root / "native-context"
        shutil.copytree(self.harness, context / "harness")
        self.call = replace(
            self.call,
            run_id="original/proposer",
            role="proposer",
            inputs={
                "mission": "Improve original instructions.",
                "system": "Use the original evidence.",
                "context": tree_ref(context),
                "harvest": "context/harness",
                "effort": "low",
            },
        )
        self.template = replace(
            self.template,
            role="proposer",
            selected_skill=reference(self.skill),
            harness_skills=(),
            skill_bases={},
            write_paths=("context/harness",),
            deliverables=(),
            harvest="context/harness",
        )
        cli = ScriptedCLI(self)
        original_completion = self.completion

        def completion(request):
            if not hasattr(self, "proposer_completed"):
                value = original_completion(request)
                candidate = decode(read_ref(value["inner"]["candidate"]))
                destination = Path(candidate["worktree"]) / "context/harness"
                shutil.copytree(self.harness, destination)
                (destination / "AGENTS.md").write_text("Improved frozen candidate.")
                candidate["files"] = {
                    str(path.relative_to(Path(candidate["worktree"]))): {
                        "sha256": reference(path)["sha256"],
                        "executable": False,
                    }
                    for path in destination.rglob("*")
                    if path.is_file()
                }
                self.completed["inner"]["candidate"] = save(
                    self.fixture / "proposer-candidate.json", candidate
                )
                self.proposer_completed = True
            return copy.deepcopy(self.completed)

        def review_transition(*_):
            cli.accepted = True
            raise UnknownEffect("fixture read-only canonical snapshot refusal")

        bridge = SoodlesOwnerBridge(
            cli, self.root / "bridge", [self.template], review=review_transition
        )
        gate.owner = bridge
        with (
            patch.object(self, "completion", side_effect=completion),
            patch("soodles_bridge.time.sleep"),
        ):
            with self.assertRaises(UnknownEffect):
                gate.execute(self.call)
        unknown_bytes = gate.journal.read_bytes()
        directory = bridge.root / self.call.fingerprint()
        fresh, fresh_ref = cli.invoke(
            "inspect", reference(directory / "request.json"), self.root / "fresh"
        )
        shutil.rmtree(self.root / "returned")
        reopened = SoodlesOwnerBridge(
            cli, bridge.root, [self.template], review=self.fail, reopen=True
        )
        artifact = directory / "result/workspace/context/harness/AGENTS.md"
        original = artifact.read_bytes()
        artifact.write_text("changed retained artifact")
        with self.assertRaises(Refused):
            reopened.reconcile_call(self.call, fresh_ref)
        self.assertEqual(gate.journal.read_bytes(), unknown_bytes)
        artifact.write_bytes(original)
        before = len([verb for verb, _ in cli.calls if verb == "enter"])
        result, proof = reopened.reconcile_call(self.call, fresh_ref)
        gate.reconcile_original_call(self.call, result, proof)
        self.assertEqual(
            len([verb for verb, _ in cli.calls if verb == "enter"]), before
        )
        self.assertEqual((Path(result.workspace) / "AGENTS.md").read_bytes(), original)
        self.assertEqual(len(gate.state["calls"]), 2)
        self.assertTrue(gate.execute(prior.call).cached)
        self.assertEqual(gate.execute(prior.call).output_sha256, baseline.output_sha256)
        self.assertTrue(gate.execute(self.call).cached)
        self.assertEqual(gate.state["initial_paid_spent_usd"], "0.091620363")

    def test_only_effect_free_canonical_snapshot_inspect_has_three_bounded_reads(self):
        script = self.root / "soodles.py"
        script.write_text("# offline fixture")
        cli = PinnedCLI(
            reference(Path(sys.executable).resolve()),
            reference(script),
            (reference(script),),
            "e" * 40,
            {"PATH": "/usr/bin:/bin"},
            30,
        )
        request = save(self.root / "request.json", {"fixture_only": True})
        native = save(self.root / "native-drift.json", {"fixture_only": True})
        refusal = {
            "owner": "soodles.loop",
            "status": "refused",
            "effects": [],
            "authorizes_landing": False,
            "problem": {
                "field": "task.session_readback.native",
                "value": {
                    "exit_code": 1,
                    "error": "session readback source changed: canonical_snapshot",
                    "receipt": native,
                },
            },
        }
        calls = []

        def run(argv, **kwargs):
            calls.append(argv)
            value = (
                refusal
                if len(calls) < 3
                else {"owner": "soodles.loop", "status": "pending", "request": request}
            )
            kwargs["stdout"].write(json.dumps(value).encode())
            return subprocess.CompletedProcess(
                argv, 2 if value["status"] == "refused" else 0
            )

        with patch("soodles_bridge.subprocess.run", side_effect=run):
            value, ref = cli.invoke("inspect", request, self.root / "read")
        self.assertEqual(len(calls), 3)
        self.assertEqual(value["status"], "pending")
        self.assertEqual(Path(ref["path"]).parent.name, "read-read-3")
        self.assertTrue((self.root / "read/receipt.json").is_file())
        for kind in ("exhausted", "other-source", "enter", "capture", "effects", "identity"):
            current = copy.deepcopy(refusal)
            if kind == "other-source":
                current["problem"]["value"]["error"] = (
                    "session readback source changed: spawn_metadata"
                )
            if kind == "effects":
                current["effects"] = ["unknown"]
            if kind == "identity":
                current["request"] = {**request, "sha256": "0" * 64}

            def refuse(argv, **kwargs):
                kwargs["stdout"].write(json.dumps(current).encode())
                return subprocess.CompletedProcess(argv, 2)

            with patch("soodles_bridge.subprocess.run", side_effect=refuse) as invoked:
                with self.assertRaises(UnknownEffect):
                    cli.invoke(
                        "enter" if kind == "enter" else "inspect",
                        request,
                        self.root / kind,
                        capture=self.root / "capture" if kind == "capture" else None,
                    )
                self.assertEqual(invoked.call_count, 3 if kind == "exhausted" else 1)

    def test_source_drift_and_fixed_route_refuse_before_owner(self):
        directory = self.root / 'prepared'
        directory.mkdir()
        for call in (replace(self.call, model='gpt-other'),
                     replace(self.call, inputs={**self.call.inputs, 'effort': 'high'})):
            with self.assertRaises(Refused):
                self.template.prepare(call, directory)
        (self.source / 'input.txt').write_text('drift')
        with self.assertRaises(Refused):
            self.template.prepare(self.call, directory)

    def test_capture_rejects_canonical_and_native_replacement(self):
        request = self.request()
        original = self.completion(request)
        for field, value in [('attempt_id', 'foreign-canonical'), ('session_id', 'foreign-session')]:
            changed = copy.deepcopy(original)
            changed['capture']['attempts'][0]['binding'][field] = value
            with self.assertRaises(Refused):
                snapshot_candidate(self.call, self.template, changed, self.root)
        for field, value in [('native_phase', 'Ready'), ('stop_acknowledged', False),
                             ('main_process_instance_id', 'replacement')]:
            changed = copy.deepcopy(original)
            changed['capture']['attempts'][0]['native_completion'][field] = value
            with self.assertRaises(Refused):
                snapshot_candidate(self.call, self.template, changed, self.root)

    def test_capture_refuses_changed_artifact_and_parent_symlink(self):
        request = self.request()
        observation = self.completion(request)
        (self.root / 'returned/answer.txt').write_text('foreign')
        result = self.root / 'result'
        result.mkdir()
        with self.assertRaises(Refused):
            snapshot_candidate(self.call, self.template, observation, result)
        link = self.root / 'symlink'
        link.symlink_to(self.source, target_is_directory=True)
        with self.assertRaises(Refused):
            reference(link / 'input.txt')

    def test_private_events_never_become_trajectory_or_reply(self):
        raw = self.root / 'private.jsonl'
        row = {'message': {'role': 'assistant', 'content': [{'type': 'text', 'text': 'public'}]}}
        hidden = {'message': {'role': 'assistant', 'content': [{'type': 'text', 'text': 'private'}]}}
        lines = [json.dumps(row).encode(), json.dumps(hidden).encode()]
        raw.write_bytes(b'\n'.join(lines) + b'\n')
        descriptor = {'provider': 'pi', 'raw': reference(raw), 'terminal': {'line': 1}, 'gaps': [],
                      'inventory': [{'surface': surface, 'line': index + 1, 'event_id': str(index),
                          'line_sha256': hashlib.sha256(lines[index]).hexdigest()}
                          for index, surface in enumerate(['public', 'private'])]}
        trace, reply = public_trace(descriptor)
        self.assertEqual(reply, 'public')
        self.assertNotIn('private', trace)

    def test_real_transport_argv_preserves_original_request_and_logs_failure(self):
        script = self.root / 'soodles.py'
        script.write_text('# explicit offline transport fixture\n')
        cli = PinnedCLI(reference(Path(sys.executable).resolve()), reference(script), (reference(script),),
                        'e2e7a0474e0fca055b512b139273d9b4b33ec97d', {'PATH': '/usr/bin:/bin'}, 30)
        request = save(self.root / 'request.json', {'fixture_only': True})

        def run(argv, **kwargs):
            self.assertEqual(argv[3:], ['loop', 'inspect', '--request-file', request['path'],
                                      '--include-result', '--capture-output', str(self.root / 'capture')])
            kwargs['stdout'].write(json.dumps({'owner': 'soodles.loop', 'status': 'pending',
                                              'request': request}).encode())
            return subprocess.CompletedProcess(argv, 0)

        with patch('soodles_bridge.subprocess.run', side_effect=run):
            value, _ = cli.invoke('inspect', request, self.root / 'op', capture=self.root / 'capture')
        self.assertEqual(value['status'], 'pending')
        with patch('soodles_bridge.subprocess.run', side_effect=TimeoutError('fixture lost response')) as run:
            with self.assertRaises(UnknownEffect):
                cli.invoke('enter', request, self.root / 'lost')
            self.assertEqual(run.call_count, 1)
        self.assertTrue((self.root / 'lost/intent.json').exists())
        script.write_text('drift')
        with self.assertRaises(Refused):
            cli.invoke('enter', request, self.root / 'never')
        self.assertFalse((self.root / 'never').exists())

    def test_duplicate_keys_refuse(self):
        with self.assertRaises(Refused):
            decode('{"request":1,"request":2}')


if __name__ == '__main__':
    unittest.main()
