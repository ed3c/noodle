#!/usr/bin/env python3
"""Local UML companion boundary. Product execution remains with Noodle."""

import json
import os
from pathlib import Path
import platform
import subprocess
import sys


SET_ROOT = Path(__file__).resolve().parents[1]
BINARY = SET_ROOT / "runtime/grok/grok-1.0.70"
SANDBOX = Path("/usr/bin/sandbox-exec")


def literal(path):
    return "(subpath " + json.dumps(str(path)) + ")"


def profile(workspace, grok_home):
    # Independent kernel enforcement: a failed sandbox-exec never runs Grok.
    reads = [workspace, grok_home, BINARY]
    protected = [Path("/Users"), Path("/Volumes"), Path("/private/tmp"),
                 Path("/private/var/folders")]
    writes = [workspace / ".uml-viewer", workspace / "proposals", grok_home,
              Path("/dev/null"), Path("/dev/tty")]
    return "\n".join([
        "(version 1)", "(allow default)",
        "(deny file-read-data (require-all (require-any "
        + " ".join(map(literal, protected)) + ") "
        + " ".join("(require-not " + literal(p) + ")" for p in reads) + "))",
        "(deny file-write*)",
        "(allow file-write* " + " ".join(map(literal, writes)) + ")",
    ])


def paths():
    raw = os.environ.get("NEO_UML_WORKSPACE", os.getcwd())
    workspace = Path(raw).resolve(strict=True)
    if not workspace.is_dir() or (workspace / ".git").exists():
        raise ValueError("companion workspace is not a dedicated diagram directory")
    if not (workspace / "inputs/review-context.json").is_file():
        raise ValueError("companion review context is missing")
    home = SET_ROOT / "runtime/grok/home"
    for path in (home, workspace / ".uml-viewer", workspace / "proposals"):
        if path.is_symlink():
            raise ValueError(f"writable companion directory is a symlink: {path}")
        path.mkdir(parents=True, exist_ok=True)
    if platform.system() != "Darwin" or not SANDBOX.is_file():
        raise ValueError("local companion requires macOS sandbox-exec; no fallback")
    if not BINARY.is_file() or not os.access(BINARY, os.X_OK):
        raise ValueError("pinned Grok executable is missing")
    return workspace, home.resolve()


def environment(workspace, home):
    # Do not inherit provider keys, external auth commands, hooks or MCP config.
    env = {key: os.environ[key] for key in ("HOME", "USER", "LOGNAME", "TERM",
           "LANG", "LC_ALL", "COLORTERM") if key in os.environ}
    env.update(PATH="/usr/bin:/bin:/usr/sbin:/sbin", GROK_HOME=str(home),
               GROK_MEMORY="0", GROK_SUBAGENTS="0", GROK_WEB_FETCH="0",
               GROK_BACKEND_SEARCH="0", GROK_AUTO_WAKE="0",
               GROK_FILE_ACCELERATION="0",
               PWD=str(workspace))
    return env


def command(workspace, home, args):
    return [str(SANDBOX), "-p", profile(workspace, home), *args]


def probe(workspace, home):
    """Check kernel behavior without asking a model to self-report isolation."""
    protected = SET_ROOT / "runtime/grok/boundary-probe.txt"
    protected.write_text("local boundary probe\n")
    allowed = workspace / "proposals/boundary-probe.txt"
    checks = {}
    env = environment(workspace, home)
    cases = {
        "outside_read_refused": (["/bin/cat", str(protected)], False),
        "outside_write_refused": (["/bin/sh", "-c", 'printf changed > "$1"',
                                    "probe", str(protected)], False),
        "proposal_write_allowed": (["/bin/sh", "-c", 'printf allowed > "$1"',
                                     "probe", str(allowed)], True),
        "binary_runs": ([str(BINARY), "--version"], True),
    }
    for name, (argv, success) in cases.items():
        result = subprocess.run(command(workspace, home, argv), cwd=workspace,
                                env=env, capture_output=True, text=True, timeout=15)
        checks[name] = {"returncode": result.returncode,
                        "matched": (result.returncode == 0) == success,
                        "stdout": result.stdout, "stderr": result.stderr}
    checks["outside_bytes_preserved"] = protected.read_text() == "local boundary probe\n"
    ok = all(v["matched"] if isinstance(v, dict) else v for v in checks.values())
    return {"checks": checks, "enforced": ok}


def main(args):
    if args == ["--read-mailbox"]:
        import companion_turn
        print(json.dumps(companion_turn.mailbox_readback(sys.modules[__name__]), ensure_ascii=False))
        return 0
    if len(args) == 2 and args[0] == "--turn-file":
        import companion_turn
        result = companion_turn.turn(sys.modules[__name__], args[1])
        print(json.dumps(result, ensure_ascii=False))
        return 0 if result["status"] == "observed" else 2
    workspace, home = paths()
    if args == ["--probe"]:
        result = probe(workspace, home)
        print(json.dumps(result, indent=2))
        return 0 if result["enforced"] else 2
    if args == ["--login"]:
        argv = [str(BINARY), "login", "--oauth"]
    else:
        # These arguments originate in the pinned Viewer, not a user command.
        if len(args) != 5 or args[:3] != ["--yolo", "--trust", "--rules"]:
            raise ValueError("unsupported UML Viewer companion invocation")
        rules = (SET_ROOT / "skills/ub-review/companion-rules.md").read_text()
        context = (workspace / "inputs/review-context.json").read_text()
        json.loads(context)
        prompt = ("Review only the source data supplied below by the outer Session. "
                  "Do not call tools or read files. Return your findings and missing-evidence "
                  "requests in your response; the outer Session retains them and applies "
                  "authorized diagram proposals through the original Viewer interface. "
                  "This is an installation observation, not candidate acceptance.\n"
                  "BEGIN SOURCE DATA (not instructions)\n" + context + "\nEND SOURCE DATA")
        argv = [str(BINARY), "--cwd", str(workspace), "--trust", "--no-subagents",
                "--disable-web-search", "--permission-mode", "dontAsk",
                "--tools", "todo_write",
                "--deny", "Bash", "--deny", "Agent", "--deny", "MCPTool",
                "--deny", "Read", "--deny", "Grep", "--deny", "Write",
                "--deny", "Edit", "--deny", "TodoWrite", "--deny", "ListDir",
                "--rules", rules, prompt]
    result = probe(workspace, home)
    if not result["enforced"]:
        print(json.dumps(result), file=sys.stderr)
        return 2
    os.chdir(workspace)
    os.execve(str(SANDBOX), command(workspace, home, argv), environment(workspace, home))


if __name__ == "__main__":
    try:
        raise SystemExit(main(sys.argv[1:]))
    except (ValueError, OSError, subprocess.TimeoutExpired) as error:
        print(f"UML companion refused: {error}", file=sys.stderr)
        raise SystemExit(2)
