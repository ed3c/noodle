# Independent evaluation and actual accounting

This module is an offline post-selection adapter to the pinned upstream
`harness2.core.judge.judge_one`. It never dispatches a worker, invokes a model judge,
reads credentials, or imports into the optimizer. Browser/semantic assessment remains
with the independent evaluator. Fixtures are contract tests, not actual quality evidence.

## Freeze before evaluation

`selected_harnesses({summary, journal, group})` binds the successful native k=1/seq
selection and baseline to their original settled solver calls and complete harness tree
hashes. It rejects an intermediate proposal or changed tree. `judge_pair` performs this
step before reading evaluator reports. Actual INV fallback remains part of the native
record; a final candidate can be evaluated without claiming all requested model proposals
ran. The continuation receipt separately exposes actual settled/refused call counts.

For each heldout case and model group, the original owner first generates and accepts one
Stage A component. Freeze its exact artifact, commit, actual browser geometry, and case
input. Baseline and selected-final Stage B runs consume this same accepted source and the
same route. They differ only in their selected editable harness. Complete fixed skill assets
still use the existing overlay/materialization. Each call requires an unused App control.

The planned two heldouts × two groups need four Stage A controls plus eight paired Stage B
controls (12 total), in addition to completed training controls. Do not reuse a prior owner
control. The optimizer receives no heldout report, oracle, judge output, or heldout trajectory.
After final selection, a heldout worker sees only its own case input; the evaluator alone
receives private oracle material. Anonymous presentation order and the frozen rubric remain
evaluator responsibilities and need recorded evidence. This plan does not authorize extra
model calls or increase the shared US$5 limit.

## Report interface

All references are exact `{path, sha256}` FileRefs; tree references use the existing
`owner_contract.tree_ref`. A `ReviewedRun(journal, group, run_id, report)` binds one original
settled solver. Safe delivery must have original `approve`, `locally_integrated`, acceptance
PASS, and the exact retained output/receipt hashes. That acceptance is not benchmark quality.

Evaluator report v1:

```json
{
  "schema_version": 1,
  "authority": "independent-evaluator",
  "task_id": "the original call task_id",
  "rubric": {"path": "absolute frozen rubric", "sha256": "exact hash"},
  "source": {"path": "fixed accepted source tree", "sha256": "tree hash"},
  "artifact": {"path": "evaluated drawing.html", "sha256": "artifact hash"},
  "conditions": [
    {"id": "legibility", "status": "FAIL", "reason": "Measured finding", "evidence": []}
  ],
  "quality": [
    {"dimension": "dimensional accuracy", "score": null, "reason": "Not observed", "evidence": []}
  ]
}
```

This abbreviated example is not gradeable: every frozen blocking gate and quality dimension
must occur exactly once, and every assessment needs actual evidence FileRefs and a reason.
Gate states are PASS/FAIL/UNKNOWN; quality values are integer 0..2 or null. A rendered pixel
finding cannot be replaced by the worker's assertion or a string-presence check. The drawing
must byte-match the original delivered `workspace/drawing.html`.

```python
native_judge = load_native_judge(config['upstream'])
result = judge_pair(
    rubric=config['rubric'],
    baseline=ReviewedRun(**config['baseline']),
    candidate=ReviewedRun(**config['candidate']),
    selection=config['selection'],
    native_judge=native_judge,
)
```

`IndependentReviewAdapter.score(run_dir, task_id)` supplies complete report measurements
to actual `judge_one`; unknown measurements return None, never zero. Pair results preserve
known failures even if other measurements remain unknown. The frozen rubric requires all
blocking gates of both baseline and candidate to pass, and no candidate quality dimension
to decrease. Therefore a clipped baseline is still a failed formal pair even if the final
candidate fixes the clipping; report the observed difference without claiming a Pareto gain.

`aggregate_pairs(report_refs)` counts PASS/FAIL/UNKNOWN per group. Unknowns remain explicit;
two fixed heldouts are not evidence of statistical significance or population generalization.
No result authorizes automatic adoption.

## Actual cost and full elapsed time

`accounting(journal_refs, external_calls=[...], wall_interval=None)` verifies every supplied
native call fingerprint and counts each original generation at most once. Pi native billing
uses existing `verify_settlement`: original raw generation IDs, actual generation bills,
exclusive-key usage delta and immutable result binding. No token-price estimates are used.
Subscription USD and unresolved bills remain null; initial key usage is not experiment cost.
Original effect-free refusals cost no model money, while unknown effects never become zero.

List every Stage A and evaluation call outside the native journal explicitly:

```python
external_call = {
    'group': 'pi', 'id': 'original-stage-a-identity', 'role': 'stage_a',
    'provider': 'pi', 'evidence': [original_request_ref, original_capture_ref],
    'descriptor': original_pi_descriptor_ref, 'billing': original_stage_a_bill_ref,
}
```

For Codex, omit billing/descriptor and retain original evidence. Its dollar cost remains
unknown. The Stage A bill reader validates the retained original GET receipt, exact raw
trace generation set, canonical model, key cap, Decimal bill sum and usage delta without
network access. Session must verify that the enumerated external call list is complete;
missing unlisted work cannot be discovered by scanning unrelated Host state.

Whole native-round time comes from the earliest original journal start through final
settlement, including review/repair downtime. Missing clocks remain null. Whole-workflow
time additionally needs an explicit original start/end interval covering Stage A,
optimization and evaluation. An optional `components` list has `kind` (stage_a, model,
tools, review, repair, evaluation, other), timezone-qualified `started_at`/`ended_at`, and
nonempty `source_evidence` refs. Components must lie within the whole interval without
overlap; remaining duration is reported as unclassified, not discarded or invented.
Original call elapsed durations are separate and must not be mistaken for whole elapsed time.

`efficiency_decision(quality_status, baseline_cost, candidate_cost, baseline_wall,
candidate_wall)` compares only after quality PASS. Missing costs/times are inconclusive.
A Pareto improvement requires both measured metrics not increase and at least one strictly
improve; otherwise report equality or a tradeoff/regression. Keep measured one-off total
including optimizer/evaluator overhead separate from any explicitly stated amortized scenario.

## Offline command entry

Run `python3.13 examples/harness2/evaluate.py pair CONFIG OUTPUT` after filling the fields
in the report interface above plus `status: "ready"` and the existing `upstream` prepared
source pin. Use `accounting` with `journals`, `external_calls`, `wall_interval`; use
`aggregate` with `reports`. All commands require explicit ready inputs, create a new output
file, and print only its FileRef. They do not make observations or issue owner/model calls.
The evaluator must supply actual observations; an incomplete draft never becomes a PASS.
