# Original billing settlement and experiment entry

`experiment_driver.run_experiment(config, review_callback)` is the explicit execution entry.
Importing the module or preparing its public inputs does not run models. Calling this entry
runs the configured real native core through the original Soodles bridge; the caller must
sequence it with other original-owner work. This increment was tested offline only.

## Prepare the accepted input once

Root/Session first generates and accepts Stage A through the original owner, measures its
geometry, and freezes the evidence. `prepare_public_task_bundle(...)` copies only explicitly
selected accepted artifact files plus explicit public measurement files into `source/`.
It verifies original `locally_integrated` / approve / acceptance PASS, candidate file hashes
and executable mode. Its inputs are:

```python
prepare_public_task_bundle(
    destination=new_external_directory,
    task_id="drawing", prompt=original_stage_b_prompt,
    artifacts={"component.html": {
        "source": exact_component_file_ref, "candidate_path": "component.html"}},
    measurements={"geometry.json": exact_measurement_file_ref},
    owner_acceptance=original_soodles_inspect_file_ref,
    provenance=(original_stage_a_billing_ref, original_stage_a_timing_ref),
)
```

The returned `task.json` FileRef describes a `FrozenAcceptedSource`. It keeps owner/accounting
provenance outside the model-visible `source/`. Measurement semantics remain with the actual
measurement owner; this copier does not invent geometry or hidden answer labels. Every
baseline/candidate Stage-B rollout uses the same source hash. Stage A is not regenerated for
each solver call, and its cost/time must still be included once in the full workflow report.

`prepare_editable_harness(destination=..., agents=FileRef, skill_bases={alias: tree_ref(...)})`
builds only AGENTS.md and skill SKILL.md files for native optimization. Full immutable examples,
licenses and scripts remain in the pinned skill bases and enter each solver through the
Soodles overlay. Neither helper creates a Git worktree, changes main, or starts an owner.

## Configure the real driver

Supply the following dictionary to `run_experiment`; this is host configuration, never a
model source or optimizer-editable artifact:

- `experiment_id`, new `output_root`, explicit `initial_paid_spent_usd`, and polling cadence
  `poll_s`. For a Pi group, initial usage comes from a fresh original-key read after Stage A,
  not an old example or a default zero. A Codex-only round retains the explicit known cumulative
  amount but performs no key read; its subscription cost remains unknown, not zero.
- `upstream`: `{path, prepared_receipt}` for the existing pinned Harness² source and its
  `prepared.json` FileRef. Source tree/pin are checked before loading the actual core.
- `cli`: all `PinnedCLI` arguments from OWNER_BRIDGE.md, with reviewed Soodles source bytes,
  exact Python/Noodle identities and an explicit noncredential environment.
- `templates`: ordered `RequestTemplate` dictionaries, one unused App control root per
  authorized call. Solver templates have `harness_skills` plus complete `skill_bases`;
  improver templates use the fixed improver Skill. Root supplies correct starting contents,
  write paths, original acceptance and explicit return paths; the driver never guesses them.
- `groups`: one or two explicit entries, each `{route, task_bundle, baseline, always_on_max}`. `route`
  contains native `Route` fields `group`, `provider`, `model`, `effort`, existing `timeout_s`,
  and optional `paid_reservation_usd`. Group names and providers are each unique; providers
  are limited to `codex` and `pi`. The fixed models are
  gpt-5.6-sol/low and openrouter/deepseek/deepseek-v4.1-flash-1017/low. Selected groups share
  the original gate. A new single-group round needs a new experiment/output namespace; it
  does not repair or replace an earlier refused round.
- `billing`: required only when a Pi group is selected;
  `{environment_file, key_name, timeout_s, exclusive_key, observations, interval_s}`.
  A Codex-only round does not construct the paid supplier or read its `.env`, even if an
  unused billing section was supplied. It makes no OpenRouter GET requests.
  The exact private .env locator is consumed only by the GET supplier in the host process.
  `key_name` is normally OPENROUTER_API_KEY. The file is not copied into any bundle, receipt
  or model environment. `exclusive_key=true` is an operator declaration, not a Python lock
  against unrelated writers. All observed extra consumption causes an unresolved bill.

Root supplies `review_callback(call, original_observation, observation_FileRef)`. It can save
original `review_input` for the Session and use the existing Soodles review APIs, then return
once original approval is submitted. No additional review format, token or optimizer judge
is introduced. The bridge waits for original integration/acceptance readback before the
next native call. Recovery or request-changes require explicit Session resolution.

The entry validates the selected group set before constructing the owner or paid supplier,
and prevalidates all selected frozen sources before execution. It calls the actual
`run_native_task` (k=1, seq) and saves each group result and the common journal. It reports
measured optimization elapsed time, but whole-workflow USD/time remain null until original
Stage-A receipts and all required measurements are aggregated. Hidden paired evaluation and
adoption remain separate; the entry does not infer success from native fallback status.

## GET-only actual billing

The supplier only allows OpenRouter `GET /api/v1/key` and `GET /api/v1/generation?id=...`.
It disables redirects and ambient proxies, never exposes Authorization or the key, and saves
only selected billing metadata plus the original response hash. JSON numeric values are
parsed as Decimal at their original precision. Duplicate/nonfinite fields refuse. No token
price table, completion API, key update, credential refresh or provider mutation is used.

These are the official endpoints:

- [Current API key usage and limits](https://openrouter.ai/docs/api/api-reference/api-keys/get-current-api-key)
- [Generation usage metadata and total_cost](https://openrouter.ai/docs/api/api-reference/generations/get-request-&-usage-metadata-for-a-generation)

`OpenRouterBilling.begin(call, directory)` runs before the original enter. It verifies the
same dedicated key, non-resetting cap no greater than US$5, positive remaining credit and
usage equal to the original serial owner's last settled observation. The initial observation
is explicitly supplied. Drift refuses before starting the task.

`settle(call, before_ref, result, new_directory)` reads only the completed call's retained Pi
capture. Every assistant message must have a unique original `responseId`; the fixed Pi
implementation assigns it from OpenRouter's streamed response ID. Each generation is read
back under the same key and must have the expected canonical billed model
`deepseek/deepseek-v4.1-flash-20261017`, a completed/cancelled bill and actual `total_cost`.
The sum must equal the dedicated key's after-minus-before usage using Decimal arithmetic.
This supplements generation bills with a completeness check for missing charged failures or
unrelated consumption. It does not claim to prevent an arbitrary external writer.

Missing IDs, delayed billing, missing generations, changed key/model or a delta mismatch
produce `status=unknown`, nullable cost and retained observations. A bounded number of
read-only observations is configurable. No additional model call occurs. Unknown billing
keeps the paid hold; unknown native effects close both routes.

## Release or reconcile the same call

A missing per-call reservation no longer invents a new required execution budget. The gate
conservatively holds its currently remaining authorized amount; the original provider cap
and actual usage are the spending authority. A successful actual bill releases the unused
hold. Codex subscription USD remains nullable without blocking that route.

The bridge automatically attempts settlement after original integration. If billing is
still unknown, native completion remains settled and the paid hold remains. Session can
perform another read-only reconciliation for that exact original call:

```python
settlement_ref = billing.settle(call, before_ref, original_result, new_evidence_directory)
updated_result = gate.reconcile_billing(call, settlement_ref)
```

`before_ref` and the first settlement are retained in the call directory's billing-status.json.
`reconcile_billing` never calls execute/enter. It verifies the call/result fingerprint,
immutable descriptor/raw bytes, generation set, original key delta and receipt dependencies.
Unknown receipts do not change accounting. Generation bills cannot be claimed by two calls.
A persisted cost is checked again on cache reuse; changed billing evidence refuses reuse.
Restart/persistence uncertainty does not authorize replaying a native call.

The full current-key usage counts prior authorized calls, including Stage A. For economic
comparison, separately aggregate actual generation costs for the experiment's own call IDs;
never report the initial account total as the cost of one experiment. Missing subscription
USD or whole-round timing remains unknown, not zero.
