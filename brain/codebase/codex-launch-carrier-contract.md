# Codex launch carrier contract

Observed for ed3c/noodle#78 on 2026-09-16. The process dispatcher inherited an
unconditional `--dangerously-bypass-approvals-and-sandbox` from `codexBaseArgs`.
The managed carrier rejected its approval/sandbox combination before Agent
events. Non-interactive `exec` does not require that override.

- Shared Codex argv owns the execution protocol. Process dispatch preserves host
  configuration and existing `agents.codex.args`; Codex validates those values.
- Sprite keeps its existing explicit override at its own argument producer.
  This preserves that carrier's behavior; it is not a new Sprite runtime proof.
- A pre-event Codex failure retains the first raw `Error:` diagnostic in the
  session outcome after stderr persistence, with `codex exec --help` as the
  supported next entry. Unknown failures, cancellation, other providers and
  failures after initialization keep their existing classification.
- Neither this diagnostic nor a CLI exit code authorizes an unchanged retry.
  Bootstrap retry/exhaustion belongs to the loop owner and is outside this fix.

The nearest controls are `dispatcher/codex_launch_test.go`. They distinguish
permission injection, lost diagnostics and changes to the Sprite caller. They
are not authenticated Agent execution receipts.

A real process-dispatch probe with an invalid sandbox value failed before Agent
events and preserved the CLI's exact field/value. The permitted-config probe
timed out after 90 seconds with no Agent events; both disposable Noodle
worktrees were cleaned through `noodle worktree cleanup`. Installed CLI
0.154.0-alpha.3 and a separate 0.153.3 direct probe have not produced a successful
Agent execution in this carrier. No model authentication failure has been
established from these timeouts. Do not infer that removing the flag completes
#78 or unblocks Soodles #18.

ChatGPT GitHub connector transport plus ordinary Actions runtime tests is a
different path from starting another Codex process inside a runner. Each actual
Agent carrier requires its own successful launch and typed-outcome receipt.

An Actions preflight incorrectly required `secrets.OPENAI_API_KEY` before any
Noodle runtime execution. Its missing-input result was not a runtime failure.
API keys are not required for the supervised connector plus Actions path, and
must not become an admission rule for Noodle worktree operations. The corrected
workflow builds the exact candidate and reuses the fixed external Soodles
worktree control at `0256f2923e978b989e25df07c74db4370d343312`, checking its bytes
before execution. That control strips inherited credentials from subprocesses
and verifies real create/exec, absent-target refusal before side effects, and
cleanup with Git readback. It emits separate runtime and nested-Agent verdicts.
This is non-authorizing runtime evidence, not the full Soodles acceptance or an
isolated landing judge. A successful worktree probe cannot establish successful
Codex inference, scheduling, or resolution of the managed carrier timeout.
