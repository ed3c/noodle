# Noodle #78: macOS ProcessDispatcher capability evidence

This is a publication of an already-run local experiment, not a new run.
Candidate: `ed3c/noodle@e6d712a2512547cdf78a85c1ee218e1ba52e3b27`.
Candidate tree: `87c23adcc96b1bf10a1fd8983409cd80e3713310`.
Carrier: macOS arm64, Codex CLI 0.153.4, existing ChatGPT login, gpt-5.6-sol.
The source candidate was not edited.

## Readback order

1. [receipt.json](receipt.json): original scoped receipt, SHA-256 `24a67f60c1c440b4ce026741ed3bb7ca9829532a1d4482aa965e036199a522d3`.
2. [sha256.json](sha256.json): original file digest map; verify the named files before interpreting them.
3. [process-probe.go](process-probe.go): external observer using the actual candidate ProcessDispatcher and read-only event reader. It does not emit the Agent outcome.
4. [permitted/session/raw.ndjson](permitted/session/raw.ndjson): Codex thread, actual command execution, exit 0, final message and completed turn.
5. [permitted/session/events.ndjson](permitted/session/events.ndjson): Noodle event log, including Agent-emitted stage_message.
6. [permitted/session/codex-startup.json](permitted/session/codex-startup.json): parent stdin and canonical initialization observations.
7. [invalid.json](invalid.json), [invalid/session/stderr.log](invalid/session/stderr.log): illegal sandbox value rejected with an actionable owner diagnostic, no Agent events and no matching typed outcome.
8. [canonical-check.json](canonical-check.json), [canonical-check.log](canonical-check.log): exact candidate pnpm check, exit 0 in 61.028987666999456 seconds, clean source.
9. [physical-checks.json](physical-checks.json), [cleanup.json](cleanup.json): session identity checks, process absence observations, actual Noodle worktree/branch removal and surviving Git readbacks.
10. [setup.json](setup.json): disposable fixture creation, Noodle worktree creation, Git identity and redacted Codex doctor result.

The full observer stdout for each arm is in permitted.json / invalid.json.
Their execution records preserve actual observer argv, exit and elapsed time.
Both raw session directories, including empty negative-arm traces, are retained.
Install, build and clone logs are also included.

## Evidence subject and limits

Successful Noodle session: `launch-probe-20260916-142217-4fc5f5`.
Codex thread: `01a0aa98-d71b-7af0-9412-88cb7c9c7eab`.
Exactly one matching outcome: order `launch-capability-78`, stage 0,
outcome `completed`, blocking false, same actual Noodle session.
Observed elapsed: 21.300518833 seconds, within the unchanged 90-second deadline.

The invalid configuration refusal is a **GREEN rejection control**.
The permitted real execution is a **GREEN legal case**.
**No old-implementation or planted-defect RED arm was run in this macOS experiment.**
This packet does not claim a newly demonstrated RED/cure GREEN/non-case GREEN trilogy.

The process/PID-group absence checks were executed and retained as booleans tied
to process.json PIDs. The full ps snapshot was not archived; it cannot be
reconstructed as contemporaneous raw evidence. The original local verification
script that computed physical-checks.json was executed in the supervising shell;
its separate source file was not persisted. The actual Noodle/Codex raw session
logs and the dispatcher probe source are preserved here.

Binary SHA-256 values are recorded; binary files are not included in this text
evidence packet. No independently reproducible-build attestation is claimed.
The receipt's provider CI summaries came from connector readback during that
session; full provider responses were not archived in this packet. They are not
a claim of required-check enforcement.

Codex doctor reported TERM=dumb as FAIL in the noninteractive pipe environment;
auth/config/repository identity and WebSocket checks passed. Actual execution
succeeded. Nonfatal hook/plugin/state warnings remain in the raw traces.

Noodle's normalized zero token counters must not be interpreted as zero cost.
The raw Codex completed-turn usage is preserved separately.

## Current routing clarification

The original receipt's next_owner_input is preserved as historical text, not
an authorization gate. The user clarified, and Soodles #18 was read back at
updated_at 2026-09-16T14:43:39Z, that unproven Work Mode execution must not
blanket-block admission implementation for a separately verified macOS carrier.
An actual execution envelope must bind its selected carrier and subject.

This packet establishes the observed macOS capability only. It does not repair
or prove the earlier Work Mode path, merge PR #79, close Issue #78, admit a
production Soodles Issue, reconcile the original production worktree, or change
the selected Soodles runtime pin or external verifier.

All receipts remain non-authorizing: authorizes_landing=false.
