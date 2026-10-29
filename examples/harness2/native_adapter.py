"""Thin native Harness² seams; owner selection remains explicit at the driver boundary."""
import json
import re
from dataclasses import dataclass
from pathlib import Path

from harness2.core import schedule, tree
from harness2.core.proposer import Proposer, ProposerError, Reply
from harness2.core.spec import BenchPrompts, BenchSpec, ChannelSpec
from owner_contract import OwnerCall, Refused, SharedGate, tree_ref


@dataclass(frozen=True)
class Route:
    group: str
    provider: str
    model: str
    effort: str
    # Metadata forwarded to the existing owner, not a claim of native enforcement.
    timeout_s: int
    paid_reservation_usd: str | None = None


@dataclass(frozen=True)
class Task:
    prompt: str
    source: Path


class InstructionSurface:
    name = "noodle-owner"

    def __init__(self, always_on_max):
        self._surface = tree.SurfaceSpec(
            name=self.name, always_on=("AGENTS.md",), always_on_max=always_on_max,
            always_on_cap_reason="explicit experiment input", skill_dir="skills",
            skill_name_rule="lowercase letters, digits, underscore or hyphen",
            trigger_hint="name selected skills in AGENTS.md", autotrigger_file="AGENTS.md",
            trigger_line="Use skills/{name}/SKILL.md when {desc}.",
            subagent_requires_mode=False, plugin_dir=None, hooks_file=None)

    def surface(self):
        return self._surface


class NoodleAdapter:
    def __init__(self, *, route: Route, gate: SharedGate, tasks: dict[str, Task],
                 base: Path, runs_root: Path, always_on_max: int, training_feedback=None):
        if not re.fullmatch(r"[a-z0-9][a-z0-9_-]*", route.group) or not tasks:
            raise Refused("experiment group or task set is invalid")
        if any(not re.fullmatch(r"[a-z0-9][a-z0-9_-]*", key) for key in tasks):
            raise Refused("task identity is not a safe native namespace")
        self.route, self.gate, self._tasks = route, gate, dict(tasks)
        self._base = Path(base)
        output = Path(runs_root).absolute()
        if output.resolve() != output:
            raise Refused("native output root traverses a symlink")
        for source in [self._base, *(task.source for task in tasks.values())]:
            original = source.resolve()
            if output.is_relative_to(original) or original.is_relative_to(output):
                raise Refused("native output overlaps frozen input")
        self.substrate = InstructionSurface(always_on_max)
        channel = ChannelSpec("noodle_owner", route.model, effort=route.effort,
                              timeout=route.timeout_s)
        self.spec = BenchSpec(name=route.group, runs_root=Path(runs_root),
                              install_mode="workspace_agents_md",
                              prompts=BenchPrompts(work="complete the original task using its source materials"),
                              channels={"proposer": channel, "selector": channel},
                              allow_subagents=False)
        # Freeze the actual baseline and exact editable paths, not arbitrary host roots.
        self._baseline_ref = tree_ref(self._base)
        initial = tree.read_tree(self.substrate.surface(), self._base)
        self._editable = frozenset(initial)
        self.validate_tree(initial)
        self._read_harness(self._base)
        self._source_refs = {key: tree_ref(value.source) for key, value in self._tasks.items()}
        self._results = {}
        self._calls = {}
        self._training_feedback = training_feedback
        self._feedback_blocked = False
        if training_feedback is not None:
            training_feedback.bind_tasks(self._source_refs, self._baseline_ref, output)

    def tasks(self, split="all"):
        if split != "all":
            raise Refused("task split is not part of this frozen increment")
        return sorted(self._tasks)

    def domain_of(self, task_id):
        self._tasks[task_id]
        return "original-task"

    def task_prompt(self, task_id):
        return self._tasks[task_id].prompt

    def task_files(self, task_id):
        source = self._tasks[task_id].source
        if tree_ref(source) != self._source_refs[task_id]:
            raise Refused("frozen task source changed")
        return source

    def base_harness(self):
        if tree_ref(self._base) != self._baseline_ref:
            raise Refused("baseline harness changed")
        return self._base

    def validate_tree(self, value):
        if set(value) != self._editable or "AGENTS.md" not in value:
            raise ValueError("candidate differs from the exact frozen editable paths")
        for name in value:
            if name != "AGENTS.md" and not re.fullmatch(r"skills/[a-z0-9][a-z0-9_-]*/SKILL\.md", name):
                raise ValueError("candidate contains a non-instruction edit")
        tree.validate(self.substrate.surface(), value, allow_subagents=False)

    def _read_harness(self, directory):
        reference = tree_ref(directory)
        rejected = []
        value = tree.read_tree(self.substrate.surface(), directory, rejected=rejected)
        if rejected:
            raise Refused("candidate contains off-surface files")
        self.validate_tree(value)
        return reference

    def execute(self, task_id, harness_dir, run_id, *, timeout=None):
        self.check_feedback_ready()
        harness = Path(harness_dir) if harness_dir is not None else self.base_harness()
        inputs = {"task_id": task_id, "prompt": self.task_prompt(task_id),
                  "source": tree_ref(self.task_files(task_id)), "harness": self._read_harness(harness),
                  "effort": self.route.effort, "timeout_s": timeout if timeout is not None else self.route.timeout_s}
        call = OwnerCall(self.route.group, run_id, "solver",
                         self.route.provider, self.route.model, inputs,
                         self.route.paid_reservation_usd)
        result = self.gate.execute(call)
        self._results[str(Path(result.run_dir))] = result
        self._calls[str(Path(result.run_dir))] = call
        return Path(result.run_dir)

    def check_feedback_ready(self):
        if self._feedback_blocked:
            raise Refused("training quality feedback is unresolved; owner dispatch is blocked")
        if self._training_feedback is not None:
            self._training_feedback.check_ready()

    def _result(self, run_dir):
        result = self._results.get(str(Path(run_dir)))
        if result is None or not result.settled:
            raise Refused("run directory has no original settled owner result")
        self.gate.verify_result(result)
        return result

    def delivered(self, run_dir):
        try:
            self._result(run_dir)
            return bool(self.deliverable_files(run_dir))
        except Refused:
            return False

    def trajectory(self, run_dir, *, obs_trunc):
        result = self._result(run_dir)
        # The original bridge projects one classified public event per JSON line.
        # Bound individual text observations while retaining every event and field.
        def clip(value):
            if isinstance(value, str) and len(value) > obs_trunc:
                return value[:obs_trunc] + " [truncated]"
            if isinstance(value, list):
                return [clip(item) for item in value]
            if isinstance(value, dict):
                return {key: clip(item) for key, item in value.items()}
            return value

        events = [clip(json.loads(event)) for event in
                  (Path(run_dir) / "trajectory.md").read_text().splitlines()]
        if self._training_feedback is not None:
            try:
                event = self._training_feedback(result, self._calls[str(Path(run_dir))],
                                                self.deliverable_files(run_dir), self.gate)
            except Exception:
                # Upstream catches trajectory errors and may fall back. Close dispatch
                # here as well, without changing its core or original owner evidence.
                self._feedback_blocked = True
                raise
            if event is not None:
                events.append(event)
        return "\n".join(json.dumps(event, ensure_ascii=False) for event in events)

    def deliverable_files(self, run_dir):
        self._result(run_dir)
        # Explicit agent-authored projection, not a scan of arbitrary owner state.
        value = json.loads((Path(run_dir) / "deliverables.json").read_text())
        if not isinstance(value, dict) or not all(isinstance(k, str) and isinstance(v, str)
                                                  for k, v in value.items()):
            raise Refused("deliverable projection is invalid")
        return value

    def deliverable_text(self, run_dir):
        return "\n\n".join(self.deliverable_files(run_dir).values())

    def score(self, run_dir, task_id):
        raise Refused("frozen independent acceptance owner is not implemented")


class OwnerProposer(Proposer):
    def __init__(self, adapter: NoodleAdapter, *, experiment_id: str):
        super().__init__(adapter.spec.channels["proposer"], attempts=1)
        if not experiment_id:
            raise Refused("experiment identity is missing")
        self.adapter, self.experiment_id = adapter, experiment_id

    def call(self, mission, context_dir, label, *, system="", timeout=None,
             max_attempts=None, harvest=None):
        # The native retry count is NOT permission to replay an unknown effect.
        # Different .a2 labels describe known-invalid edit repair, not transport retry.
        role = "selector" if label.startswith("select.") else "reflection" if label.startswith("experience.") else "proposer"
        inputs = {"mission": mission, "system": system, "context": tree_ref(context_dir),
                  "harvest": harvest, "effort": self.adapter.route.effort,
                  "timeout_s": timeout if timeout is not None else self.adapter.route.timeout_s}
        route = self.adapter.route
        run_id = self.experiment_id + "/" + label
        try:
            self.adapter.check_feedback_ready()
            result = self.adapter.gate.execute(OwnerCall(route.group, run_id, role,
                                              route.provider, route.model, inputs,
                                              route.paid_reservation_usd))
        except RuntimeError as exc:
            raise ProposerError(str(exc), attempts=1) from exc
        if harvest is not None:
            if harvest != "context/harness" or result.workspace is None:
                raise ProposerError("owner did not return the requested candidate harness", attempts=1)
            tree_ref(Path(result.workspace))  # symlinks refuse before native tree reader
        # Upstream requires a float; its defaults are never the accounting authority.
        # Unknown subscription USD remains null in the mandatory owner journal.
        reply = Reply(label, result.reply, True,
                      cost_usd=float(result.cost_usd) if result.cost_usd is not None else 0.0,
                      wall_s=result.wall_s if result.wall_s is not None else 0.0,
                      attempts=0 if result.cached else 1, cached=result.cached,
                      channel="noodle_owner", audit=Path(result.receipt_path),
                      workspace=Path(result.workspace) if result.workspace else None)
        self._calls.append(reply)
        return reply


def run_native_task(adapter, task_id, *, experiment_id):
    """Actual pinned core; this does not certify improvement or hidden acceptance."""
    if adapter.gate.owner is None:
        raise Refused("original-owner bridge is not implemented")
    adapter.check_feedback_ready()
    proposer = OwnerProposer(adapter, experiment_id=experiment_id)
    result = schedule.run_task(task_id, 1, adapter=adapter, prop=proposer,
                               selector_prop=proposer, runs_root=adapter.spec.runs_root,
                               modes=("seq",), library=None, libraries=None)
    adapter.check_feedback_ready()
    return {"native_result": result.to_dict(), "owner_journal": str(adapter.gate.journal),
            "owner_blocked": adapter.gate.state["blocked"],
            "paid_blocked": adapter.gate.state["paid_blocked"],
            "native_costs_authoritative": False, "native_time_authoritative": False,
            "hidden_acceptance": "not implemented",
            "adoption_authorized": False}
