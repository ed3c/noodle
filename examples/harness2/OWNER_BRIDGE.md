# Original Soodles owner driver

`SoodlesOwnerBridge.execute(OwnerCall) -> OwnerResult` implements the existing owner protocol.
It invokes the actual Soodles CLI and follows original owner `next` operations. It does not
implement admission, scheduling, review, artifact application, model transport or native
resource recovery. The driver was checked against Soodles
`e2e7a0474e0fca055b512b139273d9b4b33ec97d`; original owner validation remains authoritative.

This increment has offline protocol controls only. No model, Host or VM was started for
its tests. Passing these controls is not real-owner acceptance or an optimization result.

## Driver inputs

Construct one driver and one `SharedGate` for both model groups. All native solver and
proposer calls use that same gate. The driver constructor is:

```python
SoodlesOwnerBridge(cli, evidence_root, templates, review=session_review, poll_s=1.0)
```

- `cli` is `PinnedCLI(python, entrypoint, sources, source_commit, environment, timeout_s)`.
  `python`, `entrypoint`, and every `sources` member are exact `{path, sha256}` FileRefs.
  Supply the reviewed public Soodles source closure, including its imported local modules
  and referenced policy/contract files, not merely `soodles.py`. `source_commit` records the
  reviewed provenance; byte hashes, rechecked around calls, bind what actually executes.
  The driver does not discover or copy private state. Supply the existing owner environment
  explicitly (PATH/HOME/XDG paths as applicable); Python/Git injection variables and direct
  credential variables are rejected. `timeout_s` is the operator's existing local CLI timeout,
  not a remote model deadline. Timeout is unknown and never triggers a replacement call.
- `evidence_root` is a new external directory. Every native call gets a directory named by
  its full input fingerprint. It retains request, source bindings, CLI intent, stdout/stderr,
  capture, immutable candidate projection and final receipt. A previous directory is never
  overwritten. Restart recovery belongs to the original owner; this driver does not replay.
- `templates` is a finite ordered list of `RequestTemplate`. Each binds `group`, `role`, an
  unused App-created named control worktree `repository`, instruction selection,
  Noodle binary/profile FileRefs, original `write_paths`/`acceptance`, explicit text
  `deliverables`, and optional relative `harvest` path. Give a distinct worktree per call,
  with the correct committed starting content. The Soodles owner checks linked-worktree
  isolation, clean HEAD, origin, named branch, no prior owner and the binary capability.
  The driver never creates a standard worktree, changes main, resets a repository, or
  recycles a previously integrated task. App allocation remains the caller's responsibility.
  For solver templates set `selected_skill=None`, `support_skills=()`, and explicit
  `harness_skills=(selected_alias, support_alias, ...)`. The driver selects those skills
  from each call's actual hash-bound `inputs.harness/skills/<alias>/SKILL.md`. Also provide
  `skill_bases={alias: {path, sha256}, ...}` binding each complete fixed skill directory
  with `tree_ref`. The dispatch overlay copies all original examples/license/assets and
  modes, replacing only SKILL.md bytes with the evaluated candidate. Aliases must match
  exactly; symlinks, special files and private paths refuse before copying. Native
  `read_tree` only reads editable files; native `write_tree` preserves off-surface files
  only if already present, so relying on a newly created candidate directory loses assets.
  The overlay leaves the native editable surface unchanged. This cannot keep
  executing a stale baseline Skill after native optimization changes it. For proposer
  templates supply fixed selected/support Skill FileRefs and `harness_skills=()`.
- `review(call, observation, observation_ref)` is the Session's semantic review callback.
  It receives the exact original Soodles candidate/readback after complete trace capture
  and snapshot. Use original Soodles `review`/candidate APIs and original acceptance inputs;
  do not manufacture an approval record. This callback owns the original review continuation
  until it has submitted approval, or raises on refusal/unknown. The driver then reads the
  original owner until that same candidate is locally integrated with acceptance PASS.
  It does not turn a candidate into automatic approval or interpret hidden test answers.

For native `k=1, seq`, normal calls alternate solver/proposer and finish with the fourth
solver: seven templates per group. Native known-invalid repair can request an additional
proposer; provision only the explicitly authorized finite repair slots. Pool exhaustion or
a mismatched next role refuses before starting an owner. Do not silently substitute a root.

The model bindings are exact: Codex `gpt-5.6-sol`/`low`, Pi
`openrouter/deepseek/deepseek-v4.1-flash-1017`/`low`. Each generated request is `inner-only`,
`runtime=openshell`, and carries the explicit profile and Noodle binary. The native prompt or system-plus-mission is retained in the original goal. A fixed derived
solver section tells the worker to apply the evaluated AGENTS.md through the existing
materialization mapping; it does not alter frozen task or instruction bytes. Complete source/context/harness files and the
full `call.json` become original `source_refs`; selected/supporting skill subtrees are bound
separately. Soodles performs its existing frozen instruction materialization. No host path
is assumed readable by the guest. A proposer template must direct the original task to
return the requested harness at its declared `harvest` path (usually `context/harness`),
inside the configured write scope. The returned immutable directory becomes native
`Reply.workspace`; no second optimizer is involved.

## Advancing and observing

The actual transport is exactly:

```text
PYTHON -B SOODLES/soodles.py loop enter --request-file ORIGINAL
PYTHON -B SOODLES/soodles.py loop inspect --request-file ORIGINAL --include-result
PYTHON -B SOODLES/soodles.py loop inspect --request-file ORIGINAL --include-result --capture-output NEW
```

`enter` is used initially and only for original `original_task_dispatch`,
`original_checkpoint_readback`, or `original_initial_admission_readback`. An identical
original checkpoint is not advanced again; waiting calls `inspect`. The checkpoint excludes
receipt path and heartbeat noise. Unknown effects, unsupported recovery/retry operations,
changed request/source identity, multiple attempts or revision selection close the shared
gate. There is no new advance-token protocol and no automatically accepted retry/recovery.

The snapshot requires a single original canonical attempt, matching candidate/trace task
and route, successful native exit, Stop ACK and original Stopped main instance. Canonical
and native attempt IDs remain separate. Captured bytes and candidate bytes are checked;
symlinks, path traversal, out-of-scope outputs and changed executable modes refuse. Binary
files and deletion state are retained. Text deliverables must be explicitly named; binary
outputs remain in the snapshot instead of being guessed as text. Raw trace, descriptor,
source custody, native completion and raw billing fields are copied under the result tree,
so `SharedGate` reuse verifies those bytes too. Only capture-classified public events enter
`trajectory.md`; private reasoning is not fed to the optimizer. The original raw capture
is retained for audit, not printed by this driver.

The Session may approve while the current candidate field temporarily disappears during
merge. The driver retains the candidate and verifies the original review selection, waits
for `locally_integrated`, and requires original integrated acceptance PASS. This is the
local task's acceptance, not proof the optimized harness beats its baseline or permission
to adopt it. Hidden paired evaluation remains a separate frozen owner.

## Actual accounting and remaining acceptance

The existing shared paid gate retains the US$5 total. The caller supplies already-spent
usage; it is never defaulted to zero. A local reservation can hold the entire remaining
budget (for usage `0.084284887`, the remainder is `4.915715113`). This is accounting, not a
claim of a per-call provider-enforced maximum. The original OpenRouter key hard cap and
its authority must be validated externally before paid execution.

The original capture may report provider billing unobserved. The bridge now optionally
uses `OpenRouterBilling` to query the original generation IDs and reconcile actual total_cost
against the exclusive key usage delta. Only matching actual bills release the accounting
hold. Bounded observations that remain unknown preserve cost_usd=null and the paid hold;
Session can reconcile the same completed call later without dispatch. See
[BILLING_AND_RUNNER.md](BILLING_AND_RUNNER.md) for the GET-only supplier, receipt checks and
`run_experiment(config, review_callback)` entry. This implementation has offline controls;
its live billing/runner acceptance remains a separate original-owner validation.

A missing per-call reservation uses the current remaining authorized budget as a conservative
accounting hold, not a new minimum or provider-enforced per-call limit. Codex subscription
requires no paid reservation. Unknown native effects close both routes. Driver-local elapsed
time is recorded separately; missing native or whole-workflow duration is never zero.

## Offline controls

```sh
PYTHONDONTWRITEBYTECODE=1 python3.13 -m unittest discover -s examples/harness2 -p test_soodles_bridge.py -v
```

Fixtures explicitly simulate original owner responses. Controls cover generated requests,
fixed route/refusal, stable pending checkpoints, exact CLI argv and retained lost-response
intent, canonical/native identity mismatch, artifact/symlink drift, binary/mode/deletion,
private/public trace separation, original review transition and acceptance, nullable paid
billing with a full-remainder hold, and unknown closure across routes. The complete
`reproduce.py test` also runs the actual pinned native core controls from the first increment.

## Same-call readback reconciliation

The original successful first `loop enter` can return `inner.status=pending` without
`inner.owner`; authority comes from the matching original next owner/operation/request and
Host receipt. Its sanitized real fixture is retained under `fixtures/`.

`experiment_driver.resume_experiment(config, review_callback, call_ref, fresh_observation_ref)`
reconciles the exact sole unresolved call. It verifies the retained call, template sequence,
request, successful original admission receipt, source closure and unchanged original Host.
All previously settled results and bills are verified before proceeding. No new request,
enter, attempt, model call or owner is created by reconciliation. Session consumes any needed
original next action and review itself. Only the same candidate's original integrated PASS
allows the shared gate to settle the call.

A complete pre-review snapshot can be validated after its original worktree is merged away:
original capture/attempt/Stop proof, every retained artifact byte and mode, public projection,
full evidence inventory and returned harness remain checked. An uncaptured call can only
follow the known initial-admission transition. Partial/changed snapshots refuse. The first
baseline and later proposer use this same reconciliation path, not separate recovery owners.

Original unknown journal bytes/rows, native summaries and failed outputs are archived before
same-namespace continuation. Already settled solver/proposer calls return only their verified
cache; the remaining original templates and accounting continue unchanged. Unknown history
is never reset. An interrupted reconciliation retains its intent and does not authorize replay.

Only a non-capture `inspect` with original exit 2, `effects=[]`, no landing authority and the
exact `task.session_readback.native` / `session readback source changed: canonical_snapshot`
refusal can perform at most three reads. Every read retains separate intent/stdout/receipt.
Capture, enter, timeouts, other sources, changed identity and other refusals never retry.

Native experiment elapsed time uses the earliest original journal call start through final
settlement, including review/repair downtime. Driver-segment time is reported separately.
Whole workflow timing still needs Stage A and independent evaluation; missing terminal clocks
remain null. This entry does not turn subscription billing or unknown effects into zero.
