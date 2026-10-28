"""Post-selection native judging and actual-cost summaries; no model/owner operations.

Reports are supplied by the independent evaluator. This module validates evidence
bindings and comparisons, not pixels or semantic correctness by string matching.
It is deliberately not imported by the optimizer adapter or experiment runner.
"""

import math
import sys
from dataclasses import dataclass
from datetime import datetime
from decimal import Decimal
from pathlib import Path

from openrouter_billing import (
    BILLED_MODEL,
    key_usage,
    pi_generation_ids,
    verify_settlement,
)
from owner_contract import OwnerCall, OwnerResult, money, tree_ref
from soodles_bridge import decode, read_ref, require


def load_native_judge(upstream):
    """Load the exact prepared upstream judge without a worker or model transport."""
    import reproduce

    receipt = decode(read_ref(upstream["prepared_receipt"]))
    root = Path(upstream["path"])
    require(
        receipt["source_pin"] == reproduce.PIN and reproduce.fingerprint(root) == receipt["source_tree_sha256"],
        "original native judge source differs",
    )
    sys.path.insert(0, str(root))
    from harness2.core import judge

    require(
        Path(judge.__file__).resolve() == root.resolve() / "harness2/core/judge.py",
        "imported judge differs from pinned native source",
    )
    return judge


@dataclass(frozen=True)
class ReviewedRun:
    journal: dict
    group: str
    run_id: str
    report: dict


def settled_run(binding):
    journal = decode(read_ref(binding.journal))
    rows = [
        row
        for row in journal["calls"].values()
        if row["call"]["group"] == binding.group and row["call"]["run_id"] == binding.run_id
    ]
    require(len(rows) == 1, "independent review has no unique original call")
    row = rows[0]
    call = OwnerCall(**row["call"])
    require(
        row["state"] == "settled" and row["input_sha256"] == call.fingerprint() and call.role == "solver",
        "independent review call is not a settled solver",
    )
    result = OwnerResult(**row["result"])
    require(
        result.settled and tree_ref(result.run_dir)["sha256"] == result.output_sha256,
        "independent review output differs from original settlement",
    )
    receipt = decode(read_ref({"path": result.receipt_path, "sha256": result.receipt_sha256}))
    require(
        receipt["call_sha256"] == call.fingerprint(),
        "independent review receipt belongs to another call",
    )
    accepted = decode(read_ref(receipt["original_review"]))
    inner = accepted.get("inner") or {}
    progress = inner.get("review_progress") or {}
    selection = progress.get("review") or {}
    require(
        accepted.get("owner") == "soodles.loop"
        and inner.get("owner") == "issue-execution.local-task"
        and inner.get("acceptance") == "PASS"
        and progress.get("status") == "locally_integrated"
        and selection.get("decision") == "approve"
        and selection.get("candidate") == receipt["candidate"],
        "independent review has no original safe-delivery acceptance",
    )
    return call, result


def review_report(binding, rubric_ref):
    call, result = settled_run(binding)
    rubric = decode(read_ref(rubric_ref))
    report = decode(read_ref(binding.report))
    require(
        report.get("schema_version") == 1
        and report.get("authority") == "independent-evaluator"
        and report.get("task_id") == call.inputs["task_id"]
        and report.get("rubric") == rubric_ref
        and report.get("source") == call.inputs["source"],
        "independent review task, source or rubric differs",
    )
    require(
        tree_ref(report["source"]["path"]) == report["source"],
        "evaluated fixed source changed",
    )
    artifact = Path(result.run_dir) / "workspace" / "drawing.html"
    require(
        read_ref(report["artifact"]) == artifact.read_bytes(),
        "evaluated drawing differs from delivered bytes",
    )
    statuses, quality = {}, {}
    gate_ids = [item["id"] for item in rubric["blocking_gates"]]
    dimensions = rubric["quality_vectors"]["dimensions"]
    for item in report["conditions"]:
        require(
            item["id"] in gate_ids and item["id"] not in statuses and item["status"] in {"PASS", "FAIL", "UNKNOWN"},
            "quality gate identity or status differs",
        )
        evidence(item)
        statuses[item["id"]] = item["status"]
    for item in report["quality"]:
        require(
            item["dimension"] in dimensions
            and item["dimension"] not in quality
            and (item["score"] is None or (type(item["score"]) is int and 0 <= item["score"] <= 2)),
            "quality dimension identity or score differs",
        )
        evidence(item)
        quality[item["dimension"]] = item["score"]
    require(
        set(statuses) == set(gate_ids) and set(quality) == set(dimensions),
        "independent review is incomplete",
    )
    unknown = "UNKNOWN" in statuses.values() or None in quality.values()
    return (
        call,
        result,
        {
            "n_passed": sum(value == "PASS" for value in statuses.values()),
            "n_criteria": len(statuses),
            "all_pass": all(value == "PASS" for value in statuses.values()),
            "gates": statuses,
            "quality": quality,
            "unknown": unknown,
            "report": binding.report,
        },
    )


def evidence(item):
    require(
        isinstance(item.get("reason"), str)
        and item["reason"].strip()
        and isinstance(item.get("evidence"), list)
        and item["evidence"],
        "independent assessment has no evidence or reason",
    )
    for ref in item["evidence"]:
        read_ref(ref)


class IndependentReviewAdapter:
    """Only the native judge score interface; never an optimizer context source."""

    def __init__(self, rubric, bindings):
        self.rubric, self.bindings = rubric, {}
        for binding in bindings:
            call, result = settled_run(binding)
            key = (str(Path(result.run_dir)), call.inputs["task_id"])
            require(key not in self.bindings, "independent judged run is duplicated")
            self.bindings[key] = binding

    def score(self, run_dir, task_id):
        binding = self.bindings.get((str(Path(run_dir)), task_id))
        require(binding is not None, "independent judged run is not explicitly bound")
        _, _, score = review_report(binding, self.rubric)
        return None if score["unknown"] else score


def selected_harnesses(selection):
    """Bind evaluation to the original final selection, never an intermediate edit."""
    summary = decode(read_ref(selection["summary"]))
    journal = decode(read_ref(selection["journal"]))
    require(
        summary.get("status") == "ok"
        and summary.get("k") == 1
        and set(summary.get("selections", {})) == {"seq"}
        and len(summary.get("base", [])) == 1,
        "original native selection is incomplete",
    )
    chosen = summary["selections"]["seq"]
    require(
        chosen.get("ok") and chosen.get("chosen_run_id"),
        "native final candidate is missing",
    )
    result = {}
    for label, identity in [
        ("baseline", summary["base"][0]["run_id"]),
        ("candidate", chosen["chosen_run_id"]),
    ]:
        rows = [
            row
            for row in journal["calls"].values()
            if row["call"]["group"] == selection["group"] and row["call"]["run_id"] == identity
        ]
        require(
            len(rows) == 1 and rows[0]["state"] == "settled",
            "selected harness has no unique original solver",
        )
        call = OwnerCall(**rows[0]["call"])
        require(
            call.role == "solver"
            and call.inputs["task_id"] == summary["task_id"]
            and rows[0]["input_sha256"] == call.fingerprint(),
            "native selected call identity differs",
        )
        harness = call.inputs["harness"]
        require(tree_ref(harness["path"]) == harness, "native selected harness changed")
        result[label] = harness
    return result


def judge_pair(*, rubric, baseline, candidate, selection, native_judge):
    """Invoke pinned core.judge.judge_one after selection; return no adoption authority."""
    chosen = selected_harnesses(selection)  # Freeze native selection before reading any evaluator report.
    old_call, old_result, old = review_report(baseline, rubric)
    new_call, new_result, new = review_report(candidate, rubric)
    require(
        old_call.group == new_call.group
        and old_call.provider == new_call.provider
        and old_call.model == new_call.model
        and old_call.inputs["task_id"] == new_call.inputs["task_id"]
        and old_call.inputs["source"] == new_call.inputs["source"]
        and old_call.inputs.get("prompt") == new_call.inputs.get("prompt")
        and old_call.inputs.get("effort") == new_call.inputs.get("effort"),
        "paired runs have different fixed inputs or routes",
    )
    require(
        selection["group"] == old_call.group
        and old_call.inputs["harness"]["sha256"] == chosen["baseline"]["sha256"]
        and new_call.inputs["harness"]["sha256"] == chosen["candidate"]["sha256"],
        "evaluated harness differs from native baseline or final selection",
    )
    adapter = IndependentReviewAdapter(rubric, [baseline, candidate])
    rows = [
        native_judge.judge_one(
            adapter,
            call.inputs["task_id"],
            "seq",
            1,
            label,
            call.run_id,
            Path(result.run_dir),
            chosen_is_base=label == "baseline",
        )
        for label, call, result in [
            ("baseline", old_call, old_result),
            ("candidate", new_call, new_result),
        ]
    ]
    known_failure = any("FAIL" in score["gates"].values() for score in (old, new))
    known_regression = any(
        value is not None and new["quality"][key] is not None and new["quality"][key] < value
        for key, value in old["quality"].items()
    )
    if known_failure or known_regression:
        status = "FAIL"
    elif not all(row.graded for row in rows):
        status = "UNKNOWN"
    else:
        status = "PASS"
    return {
        "quality_non_regression": status,
        "group": old_call.group,
        "task_id": old_call.inputs["task_id"],
        "native_judgments": [row.__dict__ for row in rows],
        "selection": selection,
        "sample_pairs": 1,
        "generalization": "one fixed case; no statistical population claim",
        "adoption_authorized": False,
    }


def external_stage_bill(ref, descriptor_ref):
    """Validate the retained original Stage-A GET receipt format; no supplier call."""
    descriptor = decode(read_ref(descriptor_ref))
    require(
        descriptor.get("provider") == "pi" and descriptor.get("terminal") and not descriptor.get("gaps"),
        "external original trace is incomplete",
    )
    original_ids = pi_generation_ids(read_ref(descriptor["raw"]))
    value = decode(read_ref(ref))
    require(value.get("status") == "settled", "external actual bill is unsettled")
    new, _ = key_usage(value["after"])
    before = value["before"]
    old = money(before["usage"])
    require(
        Decimal(str(before["limit"])) <= 5 and old + money(before["limit_remaining"]) == Decimal(str(before["limit"])),
        "external original key interval is inconsistent",
    )
    total, ids = Decimal("0"), []
    for generation in value["generations"]:
        data = generation["data"]
        require(
            generation["resource"] == "generation"
            and generation["generation_id"] == data["id"]
            and generation["key_fingerprint"] == value["after"]["key_fingerprint"]
            and data["model"] == BILLED_MODEL
            and data["is_byok"] is False
            and (data.get("finish_reason") or data.get("cancelled") is True),
            "external generation differs",
        )
        require(data["id"] not in ids, "external generation is duplicated")
        ids.append(data["id"])
        total += money(data["total_cost"])
    require(
        ids == original_ids
        and total == new - old == money(value["usage_delta_usd"]) == money(value["generation_total_usd"]),
        "external actual bills do not match key delta",
    )
    return total, ids


def accounting(journals, *, external_calls, wall_interval=None):
    """All supplied original calls, including unresolved attempts; never price tokens.

    external_calls explicitly lists Stage A/evaluation calls outside native journals.
    Completeness of that list belongs to Session; the returned scope states it.
    """
    from experiment_driver import journal_elapsed

    seen_calls, seen_generations, rows = set(), set(), []
    native_rounds = []
    for ref in journals:
        journal = decode(read_ref(ref))
        require(journal["paid_limit_usd"] == "5", "original shared budget differs")
        complete_clocks = all("started_unix_s" in entry for entry in journal["calls"].values())
        native_rounds.append(
            {"journal": ref, "whole_native_round_wall_s": journal_elapsed(journal) if complete_clocks else None}
        )
        for entry in journal["calls"].values():
            call = OwnerCall(**entry["call"])
            identity = (call.group, call.run_id)
            require(
                identity not in seen_calls and entry["input_sha256"] == call.fingerprint(),
                "accounting call is duplicated or changed",
            )
            seen_calls.add(identity)
            cost, ids = None, []
            if entry["state"] == "settled" and entry.get("result"):
                result = OwnerResult(**entry["result"])
                if call.provider == "pi" and result.billing_receipt is not None:
                    bill = verify_settlement(call, result, result.billing_receipt)
                    require(result.cost_usd == bill["cost_usd"], "original settled amount differs from actual bill")
                    cost, ids = money(bill["cost_usd"]), bill["generation_ids"]
            elif entry["state"] == "refused":
                # Refused is contractually effect-free; every unknown remains null.
                cost = Decimal("0")
            require(
                not seen_generations.intersection(ids),
                "generation cost is counted more than once",
            )
            seen_generations.update(ids)
            duration = entry.get("elapsed_s")
            require(
                duration is None or (type(duration) in (int, float) and math.isfinite(duration) and duration >= 0),
                "original call elapsed time is invalid",
            )
            rows.append(
                {
                    "group": call.group,
                    "id": call.run_id,
                    "role": call.role,
                    "state": entry["state"],
                    "cost_usd": str(cost) if cost is not None else None,
                    "observed_call_elapsed_s": duration,
                    "evidence": ref,
                }
            )
    for item in external_calls:
        identity = (item["group"], item["id"])
        require(
            identity not in seen_calls and item["provider"] in {"codex", "pi"},
            "external call is duplicated or unsupported",
        )
        seen_calls.add(identity)
        require(item.get("evidence"), "external call has no original owner evidence")
        for ref in item["evidence"]:
            read_ref(ref)
        cost, ids = None, []
        if item["provider"] == "pi" and item.get("billing") is not None:
            cost, ids = external_stage_bill(item["billing"], item["descriptor"])
        require(
            not seen_generations.intersection(ids),
            "generation cost is counted more than once",
        )
        seen_generations.update(ids)
        rows.append(
            {
                "group": item["group"],
                "id": item["id"],
                "role": item["role"],
                "state": "external",
                "cost_usd": str(cost) if cost is not None else None,
                "observed_call_elapsed_s": None,
                "evidence": item["evidence"],
                "billing": item.get("billing"),
            }
        )
    groups = {}
    for group in sorted({row["group"] for row in rows}):
        selected = [row for row in rows if row["group"] == group]
        observed = sum(
            (money(row["cost_usd"]) for row in selected if row["cost_usd"] is not None),
            Decimal("0"),
        )
        groups[group] = {
            "known_cost_usd": str(observed),
            "total_cost_usd": str(observed) if all(row["cost_usd"] is not None for row in selected) else None,
            "unknown_cost_calls": [row["id"] for row in selected if row["cost_usd"] is None],
        }
    timing = (
        workflow_timing(wall_interval)
        if wall_interval is not None
        else {
            "whole_workflow_wall_s": None,
            "components": [],
            "unclassified_wall_s": None,
        }
    )
    return {
        "groups": groups,
        "calls": rows,
        "native_rounds": native_rounds,
        **timing,
        "wall_interval": wall_interval,
        "scope": "all supplied journals and explicit external calls; Session verifies enumeration completeness",
        "initial_account_usage_is_not_experiment_cost": True,
        "adoption_authorized": False,
    }


def workflow_timing(ref):
    """Preserve measured whole time and explicit non-overlapping phase evidence."""
    value = decode(read_ref(ref))

    def interval(item):
        require(item.get("source_evidence"), "clock interval has no original evidence")
        for evidence_ref in item["source_evidence"]:
            read_ref(evidence_ref)
        start, end = (datetime.fromisoformat(item[key]) for key in ("started_at", "ended_at"))
        require(
            start.utcoffset() is not None and end.utcoffset() is not None and end >= start,
            "clock interval is invalid",
        )
        return start, end

    start, end = interval(value)
    components, previous_end = [], start
    measured = [(interval(item), item) for item in value.get("components", [])]
    for (begin, finish), item in sorted(measured, key=lambda pair: pair[0][0]):
        require(
            item["kind"] in {"stage_a", "model", "tools", "review", "repair", "evaluation", "other"}
            and previous_end <= begin <= finish <= end,
            "workflow components overlap or exceed whole interval",
        )
        previous_end = finish
        components.append({**item, "wall_s": (finish - begin).total_seconds()})
    whole = (end - start).total_seconds()
    return {
        "whole_workflow_wall_s": whole,
        "components": components,
        "unclassified_wall_s": whole - sum(item["wall_s"] for item in components),
    }


def aggregate_pairs(reports):
    """Describe these fixed cases; do not turn two heldouts into a population claim."""
    require(reports, "paired evaluation is empty")
    groups, seen = {}, set()
    for report_ref in reports:
        report = decode(read_ref(report_ref))
        key = (report["group"], report["task_id"])
        require(
            key not in seen and report["quality_non_regression"] in {"PASS", "FAIL", "UNKNOWN"},
            "paired evaluation is duplicated or invalid",
        )
        seen.add(key)
        groups.setdefault(key[0], []).append(
            {
                "task_id": key[1],
                "status": report["quality_non_regression"],
                "report": report_ref,
            }
        )
    return {
        group: {
            "pairs": rows,
            "n_pairs": len(rows),
            "n_pass": sum(row["status"] == "PASS" for row in rows),
            "n_fail": sum(row["status"] == "FAIL" for row in rows),
            "n_unknown": sum(row["status"] == "UNKNOWN" for row in rows),
            "quality_non_regression": "FAIL"
            if any(row["status"] == "FAIL" for row in rows)
            else ("PASS" if all(row["status"] == "PASS" for row in rows) else "UNKNOWN"),
            "generalization": "only these fixed cases; no population or statistical-significance claim",
        }
        for group, rows in groups.items()
    }


def efficiency_decision(quality_status, baseline_cost, candidate_cost, baseline_wall, candidate_wall):
    if quality_status != "PASS":
        return "quality_blocked" if quality_status == "FAIL" else "inconclusive"
    if None in (baseline_cost, candidate_cost, baseline_wall, candidate_wall):
        return "inconclusive"
    old, new = money(baseline_cost), money(candidate_cost)
    require(
        all(
            type(value) in (int, float) and math.isfinite(value) and value >= 0
            for value in (baseline_wall, candidate_wall)
        ),
        "efficiency wall time is invalid",
    )
    if new <= old and candidate_wall <= baseline_wall:
        return "pareto_improvement" if new < old or candidate_wall < baseline_wall else "no_change"
    return "tradeoff_or_regression"
