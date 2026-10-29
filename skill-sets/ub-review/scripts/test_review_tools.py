import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
from concurrent.futures import ThreadPoolExecutor
import subprocess


def load(name, filename):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name(filename))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


companion = load("companion", "uml-companion.py")
snapshot = load("snapshot", "review-snapshot.py")
turns = load("turns", "companion_turn.py")


class ReviewToolsTests(unittest.TestCase):
    def turn_fixture(self, root):
        root = root.resolve()
        workspace = root / 'workspace'
        workspace.mkdir()
        skill = root / 'set'
        (skill / 'skills/ub-review').mkdir(parents=True)
        (skill / 'skills/ub-review/companion-rules.md').write_text('fixture')
        source = root / 'context.json'
        source.write_text(json.dumps({'schema': 1, 'kind': 'outer_companion_context'}))
        return workspace, skill, source

    def test_native_response_and_interrupted_result_remain_distinct(self):
        native = {'sessionId': 'native', 'stopReason': 'end_turn', 'structuredOutput': {
            'summary': 'missing evidence', 'requests': [{'kind': 'evidence', 'command_ids': [], 'reason': 'missing input',
                                                        'requested_outcome': 'read original source'}]}}
        self.assertEqual(turns.response(json.dumps(native))[1], native['structuredOutput'])
        native['structuredOutput']['requests'][0]['command_ids'] = [42]
        with self.assertRaisesRegex(ValueError, 'invented a Viewer'):
            turns.bound_response(json.dumps(native), {'viewer_mailbox': {'command_ids': []}}, 'native')
        for changed in ({**native, 'stopReason': 'max_turns'},
                        {**native, 'is_error': True},
                        {**native, 'structuredOutput': {'summary': 'invalid', 'requests': [{'command': 'run arbitrary code'}]}}):
            with self.assertRaises(ValueError):
                turns.response(json.dumps(changed))

    def test_same_context_and_concurrent_delivery_launch_only_one_native_turn(self):
        with tempfile.TemporaryDirectory() as raw:
            workspace, skill, source = self.turn_fixture(Path(raw))
            def invoke(argv, **kwargs):
                native = {'sessionId': argv[argv.index('--session-id') + 1], 'stopReason': 'end_turn',
                          'structuredOutput': {'summary': 'missing evidence', 'requests': [{
                              'kind': 'evidence', 'command_ids': [], 'reason': 'missing fixture source',
                              'requested_outcome': 'read original source'}]}}
                kwargs['stdout'].write(json.dumps(native).encode())
                return subprocess.CompletedProcess(argv, 0)
            with patch.object(companion, 'paths', return_value=(workspace, workspace)), \
                 patch.object(companion, 'SET_ROOT', skill), \
                 patch.object(companion, 'probe', return_value={'enforced': True}), \
                 patch.object(companion, 'command', side_effect=lambda w, h, a: a), \
                 patch.object(turns.subprocess, 'run', side_effect=invoke) as run:
                with ThreadPoolExecutor(max_workers=2) as pool:
                    results = list(pool.map(lambda _: turns.turn(companion, source), range(2)))
            self.assertEqual(run.call_count, 1)
            self.assertTrue(all(result['status'] == 'observed' for result in results))
            self.assertEqual(results[0]['session_id'], results[1]['session_id'])

    def test_timeout_preserves_intent_and_never_replays_the_model(self):
        with tempfile.TemporaryDirectory() as raw:
            workspace, skill, source = self.turn_fixture(Path(raw))
            with patch.object(companion, 'paths', return_value=(workspace, workspace)), \
                 patch.object(companion, 'SET_ROOT', skill), \
                 patch.object(companion, 'probe', return_value={'enforced': True}), \
                 patch.object(companion, 'command', side_effect=lambda w, h, a: a), \
                 patch.object(turns.subprocess, 'run', side_effect=subprocess.TimeoutExpired('grok', 180)) as run:
                with self.assertRaises(subprocess.TimeoutExpired):
                    turns.turn(companion, source)
                observed = turns.turn(companion, source)
            self.assertEqual(observed['status'], 'unknown')
            self.assertEqual(run.call_count, 1)
            self.assertTrue((Path(observed['capture_directory']) / 'intent.json').is_file())

    def test_product_checkout_is_not_a_companion_workspace(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            (root / ".git").mkdir()
            with patch.dict("os.environ", {"NEO_UML_WORKSPACE": raw}):
                with self.assertRaisesRegex(ValueError, "dedicated diagram"):
                    companion.paths()

    def test_writable_directory_symlink_is_refused(self):
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            (root / "inputs").mkdir()
            (root / "inputs/review-context.json").write_text("{}")
            (root / "outside").mkdir()
            (root / "proposals").symlink_to(root / "outside", target_is_directory=True)
            with patch.dict("os.environ", {"NEO_UML_WORKSPACE": raw}), patch.object(companion, "SET_ROOT", root):
                with self.assertRaisesRegex(ValueError, "symlink"):
                    companion.paths()

    def test_projection_rejects_dangling_edges(self):
        graph = {"schema": 1, "analyzer": "noodle-go-types-v1", "nodes": [],
                 "edges": [{"from": "missing", "to": "missing", "kind": "static_call"}], "unknowns": []}
        with self.assertRaisesRegex(ValueError, "missing source node"):
            snapshot.diagram(graph)

    def test_projection_cannot_inject_edn_or_open_product_source(self):
        graph = {"schema": 1, "analyzer": "noodle-go-types-v1", "unknowns": [{"reason": "interface"}],
                 "nodes": [{"id": "pkg", "kind": "package", "package": "p"},
                           {"id": "file", "kind": "file", "package": "p", "name": '\"} :file \"/secret', "file": "/product/source.go"}],
                 "edges": [{"from": "pkg", "to": "file", "kind": "contains"}]}
        diagram = snapshot.diagram(graph)
        item = diagram["packages"][0]["classes"][0]
        self.assertNotIn("file", item)
        self.assertIn("1 unresolved", diagram["title"])
        self.assertEqual(snapshot.edn(item["name"]), json.dumps(item["name"], ensure_ascii=False))


if __name__ == "__main__":
    unittest.main()
