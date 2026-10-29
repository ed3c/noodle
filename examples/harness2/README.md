# Harness² native owner contract

This example executes the real pinned Harness² `core.schedule.run_task` through explicit
owner-delegation seams. `soodles_bridge.py` supplies an explicit Soodles CLI driver with
original-request and completion verification. Offline controls do not run models, start
services/VMs, certify improvement, or authorize adopting an optimized candidate.

The first increment supports the existing `AGENTS.md` and `skills/<name>/SKILL.md` paths in
a frozen baseline. New/deleted files, scripts, plugins, hooks, subagents and execution/model
configuration are refused. Harness configuration edits need a later explicit owner mapping;
this increment never accepts a configuration file that no executor actually reads.

## Reproduce without models

Use an existing Python 3.12 or 3.13; the upstream package has no mandatory dependencies.
No installation is needed. Preparation downloads only the pinned public archive unless an
exact local archive is supplied. Choose a new directory outside a Git checkout:

```sh
python3.13 examples/harness2/reproduce.py prepare --work-dir /absolute/new/harness2-control
python3.13 examples/harness2/reproduce.py test --work-dir /absolute/new/harness2-control
```

For offline preparation add `--archive /absolute/source.tar.gz` to `prepare`. Archive,
license and patch hashes come from `source.json`. Preparation validates before extraction,
uses native `git apply --check`/`git apply`, and records the patched source fingerprint.
Tests refuse source drift, strip ambient environment, prohibit child processes/network
inside native contract controls, and preserve logs plus a unique JSON receipt. Downloading
the archive is the only network operation. Source stays unmodified during controls.

The upstream Apache-2.0 license is reproduced in `LICENSE.upstream`; the downloaded source
also retains its own license and copyright notices. `noodle-owner.patch` changes only:

- `ChannelSpec`: recognize the explicit `noodle_owner` channel;
- default `Proposer.call`: refuse this channel before cache, temporary workspace or transport;
- prompt surface: describe `noodle-owner` instruction-only edits, without pretending to be
  OpenCode or the upstream Responses-API Codex executor.

Native scheduling, recursion, candidate materialization, validation, selection, retry policy,
ledger and summaries remain upstream code. The prompt surface addition is necessary:
upstream `_st` otherwise rejects every name except `codex` and `opencode`.

## Boundary for the production bridge

`OwnerBridge.execute(call: OwnerCall) -> OwnerResult` is an explicit protocol. A caller must
explicitly configure the Soodles bridge described in [OWNER_BRIDGE.md](OWNER_BRIDGE.md).
`run_native_task` refuses a missing owner before core
execution. The test-only `FakeOwner` is a deterministic artifact fixture, not a substitute
optimizer and not proof of a real model invocation.

The real bridge must use the existing Soodles admission and original Noodle Host lifecycle.
Before returning `settled=True`, it must verify original order/session/canonical attempt and
native attempt bindings, published candidate/artifact evidence, and same-original-VM stopped
proof. These identities are mapped, never assumed equal. It must preserve the original owner
receipt and produce a dedicated, immutable result projection directory containing:

- `trajectory.md`: original capture-classified public native events;
- `deliverables.json`: explicit agent-authored filename-to-text values, excluding acceptance
  criteria, hidden answers, private values and owner metadata;
- a returned harness directory for proposer `harvest="context/harness"`;
- retained completion evidence (the `receipt_path`/`receipt_sha256` fields can also reference
  its original location).

`OwnerResult.output_sha256` binds the full projection directory using `tree_ref`; returned
workspace must be within that directory. Reused results recheck all bytes/modes and receipt
hashes. Original raw events and full artifacts remain the owner’s responsibility; this is
not a replacement artifact-application implementation.

`OwnerCall.inputs` records the actual mission/prompt, system text, source/context/harness
path-plus-hash references, effort, and existing selected timeout metadata. The bridge must
verify those references before dispatch, materialize complete instruction files through the
existing frozen-manifest contract, and map each supported field to its existing owner.
The timeout field is not a new authorization or a claim that this Python contract kills
remote work. No arbitrary deadline is imposed here. Missing owner enforcement must refuse
rather than claim a limit works. The bridge must not read ambient credentials, directly call
model APIs, bypass hooks, launch a second owner, silently fall back to another model, or
retry a lost response.

`Refused` means **no native effect began**. Any other exception, incomplete completion, I/O
uncertainty, or changed completion evidence closes the shared gate. The native core catches
errors and may continue fallback paths; all those paths hit the closed gate before dispatch.
The bridge must itself retain original ongoing resource custody; the gate does not invent
Stop/recovery operations.

## Native execution and accounting

Create exactly one `SharedGate(journal, owner, initial_paid_spent_usd=...)` per experiment
driver; every solver and proposer in both groups must share that object. Its lock serializes
owner calls, including the core's concurrent INV/DOM proposals. It writes intent before an
owner call. Same group/run ID plus the same frozen input reuses settled evidence; different
input refuses. Reopening pending/unknown evidence never replays it. This is a single-driver
journal, **not** a multi-process filesystem compare-and-swap or an arbitrary-writer lock.
An incomplete `.pending` write is retained and cannot silently be replaced on retry.

`run_native_task(adapter, task_id, experiment_id=...)` passes `k=1`, `modes=("seq",)`, no
library, and explicit delegated proposer/selector to upstream. Each successful group has
four solver rollouts and three improvement stages. Known-invalid candidate edits can receive
the upstream second edit attempt; the `.a2` label is a new repair, never transport replay.
There is no selector model call for the singleton final, and no reflection call without a
library. Distinct tasks need distinct experiment IDs; labels cannot be reused for other
inputs. The output always says `hidden_acceptance="not implemented"` and
`adoption_authorized=false`. A fallback baseline can still yield upstream `status="ok"`;
that is not evidence of improvement.

Only Pi/OpenRouter calls use the paid reservation gate. The model is fixed to
`openrouter/deepseek/deepseek-v4.1-flash-1017`; the shared limit remains US$5, with explicit
already-spent cost supplied by the authorized owner. The paid reservation is a local
accounting hold, not proof of a per-call provider limit.
It may conservatively hold the whole remaining authorized budget; it is not a new user cap.
Unknown billed cost keeps the full
reservation. A cost above its reservation blocks further paid calls and is recorded; this
contract cannot retroactively enforce a provider cap or prove a caller-supplied bound.
Production integration must validate the original provider hard limit and billing authority
before paid execution. Unknown billing cannot release an accounting hold.

Codex subscription calls do not need a paid reservation. Their per-call USD may remain
`None`/JSON `null`; no per-call bill does not block an already authorized subscription route
and is not reported as free. Unknown **native effects**, in contrast, close both routes.

The durable owner journal is the accounting authority. It preserves nullable monetary cost,
nullable owner duration, local elapsed time and original call identity. Cached reads retain
the original paid cost and reservation. The upstream `Reply`/`RunView` numeric defaults do
not represent unknown, and native rollout code does not load owner costs. Therefore raw
upstream cost/time fields are explicitly non-authoritative (`native_costs_authoritative=false`),
not a zero-dollar claim. Do not use them for economic or efficiency comparisons. The final
production experiment must aggregate all solver/improver/selector/judge/failed attempt costs
and measure whole-round elapsed time; hidden paired acceptance remains a separate owner.

## Controls and observed limits

The tests run the actual patched upstream package, not a handwritten optimizer. They cover
both route identities, R=4/P=3, real candidate-file changes, final selection, reuse and changed
input refusal, concurrent same-ID calls, lost-response closure across subsequent native
fallbacks and both routes, restart with pending effects, completion-output drift, intent I/O
failure, subscription null USD, paid budget/reservation behavior, off-surface native repair
and fallback, missing owner/default transport refusal, symlinks, and source archive drift.
All model behavior is explicitly simulated. The Soodles driver has additional offline
request/transport/capture/review controls. Its real-owner acceptance, provider billing
acceptance, fixed hidden evaluation, efficiency claims and adoption remain unfinished.
The GET-only billing implementation and execution entry are described in
[BILLING_AND_RUNNER.md](BILLING_AND_RUNNER.md).

An already captured call can explicitly select a corrected Soodles transport through
`resume_experiment(..., cli_successor=FileRef)`. The supervisor packet binds the original
call/request/intent, preserved effect-free outer refusal, and complete predecessor and
successor configurations. Only CLI source/entrypoint may differ; Python, environment,
timeout, experiment inputs and output namespace stay fixed. Both source closures remain
verified. Reconciliation only reads the same captured call and still requires its original
Host identity and integrated acceptance; it never repeats enter or dispatch. Omission of
the packet retains the original strict transport comparison.

The first control attempt exposed the missing native prompt surface (retained failure, no
model effects); the correction added the explicit surface above. A later negative fixture
incorrectly recreated an existing rejected-script directory on the second repair, which
caused a fake transport exception; fixing the fixture to preserve native repair semantics
restored the intended assertion of six known-invalid proposals. Neither failure was bypassed
or converted into a negative substitute for the positive native-core test.
