"""Offline controls execute the REAL pinned core with an explicitly fake owner."""
import json
import shutil
import socket
import subprocess
import tempfile
import threading
import unittest
from dataclasses import replace
from pathlib import Path
from unittest.mock import patch

from harness2.core.proposer import Proposer, ProposerError
from harness2.core.spec import ChannelSpec
from native_adapter import NoodleAdapter, Route, Task, run_native_task
from owner_contract import (
    OwnerResult,
    Refused,
    SharedGate,
    UnknownEffect,
    sha256,
    tree_ref,
)

PAID_MODEL = "openrouter/deepseek/deepseek-v4.1-flash-1017"


class FakeOwner:
    """Deterministic artifact fixture, NEVER evidence of a real Noodle run."""
    def __init__(self, root):
        self.root = root
        self.calls = []
        self.unknown_proposer = False
        self.invalid_edit = False
        self.trace_events = None

    def execute(self, call):
        self.calls.append(call)
        if call.role == "proposer" and self.unknown_proposer:
            raise TimeoutError("fixture accepted request then lost its response")
        directory = self.root / str(len(self.calls))
        directory.mkdir(parents=True)
        workspace = None
        if call.role == "proposer":
            workspace = directory / "returned-harness"
            shutil.copytree(Path(call.inputs["context"]["path"]) / "harness", workspace)
            (workspace / "AGENTS.md").write_text("Include every signed transaction.\n")
            if self.invalid_edit:
                (workspace / "scripts").mkdir(exist_ok=True)
                (workspace / "scripts" / "forbidden.py").write_text("print('unauthorized')\n")
        if call.role == "solver":
            instruction = (Path(call.inputs["harness"]["path"]) / "AGENTS.md").read_text()
            answer = "signed total" if "signed" in instruction else "positive total"
            (directory / "deliverables.json").write_text(json.dumps({"answer.txt": answer}))
            events = self.trace_events or [
                {"event_id": "action", "record": {"action": "Reconcile records"}},
                {"event_id": "observation", "record": {"text": answer}},
            ]
            (directory / "trajectory.md").write_text("\n".join(json.dumps(event) for event in events))
        receipt = directory / "fake-owner-receipt.json"
        receipt.write_text(json.dumps({"fixture_only": True, "input_sha256": call.fingerprint(),
                                      "canonical_attempt": "fixture-canonical", "native_attempt": "fixture-native"}))
        return OwnerResult(str(directory), tree_ref(directory)["sha256"], "<notes>Include signed amounts.</notes>",
                           str(workspace) if workspace else None, str(receipt), sha256(receipt),
                           True, "0.01" if call.provider == "pi" else None, 0.01)


class NativeContractTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name).resolve()
        self.owner = FakeOwner(self.root / "owner")
        self.gate = SharedGate(self.root / "journal.json", self.owner, initial_paid_spent_usd="0")
        # Any accidental native transport/model subprocess is a test failure.
        self.subprocess_guard = patch.object(subprocess, "run", side_effect=AssertionError("native subprocess forbidden"))
        self.socket_guard = patch.object(socket.socket, "connect", side_effect=AssertionError("network forbidden"))
        self.process_guard = patch.object(subprocess, "Popen", side_effect=AssertionError("process forbidden"))
        self.subprocess_guard.start()
        self.socket_guard.start()
        self.process_guard.start()
        self.addCleanup(self.subprocess_guard.stop)
        self.addCleanup(self.socket_guard.stop)
        self.addCleanup(self.process_guard.stop)

    def adapter(self, provider="codex"):
        base = self.root / provider / "base"
        base.mkdir(parents=True)
        (base / "AGENTS.md").write_text("Reconcile positive transaction amounts with main and helper skills.\n")
        for name in ("main", "helper"):
            skill = base / "skills" / name / "SKILL.md"
            skill.parent.mkdir(parents=True)
            skill.write_text(f"---\nname: {name}\ndescription: Reconcile transaction records\n---\nRead the source records.\n")
        sources = self.root / provider / "source"
        sources.mkdir()
        (sources / "records.txt").write_text("10\n-2\n")
        route = Route(provider, provider, PAID_MODEL if provider == "pi" else "fixture-subscription",
                      "high", 60, "0.05" if provider == "pi" else None)
        return NoodleAdapter(route=route, gate=self.gate, tasks={"records": Task("Reconcile all records.", sources)},
                             base=base, runs_root=self.root / provider / "runs", always_on_max=12000)

    def test_native_core_both_routes_real_proposals_probes_and_final(self):
        for provider in ("codex", "pi"):
            adapter = self.adapter(provider)
            result = run_native_task(adapter, "records", experiment_id=provider + "/records")
            native = result["native_result"]
            self.assertEqual(native["status"], "ok", native)
            self.assertEqual((native["modes"]["seq"]["ledger"]["R"],
                              native["modes"]["seq"]["ledger"]["P"]), (4, 3))
            self.assertEqual(native["modes"]["seq"]["fell_back"], [])
            self.assertEqual(native["selections"]["seq"]["chosen_label"], "c1")
            self.assertFalse(native["selections"]["seq"]["chosen_tree_is_base"])
            self.assertFalse(result["adoption_authorized"])
            self.assertEqual(result["hidden_acceptance"], "not implemented")
            self.assertFalse(result["native_costs_authoritative"])
            self.assertFalse(result["native_time_authoritative"])
            calls = [c for c in self.owner.calls if c.provider == provider]
            self.assertEqual(sum(c.role == "solver" for c in calls), 4)
            self.assertEqual(sum(c.role == "proposer" for c in calls), 3)
            self.assertEqual({c.role for c in calls}, {"solver", "proposer"})
        journal = json.loads(self.gate.journal.read_text())
        self.assertEqual(len(journal["calls"]), 14)
        subscription = [r for r in journal["calls"].values() if r["provider"] == "codex"]
        self.assertTrue(all(r["result"]["cost_usd"] is None for r in subscription))
        self.assertEqual(str(self.gate._spent()), "0.07")

    def test_trajectory_bounds_each_text_but_retains_late_events_and_fields(self):
        adapter = self.adapter()
        self.owner.trace_events = [
            {"event_id": "early", "record": {"output": "x" * 30000, "exit_code": 0}},
            {"event_id": "multi", "entries": [
                {"path": ["outputs", 0], "value": "a" * 30000},
                {"path": ["outputs", 1], "value": "b" * 30000}]},
            {"event_id": "late-error", "record": {"error": "drawing label clipped", "exit_code": 1}},
            {"event_id": "terminal", "record": {"type": "task_complete", "text": "artifact delivered"}},
        ]
        run = adapter.execute("records", None, "trace")
        original = (Path(run) / "trajectory.md").read_bytes()
        events = [json.loads(line) for line in adapter.trajectory(run, obs_trunc=10000).splitlines()]
        self.assertEqual([row["event_id"] for row in events], [row["event_id"] for row in self.owner.trace_events])
        self.assertEqual(events[0]["record"], {"output": "x" * 10000 + " [truncated]", "exit_code": 0})
        self.assertEqual(events[1]["entries"][0]["value"], "a" * 10000 + " [truncated]")
        self.assertEqual(events[1]["entries"][1]["value"], "b" * 10000 + " [truncated]")
        self.assertEqual(events[1]["entries"][1]["path"], ["outputs", 1])
        self.assertEqual(events[2:], self.owner.trace_events[2:])
        self.assertEqual((Path(run) / "trajectory.md").read_bytes(), original)
        self.assertEqual(adapter.deliverable_files(run), {"answer.txt": "positive total"})

    def test_same_run_reuses_owner_and_different_input_refuses(self):
        adapter = self.adapter()
        result = adapter.execute("records", None, "one")
        self.assertEqual(adapter.execute("records", None, "one"), result)
        self.assertEqual(len(self.owner.calls), 1)
        original = self.owner.calls[0]
        with self.assertRaisesRegex(Refused, "different input"):
            self.gate.execute(replace(original, inputs={**original.inputs, "prompt": "changed"}))
        self.assertEqual(len(self.owner.calls), 1)
        reopened = SharedGate(self.gate.journal, self.owner, initial_paid_spent_usd="0")
        self.assertEqual(reopened.execute(original).run_dir, str(result))
        self.assertEqual(len(self.owner.calls), 1)

    def test_unknown_improver_closes_later_native_fallback_and_other_route(self):
        adapter = self.adapter()
        self.owner.unknown_proposer = True
        result = run_native_task(adapter, "records", experiment_id="unknown")
        self.assertTrue(result["owner_blocked"], result)
        self.assertEqual(len(self.owner.calls), 2)  # base + first accepted/lost proposer
        self.assertNotEqual(result["native_result"]["status"], "ok")
        paid = self.adapter("pi")
        with self.assertRaises(UnknownEffect):
            paid.execute("records", None, "other-route")
        reopened = SharedGate(self.gate.journal, self.owner, initial_paid_spent_usd="0")
        with self.assertRaises(UnknownEffect):
            reopened.execute(self.owner.calls[0])
        self.assertEqual(len(self.owner.calls), 2)

    def test_paid_budget_does_not_block_subscription_unknown_dollars(self):
        self.gate = SharedGate(self.root / "spent.json", self.owner, initial_paid_spent_usd="5")
        paid = self.adapter("pi")
        with self.assertRaisesRegex(Refused, "remaining authorized budget"):
            paid.execute("records", None, "paid")
        subscription = self.adapter()
        subscription.execute("records", None, "subscription")
        self.assertEqual(len(self.owner.calls), 1)
        self.assertIsNone(next(iter(self.gate.state["calls"].values()))["result"]["cost_usd"])

    def test_missing_owner_and_default_channel_transport_refuse(self):
        adapter = self.adapter()
        adapter.gate.owner = None
        with self.assertRaisesRegex(Refused, "not implemented"):
            run_native_task(adapter, "records", experiment_id="missing")
        with self.assertRaisesRegex(ProposerError, "explicit original-owner"):
            Proposer(ChannelSpec("noodle_owner", "fixture")).call("task", None, "native")
        self.assertEqual(self.owner.calls, [])

    def test_native_rejects_off_surface_proposals_and_never_solves_them(self):
        adapter = self.adapter()
        self.owner.invalid_edit = True
        result = run_native_task(adapter, "records", experiment_id="invalid")
        self.assertIn("fell_back", result["native_result"]["modes"]["seq"], result)
        self.assertEqual(len(result["native_result"]["modes"]["seq"]["fell_back"]), 3)
        self.assertTrue(result["native_result"]["selections"]["seq"]["chosen_tree_is_base"])
        self.assertFalse(result["adoption_authorized"])
        self.assertEqual(sum(c.role == "proposer" for c in self.owner.calls), 6)
        for call in self.owner.calls:
            if call.role == "solver":
                self.assertFalse((Path(call.inputs["harness"]["path"]) / "scripts").exists())
        with self.assertRaises(ValueError):
            adapter.validate_tree({"AGENTS.md": "hello", "harness.json": '{"model":"other"}'})
        with self.assertRaises(Refused):
            adapter.score(Path("unused"), "records")

    def test_concurrent_same_id_only_delegates_once(self):
        adapter = self.adapter()
        results, errors = [], []
        def run():
            try:
                results.append(adapter.execute("records", None, "same"))
            except Exception as exc:
                errors.append(exc)
        threads = [threading.Thread(target=run) for _ in range(2)]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join()
        self.assertEqual(errors, [])
        self.assertEqual(len(results), 2)
        self.assertEqual(len(self.owner.calls), 1)

    def test_paid_unknown_cost_keeps_hold_and_missing_amount_uses_remainder(self):
        adapter = self.adapter("pi")
        actual = self.owner.execute
        self.owner.execute = lambda call: replace(actual(call), cost_usd=None)
        adapter.execute("records", None, "one")
        self.assertEqual(str(self.gate._spent()), "0.05")
        self.gate.execute(replace(self.owner.calls[0], run_id="remaining", paid_reservation_usd=None))
        self.assertEqual(str(self.gate._spent()), "5.00")
        self.assertEqual(len(self.owner.calls), 2)
        with self.assertRaisesRegex(Refused, "remaining authorized budget"):
            self.gate.execute(replace(self.owner.calls[0], run_id="exhausted", paid_reservation_usd=None))

    def test_persisted_pending_never_replays_and_changed_output_closes_gate(self):
        adapter = self.adapter()
        directory = adapter.execute("records", None, "one")
        call = self.owner.calls[0]
        (directory / "deliverables.json").write_text('{"answer.txt":"changed"}')
        with self.assertRaises(UnknownEffect):
            self.gate.execute(call)
        self.assertTrue(self.gate.state["blocked"])
        state = json.loads(self.gate.journal.read_text())
        next(iter(state["calls"].values()))["state"] = "pending"
        state["blocked"] = ""
        self.gate.journal.write_text(json.dumps(state))
        reopened = SharedGate(self.gate.journal, self.owner, initial_paid_spent_usd="0")
        with self.assertRaises(UnknownEffect):
            reopened.execute(replace(call, run_id="different"))
        self.assertEqual(len(self.owner.calls), 1)

    def test_intent_write_failure_never_starts_owner(self):
        adapter = self.adapter()
        with patch.object(self.gate, "_save", side_effect=OSError("fixture I/O unknown")):
            with self.assertRaises(UnknownEffect):
                adapter.execute("records", None, "one")
        self.assertEqual(self.owner.calls, [])
        self.assertTrue(self.gate.state["blocked"])

    def test_symlink_and_baseline_drift_refuse_before_owner(self):
        adapter = self.adapter()
        (adapter._base / "link").symlink_to(adapter._base / "AGENTS.md")
        with self.assertRaises(Refused):
            adapter.execute("records", None, "escape")
        self.assertEqual(self.owner.calls, [])


if __name__ == "__main__":
    unittest.main()
