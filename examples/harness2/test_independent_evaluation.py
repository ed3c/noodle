"""Offline external judge controls: fabricated reports are labeled fixtures, not real measurements."""

import copy
import json
import tempfile
import unittest
from dataclasses import asdict
from pathlib import Path
from unittest.mock import patch

from harness2.core import judge
from independent_evaluation import (
    ReviewedRun,
    accounting,
    aggregate_pairs,
    efficiency_decision,
    external_stage_bill,
    judge_pair,
    load_native_judge,
    workflow_timing,
)
from owner_contract import OwnerCall, OwnerResult, Refused, tree_ref
from soodles_bridge import reference, save


class IndependentTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        self.source = self.root / "frozen-public-source"
        self.source.mkdir()
        (self.source / "component.html").write_text("fixture accepted source")
        self.rubric = save(
            self.root / "rubric.json",
            {
                "blocking_gates": [{"id": k} for k in ("owner", "legibility")],
                "quality_vectors": {"dimensions": ["accuracy", "readability"]},
            },
        )
        self.evidence = save(
            self.root / "observation.json",
            {"fixture_only": True, "measurement": "not a real browser observation"},
        )
        self.rows = {}
        self.reports = {}
        self.templates = {}
        self.profile = save(self.root / "profile.json", {"fixture_only": True})
        self.noodle = save(self.root / "noodle.json", {"fixture_only": True})
        self.skill_base = self.root / "fixed-assets"
        self.skill_base.mkdir()
        (self.skill_base / "example.html").write_text("fixed fixture asset")
        for label in ("baseline", "candidate"):
            call_root = self.root / label
            run = call_root / "result"
            (run / "workspace").mkdir(parents=True)
            (run / "workspace/drawing.html").write_text("<svg>fixture drawing " + label + "</svg>")
            harness = self.root / (label + "-harness")
            harness.mkdir()
            (harness / "AGENTS.md").write_text(label + " frozen instructions")
            call = OwnerCall(
                "codex",
                label,
                "solver",
                "codex",
                "gpt-5.6-sol",
                {
                    "task_id": "public-case",
                    "effort": "low",
                    "timeout_s": 60,
                    "source": tree_ref(self.source),
                    "harness": tree_ref(harness),
                },
            )
            template = {
                "noodle": self.noodle,
                "profile": self.profile,
                "repository": str(call_root / "control"),
                "harness_skills": ["main"],
                "skill_bases": {"main": tree_ref(self.skill_base)},
                "write_paths": ["drawing.html"],
                "acceptance": ["Deliver exact bytes."],
                "deliverables": ["drawing.html"],
            }
            self.templates[label] = template
            save(call_root / "binding.json", {"call_sha256": call.fingerprint(), "template": template})
            request = save(
                call_root / "request.json",
                {"noodle": self.noodle["path"], "profile": self.profile["path"], "repository": template["repository"]},
            )
            (call_root / "operation-0000").mkdir()
            save(
                call_root / "operation-0000/intent.json",
                {
                    "request": request,
                    "source_commit": "f" * 40,
                    "python": self.noodle,
                    "sources": [self.noodle],
                    "argv": [self.noodle["path"], "-B", self.noodle["path"], "loop", "enter"],
                },
            )
            envelope = save(
                call_root / "envelope.json",
                {
                    "runtime": "openshell",
                    "provider": call.provider,
                    "model": call.model,
                    "model_reasoning_effort": "low",
                    "profile": self.profile,
                    "noodle": self.noodle,
                },
            )
            candidate = save(self.root / (label + "-candidate.json"), {"task_envelope": envelope})
            accepted = save(
                self.root / (label + "-accepted.json"),
                {
                    "owner": "soodles.loop",
                    "inner": {
                        "owner": "issue-execution.local-task",
                        "acceptance": "PASS",
                        "review_progress": {
                            "status": "locally_integrated",
                            "review": {"decision": "approve", "candidate": candidate},
                        },
                    },
                },
            )
            receipt = save(
                call_root / "settled.json",
                {
                    "call_sha256": call.fingerprint(),
                    "original_review": accepted,
                    "candidate": candidate,
                    "request": request,
                },
            )
            result = OwnerResult(
                str(run),
                tree_ref(run)["sha256"],
                "fixture",
                None,
                receipt["path"],
                receipt["sha256"],
                True,
                None,
                None,
            )
            self.rows[label] = {
                "call": asdict(call),
                "state": "settled",
                "input_sha256": call.fingerprint(),
                "result": asdict(result),
                "elapsed_s": 5,
            }
            report = {
                "schema_version": 1,
                "authority": "independent-evaluator",
                "task_id": "public-case",
                "rubric": self.rubric,
                "source": tree_ref(self.source),
                "artifact": reference(run / "workspace/drawing.html"),
                "conditions": [
                    {
                        "id": k,
                        "status": "PASS",
                        "evidence": [self.evidence],
                        "reason": "fixture assessment",
                    }
                    for k in ("owner", "legibility")
                ],
                "quality": [
                    {
                        "dimension": k,
                        "score": 2,
                        "evidence": [self.evidence],
                        "reason": "fixture assessment",
                    }
                    for k in ("accuracy", "readability")
                ],
            }
            self.reports[label] = report
        self.journal = save(
            self.root / "journal.json",
            {
                "paid_limit_usd": "5",
                "initial_paid_spent_usd": "0.09",
                "calls": self.rows,
            },
        )
        self.summary = save(
            self.root / "summary.json",
            {
                "status": "ok",
                "task_id": "public-case",
                "k": 1,
                "base": [{"run_id": "baseline"}],
                "selections": {"seq": {"ok": True, "chosen_run_id": "candidate"}},
            },
        )
        self.selection = {
            "summary": self.summary,
            "journal": self.journal,
            "group": "codex",
        }

    def compare(self, suffix):
        bindings = [
            ReviewedRun(
                self.journal,
                "codex",
                name,
                save(self.root / (name + suffix + ".json"), self.reports[name]),
            )
            for name in ("baseline", "candidate")
        ]
        return judge_pair(
            rubric=self.rubric,
            baseline=bindings[0],
            candidate=bindings[1],
            selection=self.selection,
            native_judge=judge,
        )

    def change_candidate_environment(self, kind):
        root = self.root / "candidate"
        row = self.rows["candidate"]
        binding = json.loads((root / "binding.json").read_text())
        request = json.loads((root / "request.json").read_text())
        envelope = json.loads((root / "envelope.json").read_text())
        intent = json.loads((root / "operation-0000/intent.json").read_text())
        receipt_path = Path(row["result"]["receipt_path"])
        receipt = json.loads(receipt_path.read_text())
        candidate_path = Path(receipt["candidate"]["path"])
        candidate = json.loads(candidate_path.read_text())
        accepted_path = Path(receipt["original_review"]["path"])
        accepted = json.loads(accepted_path.read_text())
        if kind in {"profile", "noodle"}:
            changed = save(self.root / ("different-" + kind + ".json"), {"different_fixture": kind})
            binding["template"][kind] = changed
            request[kind] = changed["path"]
            envelope[kind] = changed
        elif kind == "timeout":
            row["call"]["inputs"]["timeout_s"] = 120
            fingerprint = OwnerCall(**row["call"]).fingerprint()
            row["input_sha256"] = binding["call_sha256"] = receipt["call_sha256"] = fingerprint
        elif kind == "fixed_assets":
            changed = self.root / "different-assets"
            changed.mkdir()
            (changed / "example.html").write_text("different immutable fixture")
            binding["template"]["skill_bases"]["main"] = tree_ref(changed)
        else:
            intent["source_commit"] = "a" * 40
        (root / "binding.json").write_text(json.dumps(binding))
        (root / "request.json").write_text(json.dumps(request))
        (root / "envelope.json").write_text(json.dumps(envelope))
        intent["request"] = receipt["request"] = reference(root / "request.json")
        (root / "operation-0000/intent.json").write_text(json.dumps(intent))
        candidate["task_envelope"] = reference(root / "envelope.json")
        candidate_path.write_text(json.dumps(candidate))
        receipt["candidate"] = reference(candidate_path)
        accepted["inner"]["review_progress"]["review"]["candidate"] = receipt["candidate"]
        accepted_path.write_text(json.dumps(accepted))
        receipt["original_review"] = reference(accepted_path)
        receipt_path.write_text(json.dumps(receipt))
        row["result"]["receipt_sha256"] = reference(receipt_path)["sha256"]
        self.journal = save(self.root / ("journal-" + kind + ".json"), {"paid_limit_usd": "5", "calls": self.rows})
        self.selection["journal"] = self.journal

    def test_pair_requires_same_original_execution_environment(self):
        self.assertEqual(self.compare("normal")["quality_non_regression"], "PASS")
        for kind in ("profile", "noodle", "timeout", "fixed_assets", "cli_source"):
            with self.subTest(kind=kind):
                fixture = IndependentTests()
                fixture.setUp()
                self.addCleanup(fixture.doCleanups)
                fixture.change_candidate_environment(kind)
                with self.assertRaisesRegex(Refused, "paired execution environment differs"):
                    fixture.compare("changed-environment")

    def test_native_judge_loader_requires_the_original_prepared_source(self):
        import reproduce

        root = Path(judge.__file__).resolve().parents[2]
        prepared = save(
            self.root / "prepared.json",
            {"source_pin": reproduce.PIN, "source_tree_sha256": reproduce.fingerprint(root)},
        )
        self.assertIs(load_native_judge({"path": str(root), "prepared_receipt": prepared}), judge)
        changed = save(
            self.root / "wrong-prepared.json", {"source_pin": {}, "source_tree_sha256": reproduce.fingerprint(root)}
        )
        with self.assertRaises(Refused):
            load_native_judge({"path": str(root), "prepared_receipt": changed})

    def test_native_judge_keeps_delivery_pass_separate_from_clipped_quality_failure(
        self,
    ):
        self.reports["baseline"]["conditions"][1]["status"] = "FAIL"
        result = self.compare("fail")
        self.assertEqual(result["quality_non_regression"], "FAIL")
        self.assertTrue(result["native_judgments"][0]["n_criteria"] > 0)
        self.assertEqual(result["native_judgments"][0]["n_passed"], 1)
        self.assertFalse(result["native_judgments"][0]["all_pass"])
        self.assertEqual(efficiency_decision("FAIL", "1", "0.1", 10, 1), "quality_blocked")

    def test_quality_regression_and_unknown_do_not_become_improvement(self):
        self.reports["candidate"]["quality"][0]["score"] = 1
        self.assertEqual(self.compare("regression")["quality_non_regression"], "FAIL")
        self.reports["candidate"]["quality"][0]["score"] = None
        result = self.compare("unknown")
        self.assertEqual(result["quality_non_regression"], "UNKNOWN")
        self.assertIsNone(result["native_judgments"][1]["n_criteria"])
        self.assertEqual(efficiency_decision("PASS", None, "0.01", 10, 1), "inconclusive")
        self.assertEqual(efficiency_decision("PASS", "1", "0.5", 10, 11), "tradeoff_or_regression")
        self.assertEqual(efficiency_decision("PASS", "1", "0.5", 10, 9), "pareto_improvement")

    def test_changed_artifact_and_unselected_candidate_refuse(self):
        artifact = Path(self.reports["candidate"]["artifact"]["path"])
        artifact.write_text("changed")
        with self.assertRaises(Refused):
            self.compare("changed")

    def test_final_selection_not_intermediate_candidate_is_required(self):
        self.selection["summary"] = save(
            self.root / "wrong-summary.json",
            {
                "status": "ok",
                "task_id": "public-case",
                "k": 1,
                "base": [{"run_id": "baseline"}],
                "selections": {"seq": {"ok": True, "chosen_run_id": "not-run"}},
            },
        )
        with patch(
            "independent_evaluation.review_report", side_effect=AssertionError("heldout report read before freeze")
        ):
            with self.assertRaises(Refused):
                self.compare("unselected")

    def bill(self, name="stage-a"):
        raw = self.root / (name + "-raw.jsonl")
        raw.write_text(
            json.dumps(
                {
                    "type": "message",
                    "message": {
                        "role": "assistant",
                        "provider": "openrouter",
                        "model": "deepseek/deepseek-v4.1-flash-1017",
                        "responseId": "gen-fixture",
                    },
                }
            )
            + "\n"
        )
        self.descriptor = save(
            self.root / (name + "-descriptor.json"),
            {
                "provider": "pi",
                "terminal": {"line": 1},
                "gaps": [],
                "raw": reference(raw),
            },
        )
        data = {
            "limit": "5",
            "usage": "0.102",
            "limit_remaining": "4.898",
            "limit_reset": None,
            "is_management_key": False,
        }
        key = {"resource": "key", "key_fingerprint": "b" * 64, "data": data}
        generation = {
            "resource": "generation",
            "generation_id": "gen-fixture",
            "key_fingerprint": "b" * 64,
            "data": {
                "id": "gen-fixture",
                "model": "deepseek/deepseek-v4.1-flash-20261017",
                "total_cost": "0.002",
                "is_byok": False,
                "finish_reason": "stop",
                "cancelled": False,
            },
        }
        return save(
            self.root / (name + ".json"),
            {
                "status": "settled",
                "before": {"usage": "0.100", "limit": 5, "limit_remaining": "4.900"},
                "after": key,
                "generations": [generation],
                "generation_total_usd": "0.002",
                "usage_delta_usd": "0.002",
            },
        )

    def test_accounting_uses_actual_delta_not_initial_account_usage_and_keeps_unknowns(
        self,
    ):
        bill = self.bill()
        extra = [
            {
                "group": "pi",
                "id": "stage-a",
                "role": "stage_a",
                "provider": "pi",
                "billing": bill,
                "descriptor": self.descriptor,
                "evidence": [self.evidence],
            }
        ]
        report = accounting([self.journal], external_calls=extra)
        self.assertEqual(report["groups"]["pi"]["total_cost_usd"], "0.002")
        self.assertIsNone(report["groups"]["codex"]["total_cost_usd"])
        self.assertIsNone(report["whole_workflow_wall_s"])
        with self.assertRaises(Refused):
            accounting([self.journal, self.journal], external_calls=[])
        with self.assertRaises(Refused):
            accounting([], external_calls=extra + [{**extra[0], "id": "other"}])
        timing = save(
            self.root / "timing.json",
            {
                "started_at": "2026-10-28T00:00:00+00:00",
                "ended_at": "2026-10-28T00:01:15+00:00",
                "source_evidence": [self.evidence],
            },
        )
        self.assertEqual(
            accounting([self.journal], external_calls=extra, wall_interval=timing)["whole_workflow_wall_s"],
            75,
        )
        unknown = copy.deepcopy(self.rows["candidate"])
        unknown["call"]["group"] = "pi"
        unknown["call"]["provider"] = "pi"
        unknown["call"]["model"] = "openrouter/deepseek/deepseek-v4.1-flash-1017"
        unknown["state"] = "unknown"
        unknown["result"] = None
        unknown["input_sha256"] = OwnerCall(**unknown["call"]).fingerprint()
        journal = save(
            self.root / "unknown.json",
            {"paid_limit_usd": "5", "calls": {"unknown": unknown}},
        )
        self.assertIsNone(accounting([journal], external_calls=extra)["groups"]["pi"]["total_cost_usd"])

    def test_known_clipping_failure_is_not_hidden_by_other_unknown_measurements(self):
        self.reports["baseline"]["conditions"][1]["status"] = "FAIL"
        self.reports["candidate"]["quality"][0]["score"] = None
        report = self.compare("known-fail")
        self.assertEqual(report["quality_non_regression"], "FAIL")
        ref = save(self.root / "pair.json", report)
        aggregate = aggregate_pairs([ref])
        self.assertEqual(aggregate["codex"]["n_fail"], 1)
        with self.assertRaises(Refused):
            aggregate_pairs([ref, ref])

    def test_time_components_keep_review_repair_and_unclassified_time(self):
        value = {
            "started_at": "2026-10-28T00:00:00+00:00",
            "ended_at": "2026-10-28T00:02:00+00:00",
            "source_evidence": [self.evidence],
            "components": [
                {
                    "kind": "repair",
                    "started_at": "2026-10-28T00:00:10+00:00",
                    "ended_at": "2026-10-28T00:00:30+00:00",
                    "source_evidence": [self.evidence],
                },
                {
                    "kind": "review",
                    "started_at": "2026-10-28T00:01:00+00:00",
                    "ended_at": "2026-10-28T00:01:50+00:00",
                    "source_evidence": [self.evidence],
                },
            ],
        }
        measured = workflow_timing(save(self.root / "clock.json", value))
        self.assertEqual(measured["whole_workflow_wall_s"], 120)
        self.assertEqual(measured["unclassified_wall_s"], 50)
        self.assertEqual([part["wall_s"] for part in measured["components"]], [20, 50])
        value["components"][1]["started_at"] = "2026-10-28T00:00:20+00:00"
        with self.assertRaises(Refused):
            workflow_timing(save(self.root / "overlapping-clock.json", value))

    def test_external_bill_must_bind_original_trace_generation_set(self):
        ref = self.bill()
        descriptor = json.loads(Path(self.descriptor["path"]).read_text())
        raw = Path(descriptor["raw"]["path"])
        raw.write_text(raw.read_text().replace("gen-fixture", "gen-other"))
        descriptor["raw"] = reference(raw)
        changed = save(self.root / "other-trace.json", descriptor)
        with self.assertRaises(Refused):
            external_stage_bill(ref, changed)

    def test_external_actual_bill_mismatch_refuses(self):
        ref = self.bill()
        value = json.loads(Path(ref["path"]).read_text())
        value["usage_delta_usd"] = "0.003"
        changed = save(self.root / "bad-bill.json", value)
        with self.assertRaises(Refused):
            external_stage_bill(changed, self.descriptor)


if __name__ == "__main__":
    unittest.main()
