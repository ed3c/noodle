"""Offline feedback controls: real pinned adapter/core, fake owner and checker."""
import copy
import json
import shutil
import socket
import subprocess
import tempfile
import unittest
from dataclasses import replace
from pathlib import Path
from unittest.mock import patch

from drawing_feedback import DrawingFeedback, ENGINE_NAMES, public_finding
from native_adapter import NoodleAdapter, Route, Task, run_native_task
from owner_contract import Refused, SharedGate, UnknownEffect, tree_ref
from soodles_bridge import MODELS, decode, read_ref, reference, save
from test_native_contract import FakeOwner

HERE = Path(__file__).resolve().parent


class DrawingOwner(FakeOwner):
    def execute(self, call):
        result = super().execute(call)
        if call.role == 'solver':
            run = Path(result.run_dir)
            (run / 'workspace').mkdir()
            # Non-ASCII bytes prove projection comparison is not a loose text match.
            html = getattr(self, 'drawing', '<html><body><svg><text>尺寸 12 mm</text></svg></body></html>\n')
            (run / 'workspace' / 'drawing.html').write_bytes(html.encode())
            (run / 'deliverables.json').write_text(json.dumps({'drawing.html': html}))
            result = replace(result, output_sha256=tree_ref(run)['sha256'])
        return result


class DrawingFeedbackTests(unittest.TestCase):
    def test_public_feedback_keeps_actionable_artifact_locations_without_oracle(self):
        box = {'x': 10, 'y': 20, 'width': 40, 'height': 12}
        finding = {'status': 'FAIL', 'kind': 'text-collision', 'expected': 'private oracle',
                   'texts': [{'text': 'W1', 'box': box}, {'text': 'Plan title', 'box': box}],
                   'pixels': 9, 'location': 'svg > text', 'code': 'W1'}
        result = public_finding('visual', finding)
        self.assertEqual(result['texts'], finding['texts'])
        self.assertEqual(result['pixels'], 9)
        self.assertEqual(result['location'], 'svg > text')
        self.assertNotIn('expected', result)
        self.assertEqual(len(public_finding('visual', {'status': 'UNKNOWN', 'text': 'x' * 20000})['text']), 160)

    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        self.source = self.root / 'source'
        self.source.mkdir()
        self.measurement = self.source / 'geometry.json'
        self.measurement.write_text('{"length":12}')
        self.facts = save(self.root / 'source-facts.json', {'fixture_training_measurement': 12})
        self.base = self.root / 'base'
        self.base.mkdir()
        (self.base / 'AGENTS.md').write_text('Draw the signed measured component.')
        self.config = {'scope': 'training', 'node': reference(Path(shutil.which('node')).resolve()),
            'engine': [reference(HERE / name) for name in ENGINE_NAMES], 'timeout_s': 30,
            'allowlist': [{'task_id': 'drawing', 'source': tree_ref(self.source),
                'source_files': [reference(self.measurement)], 'source_facts': self.facts}]}
        self.output = self.root / 'output'
        self.feedback = DrawingFeedback(self.config, self.output / 'quality-feedback' / 'codex')
        self.owner = DrawingOwner(self.root / 'owner')
        self.gate = SharedGate(self.root / 'journal.json', self.owner, initial_paid_spent_usd='0')
        self.adapter = self.make_adapter(self.feedback)
        self.status = 'FAIL'
        self.exit_code = None
        self.mutate_report = lambda report: None
        self.checks = []

    def make_adapter(self, feedback=None):
        return NoodleAdapter(route=Route('codex', 'codex', MODELS['codex'], 'low', 60), gate=self.gate,
            tasks={'drawing': Task('Draw measured component.', self.source),
                   'test-task': Task('Unseen drawing.', self.source)},
            base=self.base, runs_root=self.output / 'native', always_on_max=12000,
            training_feedback=feedback)

    def checker(self, args, **kwargs):
        self.checks.append(args)
        self.assertEqual(args[:2], [self.config['node']['path'], self.config['engine'][0]['path']])
        self.assertEqual(len(args), 5)
        self.assertNotIn('NODE_OPTIONS', kwargs['env'])
        checker = Path(args[3])
        checker.mkdir()
        (checker / 'drawing.html').write_bytes(Path(args[2]).read_bytes())
        artifact = reference(checker / 'drawing.html')
        oracle = save(checker / 'source-facts.json', decode(Path(args[4]).read_bytes()))
        sheet = checker / 'sheet.png'
        sheet.write_bytes(b'fixture-only')
        finding = {'status': self.status, 'kind': 'label-clipped' if self.status == 'FAIL' else 'fixture',
                   'expected': 'ORACLE-DO-NOT-PROJECT', 'text': 'x' * 20000}
        report = {'schema_version': 1, 'status': self.status, 'artifact': artifact, 'oracle': oracle,
            'engine': copy.deepcopy(self.config['engine']), 'environment': {'fixture_only': True},
            'results': [{'viewport': {'width': 1800, 'height': 1200},
                        'facts': {'status': 'PASS', 'findings': []},
                        'visual': {'status': self.status, 'findings': [finding] * 20}, 'status': self.status}],
            'evidence': [artifact, oracle, reference(sheet)]}
        self.mutate_report(report)
        ref = save(checker / 'report.json', report)
        kwargs['stdout'].write(b'progress\n' + json.dumps({'status': self.status, 'report': ref}).encode() + b'\n')
        return subprocess.CompletedProcess(args, self.exit_code if self.exit_code is not None else
                                           {'PASS': 0, 'FAIL': 1, 'UNKNOWN': 2}[self.status])

    def events(self, run, adapter=None):
        return [json.loads(line) for line in (adapter or self.adapter).trajectory(run, obs_trunc=10).splitlines()]

    def test_fail_feedback_binds_original_run_and_is_cached_without_owner_mutation(self):
        run = self.adapter.execute('drawing', None, 'base1')
        original_tree = tree_ref(run)
        journal = self.gate.journal.read_bytes()
        with patch('drawing_feedback.subprocess.run', side_effect=self.checker):
            events = self.events(run)
            self.assertEqual(self.events(run), events)
            # Reopened native execute returns cached=True; same original result gets no new check.
            self.adapter.execute('drawing', None, 'base1')
            self.assertEqual(self.events(run), events)
        self.assertEqual(len(self.checks), 1)
        event = events[-1]
        self.assertEqual(event['event_id'], 'machine_quality_feedback')
        self.assertEqual(event['record']['status'], 'FAIL')
        self.assertFalse(event['record']['quality_pass'])
        self.assertEqual(event['record']['artifact'], reference(run / 'workspace' / 'drawing.html'))
        self.assertEqual(event['record']['source'], tree_ref(self.source))
        self.assertEqual(event['record']['harness'], tree_ref(self.base))
        self.assertEqual(event['record']['result'], original_tree)
        self.assertEqual(event['record']['run_id'], 'base1')
        self.assertEqual(len(event['record']['results'][0]['findings']), 8)
        self.assertNotIn('ORACLE-DO-NOT-PROJECT', json.dumps(events))
        self.assertLess(len(json.dumps(event)), 5000)
        self.assertEqual(tree_ref(run), original_tree)
        self.assertEqual(self.gate.journal.read_bytes(), journal)
        self.gate.verify_result(self.adapter._result(run))
        with self.assertRaises(Refused):
            self.adapter.score(run, 'drawing')

    def test_pass_fail_unknown_remain_exact_quality_statuses(self):
        for status in ('PASS', 'FAIL', 'UNKNOWN'):
            with self.subTest(status=status):
                self.status = status
                run = self.adapter.execute('drawing', None, status.lower())
                with patch('drawing_feedback.subprocess.run', side_effect=self.checker):
                    record = self.events(run)[-1]['record']
                self.assertEqual(record['status'], status)
                self.assertEqual(record['quality_pass'], status == 'PASS')
                self.assertFalse(self.gate.state['blocked'])

    def test_real_checker_cli_preserves_pass_fail_unknown_and_original_owner_bytes(self):
        html = '<p>Units</p><p>px</p><table><tr><th>Code</th><th>Variable</th><th>Value</th></tr>' \
               '<tr><td>W1</td><td>width</td><td>480</td></tr></table>' \
               '<svg width="200" height="70"><text x="20" y="30">W1</text></svg>'
        for status, drawing, values in [('PASS', html, {'W1': '480 px'}),
                ('FAIL', html.replace('>480<', '>481<'), {'W1': '480 px'}),
                ('UNKNOWN', html, {})]:
            with self.subTest(status=status):
                config = copy.deepcopy(self.config)
                config['allowlist'][0]['source_facts'] = save(self.root / f'{status}-facts.json',
                                                            {'values': values, 'defaultUnit': 'px'})
                feedback = DrawingFeedback(config, self.root / 'real-checker' / status)
                adapter = self.make_adapter(feedback)
                self.owner.drawing = drawing
                run = adapter.execute('drawing', None, 'real-' + status.lower())
                before = tree_ref(run)
                record = self.events(run, adapter)[-1]['record']
                self.assertEqual(record['status'], status)
                self.assertEqual(record['quality_pass'], status == 'PASS')
                self.assertEqual(len(record['results']), 2)
                self.assertEqual(tree_ref(run), before)
                self.assertEqual(self.events(run, adapter)[-1]['record'], record)

    def test_unseen_or_disabled_never_checks_or_adds_event(self):
        for task, adapter in [('test-task', self.adapter), ('drawing', self.make_adapter())]:
            run = adapter.execute(task, None, task)
            with patch('drawing_feedback.subprocess.run', side_effect=AssertionError('no checker')):
                self.assertEqual(len(self.events(run, adapter)), 2)
        self.assertFalse(self.feedback.output.exists())

    def test_projection_mismatch_and_owner_unknown_refuse_before_checker(self):
        run = self.adapter.execute('drawing', None, 'projection')
        result = self.adapter._result(run)
        call = self.adapter._calls[str(run)]
        with patch('drawing_feedback.subprocess.run', side_effect=AssertionError('no checker')):
            with self.assertRaises(Refused):
                self.feedback(result, call, {'drawing.html': 'other bytes'}, self.gate)
            (run / 'workspace' / 'drawing.html').write_text('tampered')
            with self.assertRaises(UnknownEffect):
                self.events(run)
        self.assertTrue(self.gate.state['blocked'])
        self.assertFalse(self.feedback.output.exists())

    def test_invalid_execution_retains_unknown_and_never_retries(self):
        self.exit_code = 0  # FAIL with exit 0 is execution-unknown, never a pass.
        run = self.adapter.execute('drawing', None, 'mismatch')
        with patch('drawing_feedback.subprocess.run', side_effect=self.checker):
            for _ in range(2):
                with self.assertRaisesRegex(Refused, 'unknown.*blocked'):
                    self.events(run)
        self.assertEqual(len(self.checks), 1)
        record = decode(next(self.feedback.output.glob('*/feedback.json')).read_bytes())
        self.assertEqual(record['event']['record']['status'], 'UNKNOWN')
        self.assertFalse(record['event']['record']['quality_pass'])
        self.assertEqual(record['execution_status'], 'unknown')
        with self.assertRaises(Refused):
            self.adapter.execute('drawing', None, 'another-owner')
        with self.assertRaises(Refused):
            DrawingFeedback(self.config, self.feedback.output)

    def test_timeout_keeps_intent_and_blocks_second_launch(self):
        run = self.adapter.execute('drawing', None, 'timeout')
        with patch('drawing_feedback.subprocess.run', side_effect=subprocess.TimeoutExpired('fixture', 30)) as checker:
            for _ in range(2):
                with self.assertRaises(Refused):
                    self.events(run)
        self.assertEqual(checker.call_count, 1)
        self.assertTrue(list(self.feedback.output.glob('*/intent.json')))

    def test_misbound_or_weakened_reports_block(self):
        variants = {
            'schema': lambda r: r.update(schema_version=2),
            'engine': lambda r: r.update(engine=r['engine'][:1]),
            'artifact': lambda r: r.update(artifact=r['oracle']),
            'oracle': lambda r: r.update(oracle=r['artifact']),
            'outside-evidence': lambda r: r['evidence'].append(self.facts),
            'erased-fail': lambda r: r.update(status='PASS'),
            'erased-dimension': lambda r: r['results'][0]['visual'].update(status='PASS'),
            'missing-visual': lambda r: r['results'][0].pop('visual'),
        }
        for name, mutate in variants.items():
            with self.subTest(name=name):
                self.mutate_report = mutate
                feedback = DrawingFeedback(self.config, self.root / 'invalid-feedback' / name)
                adapter = self.make_adapter(feedback)
                run = adapter.execute('drawing', None, name)
                with patch('drawing_feedback.subprocess.run', side_effect=self.checker):
                    with self.assertRaises(Refused):
                        self.events(run, adapter)

    def test_checker_unknown_catch_shape_is_feedback_without_pass(self):
        self.status = 'UNKNOWN'
        def caught(report):
            report['results'] = [{'viewport': {'width': 390, 'height': 844},
                                  'status': 'UNKNOWN', 'error': 'fixture browser failure'}]
        self.mutate_report = caught
        run = self.adapter.execute('drawing', None, 'caught')
        with patch('drawing_feedback.subprocess.run', side_effect=self.checker):
            record = self.events(run)[-1]['record']
        self.assertEqual(record['status'], 'UNKNOWN')
        self.assertFalse(record['quality_pass'])

    def test_configuration_is_explicit_and_source_engine_node_drift_refuses(self):
        for field, value in [('scope', 'test'), ('allowlist', []), ('engine', self.config['engine'][:1])]:
            bad = copy.deepcopy(self.config)
            bad[field] = value
            with self.subTest(field=field), self.assertRaises(Refused):
                DrawingFeedback(bad, self.root / 'bad')
        bad = copy.deepcopy(self.config)
        bad['allowlist'][0]['task_id'] = 'not-an-adapter-task'
        with self.assertRaises(Refused):
            self.make_adapter(DrawingFeedback(bad, self.root / 'bad'))
        for ref in [self.config['node'], *self.config['engine']]:
            bad = copy.deepcopy(self.config)
            target = bad['node'] if ref == self.config['node'] else next(r for r in bad['engine'] if r == ref)
            target['sha256'] = '0' * 64
            with self.assertRaises(Refused):
                DrawingFeedback(bad, self.root / 'bad')
        run = self.adapter.execute('drawing', None, 'source-drift')
        self.measurement.write_text('{}')
        with patch('drawing_feedback.subprocess.run', side_effect=AssertionError('no checker')), self.assertRaises(Refused):
            self.events(run)

    def test_cached_evidence_drift_refuses_without_retry(self):
        run = self.adapter.execute('drawing', None, 'cached')
        with patch('drawing_feedback.subprocess.run', side_effect=self.checker):
            self.events(run)
        next(self.feedback.output.glob('*/execution/checker/sheet.png')).write_bytes(b'changed')
        with patch('drawing_feedback.subprocess.run', side_effect=AssertionError('no retry')), self.assertRaises(Refused):
            self.events(run)

    def test_real_native_core_projects_feedback_to_proposer_without_score_or_network(self):
        with (patch('drawing_feedback.subprocess.run', side_effect=self.checker),
              patch.object(subprocess, 'Popen', side_effect=AssertionError('no process')),
              patch.object(socket.socket, 'connect', side_effect=AssertionError('no network')),
              patch.object(self.adapter, 'score', side_effect=AssertionError('core does not consume score'))):
            result = run_native_task(self.adapter, 'drawing', experiment_id='feedback-fixture')
        self.assertEqual(result['native_result']['status'], 'ok')
        self.assertTrue(self.checks)
        proposer_calls = [call for call in self.owner.calls if call.role == 'proposer']
        self.assertTrue(proposer_calls)
        contexts = [Path(call.inputs['context']['path']) for call in proposer_calls]
        self.assertTrue(any('machine_quality_feedback' in path.read_text()
                            for root in contexts for path in root.rglob('*') if path.is_file()))
        self.assertFalse(result['adoption_authorized'])

    def test_core_cannot_dispatch_fallback_after_unknown_checker_execution(self):
        self.exit_code = 9
        with (patch('drawing_feedback.subprocess.run', side_effect=self.checker),
              patch.object(subprocess, 'Popen', side_effect=AssertionError('no process')),
              self.assertRaises(Refused)):
            run_native_task(self.adapter, 'drawing', experiment_id='unknown-checker')
        self.assertEqual(len(self.checks), 1)
        self.assertEqual([call.role for call in self.owner.calls], ['solver'])

    def test_driver_wires_only_explicit_training_config(self):
        import reproduce
        from harness2.core import schedule
        from experiment_driver import run_experiment
        from test_experiment_driver import PublicBundleTests
        public = PublicBundleTests()
        public.setUp()
        self.addCleanup(public.doCleanups)
        bundle_ref = public.prepare()
        bundle = decode(read_ref(bundle_ref))
        config = copy.deepcopy(self.config)
        config['allowlist'] = [{'task_id': bundle['task_id'], 'source': bundle['source'],
            'source_files': [reference(Path(bundle['source']['path']) / 'geometry.json')], 'source_facts': self.facts}]
        upstream = Path(schedule.__file__).resolve().parents[2]
        prepared = save(self.root / 'prepared.json', {'source_pin': reproduce.PIN,
            'source_tree_sha256': reproduce.fingerprint(upstream)})
        experiment = {'upstream': {'path': str(upstream), 'prepared_receipt': prepared},
            'output_root': str(self.root / 'experiment'), 'experiment_id': 'fixture', 'cli': {},
            'initial_paid_spent_usd': '0', 'poll_s': 1, 'templates': [],
            'groups': [{'route': {'group': 'codex', 'provider': 'codex', 'model': MODELS['codex'],
                        'effort': 'low', 'timeout_s': 60}, 'task_bundle': bundle_ref,
                        'baseline': tree_ref(self.base), 'always_on_max': 12000, 'training_feedback': config}]}
        seen = []
        def native(adapter, *args, **kwargs):
            seen.append(adapter._training_feedback)
            return {'fixture_only': True}
        with (patch('experiment_driver.PinnedCLI'), patch('native_adapter.run_native_task', side_effect=native),
              patch.object(subprocess, 'run', side_effect=AssertionError('no process'))):
            run_experiment(experiment, lambda *_: self.fail('no review'))
        self.assertIsInstance(seen[0], DrawingFeedback)
        self.assertEqual(seen[0].output, Path(experiment['output_root']) / 'quality-feedback' / 'codex')
        # Both providers must use the same frozen executable, engine and bound.
        experiment['output_root'] = str(self.root / 'mismatched-groups')
        pi = copy.deepcopy(experiment['groups'][0])
        pi['route'].update(group='pi', provider='pi', model=MODELS['pi'])
        pi['training_feedback']['timeout_s'] += 1
        experiment['groups'].append(pi)
        experiment['billing'] = {'environment_file': str(self.root / 'never-read'), 'key_name': 'DUMMY',
            'timeout_s': 1, 'exclusive_key': True, 'observations': 1, 'interval_s': 0}
        with (patch('experiment_driver.PinnedCLI'), patch('experiment_driver.ReadOnlyOpenRouter'),
              patch('experiment_driver.OpenRouterBilling'), patch('native_adapter.run_native_task') as native,
              self.assertRaisesRegex(Refused, 'different fixed quality checkers')):
            run_experiment(experiment, lambda *_: self.fail('no review'))
        native.assert_not_called()


if __name__ == '__main__':
    unittest.main()
