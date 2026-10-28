"""Offline stopped-reader protocol fixtures; never original Host evidence."""
import copy
import json
import shutil
import subprocess
import unittest
from dataclasses import replace
from pathlib import Path
from unittest.mock import patch

import test_soodles_bridge as fixtures
from owner_contract import Refused, SharedGate, UnknownEffect
from soodles_bridge import SoodlesOwnerBridge, decode, read_ref, reference, save
from stopped_settlement import settle_stopped_call


class StoppedSettlementTests(unittest.TestCase):
    def setUp(self):
        self.fixture = f = fixtures.BridgeTests()
        f.setUp()
        self.addCleanup(f.doCleanups)
        if 'paid' in self._testMethodName:
            f.call = replace(f.call, group='pi', provider='pi', model=fixtures.MODELS['pi'])
            f.template = replace(f.template, group='pi')
        self.cli = fixtures.ScriptedCLI(f)

        def review(*_):
            raise UnknownEffect('fixture original integration readback interrupted')

        self.bridge = SoodlesOwnerBridge(self.cli, f.root / 'bridge', [f.template], review=review, billing=fixtures.UnknownBilling())
        self.gate = SharedGate(f.root / 'gate.json', self.bridge, initial_paid_spent_usd='0.091620363')
        with patch('soodles_bridge.time.sleep'), self.assertRaises(UnknownEffect):
            self.gate.execute(f.call)
        self.bridge = SoodlesOwnerBridge(self.cli, self.bridge.root, [f.template], review=self.fail, reopen=True)
        self.before = self.gate.journal.read_bytes()
        self.calls = list(self.cli.calls)
        self.directory = self.bridge.root / f.call.fingerprint()
        self.request = reference(self.directory / 'request.json')
        captured = copy.deepcopy(f.completed)
        captured['inner']['native_host'] = f.initial_pending(self.request)['inner']['native_host']
        capture_ref = save(f.root / 'original-capture.json', captured)
        candidate = captured['inner']['candidate']
        native_ref = save(f.root / 'native-reconciliation.json', {
            'schema': 1, 'owner': 'Noodle stopped local merge', 'status': 'reconciled',
            'candidate': candidate, 'effects': ['local_canonical_reconciliation']})
        reader = f.root / 'task_stopped_integration.py'
        reader.write_text('# Explicit test-only transport sentinel.\n')
        self.migration = {'schema': 1, 'kind': 'stopped-original-task-integration',
            'request': self.request, 'candidate': candidate, 'reconciliation': native_ref,
            'closure': save(f.root / 'closure.json', {'fixture_only': True}),
            'reader': {'source_commit': 'a' * 40, 'python': reference(f.noodle),
                       'sources': [reference(reader)]}}
        self.migration_ref = save(f.root / 'migration.json', self.migration)
        inner = captured['inner']
        self.observation = {'owner': 'soodles.loop', 'kind': 'stopped_original_task_integration',
            'request': self.request, 'status': 'observed', 'effects': [], 'authorizes_landing': False,
            'execution_time_scope': 'historical_original_verified_capture',
            'integration_time_scope': 'current_stopped_native_reconciliation',
            'sources': {'migration': self.migration_ref, 'reconciliation': native_ref,
                        'closure': self.migration['closure'], 'original_capture': capture_ref},
            'inner': {**inner, 'owner': 'issue-execution.local-task', 'acceptance': 'PASS',
                'native_host': {**inner['native_host'], 'context_state': 'closed'},
                'native_host_time_scope': 'historical_identity_original_owner_closed',
                'review_progress': {'status': 'locally_integrated',
                                   'review': {'decision': 'approve', 'candidate': candidate}}}}
        # A stopped integration may have removed the original worker. The saved
        # candidate/capture, not that directory, is the bridge's output authority.
        shutil.rmtree(f.root / 'returned')

    def reader(self, command, **kwargs):
        self.assertEqual(command[:4], [self.fixture.noodle.as_posix(), '-B',
                         self.migration['reader']['sources'][0]['path'], 'inspect'])
        self.assertNotIn('NOODLE_HOST_DIR', kwargs['env'])
        self.assertEqual(command[command.index('--request-file') + 1], self.request['path'])
        output = Path(command[command.index('--output') + 1])
        reference_out = save(output, self.observation)
        kwargs['stdout'].write(json.dumps(reference_out).encode())
        return subprocess.CompletedProcess(command, 0)

    def test_same_call_settles_retained_result_preserving_unknown_history_and_cost(self):
        with patch('stopped_settlement.subprocess.run', side_effect=self.reader) as invoke:
            result, proof = settle_stopped_call(self.bridge, self.gate, self.fixture.call, self.migration_ref)
        self.assertEqual(invoke.call_count, 1)
        self.assertEqual(self.cli.calls, self.calls)
        row = next(iter(self.gate.state['calls'].values()))
        history = row['reconciliations'][0]
        self.assertEqual(history['kind'], 'stopped-original-call-integration')
        self.assertEqual(read_ref(history['prior_journal']), self.before)
        self.assertEqual(history['prior_row']['state'], 'unknown')
        self.assertIsNone(result.cost_usd)
        self.assertIsNone(result.wall_s)
        self.assertEqual(self.gate.state['initial_paid_spent_usd'], '0.091620363')
        self.assertEqual(row['started_unix_s'], history['prior_row']['started_unix_s'])
        self.assertTrue(self.gate.execute(self.fixture.call).cached)
        self.assertEqual(self.cli.calls, self.calls)
        self.assertEqual(decode(read_ref(proof))['request'], self.request)

    def test_paid_unknown_bill_preserves_original_remaining_hold(self):
        spent = self.gate._spent()
        reserved = next(iter(self.gate.state['calls'].values()))['reserved_usd']
        with patch('stopped_settlement.subprocess.run', side_effect=self.reader):
            result, _ = settle_stopped_call(self.bridge, self.gate, self.fixture.call, self.migration_ref)
        self.assertIsNone(result.cost_usd)
        self.assertEqual(self.gate._spent(), spent)
        self.assertEqual(next(iter(self.gate.state['calls'].values()))['reserved_usd'], reserved)
        self.assertEqual(self.cli.calls, self.calls)

    def test_incomplete_acceptance_keeps_unknown_and_never_reenters_owner(self):
        self.observation['inner']['acceptance'] = 'UNKNOWN'
        with patch('stopped_settlement.subprocess.run', side_effect=self.reader):
            with self.assertRaisesRegex(Refused, 'acceptance is incomplete'):
                settle_stopped_call(self.bridge, self.gate, self.fixture.call, self.migration_ref)
        self.assertEqual(self.gate.journal.read_bytes(), self.before)
        self.assertEqual(self.cli.calls, self.calls)

    def test_changed_historical_host_is_refused_and_keeps_original_journal(self):
        self.observation['inner']['native_host']['host_instance_id'] = 'replacement'
        with patch('stopped_settlement.subprocess.run', side_effect=self.reader):
            with self.assertRaisesRegex(Refused, 'Host identity differs'):
                settle_stopped_call(self.bridge, self.gate, self.fixture.call, self.migration_ref)
        self.assertEqual(self.gate.journal.read_bytes(), self.before)

    def test_changed_template_never_dispatches_or_inserts_default_fields(self):
        binding = decode((self.directory / 'binding.json').read_bytes())
        # A removed field is a changed immutable template, not permission to
        # apply today's dataclass defaults to a historical training request.
        binding['template'].pop('acceptance')
        (self.directory / 'binding.json').write_text(json.dumps(binding))
        with patch('stopped_settlement.subprocess.run', side_effect=AssertionError('reader invoked')):
            with self.assertRaisesRegex(Refused, 'template changed'):
                settle_stopped_call(self.bridge, self.gate, self.fixture.call, self.migration_ref)
        self.assertEqual(self.gate.journal.read_bytes(), self.before)


if __name__ == '__main__':
    unittest.main()
