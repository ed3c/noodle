"""Soodles CLI owner delegation; no Host, provider or optimizer implementation.

The caller allocates a finite set of original App worktrees and supplies the
Session review callback. Only Soodles' original next operation advances work.
"""
import hashlib
import json
import os
import re
import stat
import subprocess
import time
from dataclasses import asdict, dataclass, field, replace
from pathlib import Path, PurePosixPath
from typing import Callable

from owner_contract import (
    OwnerCall,
    OwnerResult,
    Refused,
    UnknownEffect,
    canonical,
    sha256,
    tree_ref,
)

MODELS = {"codex": "gpt-5.6-sol", "pi": "openrouter/deepseek/deepseek-v4.1-flash-1017"}
ADVANCE = {"original_task_dispatch", "original_checkpoint_readback", "original_initial_admission_readback"}
WAIT = {"original_task_readback"}


def require(condition, message):
    if not condition:
        raise Refused(message)


def unique(pairs):
    value = {}
    for key, item in pairs:
        require(key not in value, "JSON contains duplicate keys")
        value[key] = item
    return value


def decode(raw):
    def invalid(_):
        raise Refused("JSON contains a nonfinite number")
    return json.loads(raw, object_pairs_hook=unique, parse_constant=invalid)


def reference(path):
    path = Path(path)
    require(path.is_absolute() and path.resolve() == path, "source path is not canonical")
    return {"path": str(path), "sha256": sha256(path)}


def read_ref(ref):
    require(isinstance(ref, dict) and set(ref) == {"path", "sha256"}, "file reference is invalid")
    require(reference(ref["path"]) == ref, "bound file bytes changed")
    raw = Path(ref["path"]).read_bytes()
    require(hashlib.sha256(raw).hexdigest() == ref["sha256"], "bound file changed during read")
    return raw


def save(path, value):
    raw = canonical(value) + b"\n"
    with Path(path).open("xb") as stream:
        stream.write(raw)
        stream.flush()
        os.fsync(stream.fileno())
    descriptor = os.open(Path(path).parent, os.O_RDONLY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)
    return reference(path)


@dataclass(frozen=True)
class PinnedCLI:
    python: dict
    entrypoint: dict
    # Exact public source closure, supplied from the reviewed Soodles revision.
    sources: tuple[dict, ...]
    source_commit: str
    environment: dict
    timeout_s: float

    def verify(self):
        require(re.fullmatch(r"[0-9a-f]{40}", self.source_commit), "Soodles source commit is invalid")
        require(self.sources and self.entrypoint in self.sources, "Soodles entrypoint is not source-bound")
        for ref in (self.python, *self.sources):
            read_ref(ref)
        require(not any(k.startswith(("PYTHON", "GIT_")) or
                        any(word in k.upper() for word in ("TOKEN", "SECRET", "PASSWORD", "API_KEY"))
                        for k in self.environment), "CLI environment contains an unbound injection or secret")
        require(self.timeout_s > 0, "CLI operation timeout is invalid")

    def invoke(self, verb, request, evidence, *, capture=None):
        require(verb in {"enter", "inspect"}, "bridge operation is outside original owner scope")
        self.verify()
        read_ref(request)
        argv = [self.python["path"], "-B", self.entrypoint["path"], "loop", verb,
                "--request-file", request["path"]]
        if verb == "inspect":
            argv.append("--include-result")
            if capture is not None:
                argv += ["--capture-output", str(capture)]
        else:
            require(capture is None, "capture cannot advance an owner")
        evidence = Path(evidence)
        evidence.mkdir()  # no overwrite/retry of an original operation
        save(evidence / "intent.json", {"source_commit": self.source_commit,
             "python": self.python, "sources": self.sources, "request": request, "argv": argv})
        # An exception after this point is not proof that an enter was effect-free.
        try:
            with (evidence / "stdout.json").open("xb") as out, (evidence / "stderr.log").open("xb") as err:
                run = subprocess.run(argv, cwd=Path(self.entrypoint["path"]).parent,
                                     env={**self.environment, "PYTHONDONTWRITEBYTECODE": "1"},
                                     stdout=out, stderr=err, timeout=self.timeout_s, check=False)
            self.verify()
            read_ref(request)
            value = decode((evidence / "stdout.json").read_bytes())
            save(evidence / "receipt.json", {"exit_code": run.returncode,
                 "stdout": reference(evidence / "stdout.json"), "stderr": reference(evidence / "stderr.log")})
            require(run.returncode == 0 and isinstance(value, dict) and value.get("owner") == "soodles.loop"
                    and value.get("status") not in {"refused", "unknown", "failed"},
                    "original Soodles operation is unresolved or refused")
            if "request" in value:
                require(value["request"] == request, "original request identity differs")
            return value, reference(evidence / "stdout.json")
        except Exception as exc:
            try:
                save(evidence / "unknown.json", {"status": "unknown", "error_type": type(exc).__name__,
                                               "request": request, "operation": verb})
            except OSError as persistence_error:
                raise UnknownEffect("original Soodles operation unresolved; evidence persistence failed") from persistence_error
            raise UnknownEffect("original Soodles operation has no validated response; never replay") from exc


def skill_overlay(bases, names, harness, directory):
    """Preserve pinned skill assets; only native SKILL.md text is editable."""
    require(set(bases) == set(names), "fixed skill bases differ from explicit harness aliases")
    entries, receipts = [], []
    private = {".git", ".ssh", ".gnupg", ".aws", ".env"}
    leaves = {"auth.json", "credentials", "credentials.json", "credentials.toml"}
    for name in names:
        base = bases[name]
        root = Path(base["path"])
        require(root.is_absolute() and root.resolve() == root and root.is_dir(), "fixed skill base is unavailable")
        require(not any(part.casefold() in private or part.casefold().startswith(".env.")
                        for part in root.parts), "fixed skill base is in a private path")
        paths = sorted(root.rglob("*"))
        for path in paths:
            parts = [part.casefold() for part in path.relative_to(root).parts]
            require(not any(part in private or part.startswith(".env.") for part in parts)
                    and parts[-1] not in leaves and not parts[-1].endswith((".pem", ".key")),
                    "fixed skill base contains a private path")
            mode = path.lstat().st_mode
            require(stat.S_ISDIR(mode) or stat.S_ISREG(mode), "fixed skill base contains a link or special file")
            require(not mode & 0o7000, "fixed skill base contains unsupported mode bits")
        require(tree_ref(root) == base, "fixed skill base changed")
        target = directory / "skills" / name
        target.mkdir(parents=True)
        for path in paths:
            output = target / path.relative_to(root)
            if path.is_dir():
                output.mkdir(exist_ok=True)
            else:
                output.parent.mkdir(parents=True, exist_ok=True)
                output.write_bytes(path.read_bytes())
                output.chmod(stat.S_IMODE(path.stat().st_mode))
        candidate = reference(Path(harness["path"]) / "skills" / name / "SKILL.md")
        require((target / "SKILL.md").is_file(), "fixed skill base has no original SKILL.md")
        (target / "SKILL.md").write_bytes(read_ref(candidate))
        require(tree_ref(root) == base and tree_ref(harness["path"]) == harness,
                "skill input changed while materializing")
        entries.append(reference(target / "SKILL.md"))
        receipts.append({"name": name, "base": base, "candidate_skill": candidate, "prepared": tree_ref(target)})
    save(directory / "skill-overlay.json", receipts)
    return tuple(entries)


@dataclass(frozen=True)
class RequestTemplate:
    """One preallocated, fresh App control worktree; consumed once, never recycled."""
    group: str
    role: str
    repository: str
    selected_skill: dict | None
    support_skills: tuple[dict, ...]
    noodle: dict
    profile: dict
    write_paths: tuple[str, ...]
    acceptance: tuple[str, ...]
    # Return paths are original candidate files, not an arbitrary filesystem scan.
    deliverables: tuple[str, ...]
    harvest: str | None = None
    harness_skills: tuple[str, ...] = ()
    skill_bases: dict = field(default_factory=dict)

    def prepare(self, call, directory):
        require((self.group, self.role) == (call.group, call.role), "task template belongs to another call route")
        require(call.model == MODELS.get(call.provider) and call.inputs.get("effort") == "low",
                "call differs from fixed model or reasoning selection")
        require(self.write_paths and self.acceptance, "original write scope or acceptance is missing")
        selected, support = self.selected_skill, self.support_skills
        if call.role == "solver":
            require(selected is None and not support and self.harness_skills,
                    "solver has no explicit evaluated harness skill selection")
            require(len(set(self.harness_skills)) == len(self.harness_skills) and all(
                re.fullmatch(r"[a-z0-9][a-z0-9_-]*", name) for name in self.harness_skills),
                "evaluated skill aliases are invalid or duplicated")
            harness = call.inputs["harness"]
            require(tree_ref(harness["path"]) == harness, "evaluated harness changed")
            entries = skill_overlay(self.skill_bases, self.harness_skills, harness, directory)
            selected, support = entries[0], entries[1:]
        else:
            require(selected is not None and not self.harness_skills and not self.skill_bases,
                    "improver skill selection is ambiguous")
        for ref in (selected, *support, self.noodle, self.profile):
            read_ref(ref)
        require(not (Path(self.repository) / ".noodle").exists(), "control worktree already has an owner")
        call_ref = save(directory / "call.json", asdict(call))
        sources = [call_ref]
        for key in ("source", "harness", "context"):
            if key not in call.inputs:
                continue
            ref = call.inputs[key]
            require(tree_ref(ref["path"]) == ref, "original native input tree differs")
            sources.extend(reference(path) for path in sorted(Path(ref["path"]).rglob("*")) if path.is_file())
        goal = (call.inputs["prompt"] if call.role == "solver" else
                call.inputs.get("system", "") + "\n\n" + call.inputs["mission"])
        if call.role == "solver":
            goal += ("\n\n# Evaluated Harness² instructions\n"
                     "Apply the evaluated harness AGENTS.md and selected/support skills using the runtime's "
                     "Materialized task instructions mapping. The original harness source is "
                     + str(Path(call.inputs["harness"]["path"]) / "AGENTS.md") + ".")
        request = {"schema": 1, "mode": "inner-only", "repository": self.repository,
            "goal": goal, "output": str(directory / "owner"),
            "selected_skill": selected["path"],
            "support_skills": [ref["path"] for ref in support],
            "model": call.model, "model_reasoning_effort": "low", "runtime": "openshell",
            "provider": call.provider, "profile": self.profile["path"], "noodle": self.noodle["path"],
            "write_paths": list(self.write_paths), "acceptance": list(self.acceptance),
            "source_refs": list(dict.fromkeys(ref["path"] for ref in sources))}
        save(directory / "binding.json", {"call_sha256": call.fingerprint(), "template": asdict(self),
                                         "inputs": sources})
        save(directory / "instruction-trees.json", [tree_ref(Path(ref["path"]).parent)
            for ref in (selected, *support)])
        return save(directory / "request.json", request)


def public_trace(descriptor):
    """Use the original capture's public event classification; retain raw separately."""
    raw = read_ref(descriptor["raw"])
    lines, public, replies = raw.splitlines(), [], []
    require(not descriptor.get("gaps") and descriptor.get("terminal"), "capture has no complete native terminal")
    for event in descriptor["inventory"]:
        if event["surface"] != "public":
            continue
        line = lines[event["line"] - 1]
        require(hashlib.sha256(line).hexdigest() == event["line_sha256"], "public event bytes differ")
        row = decode(line)
        # Only explicitly classified public leaves enter optimization context.
        leaves = []
        for path in event.get("public_entries", []):
            leaf = row
            for key in path:
                leaf = leaf[key]
            leaves.append({"path": path, "value": leaf})
        public.append({"event_id": event["event_id"], "entries": leaves} if leaves else
                      {"event_id": event["event_id"], "record": row})
        if descriptor["provider"] == "pi":
            message = row.get("message", {})
            if message.get("role") == "assistant":
                replies.extend(part["text"] for part in message.get("content", []) if part.get("type") == "text")
        else:
            payload = row.get("payload", {})
            if payload.get("type") == "agent_message" and isinstance(payload.get("message"), str):
                replies.append(payload["message"])
            if payload.get("type") == "task_complete" and isinstance(payload.get("last_agent_message"), str):
                replies.append(payload["last_agent_message"])
    require(replies, "original captured assistant reply is unavailable")
    return "\n".join(json.dumps(row, ensure_ascii=False) for row in public), replies[-1]


def relative(name):
    path = PurePosixPath(name)
    require(isinstance(name, str) and name and not path.is_absolute() and ".." not in path.parts
            and path.as_posix() == name and ".git" not in path.parts and "\\" not in name,
            "candidate path is outside the output scope")
    return path


def snapshot_candidate(call, template, observation, directory):
    inner, capture = observation["inner"], observation["capture"]
    candidate_ref = inner["candidate"]
    candidate = decode(read_ref(candidate_ref))
    require(capture.get("owner") == "issue-execution.local-task.capture" and capture.get("status") == "observed"
            and not capture.get("gaps") and capture.get("request") == observation["request"],
            "original attempt capture is incomplete")
    require(candidate["task_envelope"] == inner["task_envelope"] == capture["task_envelope"],
            "candidate and capture belong to different tasks")
    envelope = decode(read_ref(inner["task_envelope"]))
    request = decode(read_ref(observation["request"]))
    require(envelope.get("provider") == call.provider and envelope.get("model") == call.model
            and envelope.get("runtime") == "openshell" and envelope.get("model_reasoning_effort") == "low"
            and envelope.get("control_root") == template.repository and envelope.get("profile") == template.profile
            and envelope.get("noodle") == template.noodle and envelope.get("goal") == request["goal"]
            and envelope.get("source_refs") == [reference(path) for path in request["source_refs"]],
            "captured task route or original request differs")
    attempts = capture["attempts"]
    require(len(attempts) == 1, "original call has multiple attempts; retries are outside this driver")
    attempt = attempts[0]
    require(attempt.get("status") == "observed" and not attempt.get("gaps"), "original attempt has capture gaps")
    for key in ("order_id", "attempt_id", "session_id"):
        require(attempt["binding"].get(key) == candidate[key], "candidate canonical attempt differs")
    proof = attempt["native_completion"]
    require(proof["candidate_head"] == candidate["head"] and proof["native_exec_exit_code"] == 0
            and proof["native_phase"] == "Stopped" and proof["stop_acknowledged"] is True
            and proof["main_process_instance_id"] and proof["native_attempt_id"],
            "original native completion is unresolved")
    require(all(proof.get(k) == v for k, v in candidate["native_execution"].items()),
            "candidate and captured native completion differ")
    saved_capture = decode(read_ref(capture["observation"]))
    require(saved_capture == {k: v for k, v in capture.items() if k != "observation"}, "saved capture differs")
    threads = attempt["capture"]["threads"]
    require(len(threads) == 1 and threads[0]["thread_id"] == attempt["capture"]["root_thread_id"],
            "capture contains a different thread or subagent")
    descriptor = decode(read_ref(threads[0]["descriptor"]))
    require(descriptor["provider"] == call.provider and descriptor["native_completion"] == proof["observation"],
            "native thread completion differs")
    trace, reply = public_trace(descriptor)
    retained = directory / "evidence"
    retained.mkdir()
    refs = [capture["observation"], capture["snapshot"], candidate_ref]
    refs += [row["saved"] for row in capture["source_custody"]]
    refs += [ref for thread in threads for key, ref in thread.items() if key in {"raw", "descriptor", "review"}]
    custody = []
    for ref in refs:
        data = read_ref(ref)
        target = retained / ref["sha256"]
        if not target.exists():
            target.write_bytes(data)
        custody.append({"original": ref, "saved": reference(target)})
    save(retained / "index.json", custody)
    workspace = directory / "workspace"
    workspace.mkdir()
    for name, metadata in candidate["files"].items():
        relative(name)
        require(any(name == scope or name.startswith(scope.rstrip("/") + "/")
                    for scope in template.write_paths), "candidate includes an out-of-scope path")
        if metadata is None:
            continue
        original = Path(candidate["worktree"]) / name
        data = read_ref({"path": str(original), "sha256": metadata["sha256"]})
        require(bool(original.stat().st_mode & 0o111) == metadata["executable"], "candidate file mode differs")
        target = workspace / name
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(data)
        target.chmod(0o755 if metadata["executable"] else 0o644)
    deliverables = {}
    for name in template.deliverables:
        relative(name)
        require(name in candidate["files"] and candidate["files"][name] is not None, "declared deliverable is unavailable")
        deliverables[name] = (workspace / name).read_text()
    save(directory / "deliverables.json", deliverables)
    (directory / "trajectory.md").write_text(trace)
    save(directory / "native-evidence.json", {"candidate": candidate_ref, "capture": capture["observation"],
         "descriptor": threads[0]["descriptor"], "native_completion": proof,
         "billing": descriptor.get("cost"), "cost_usd": None, "wall_s": None})
    harvested = None
    if template.harvest is not None:
        relative(template.harvest)
        harvested = workspace / template.harvest
        require(harvested.is_dir() and call.inputs.get("harvest") == "context/harness",
                "native requested harness is unavailable")
    return candidate_ref, reply, str(harvested) if harvested else None


class SoodlesOwnerBridge:
    def __init__(self, cli: PinnedCLI, root: Path, templates: list[RequestTemplate], *,
                 review: Callable[[OwnerCall, dict, dict], None], billing=None, poll_s=1.0):
        self.cli, self.root, self.templates, self.review = cli, Path(root), list(templates), review
        require(poll_s > 0, "owner polling interval is invalid")
        self.poll_s, self.billing = poll_s, billing
        self.root.mkdir(parents=True, exist_ok=False)
        require(len({t.repository for t in templates}) == len(templates), "control worktree pool contains duplicates")

    def execute(self, call: OwnerCall) -> OwnerResult:
        require(self.templates, "preallocated original control worktrees are exhausted")
        template = self.templates[0]
        self.cli.verify()
        directory = self.root / call.fingerprint()
        directory.mkdir()  # a prior interrupted call cannot be replayed
        request = template.prepare(call, directory)
        binding_ref = reference(directory / "binding.json")
        instructions_ref = reference(directory / "instruction-trees.json")
        self.templates.pop(0)
        result_dir = directory / "result"
        result_dir.mkdir()
        sequence, advanced, reviewed, frozen = 0, set(), set(), None
        before = None
        if call.provider == "pi":
            require(self.billing is not None, "original paid key readback supplier is missing")
            from openrouter_billing import BillingUnavailable
            try:
                before = self.billing.begin(call, directory / "billing-before")
            except BillingUnavailable as exc:
                raise Refused("paid key preflight unavailable; no owner call started") from exc

        def invoke(verb, capture=None):
            nonlocal sequence
            for key in ("source", "harness", "context"):
                if key in call.inputs:
                    require(tree_ref(call.inputs[key]["path"]) == call.inputs[key], "native input tree differs")
            binding = decode(read_ref(binding_ref))
            require(binding["call_sha256"] == call.fingerprint(), "original call binding differs")
            for ref in binding["inputs"]:
                read_ref(ref)
            for ref in decode(read_ref(instructions_ref)):
                require(tree_ref(ref["path"]) == ref, "original instruction tree differs")
            value, ref = self.cli.invoke(verb, request, directory / f"operation-{sequence:04d}", capture=capture)
            sequence += 1
            for source in binding["inputs"]:
                read_ref(source)
            return value, ref

        try:
            observation, observation_ref = invoke("enter")
            while True:
                inner = observation.get("inner", {})
                require(inner.get("owner") == "issue-execution.local-task", "original local-task owner is missing")
                require(inner.get("status") not in {"failed", "cancelled", "unknown", "refused"},
                        "original task is unresolved")
                candidate = inner.get("candidate")
                if candidate and frozen is None:
                    observation, observation_ref = invoke("inspect", directory / "capture")
                    frozen = snapshot_candidate(call, template, observation, result_dir)
                    inner = observation["inner"]
                if frozen is not None:
                    progress = inner.get("review_progress") or {}
                    selection = progress.get("review") or {}
                    require((inner.get("candidate") or selection.get("candidate")) == frozen[0],
                            "original reviewed candidate changed")
                    require(not selection or selection.get("decision") == "approve",
                            "Session selected a revision; no automatic replacement call is authorized")
                    require(progress.get("status") not in {"needs_native_recovery", "revision_running", "revision_ready"},
                            "original review requires recovery or a different attempt")
                    if progress.get("status") == "locally_integrated":
                        require(selection.get("candidate") == frozen[0] and selection.get("decision") == "approve",
                                "integrated result has no original candidate-bound approval")
                        require(inner.get("acceptance") == "PASS", "original integrated acceptance is unresolved")
                        receipt = save(directory / "settled.json", {"call_sha256": call.fingerprint(),
                            "request": request, "candidate": frozen[0], "original_review": observation_ref,
                            "result": tree_ref(result_dir), "cost_usd": None, "wall_s": None,
                            "billing_authority": "unresolved", "hidden_acceptance": "not evaluated"})
                        result = OwnerResult(str(result_dir), tree_ref(result_dir)["sha256"], frozen[1], frozen[2],
                                             receipt["path"], receipt["sha256"], True, None, None)
                        if before is not None:
                            bill_ref = self.billing.settle(call, before, result, directory / "billing-after")
                            bill = decode(read_ref(bill_ref))
                            save(directory / "billing-status.json", {"before": before, "settlement": bill_ref})
                            if bill.get("status") == "settled":
                                result = replace(result, cost_usd=bill["cost_usd"], billing_receipt=bill_ref)
                        return result
                    if frozen[0]["sha256"] not in reviewed:
                        reviewed.add(frozen[0]["sha256"])
                        self.review(call, observation, observation_ref)
                else:
                    action = observation.get("next") or inner.get("next") or {}
                    operation = action.get("operation") if isinstance(action, dict) else action
                    require(operation in ADVANCE | WAIT, "original next operation needs Session resolution")
                    # Native checkpoint identity excludes path/publication noise.
                    checkpoint = hashlib.sha256(canonical({"operation": operation, "order": inner.get("order"),
                        "host": {key: (inner.get("native_host") or {}).get(key) for key in
                            ("host_instance_id", "context_id", "request_id", "context_state")},
                        "runtime_mode": inner.get("runtime_mode")})).hexdigest()
                    if operation in ADVANCE and checkpoint not in advanced:
                        advanced.add(checkpoint)
                        observation, observation_ref = invoke("enter")
                        continue
                time.sleep(self.poll_s)
                observation, observation_ref = invoke("inspect")
        except Exception as exc:
            raise UnknownEffect("original Soodles call is unresolved; preserve evidence and do not dispatch again") from exc
