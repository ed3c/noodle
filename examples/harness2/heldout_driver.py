"""Two fixed original-owner solver calls after native selection; no optimization.

The Session supplies the finite worker input, accepted producer and original
review callback. Evaluator reports/oracles never belong in this configuration.
"""

import math
import re
from dataclasses import asdict
from pathlib import Path

from experiment_driver import (
    FrozenAcceptedSource,
    journal_elapsed,
    public_destination,
    public_tree,
)
from independent_evaluation import load_native_judge, selected_harnesses
from openrouter_billing import OpenRouterBilling, ReadOnlyOpenRouter
from owner_contract import OwnerCall, SharedGate, money
from soodles_bridge import (
    MODELS,
    PinnedCLI,
    RequestTemplate,
    SoodlesOwnerBridge,
    decode,
    read_ref,
    reference,
    require,
    save,
)


def verify_fixed_composition(template, route, harness):
    """Check both declared consumers before dispatch; original CLI still owns Git verification."""
    from composition_handoff import verify_derived_skills

    catalog = decode(read_ref(template.composition["definition"]))
    require(catalog.get("schema") == 1, "heldout composition schema differs")
    selected = [value for value in catalog["compositions"] if value["id"] == template.composition["id"]]
    require(len(selected) == 1 and len(selected[0]["stages"]) == 1,
            "heldout consumer is not one independent fixed stage")
    stage = selected[0]["stages"][0]
    require(
        stage.get("runtime") == "openshell" and stage.get("provider") == route["provider"]
        and stage.get("model") == route["model"] and stage.get("model_reasoning_effort") == route["effort"]
        and stage.get("profile") == template.profile and "handoff" not in stage,
        "heldout composition changed the fixed route or profile",
    )
    skills = [stage["selected_skill"], *stage["support_skills"]]
    paths, bindings = [], []
    for skill in skills:
        sources = [value for value in catalog["sources"] if value["alias"] == skill["source"]]
        require(len(sources) == 1, "heldout module source is missing or ambiguous")
        entry = public_destination(skill["entry"])
        path = Path(sources[0]["root"]) / entry
        public_destination(path.as_posix().lstrip("/"))
        require(path.is_absolute() and path.resolve() == path, "derived module path traverses a link")
        paths.append(str(path))
        bindings.append({"alias": skill["alias"], "path": str(path)})
    require(paths, "heldout selected skill is missing")
    call = OwnerCall(route["group"], "preflight", "solver", route["provider"], route["model"], {"harness": harness})
    verify_derived_skills(template, call, {
        "selected_skill": paths[0], "support_skills": paths[1:], "skill_bindings": bindings,
    })


def preflight_pair(config):
    """Only bound public files are read; no CLI process, provider or credential GET."""
    require(config["schema_version"] == 1 and config.get("status") == "ready",
            "heldout caller configuration is not ready")
    route = config["route"]
    require(
        route["model"] == MODELS.get(route["provider"])
        and route["effort"] == "low"
        and route.get("paid_reservation_usd") is None,
        "heldout route differs from the authorized fixed model",
    )
    require(
        type(route["timeout_s"]) in (int, float)
        and math.isfinite(route["timeout_s"])
        and route["timeout_s"] > 0,
        "heldout timeout metadata is invalid",
    )
    require(money(config["initial_paid_spent_usd"]) <= money("5"), "shared paid budget is exhausted")
    require(config["selection"]["group"] == route["group"], "native selection belongs to another group")
    harnesses = selected_harnesses(config["selection"])
    cli = PinnedCLI(**config["cli"])
    require(
        Path(config["host_directory"]).is_absolute()
        and cli.environment.get("NOODLE_HOST_DIR") == config["host_directory"],
        "original Host locator is missing or differs",
    )
    cli.verify()
    bundle = FrozenAcceptedSource(**decode(read_ref(config["task_bundle"])))
    public_tree(bundle.source)
    provenance = decode(read_ref(bundle.provenance))
    require(
        provenance["source"] == bundle.source
        and provenance["stage_a_regenerated_per_solver"] is False,
        "paired accepted source differs",
    )
    accepted = decode(read_ref(provenance["owner_acceptance"]))
    inner = accepted.get("inner") or {}
    progress = inner.get("review_progress") or {}
    review = progress.get("review") or {}
    require(
        accepted.get("owner") == "soodles.loop"
        and inner.get("owner") == "issue-execution.local-task"
        and inner.get("acceptance") == "PASS"
        and progress.get("status") == "locally_integrated"
        and review.get("decision") == "approve"
        and review.get("candidate") == provenance["candidate"],
        "paired source has no original integrated acceptance",
    )
    read_ref(provenance["candidate"])
    slots = config["slots"]
    require(
        len(slots) == 2 and {slot["variant"] for slot in slots} == {"baseline", "candidate"}
        and len({slot["run_id"] for slot in slots}) == 2
        and all(re.fullmatch(r"[a-z0-9][a-z0-9_-]*", slot["run_id"]) for slot in slots),
        "heldout slots are not two distinct fixed versions",
    )
    templates = [RequestTemplate(**slot["template"]) for slot in slots]
    require(len({t.repository for t in templates}) == 2, "paired controls are not independent")
    fixed = []
    for slot, template in zip(slots, templates):
        require(
            (template.group, template.role) == (route["group"], "solver")
            and template.selected_skill is None and not template.support_skills
            and template.harvest is None and template.harness_skills
            and set(template.skill_bases) == set(template.harness_skills)
            and template.write_paths and template.acceptance and template.deliverables,
            "heldout template is not a complete original solver",
        )
        control = Path(template.repository)
        require(
            control.is_absolute() and control.resolve() == control and control.is_dir()
            and not (control / ".noodle").exists(),
            "heldout control is unavailable or already has an owner",
        )
        require(
            isinstance(template.composition, dict)
            and set(template.composition) == {"definition", "id"}
            and isinstance(template.producer_handoff, dict)
            and set(template.producer_handoff) == {"request", "artifact_path"},
            "heldout original composition and producer are incomplete",
        )
        producer = template.producer_handoff
        read_ref(producer["request"])
        require(
            producer["request"] in provenance["provenance"]
            and producer["artifact_path"] in {
                item["candidate_path"] for item in provenance["artifacts"].values()
            },
            "handoff producer is detached from the accepted task bundle",
        )
        for ref in (template.noodle, template.profile, template.composition["definition"]):
            read_ref(ref)
        for base in template.skill_bases.values():
            public_tree(base)
        verify_fixed_composition(template, route, harnesses[slot["variant"]])
        fixed.append({k: v for k, v in asdict(template).items() if k not in {"repository", "composition"}})
    require(fixed[0] == fixed[1], "paired execution environment or accepted producer differs")
    output = Path(config["output_root"])
    require(output.is_absolute() and output.resolve() == output and not output.exists(),
            "heldout output is not a fresh canonical directory")
    billing = config.get("billing")
    require(billing is not None and billing["exclusive_key"] is True,
            "shared paid-key accounting supplier is missing")
    require(config["poll_s"] > 0 and config["always_on_max"] > 0, "caller bounds are invalid")
    return cli, bundle, harnesses, templates


def run_pair(config, review_callback):
    """Explicit effectful entry. Unknown stops the pair; never retries or reopens it.

The callback must finish original candidate AND integrated acceptance. Quality
reports are evaluated separately, after both original outputs have settled.
"""
    cli, bundle, harnesses, templates = preflight_pair(config)
    # Source validation imports the actual pinned core, never a replacement judge.
    # No judge is executed here: heldout grading remains an independent later step.
    load_native_judge(config["upstream"])
    from native_adapter import NoodleAdapter, Route, Task

    output = Path(config["output_root"])
    output.mkdir(parents=True, exist_ok=False)
    config_ref = save(output / "config.json", config)
    billing_config = config["billing"]
    client = ReadOnlyOpenRouter(
        Path(billing_config["environment_file"]), key_name=billing_config["key_name"],
        timeout_s=billing_config["timeout_s"],
    )
    billing = OpenRouterBilling(
        client, initial_usage_usd=config["initial_paid_spent_usd"],
        exclusive_key=billing_config["exclusive_key"], observations=billing_config["observations"],
        interval_s=billing_config["interval_s"],
    )
    bridge = SoodlesOwnerBridge(cli, output / "owner", templates, review=review_callback,
                               billing=billing, poll_s=config["poll_s"])
    gate = SharedGate(output / "owner-journal.json", bridge,
                      initial_paid_spent_usd=config["initial_paid_spent_usd"])
    adapter = NoodleAdapter(
        route=Route(**config["route"]), gate=gate,
        tasks={bundle.task_id: Task(bundle.prompt, Path(bundle.source["path"]))},
        base=Path(harnesses["baseline"]["path"]), runs_root=output / "native",
        always_on_max=config["always_on_max"],
    )
    # Validate BOTH frozen surfaces before dispatching even the first slot.
    for harness in harnesses.values():
        adapter._read_harness(Path(harness["path"]))
    results = []
    for slot in config["slots"]:
        result = adapter.execute(bundle.task_id, Path(harnesses[slot["variant"]]["path"]), slot["run_id"])
        results.append({"variant": slot["variant"], "run_id": slot["run_id"], "run_dir": str(result)})
    return save(output / "pair.json", {
        "schema_version": 1, "config": config_ref, "results": results,
        "owner_journal": reference(gate.journal), "pair_elapsed_s": journal_elapsed(gate.state),
        "quality": "not evaluated", "whole_workflow_cost_usd": None,
        "whole_workflow_wall_s": None, "adoption_authorized": False,
    })
