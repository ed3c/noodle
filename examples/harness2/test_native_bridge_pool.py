"""Pinned native scheduling with the real bridge and a scripted owner transport."""
import copy
import shutil
import socket
import subprocess
import tempfile
import unittest
from dataclasses import asdict, replace
from pathlib import Path
from unittest.mock import patch

import test_soodles_bridge as fixtures
from native_adapter import NoodleAdapter, Route, Task, run_native_task
from owner_contract import OwnerCall, Refused, SharedGate
from soodles_bridge import (
    MODELS,
    RequestTemplate,
    SoodlesOwnerBridge,
    decode,
    read_ref,
    reference,
    save,
)


class PoolCLI:
    """Only transport responses are fabricated; template allocation/owner gate are real."""
    def __init__(self, test):
        self.test, self.sessions, self.owner_calls = test, {}, []

    def verify(self):
        pass

    def invoke(self, verb, request, evidence, *, capture=None):
        directory = Path(request['path']).parent
        call = OwnerCall(**decode((directory / 'call.json').read_bytes()))
        key = call.fingerprint()
        if key not in self.sessions:
            fixture = fixtures.BridgeTests()
            fixture.setUp()
            self.test.addCleanup(fixture.doCleanups)
            fixture.call = call
            fixture.template = RequestTemplate(**decode((directory / 'binding.json').read_bytes())['template'])
            original = fixture.completion

            def complete(ref):
                if not hasattr(fixture, 'pool_completed'):
                    value = original(ref)
                    if call.role == 'proposer':
                        candidate = decode(read_ref(value['inner']['candidate']))
                        returned = Path(candidate['worktree']) / 'context/harness'
                        shutil.copytree(Path(call.inputs['context']['path']) / 'harness', returned)
                        (returned / 'AGENTS.md').write_text('Include every signed source transaction.\n')
                        candidate['files'] = {str(path.relative_to(Path(candidate['worktree']))): {
                            'sha256': reference(path)['sha256'], 'executable': False}
                            for path in returned.rglob('*') if path.is_file()}
                        fixture.completed['inner']['candidate'] = save(fixture.fixture / 'pool-candidate.json', candidate)
                    fixture.pool_completed = True
                return copy.deepcopy(fixture.completed)

            fixture.completion = complete
            self.sessions[key] = fixtures.ScriptedCLI(fixture)
            self.owner_calls.append(call)
        return self.sessions[key].invoke(verb, request, evidence, capture=capture)

    def review(self, call, *_):
        self.sessions[call.fingerprint()].accepted = True


class NativeBridgePoolTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        self.fixture = fixtures.BridgeTests()
        self.fixture.setUp()
        self.fixture.skill.write_text('---\nname: main\ndescription: Reconcile signed transactions\n---\nRead all signed records.\n')
        self.addCleanup(self.fixture.doCleanups)
        self.cli = PoolCLI(self)
        self.templates = []
        # Deliberately interleaved: native seq never promises this arrival order.
        for index, role in enumerate(('solver', 'proposer', 'solver', 'proposer', 'solver', 'proposer', 'solver')):
            repository = self.root / f'control-{index}'
            repository.mkdir()
            template = replace(self.fixture.template, repository=str(repository), role=role)
            if role == 'proposer':
                template = replace(template, selected_skill=reference(self.fixture.skill),
                    harness_skills=(), skill_bases={}, write_paths=('context/harness',),
                    deliverables=(), harvest='context/harness')
            self.templates.append(template)
        self.bridge = SoodlesOwnerBridge(self.cli, self.root / 'owner', self.templates, review=self.cli.review)
        self.gate = SharedGate(self.root / 'journal.json', self.bridge, initial_paid_spent_usd='0.09')

    def test_native_seq_uses_role_pool_and_preserves_each_exact_control_binding(self):
        adapter = NoodleAdapter(route=Route('codex', 'codex', MODELS['codex'], 'low', 60), gate=self.gate,
            tasks={'records': Task('Reconcile all signed records.', self.fixture.source)},
            base=self.fixture.harness, runs_root=self.root / 'native', always_on_max=12000)
        with (patch.object(subprocess, 'run', side_effect=AssertionError('no process')),
             patch.object(subprocess, 'Popen', side_effect=AssertionError('no process')),
             patch.object(socket.socket, 'connect', side_effect=AssertionError('no network')),
             patch('soodles_bridge.time.sleep')):
            result = run_native_task(adapter, 'records', experiment_id='pool-fixture')
        native = result['native_result']
        self.assertEqual(native['status'], 'ok', native)
        self.assertEqual(native['modes']['seq']['fell_back'], [])
        self.assertEqual([call.role for call in self.cli.owner_calls],
                         ['solver', 'proposer', 'proposer', 'solver', 'solver', 'proposer', 'solver'])
        self.assertFalse(self.bridge.templates)
        bindings = [decode((self.bridge.root / call.fingerprint() / 'binding.json').read_bytes())['template']
                    for call in self.cli.owner_calls]
        self.assertEqual(len({binding['repository'] for binding in bindings}), 7)
        reopened = SoodlesOwnerBridge(self.cli, self.bridge.root, self.templates, review=self.cli.review, reopen=True)
        reopened.restore_templates(list(self.gate.state['calls'].values()))
        self.assertEqual(reopened.templates, [])
        foreign = replace(self.templates[0], repository=str(self.root / 'different-control'))
        changed = SoodlesOwnerBridge(self.cli, self.bridge.root, [foreign] + self.templates[1:],
                                     review=self.cli.review, reopen=True)
        with self.assertRaises(Refused):
            changed.restore_templates(list(self.gate.state['calls'].values()))

    def test_single_codex_driver_runs_complete_native_seq_without_paid_client(self):
        import reproduce
        from experiment_driver import run_experiment
        from harness2.core import schedule
        from owner_contract import tree_ref
        from test_experiment_driver import PublicBundleTests

        public = PublicBundleTests()
        public.setUp()
        self.addCleanup(public.doCleanups)
        bundle = public.prepare()
        upstream = Path(schedule.__file__).resolve().parents[2]
        prepared = save(self.root / 'prepared-source.json', {'source_pin': reproduce.PIN,
            'source_tree_sha256': reproduce.fingerprint(upstream)})
        output = self.root / 'single-codex-experiment'
        config = {'upstream': {'path': str(upstream), 'prepared_receipt': prepared},
            'output_root': str(output), 'experiment_id': 'new-codex-full-round',
            'initial_paid_spent_usd': '0.388148325', 'poll_s': 0.001,
            'templates': [asdict(template) for template in self.templates], 'cli': {},
            # Even an irrelevant billing section must not touch credentials/network.
            'billing': {'environment_file': str(self.root / 'must-not-open.env')},
            'groups': [{'route': {'group': 'codex', 'provider': 'codex', 'model': MODELS['codex'],
                       'effort': 'low', 'timeout_s': 60}, 'task_bundle': bundle,
                       'baseline': tree_ref(self.fixture.harness), 'always_on_max': 12000}]}
        with (patch('experiment_driver.PinnedCLI', return_value=self.cli),
              patch('experiment_driver.ReadOnlyOpenRouter', side_effect=AssertionError('no paid-key client')),
              patch('experiment_driver.OpenRouterBilling', side_effect=AssertionError('no paid accounting client')),
              patch.object(subprocess, 'run', side_effect=AssertionError('no process')),
              patch.object(subprocess, 'Popen', side_effect=AssertionError('no process')),
              patch.object(socket.socket, 'connect', side_effect=AssertionError('no network')),
              patch('soodles_bridge.time.sleep')):
            receipt = run_experiment(config, self.cli.review)
        report = decode(read_ref(receipt))
        self.assertEqual([row['group'] for row in report['groups']], ['codex'])
        native = report['groups'][0]['result']['native_result']
        self.assertEqual(native['status'], 'ok', native)
        self.assertEqual(native['modes']['seq']['fell_back'], [])
        self.assertEqual((native['modes']['seq']['ledger']['R'], native['modes']['seq']['ledger']['P']), (4, 3))
        self.assertEqual(native['selections']['seq']['chosen_label'], 'c1')
        self.assertEqual([call.role for call in self.cli.owner_calls],
                         ['solver', 'proposer', 'proposer', 'solver', 'solver', 'proposer', 'solver'])
        self.assertTrue(all(call.provider == 'codex' and call.model == MODELS['codex']
                            and call.inputs['effort'] == 'low' for call in self.cli.owner_calls))
        journal = decode((output / 'owner-journal.json').read_bytes())
        self.assertEqual(len(journal['calls']), 7)
        self.assertTrue(all(row['state'] == 'settled' and row['result']['cost_usd'] is None
                            for row in journal['calls'].values()))
        bindings = [decode((output / 'owner' / call.fingerprint() / 'binding.json').read_bytes())['template']
                    for call in self.cli.owner_calls]
        self.assertEqual(len({binding['repository'] for binding in bindings}), 7)
        self.assertEqual([binding['role'] for binding in bindings], [call.role for call in self.cli.owner_calls])
        self.assertFalse((output / 'pi').exists())

    def test_exhausted_role_refuses_before_directory_or_owner_and_unused_refusal_preserves_pool(self):
        call = replace(self.fixture.call, role='proposer')
        bridge = SoodlesOwnerBridge(self.cli, self.root / 'only-solvers', [self.templates[0]], review=self.cli.review)
        with self.assertRaises(Refused):
            bridge.execute(call)
        self.assertEqual(list(bridge.root.iterdir()), [])
        self.assertFalse(self.cli.owner_calls)
        old_empty = bridge.root / call.fingerprint()
        old_empty.mkdir()
        row = {'state': 'refused', 'error': 'task template belongs to another call route', 'call': asdict(call)}
        bridge.restore_templates([row])
        self.assertEqual(bridge.templates, [self.templates[0]])
        (old_empty / 'intent.json').write_text('{}')
        with self.assertRaises(Refused):
            bridge.restore_templates([row])
