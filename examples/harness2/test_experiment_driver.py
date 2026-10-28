"""Public preparation controls do not import/execute a model or owner transport."""
import sys
import tempfile
import types
import unittest
from pathlib import Path
from unittest.mock import patch

from experiment_driver import (
    journal_elapsed,
    ordered_calls,
    prepare_editable_harness,
    prepare_public_task_bundle,
    public_tree,
    run_experiment,
)
from owner_contract import Refused, tree_ref
from soodles_bridge import decode, reference, save


class PublicBundleTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        self.component = self.root / 'component.html'
        self.component.write_text('<svg>accepted original model output</svg>')
        self.geometry = self.root / 'geometry.json'
        self.geometry.write_text('{"measurement_fixture":true}')
        self.candidate = save(self.root / 'candidate.json', {'head': 'b' * 40, 'files': {
            'component.html': {'sha256': reference(self.component)['sha256'], 'executable': False}}})
        self.accepted = save(self.root / 'accepted.json', {'owner': 'soodles.loop', 'inner': {
            'owner': 'issue-execution.local-task', 'candidate': self.candidate, 'acceptance': 'PASS',
            'review_progress': {'status': 'locally_integrated', 'review': {
                'decision': 'approve', 'candidate': self.candidate}}}})

    def prepare(self, **changes):
        args = {'destination': self.root / 'bundle', 'task_id': 'component', 'prompt': 'Draw the accepted component.',
                'artifacts': {'component.html': {'source': reference(self.component), 'candidate_path': 'component.html'}},
                'measurements': {'geometry.json': reference(self.geometry)}, 'owner_acceptance': self.accepted,
                'provenance': (self.accepted,)}
        args.update(changes)
        return prepare_public_task_bundle(**args)

    def test_elapsed_uses_original_journal_start_and_includes_reconciliation_wait(self):
        state = {'calls': {'baseline': {'call': {'group': 'codex'}, 'state': 'settled',
                 'started_unix_s': 100, 'settled_unix_s': 120},
                 'proposer': {'call': {'group': 'codex'}, 'state': 'settled',
                 'started_unix_s': 125, 'settled_unix_s': 300}}}
        self.assertEqual(journal_elapsed(state), 200)
        state['calls'] = dict(reversed(list(state['calls'].items())))
        self.assertEqual(journal_elapsed(state), 200)
        self.assertEqual(ordered_calls(state)[-1]['started_unix_s'], 125)
        state['calls']['proposer']['state'] = 'unknown'
        self.assertIsNone(journal_elapsed(state))
        state['calls']['proposer']['state'] = 'settled'
        del state['calls']['proposer']['settled_unix_s']
        self.assertIsNone(journal_elapsed(state))

    def test_fixed_accepted_artifact_and_geometry_only_enter_model_source(self):
        with patch('subprocess.run', side_effect=AssertionError('no owner/model process')):
            ref = self.prepare()
        bundle = decode(Path(ref['path']).read_bytes())
        source = Path(bundle['source']['path'])
        self.assertEqual({p.name for p in source.iterdir()}, {'component.html', 'geometry.json'})
        self.assertEqual((source / 'component.html').read_bytes(), self.component.read_bytes())
        provenance = decode(Path(bundle['provenance']['path']).read_bytes())
        self.assertEqual(provenance['candidate_head'], 'b' * 40)
        self.assertFalse(provenance['stage_a_regenerated_per_solver'])
        public_tree(bundle['source'])

    def test_entry_prevalidates_both_groups_and_shares_original_gate(self):
        import reproduce
        bundle = self.prepare()
        upstream = self.root / 'upstream-fixture'
        upstream.mkdir()
        (upstream / 'schedule.py').write_text('# fixture source; no optimizer executes in this entry test')
        prepared = save(self.root / 'prepared.json', {'source_pin': reproduce.PIN,
                         'source_tree_sha256': reproduce.fingerprint(upstream)})
        baseline = self.root / 'baseline'
        baseline.mkdir()
        (baseline / 'AGENTS.md').write_text('Use the drawing skill.')
        script = self.root / 'soodles.py'
        script.write_text('# fixture entrypoint, never executed')
        calls = []
        native = types.ModuleType('native_adapter')
        native.Route = lambda **kwargs: types.SimpleNamespace(**kwargs)
        native.Task = lambda prompt, source: (prompt, source)
        native.NoodleAdapter = lambda **kwargs: types.SimpleNamespace(**kwargs)

        def run(adapter, task_id, **kwargs):
            calls.append((adapter.gate, adapter.route.provider, task_id))
            return {'entry_fixture_only': True}

        native.run_native_task = run
        schedule = types.ModuleType('harness2.core.schedule')
        schedule.__file__ = str(upstream / 'schedule.py')
        core = types.ModuleType('harness2.core')
        core.schedule = schedule
        package = types.ModuleType('harness2')
        package.core = core
        config = {'upstream': {'path': str(upstream), 'prepared_receipt': prepared},
            'output_root': str(self.root / 'experiment'), 'experiment_id': 'fixture',
            'initial_paid_spent_usd': '0.087106123', 'poll_s': 1, 'templates': [],
            'billing': {'environment_file': str(self.root / 'private-fixture.env'), 'key_name': 'OPENROUTER_API_KEY',
                        'timeout_s': 30, 'exclusive_key': True, 'observations': 1, 'interval_s': 0},
            'cli': {'python': reference(Path(sys.executable).resolve()), 'entrypoint': reference(script),
                    'sources': [reference(script)], 'source_commit': 'e' * 40, 'environment': {}, 'timeout_s': 30},
            'groups': [{'route': {'group': provider, 'provider': provider, 'model': model, 'effort': 'low', 'timeout_s': 60},
                        'task_bundle': bundle, 'baseline': tree_ref(baseline), 'always_on_max': 12000}
                       for provider, model in [('codex', 'gpt-5.6-sol'),
                           ('pi', 'openrouter/deepseek/deepseek-v4.1-flash-1017')]]}
        with patch.dict(sys.modules, {'native_adapter': native, 'harness2': package,
                                      'harness2.core': core, 'harness2.core.schedule': schedule}):
            receipt = run_experiment(config, lambda *_: self.fail('entry fixture cannot request real review'))
            self.assertIs(calls[0][0], calls[1][0])
            self.assertEqual([item[1] for item in calls], ['codex', 'pi'])
            value = decode(Path(receipt['path']).read_bytes())
            self.assertIsNone(value['whole_workflow_cost_usd'])
            self.assertFalse(value['adoption_authorized'])
            calls.clear()
            config['output_root'] = str(self.root / 'bad-experiment')
            config['groups'][1]['route']['model'] = 'wrong'
            with self.assertRaises(Refused):
                run_experiment(config, lambda *_: None)
            self.assertEqual(calls, [])

    def test_unaccepted_or_changed_artifact_refuses(self):
        value = decode(Path(self.accepted['path']).read_bytes())
        value['inner']['acceptance'] = 'UNKNOWN'
        bad = save(self.root / 'not-accepted.json', value)
        with self.assertRaises(Refused):
            self.prepare(owner_acceptance=bad)
        self.assertFalse((self.root / 'bundle').exists())
        self.component.write_text('different component')
        with self.assertRaises(Refused):
            self.prepare()

    def test_private_source_is_not_read_or_copied(self):
        private = self.root / '.env'
        private.write_text('fixture-secret')
        # Computing this fixture ref is setup; the public copier must refuse before read.
        secret_ref = reference(private)
        with patch('experiment_driver.read_ref', wraps=__import__('experiment_driver').read_ref) as reader:
            with self.assertRaisesRegex(Refused, 'private destination'):
                self.prepare(measurements={'geometry.json': secret_ref})
        self.assertNotIn(secret_ref, [call.args[0] for call in reader.call_args_list])
        self.assertFalse((self.root / 'bundle/source/geometry.json').exists())

    def test_native_editable_harness_does_not_make_assets_editable(self):
        skill = self.root / 'drawing'
        skill.mkdir()
        (skill / 'SKILL.md').write_text('Draw measured geometry.')
        (skill / 'examples').mkdir()
        (skill / 'examples/component.html').write_text('<svg>immutable example</svg>')
        agents = self.root / 'AGENTS.md'
        agents.write_text('Use the drawing skill.')
        ref = prepare_editable_harness(destination=self.root / 'editable', agents=reference(agents),
                                      skill_bases={'drawing': tree_ref(skill)})
        path = Path(ref['path'])
        self.assertTrue((path / 'skills/drawing/SKILL.md').is_file())
        self.assertFalse((path / 'skills/drawing/examples').exists())
        (skill / '.env').write_text('fixture-secret')
        with self.assertRaises(Refused):
            prepare_editable_harness(destination=self.root / 'bad-editable', agents=reference(agents),
                                     skill_bases={'drawing': tree_ref(skill)})


if __name__ == '__main__':
    unittest.main()
