"""One durable prompt-only Grok turn. This journal owns no product task state."""
import fcntl
import hashlib
import json
from pathlib import Path
import subprocess
import uuid
import os


RESPONSE_SCHEMA = {
    "type": "object", "additionalProperties": False,
    "properties": {
        "summary": {"type": "string"},
        "requests": {"type": "array", "minItems": 1, "maxItems": 16, "items": {
            "type": "object", "additionalProperties": False,
            "properties": {
                "kind": {"type": "string", "enum": ["evidence", "metrics", "repair", "policy", "no_change"]},
                "command_ids": {"type": "array", "items": {"type": "integer"}},
                "reason": {"type": "string"},
                "requested_outcome": {"type": "string"}},
            "required": ["kind", "command_ids", "reason", "requested_outcome"]}}},
    "required": ["summary", "requests"]}


def save(path, value):
    raw = (json.dumps(value, ensure_ascii=False, sort_keys=True) + "\n").encode()
    with path.open("xb") as output:
        output.write(raw)
        output.flush()
        os.fsync(output.fileno())
    sync_directory(path.parent)


def sync_directory(path):
    descriptor = os.open(path, os.O_RDONLY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def response(raw):
    native = json.loads(raw)
    if not isinstance(native, dict) or native.get("is_error") is True:
        raise ValueError("native Grok turn did not return a successful result")
    if native.get("stopReason") != "end_turn" or not isinstance(native.get("sessionId"), str):
        raise ValueError("native Grok turn is incomplete")
    value = native.get("structuredOutput")
    if value is None and isinstance(native.get("text"), str):
        value = json.loads(native["text"])
    if not isinstance(value, dict) or set(value) != {"summary", "requests"}:
        raise ValueError("native Grok result has no structured review response")
    if not isinstance(value["summary"], str) or not isinstance(value["requests"], list):
        raise ValueError("Grok response shape changed")
    if not 1 <= len(value["requests"]) <= 16:
        raise ValueError("Grok response exceeds the bounded request count")
    for request in value["requests"]:
        if (not isinstance(request, dict) or set(request) != {"kind", "command_ids", "reason", "requested_outcome"}
                or request["kind"] not in RESPONSE_SCHEMA["properties"]["requests"]["items"]["properties"]["kind"]["enum"]
                or not all(isinstance(request[k], str) and request[k].strip() for k in ('reason', 'requested_outcome'))
                or not isinstance(request['command_ids'], list)
                or not all(type(value) is int for value in request['command_ids'])):
            raise ValueError("Grok request is invalid")
    return native, value


def mailbox_readback(boundary):
    workspace, home = boundary.paths()
    source = workspace / '.uml-viewer/to-agent.edn'
    raw = source.read_bytes() if source.exists() else None
    env = {**os.environ, 'JAVA_HOME': str(boundary.SET_ROOT / 'runtime/java'),
           'CLJ_CONFIG': str(boundary.SET_ROOT / 'runtime/clojure-config')}
    deps = '{:deps {neo/uml-viewer {:local/root ' + json.dumps(str(boundary.SET_ROOT / 'tools/uml-viewer')) + '}}}'
    run = subprocess.run([str(boundary.SET_ROOT / 'runtime/clojure/bin/clojure'), '-Sdeps', deps,
                          '-M', str(boundary.SET_ROOT / 'scripts/mailbox_readback.clj'), str(workspace)],
                         cwd=workspace, env=env, capture_output=True, text=True, timeout=30)
    if run.returncode:
        raise ValueError('original UML mailbox read failed: ' + run.stderr[-2000:])
    if (source.read_bytes() if source.exists() else None) != raw:
        raise ValueError('original UML mailbox changed during readback')
    ids = [int(item) for item in run.stdout.strip().split(',') if item]
    return {'status': 'observed' if raw is not None else 'absent', 'command_ids': ids,
            'source': {'path': str(source), 'sha256': hashlib.sha256(raw).hexdigest()} if raw is not None else None,
            'commands_edn': raw.decode() if raw is not None else None}


def bound_response(raw, context, session):
    native, value = response(raw)
    if native['sessionId'] != session:
        raise ValueError('Grok native session identity differs from intent')
    ids = set(context.get('viewer_mailbox', {}).get('command_ids', []))
    if any(set(request['command_ids']) - ids for request in value['requests']):
        raise ValueError('Grok response invented a Viewer command identity')
    return value


def turn(boundary, input_file):
    workspace, home = boundary.paths()
    raw = Path(input_file).read_bytes()
    if len(raw) > 1024 * 1024:
        raise ValueError("companion prompt exceeds 1 MiB")
    context = json.loads(raw)
    if context.get("schema") != 1 or context.get("kind") != "outer_companion_context":
        raise ValueError("original outer companion context is missing")
    if context.get("scope") == "candidate_final_review":
        references = context.get("companion_sources")
        if not isinstance(references, list) or not references:
            raise ValueError("final review has no bound companion sources")
        for reference in references:
            source = Path(reference["path"])
            if (not source.is_relative_to(boundary.SET_ROOT) or source.resolve() != source
                    or hashlib.sha256(source.read_bytes()).hexdigest() != reference["sha256"]):
                raise ValueError("companion source changed before native execution")
    digest = hashlib.sha256(raw).hexdigest()
    root = workspace / ".uml-viewer/outer-turns" / digest
    root.mkdir(parents=True, exist_ok=True)
    if root.resolve() != root:
        raise ValueError("companion capture path is a symlink")
    sync_directory(workspace)
    sync_directory(root.parent.parent)
    sync_directory(root.parent)
    readback = {"owner": "Noodle.ub-review.companion", "context_sha256": digest,
                "capture_directory": str(root), "authorizes_landing": False}
    with (root / "turn.lock").open("a+b") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        if (root / "result.json").exists():
            original = json.loads((root / "result.json").read_bytes())
            if original.get("exit_code") == 0:
                try:
                    value = bound_response((root / "stdout.json").read_bytes(), context, original['session_id'])
                    current = {k: v for k, v in original.items() if k != "problem"}
                    return {**current, "status": "observed", "response": value,
                            "capture_readback": {"original_status": original["status"],
                                "original_problem": original.get("problem"),
                                "source": str(root / "result.json"), "model_replayed": False}}
                except (ValueError, TypeError):
                    pass  # Preserve the original terminal failure when capture still cannot be interpreted.
            return original
        if (root / "intent.json").exists():
            return {**readback, "status": "unknown", "intent": json.loads((root / "intent.json").read_bytes()),
                    "next": {"owner": "Noodle.ub-review.companion", "operation": "read_original_capture"}}
        rules = (boundary.SET_ROOT / "skills/ub-review/companion-rules.md").read_text()
        prompt = ("Review this outer-supplied context. Do not call tools or read files. "
                  "Return only the requested structured response. Preserve missing evidence. "
                  "This does not authorize any product effect.\nBEGIN SOURCE DATA\n"
                  + raw.decode() + "\nEND SOURCE DATA")
        probe = boundary.probe(workspace, home)
        if not probe["enforced"]:
            raise ValueError("companion kernel boundary probe failed")
        session = str(uuid.uuid4())
        save(root / "context.json", context)
        save(root / "intent.json", {"session_id": session, "context_sha256": digest})
        argv = [str(boundary.BINARY), "--cwd", str(workspace), "--trust", "--no-subagents",
                "--disable-web-search", "--permission-mode", "dontAsk", "--tools", "todo_write",
                "--deny", "Bash", "--deny", "Agent", "--deny", "MCPTool", "--deny", "Read",
                "--deny", "Grep", "--deny", "Write", "--deny", "Edit", "--deny", "TodoWrite",
                "--deny", "ListDir", "--session-id", session, "--max-turns", "1",
                "--rules", rules, "--json-schema", json.dumps(RESPONSE_SCHEMA),
                "--output-format", "json", "-p", prompt]
        with (root / "stdout.json").open("xb") as stdout, (root / "stderr.txt").open("xb") as stderr:
            try:
                run = subprocess.run(boundary.command(workspace, home, argv), cwd=workspace,
                                     env=boundary.environment(workspace, home), stdout=stdout,
                                     stderr=stderr, timeout=180)
            except (OSError, subprocess.TimeoutExpired):
                # Intent survives unknown execution; a second call never launches another turn.
                raise
        result = {**readback, "status": "failed", "session_id": session, "exit_code": run.returncode}
        if run.returncode == 0:
            try:
                value = bound_response((root / "stdout.json").read_bytes(), context, session)
                result.update(status="observed", response=value)
            except (ValueError, TypeError) as error:
                result["problem"] = str(error)
        save(root / "result.json", result)
        return result
