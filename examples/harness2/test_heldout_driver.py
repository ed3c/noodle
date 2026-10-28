"""Offline fixed-pair controls; fabricated results are never live acceptance."""

import copy
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from heldout_driver import preflight_pair, run_pair
from owner_contract import OwnerCall, Refused, UnknownEffect, tree_ref
from soodles_bridge import decode, reference, save
from test_native_contract import FakeOwner


class HeldoutCallerTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        source = self.root / "source"
        source.mkdir()
        (source / "component.html").write_text("fixture accepted component")
        (source / "geometry.json").write_text('{"fixture_only":true}')
        candidate = save(self.root / "a-candidate.json", {"fixture_only": True})
        acceptance = save(self.root / "a-accepted.json", {"owner": "soodles.loop", "inner": {
            "owner": "issue-execution.local-task", "acceptance": "PASS",
            "review_progress": {"status": "locally_integrated", "review": {
                "decision": "approve", "candidate": candidate}}}})
        producer = save(self.root / "a-request.json", {"fixture_only": True})
        provenance = save(self.root / "provenance.json", {
            "source": tree_ref(source), "owner_acceptance": acceptance, "candidate": candidate,
            "stage_a_regenerated_per_solver": False, "provenance": [producer],
            "artifacts": {"component.html": {"candidate_path": "component.html"}},
        })
        bundle = save(self.root / "task.json", {"task_id": "heldout-1", "prompt": "Draw the fixed component.",
            "source": tree_ref(source), "provenance": provenance})
        self.harnesses = {}
        rows = {}
        for variant in ("baseline", "candidate"):
            harness = self.root / variant
            (harness / "skills/main").mkdir(parents=True)
            (harness / "AGENTS.md").write_text("Use the main skill. " + variant)
            (harness / "skills/main/SKILL.md").write_text(
                "---\nname: main\ndescription: Draw the accepted source\n---\nUse measured geometry.\n")
            self.harnesses[variant] = tree_ref(harness)
            call = OwnerCall("codex", variant, "solver", "codex", "gpt-5.6-sol",
                             {"task_id": "training", "harness": tree_ref(harness)})
            rows[variant] = {"state": "settled", "call": call.__dict__, "input_sha256": call.fingerprint()}
        selection = {"group": "codex", "journal": save(self.root / "training-journal.json", {"calls": rows}),
                     "summary": save(self.root / "summary.json", {"status": "ok", "k": 1,
                         "task_id": "training", "base": [{"run_id": "baseline"}],
                         "selections": {"seq": {"ok": True, "chosen_run_id": "candidate"}}})}
        cli_file = self.root / "soodles.py"
        cli_file.write_text("# fixture; this process is never invoked")
        profile = save(self.root / "profile.json", {"fixture_only": True})
        slots = []
        for index, variant in enumerate(("candidate", "baseline")):
            control = self.root / f"control-{index}"
            control.mkdir()
            definition = save(self.root / f"composition-{index}.json", {
                "schema": 1, "sources": [{"alias": "fixture", "root": str(self.root), "commit": "f" * 40}],
                "compositions": [{"id": "drawing", "stages": [{"id": "draw", "runtime": "openshell",
                    "provider": "codex", "model": "gpt-5.6-sol", "model_reasoning_effort": "low", "profile": profile,
                    "selected_skill": {"source": "fixture", "entry": variant + "/skills/main/SKILL.md", "alias": "main"},
                    "support_skills": []}]}],
            })
            slots.append({"run_id": f"slot-{index}", "variant": variant, "template": {
                "group": "codex", "role": "solver", "repository": str(control),
                "selected_skill": None, "support_skills": [], "noodle": reference(cli_file),
                "profile": profile, "write_paths": ["drawing.html"], "acceptance": ["Exact safe delivery"],
                "deliverables": ["drawing.html"], "harness_skills": ["main"],
                "skill_bases": {"main": tree_ref(self.root / "baseline/skills/main")},
                "composition": {"definition": definition, "id": "drawing"},
                "producer_handoff": {"request": producer, "artifact_path": "component.html"},
            }})
        self.config = {"schema_version": 1, "status": "ready", "route": {"group": "codex", "provider": "codex",
            "model": "gpt-5.6-sol", "effort": "low", "timeout_s": 3600}, "selection": selection,
            "task_bundle": bundle, "slots": slots, "host_directory": str(self.root / "original-host"),
            "output_root": str(self.root / "pair"), "initial_paid_spent_usd": "0.12", "poll_s": 2,
            "always_on_max": 12000, "upstream": {"fixture_only": True},
            "cli": {"python": reference(Path(sys.executable).resolve()), "entrypoint": reference(cli_file),
                "sources": [reference(cli_file)], "source_commit": "f" * 40, "timeout_s": 120,
                "environment": {"NOODLE_HOST_DIR": str(self.root / "original-host")}},
            "billing": {"environment_file": str(self.root / "not-opened.env"), "key_name": "OPENROUTER_API_KEY",
                "timeout_s": 30, "exclusive_key": True, "observations": 3, "interval_s": 1}}
        for target in ("subprocess.run", "subprocess.Popen", "socket.socket.connect"):
            guard = patch(target, side_effect=AssertionError("no process/network in offline controls"))
            guard.start()
            self.addCleanup(guard.stop)

    def test_preflight_keeps_selection_and_host_and_does_not_read_credentials(self):
        _, bundle, chosen, templates = preflight_pair(self.config)
        self.assertEqual(chosen, self.harnesses)
        self.assertEqual(bundle.task_id, "heldout-1")
        self.assertNotEqual(templates[0].repository, templates[1].repository)
        self.assertFalse(Path(self.config["output_root"]).exists())

    def test_pair_uses_native_adapter_and_shared_gate_but_no_optimizer(self):
        owner = FakeOwner(self.root / "fixture-owner")
        with patch("heldout_driver.load_native_judge"), patch("heldout_driver.SoodlesOwnerBridge", return_value=owner), \
                patch("harness2.core.schedule.run_task", side_effect=AssertionError("heldout cannot optimize")):
            receipt = run_pair(self.config, lambda *_: self.fail("fake owner has no Session review"))
        self.assertEqual([c.role for c in owner.calls], ["solver", "solver"])
        self.assertEqual([c.run_id for c in owner.calls], ["slot-0", "slot-1"])
        self.assertEqual(owner.calls[0].inputs["harness"], self.harnesses["candidate"])
        self.assertEqual(owner.calls[1].inputs["harness"], self.harnesses["baseline"])
        for field in ("source", "prompt", "effort", "timeout_s"):
            self.assertEqual(owner.calls[0].inputs[field], owner.calls[1].inputs[field])
        journal = decode((self.root / "pair/owner-journal.json").read_bytes())
        self.assertEqual(journal["initial_paid_spent_usd"], "0.12")
        self.assertTrue(all(row["result"]["cost_usd"] is None for row in journal["calls"].values()))
        self.assertEqual(decode(Path(receipt["path"]).read_bytes())["quality"], "not evaluated")

    def test_unknown_first_slot_blocks_second_and_keeps_original_journal(self):
        owner = FakeOwner(self.root / "fixture-owner")
        with patch("heldout_driver.load_native_judge"), patch("heldout_driver.SoodlesOwnerBridge", return_value=owner), \
                patch.object(owner, "execute", side_effect=TimeoutError("fixture unknown effect")) as execute:
            with self.assertRaises(UnknownEffect):
                run_pair(self.config, lambda *_: None)
        self.assertEqual(execute.call_count, 1)
        journal = decode((self.root / "pair/owner-journal.json").read_bytes())
        self.assertEqual([row["state"] for row in journal["calls"].values()], ["unknown"])
        with self.assertRaisesRegex(Refused, "fresh canonical"):
            preflight_pair(self.config)

    def test_environment_drift_refuses_before_any_dispatch(self):
        for field in ("profile", "noodle", "skill_bases", "acceptance", "producer_handoff"):
            value = copy.deepcopy(self.config)
            if field in {"profile", "noodle"}:
                value["slots"][1]["template"][field] = save(self.root / f"different-{field}.json", {"different": True})
            elif field == "skill_bases":
                value["slots"][1]["template"][field] = {"main": tree_ref(self.root / "candidate/skills/main")}
            elif field == "acceptance":
                value["slots"][1]["template"][field] = ["Different acceptance"]
            else:
                value["slots"][1]["template"][field]["artifact_path"] = "other.html"
            with self.subTest(field=field), self.assertRaises(Refused):
                preflight_pair(value)

    def test_missing_host_reused_control_or_unselected_harness_refuses(self):
        value = copy.deepcopy(self.config)
        value["cli"]["environment"] = {}
        with self.assertRaisesRegex(Refused, "Host locator"):
            preflight_pair(value)
        value = copy.deepcopy(self.config)
        value["slots"][1]["template"]["repository"] = value["slots"][0]["template"]["repository"]
        with self.assertRaisesRegex(Refused, "independent"):
            preflight_pair(value)
        (self.root / "control-1/.noodle").mkdir()
        with self.assertRaisesRegex(Refused, "already has an owner"):
            preflight_pair(self.config)
        (self.root / "control-1/.noodle").rmdir()
        (self.root / "candidate/AGENTS.md").write_text("Changed after native selection")
        with self.assertRaisesRegex(Refused, "native selected harness changed"):
            preflight_pair(self.config)

    def test_draft_and_unknown_acceptance_refuse_without_owner(self):
        draft = copy.deepcopy(self.config)
        draft["status"] = "draft"
        with self.assertRaisesRegex(Refused, "not ready"):
            preflight_pair(draft)
        bundle = decode(Path(self.config["task_bundle"]["path"]).read_bytes())
        provenance = decode(Path(bundle["provenance"]["path"]).read_bytes())
        accepted = decode(Path(provenance["owner_acceptance"]["path"]).read_bytes())
        accepted["inner"]["acceptance"] = "UNKNOWN"
        provenance["owner_acceptance"] = save(self.root / "unknown-accepted.json", accepted)
        bundle["provenance"] = save(self.root / "unknown-provenance.json", provenance)
        self.config["task_bundle"] = save(self.root / "unknown-task.json", bundle)
        with self.assertRaisesRegex(Refused, "integrated acceptance"):
            preflight_pair(self.config)

    def test_second_catalog_drift_refuses_before_first_owner(self):
        template = self.config["slots"][1]["template"]
        catalog = decode(Path(template["composition"]["definition"]["path"]).read_bytes())
        catalog["compositions"][0]["stages"][0]["model_reasoning_effort"] = "high"
        template["composition"]["definition"] = save(self.root / "bad-catalog.json", catalog)
        with self.assertRaisesRegex(Refused, "fixed route or profile"):
            preflight_pair(self.config)
        self.assertFalse((self.root / "pair").exists())


if __name__ == "__main__":
    unittest.main()
