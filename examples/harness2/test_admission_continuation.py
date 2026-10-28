"""Offline capacity continuation controls; no owner processes are launched."""
import copy
import json
import unittest
from pathlib import Path
from unittest.mock import patch

import test_soodles_bridge as fixtures
from owner_contract import SharedGate, UnknownEffect, Refused
from soodles_bridge import SoodlesOwnerBridge, decode, host_identity, read_ref, reference, save, verify_call_reconciliation


class CapacityCLI(fixtures.ScriptedCLI):
    def invoke(self, verb, request, evidence, *, capture=None):
        if self.calls:
            return super().invoke(verb, request, evidence, capture=capture)
        self.calls.append((verb, capture))
        self.test.assertEqual(verb, 'enter')
        evidence.mkdir()
        self.test.initial_pending(request)  # Only creates offline config bytes.
        observation = save(evidence / 'stdout.json', {'owner': 'soodles.loop', 'status': 'refused',
            'problem': {'field': 'task.host.attach', 'value': 'host context limit reached (16)'},
            'effects': [], 'authorizes_landing': False,
            'next': {'kind': 'input', 'owner': 'Noodle', 'required': ['original_request_readback']}})
        stderr = save(evidence / 'stderr.log', {'fixture_only': True})
        save(evidence / 'intent.json', {'request': request,
            'argv': [self.python['path'], '-B', self.sources[0]['path'], 'loop', 'enter', '--request-file', request['path']],
            'source_commit': self.source_commit, 'python': self.python, 'sources': self.sources})
        save(evidence / 'receipt.json', {'exit_code': 2, 'stdout': observation, 'stderr': stderr})
        raise UnknownEffect('original capacity refusal remains preserved')


class AdmissionContinuationTests(unittest.TestCase):
    def setUp(self):
        class Fixture(fixtures.BridgeTests):
            def initial_pending(self, request):
                value = super().initial_pending(request)
                body = decode(read_ref(request))
                config = Path(body['output']) / 'admission/noodle.toml'
                config.parent.mkdir(parents=True, exist_ok=True)
                if not config.exists():
                    config.write_text('# fixed offline original config\n')
                value['inner']['native_host'].update(config_path=str(config), config_sha256=reference(config)['sha256'],
                    binary_sha256=reference(self.noodle)['sha256'])
                return value
        self.f = Fixture()
        self.f.setUp()
        self.addCleanup(self.f.doCleanups)
        self.cli = CapacityCLI(self.f)
        def review(*_):
            self.cli.accepted = True
        self.bridge = SoodlesOwnerBridge(self.cli, self.f.root / 'bridge', [self.f.template], review=review, poll_s=0.001)
        self.gate = SharedGate(self.f.root / 'gate.json', self.bridge, initial_paid_spent_usd='0.091620363')
        with self.assertRaises(UnknownEffect):
            self.gate.execute(self.f.call)
        self.original_journal = self.gate.journal.read_bytes()
        self.directory = self.bridge.root / self.f.call.fingerprint()
        self.request = reference(self.directory / 'request.json')
        self.success = self.f.initial_pending(self.request)
        native = self.success['inner']['native_host']
        body = decode(read_ref(self.request))
        d = self.f.root / 'operator-proof'
        d.mkdir()
        before = {'schema': 1, 'request_id': native['request_id'], 'operation': 'attach',
            'project_dir': body['repository'], 'config_path': native['config_path'], 'config_sha256': native['config_sha256'],
            'mode': 'manual', 'binary_sha256': native['binary_sha256'], 'environment_sha256': 'e' * 64,
            'status': 'busy', 'host_dir': native['host_dir'], 'cleanup_completed': True}
        save(Path(body['output']) / 'admission/host-request.json', {**before, 'status': 'accepted'})
        failed = self.directory / 'operation-0000'
        self.proof = {'kind': 'original-call-readback', 'call_sha256': self.f.call.fingerprint(),
            'binding': reference(self.directory / 'binding.json'), 'request': self.request,
            'call': reference(self.directory / 'call.json'), 'retained_result': None,
            'original_observation': reference(failed / 'stdout.json'),
            'original_intent': reference(failed / 'intent.json'), 'original_receipt': reference(failed / 'receipt.json'),
            'fresh_observation': save(d / 'fresh.json', self.success)}
        self.evidence = {'schema': 1, 'kind': 'original-capacity-admission-continuation',
            'call_sha256': self.f.call.fingerprint(), 'request': self.request,
            'original_observation': self.proof['original_observation'],
            'native_request_path': str(Path(body['output']) / 'admission/host-request.json'),
            'request_before': save(d / 'before.json', before),
            'request_after': save(d / 'after.json', {**before, 'status': 'accepted'})}
        packets = {
            'resources_before': ([body['noodle'], 'host', 'resources'], {'schema': 1, 'protocol': 'noodle-host-v1',
                'host_instance_id': native['host_instance_id'], 'host_pid': native['host_pid'], 'contexts': 15}),
            'native_attach': ([body['noodle'], '--project-dir', body['repository'], 'host', 'attach', '--request-file',
                self.evidence['native_request_path'], '--mode', 'manual', '--config-file', native['config_path'], '--cleanup-completed'], native)}
        for key, (argv, observation) in packets.items():
            self.evidence[key + '_intent'] = save(d / (key + '-intent.json'), {'argv': argv})
            stdout = save(d / (key + '-stdout.json'), observation)
            stderr = save(d / (key + '-stderr.json'), {'offline': True})
            self.evidence[key + '_stdout'], self.evidence[key + '_stderr'] = stdout, stderr
            self.evidence[key + '_receipt'] = save(d / (key + '-receipt.json'), {'exit_code': 0,
                'stdout_sha256': stdout['sha256'], 'stderr_sha256': stderr['sha256']})
        self.evidence['admission_observation'] = save(d / 'admission.json', self.success)
        self.evidence['admission_intent'] = save(d / 'admission-intent.json', decode(read_ref(self.proof['original_intent'])))
        self.evidence['admission_receipt'] = save(d / 'admission-receipt.json', {'exit_code': 0,
            'stdout': self.evidence['admission_observation'], 'stderr': self.evidence['native_attach_stderr']})
        self.proof['admission_continuation'] = save(d / 'proof.json', self.evidence)

    def test_exact_busy_continuation_reconciles_without_enter_or_erasing_failure(self):
        self.assertEqual(verify_call_reconciliation(self.f.call, self.proof), host_identity(self.success))
        failed = {p.name: p.read_bytes() for p in (self.directory / 'operation-0000').iterdir()}
        reopened = SoodlesOwnerBridge(self.cli, self.bridge.root, [self.f.template], review=self.bridge.review, reopen=True, poll_s=0.001)
        result, proof = reopened.reconcile_call(self.f.call, self.proof['fresh_observation'],
            admission_continuation=self.proof['admission_continuation'])
        self.assertEqual([verb for verb, _ in self.cli.calls].count('enter'), 1)
        self.assertTrue(result.settled)
        self.assertEqual(self.original_journal, self.gate.journal.read_bytes())
        self.assertEqual(failed, {p.name: p.read_bytes() for p in (self.directory / 'operation-0000').iterdir()})
        self.gate.reconcile_original_call(self.f.call, result, proof)
        row = next(iter(self.gate.state['calls'].values()))
        self.assertEqual(row['state'], 'settled')
        self.assertEqual(read_ref(row['reconciliations'][0]['prior_journal']), self.original_journal)

    def test_failed_reconciliation_can_read_only_settle_same_captured_approved_candidate(self):
        failed = {p.name: p.read_bytes() for p in (self.directory / 'operation-0000').iterdir()}
        def interrupted_review(*_):
            self.cli.accepted = True
            raise UnknownEffect('Session review readback interrupted')
        first = SoodlesOwnerBridge(self.cli, self.bridge.root, [self.f.template],
            review=interrupted_review, reopen=True, poll_s=0.001)
        with self.assertRaises(UnknownEffect):
            first.reconcile_call(self.f.call, self.proof['fresh_observation'],
                admission_continuation=self.proof['admission_continuation'])
        prior = list(self.directory.glob('reconciliation-*'))
        self.assertEqual(len(prior), 1)
        preserved = {p.relative_to(prior[0]): p.read_bytes() for p in prior[0].rglob('*') if p.is_file()}
        self.assertTrue((self.directory / 'result/native-evidence.json').exists())
        original = self.f.completed['inner']['candidate']
        candidate = decode(read_ref(original))
        self.cli.published_candidate = save(self.f.root / 'later-review-metadata.json',
            {**candidate, 'review_sha256': 'f' * 64})
        fresh, fresh_ref = self.cli.invoke('inspect', self.request, self.f.root / 'fresh-after-review')
        self.assertNotEqual(fresh['inner']['candidate'], original)
        self.assertEqual(fresh['inner']['review_progress']['review']['candidate'], original)
        reopened = SoodlesOwnerBridge(self.cli, self.bridge.root, [self.f.template],
            review=self.fail, reopen=True, poll_s=0.001)
        changed = copy.deepcopy(fresh)
        changed['inner']['review_progress']['review']['candidate'] = self.cli.published_candidate
        wrong_ref = save(self.f.root / 'different-selected-candidate.json', changed)
        with self.assertRaisesRegex(Refused, 'fresh original candidate differs'):
            reopened.reconcile_call(self.f.call, wrong_ref, admission_continuation=self.proof['admission_continuation'])
        artifact = self.directory / 'result/workspace/answer.txt'
        original_artifact = artifact.read_bytes()
        artifact.write_text('changed saved result')
        with self.assertRaises(Refused):
            reopened.reconcile_call(self.f.call, fresh_ref, admission_continuation=self.proof['admission_continuation'])
        artifact.write_bytes(original_artifact)
        self.assertEqual(self.gate.journal.read_bytes(), self.original_journal)
        result, proof = reopened.reconcile_call(self.f.call, fresh_ref,
            admission_continuation=self.proof['admission_continuation'])
        self.assertEqual(len(list(self.directory.glob('reconciliation-*'))), 2)
        self.assertEqual(preserved, {p.relative_to(prior[0]): p.read_bytes() for p in prior[0].rglob('*') if p.is_file()})
        self.assertEqual(failed, {p.name: p.read_bytes() for p in (self.directory / 'operation-0000').iterdir()})
        self.assertIsNotNone(decode(read_ref(proof))['retained_result'])
        self.gate.reconcile_original_call(self.f.call, result, proof)
        self.assertEqual([verb for verb, _ in self.cli.calls].count('enter'), 1)
        self.assertEqual(next(iter(self.gate.state['calls'].values()))['state'], 'settled')

    def test_settlement_write_failure_preserves_unknown_and_reuses_only_exact_archive(self):
        reopened = SoodlesOwnerBridge(self.cli, self.bridge.root, [self.f.template],
            review=self.bridge.review, reopen=True, poll_s=0.001)
        result, proof = reopened.reconcile_call(self.f.call, self.proof['fresh_observation'],
            admission_continuation=self.proof['admission_continuation'])
        with patch.object(self.gate, '_save', side_effect=OSError('fixture persistence failure')):
            with self.assertRaises(UnknownEffect):
                self.gate.reconcile_original_call(self.f.call, result, proof)
        self.assertEqual(self.gate.journal.read_bytes(), self.original_journal)
        archive, = self.f.root.glob('gate.json.unknown-*.json')
        self.assertEqual(archive.read_bytes(), self.original_journal)
        fresh_gate = SharedGate(self.gate.journal, None, initial_paid_spent_usd='0.091620363')
        archive.write_text('corrupt earlier archive')
        with self.assertRaisesRegex(Refused, 'bound file bytes changed'):
            fresh_gate.reconcile_original_call(self.f.call, result, proof)
        self.assertEqual(self.gate.journal.read_bytes(), self.original_journal)
        archive.write_bytes(self.original_journal)
        fresh_gate.reconcile_original_call(self.f.call, result, proof)
        self.assertEqual(archive.read_bytes(), self.original_journal)
        self.assertEqual(next(iter(fresh_gate.state['calls'].values()))['state'], 'settled')
        self.assertEqual([verb for verb, _ in self.cli.calls].count('enter'), 1)

    def test_capacity_refusal_requires_explicit_continuation(self):
        proof = {k: v for k, v in self.proof.items() if k != 'admission_continuation'}
        with self.assertRaisesRegex(Refused, 'successful bound receipt'):
            verify_call_reconciliation(self.f.call, proof)

    def test_changed_binding_transport_or_owner_refuses_without_effect(self):
        cases = [('request_after', 'request_id', 'other'), ('request_after', 'environment_sha256', 'f' * 64),
            ('request_after', 'config_sha256', 'f' * 64), ('request_after', 'binary_sha256', 'f' * 64),
            ('request_before', 'status', 'unknown'), ('native_attach_stdout', 'host_instance_id', 'new-host'),
            ('native_attach_stdout', 'context_state', 'detached'), ('native_attach_stdout', 'context_id', 'foreign-context'), ('resources_before_stdout', 'contexts', 16),
            ('native_attach_receipt', 'exit_code', 1), ('admission_intent', 'source_commit', 'd' * 40)]
        for index, (field, key, value) in enumerate(cases):
            with self.subTest(field=field, key=key):
                evidence = copy.deepcopy(self.evidence)
                item = decode(read_ref(evidence[field])); item[key] = value
                evidence[field] = save(self.f.root / f'changed-{index}.json', item)
                if field.endswith('_stdout'):
                    receipt_key = field.removesuffix('_stdout') + '_receipt'
                    receipt = decode(read_ref(evidence[receipt_key])); receipt['stdout_sha256'] = evidence[field]['sha256']
                    evidence[receipt_key] = save(self.f.root / f'receipt-{index}.json', receipt)
                proof = {**self.proof, 'admission_continuation': save(self.f.root / f'proof-{index}.json', evidence)}
                with self.assertRaises(Refused):
                    verify_call_reconciliation(self.f.call, proof)
        self.assertEqual(self.gate.journal.read_bytes(), self.original_journal)
        self.assertEqual(len(self.cli.calls), 1)

    def test_original_current_request_and_template_cannot_be_substituted(self):
        native_path = Path(self.evidence['native_request_path'])
        original = native_path.read_bytes()
        native = decode(original)
        native['request_id'] = 'replacement-request'
        native_path.write_text(json.dumps(native))
        with self.assertRaisesRegex(Refused, 'original current accepted binding'):
            verify_call_reconciliation(self.f.call, self.proof)
        native_path.write_bytes(original)
        self.f.profile.write_text('{"changed":true}')
        with self.assertRaisesRegex(Refused, 'bound file bytes changed'):
            verify_call_reconciliation(self.f.call, self.proof)

    def test_other_refusal_effects_or_native_argv_are_not_fallbacks(self):
        for index, change in enumerate([{'problem': {'field': 'task.host.attach', 'value': 'native outcome unknown'}},
                                       {'effects': ['worker_started']}, {'inner': {}}, {'authorizes_landing': True}]):
            refused = {**decode(read_ref(self.proof['original_observation'])), **change}
            original = save(self.f.root / f'other-refusal-{index}.json', refused)
            receipt = {**decode(read_ref(self.proof['original_receipt'])), 'stdout': original}
            evidence = {**self.evidence, 'original_observation': original}
            proof = {**self.proof, 'original_observation': original,
                'original_receipt': save(self.f.root / f'other-receipt-{index}.json', receipt),
                'admission_continuation': save(self.f.root / f'other-proof-{index}.json', evidence)}
            with self.assertRaises(Refused):
                verify_call_reconciliation(self.f.call, proof)
        evidence = copy.deepcopy(self.evidence)
        intent = decode(read_ref(evidence['native_attach_intent'])); intent['argv'].append('--other')
        evidence['native_attach_intent'] = save(self.f.root / 'other-native-intent.json', intent)
        proof = {**self.proof, 'admission_continuation': save(self.f.root / 'other-native-proof.json', evidence)}
        with self.assertRaises(Refused):
            verify_call_reconciliation(self.f.call, proof)


if __name__ == '__main__':
    unittest.main()
