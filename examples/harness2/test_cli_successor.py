"""Explicit CLI source migration controls; no process or owner calls."""
import copy
import json
import sys
import tempfile
import unittest
from dataclasses import asdict
from pathlib import Path
from unittest.mock import patch

from cli_successor import verify_cli_successor
from owner_contract import OwnerCall, Refused, SharedGate, UnknownEffect, tree_ref
from soodles_bridge import PinnedCLI, SoodlesOwnerBridge, decode, reference, save


class CLISuccessorTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        self.sequence = 0
        self.call = OwnerCall('codex', 'same-run', 'solver', 'codex', 'gpt-5.6-sol', {})
        entries = []
        for name in ('old.py', 'new.py'):
            path = self.root / name
            path.write_text('# ' + name)
            entries.append(reference(path))
        common = {'python': reference(Path(sys.executable).resolve()),
                  'environment': {'HOME': '/offline'}, 'timeout_s': 120}
        self.old = asdict(PinnedCLI(**common, entrypoint=entries[0], sources=[entries[0]], source_commit='a' * 40))
        self.new = asdict(PinnedCLI(**common, entrypoint=entries[1], sources=[entries[1]], source_commit='b' * 40))
        base = {'experiment_id': 'same', 'output_root': str(self.root / 'run'), 'groups': ['codex']}
        self.old_config = {**base, 'cli': self.old}
        self.new_config = {**base, 'cli': self.new}
        intent = {name: self.old[name] for name in ('source_commit', 'python', 'sources')}
        intent['argv'] = [self.old['python']['path'], '-B', self.old['entrypoint']['path'], 'loop', 'enter']
        self.proof = {'call_sha256': self.call.fingerprint(),
                      'request': self.record({'original': 'request'}),
                      'original_intent': self.record(intent), 'retained_result': tree_ref(self.root)}
        self.packet = {'schema': 1, 'kind': 'captured-original-call-cli-successor',
                       'call_sha256': self.call.fingerprint(), 'request': self.proof['request'],
                       'original_intent': self.proof['original_intent'],
                       'predecessor_config': self.record(self.old_config),
                       'successor_config': self.record(self.new_config),
                       'failure': self.record({'owner': 'soodles.loop', 'status': 'refused', 'effects': []})}

    def record(self, value):
        self.sequence += 1
        return save(self.root / ('record-' + str(self.sequence) + '.json'), value)

    def verify(self, packet=None, **changes):
        proof = {**self.proof, **changes, 'cli_successor': self.record(packet or self.packet)}
        return verify_cli_successor(self.call, proof)

    def test_same_captured_call_can_select_a_verified_new_source(self):
        self.assertEqual(self.verify(), self.new)

    def test_experiment_and_execution_environment_cannot_change(self):
        for field, value in (('experiment_id', 'other'), ('output_root', '/other'), ('groups', ['pi'])):
            new = {**self.new_config, field: value}
            with self.subTest(field=field), self.assertRaises(Refused):
                self.verify({**self.packet, 'successor_config': self.record(new)})
        for field, value in (('environment', {'HOME': '/other'}), ('timeout_s', 1), ('python', self.new['entrypoint'])):
            new = {**self.new_config, 'cli': {**self.new, field: value}}
            with self.subTest(field=field), self.assertRaises(Refused):
                self.verify({**self.packet, 'successor_config': self.record(new)})

    def test_original_call_request_intent_and_source_remain_bound(self):
        for field, value in (('call_sha256', 'c' * 64), ('request', self.record({})),
                             ('original_intent', self.record({})), ('kind', 'automatic-upgrade')):
            with self.subTest(field=field), self.assertRaises(Refused):
                self.verify({**self.packet, field: value})
        wrong = copy.deepcopy(self.old_config)
        wrong['cli']['source_commit'] = 'd' * 40
        with self.assertRaises(Refused):
            self.verify({**self.packet, 'predecessor_config': self.record(wrong)})
        Path(self.new['entrypoint']['path']).write_text('changed')
        with self.assertRaises(Refused):
            self.verify()

    def test_uncaptured_admission_unknown_effects_and_unobserved_failure_refuse(self):
        for changes in ({'retained_result': None}, {'admission_continuation': self.record({})}):
            with self.subTest(changes=changes), self.assertRaises(Refused):
                self.verify(**changes)
        for failure in ({'owner': 'other', 'status': 'refused', 'effects': []},
                        {'owner': 'soodles.loop', 'status': 'unknown', 'effects': []},
                        {'owner': 'soodles.loop', 'status': 'refused', 'effects': ['unknown']}):
            with self.subTest(failure=failure), self.assertRaises(Refused):
                self.verify({**self.packet, 'failure': self.record(failure)})

    def test_original_captured_call_settles_with_new_cli_without_another_enter(self):
        import test_soodles_bridge as fixtures
        f = fixtures.BridgeTests()
        f.setUp()
        self.addCleanup(f.doCleanups)
        scripted = fixtures.ScriptedCLI(f)
        old = PinnedCLI(**self.old)
        new = PinnedCLI(**self.new)

        def invoke(cli, verb, request, evidence, *, capture=None):
            value, ref = scripted.invoke(verb, request, evidence, capture=capture)
            # The offline transport emits the same pinned argv/source envelope
            # as the real PinnedCLI, without executing an owner process.
            intent = decode((evidence / 'intent.json').read_bytes())
            intent.update(source_commit=cli.source_commit, python=cli.python, sources=cli.sources,
                          argv=[cli.python['path'], '-B', cli.entrypoint['path'], 'loop', verb])
            (evidence / 'intent.json').write_text(json.dumps(intent))
            return value, ref

        def refused_review(*_):
            raise UnknownEffect('preserved outer review refusal')

        root = f.root / 'bridge'
        with patch.object(PinnedCLI, 'invoke', invoke):
            bridge = SoodlesOwnerBridge(old, root, [f.template], review=refused_review, poll_s=0.001)
            gate = SharedGate(f.root / 'journal.json', bridge, initial_paid_spent_usd='0')
            with self.assertRaises(UnknownEffect):
                gate.execute(f.call)
            original_enters = [verb for verb, _ in scripted.calls].count('enter')
            prior_journal = gate.journal.read_bytes()
            directory = root / f.call.fingerprint()
            original_intent = reference(directory / 'operation-0000/intent.json')
            original_bytes = Path(original_intent['path']).read_bytes()
            scripted.accepted = True
            request = reference(directory / 'request.json')
            _, fresh = new.invoke('inspect', request, f.root / 'fresh')
            packet = {**self.packet, 'call_sha256': f.call.fingerprint(), 'request': request,
                      'original_intent': original_intent}
            successor = save(f.root / 'successor.json', packet)
            resumed = SoodlesOwnerBridge(new, root, [f.template], review=lambda *_: None,
                                        reopen=True, poll_s=0.001)
            result, proof = resumed.reconcile_call(f.call, fresh, cli_successor=successor)
            self.assertTrue(result.settled)
            self.assertEqual(gate.journal.read_bytes(), prior_journal)
            gate.reconcile_original_call(f.call, result, proof)
            self.assertEqual(next(iter(gate.state['calls'].values()))['state'], 'settled')
            self.assertEqual(Path(original_intent['path']).read_bytes(), original_bytes)
            self.assertEqual([verb for verb, _ in scripted.calls].count('enter'), original_enters)


if __name__ == '__main__':
    unittest.main()
