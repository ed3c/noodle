# Fixed heldout pair caller

`heldout_driver.preflight_pair(config)` reads only bound public source files. It
does not run the CLI, inspect a Host, read credentials or call a provider.
`run_pair(config, review_callback)` is the explicit effectful entry: two fixed
solver calls through the existing `NoodleAdapter`, one `SharedGate`, and original
`SoodlesOwnerBridge`. It never calls an optimizer/proposer or grades its outputs.

The Session first freezes each group's actual native final selection, then opens
that group's heldout input. For each case it obtains an original accepted Stage A
artifact and measurements. Use `prepare_public_task_bundle` with only case worker
input, accepted artifact and geometry; keep reports, rubric and oracle outside
that source tree. Include the original producer request in `provenance`. Stage A
is shared between the two consumers, not regenerated for each B.

The configuration has these explicit fields (all paths are absolute):

- `schema_version: 1`, `status: "ready"` after actual sources are available.
- `upstream`: the existing pinned core `path` and `prepared_receipt` FileRef.
- `selection`: immutable native `summary` and `journal` FileRefs, and `group`.
  `selected_harnesses` derives baseline/final from the actual settled native calls.
- `task_bundle`: the new accepted-source task.json FileRef.
- `route`: one fixed `group`, `provider`, exact `model`, `effort: "low"`, and
  `timeout_s`. The timeout is metadata; this does not claim Host enforcement.
- `cli`: the complete `PinnedCLI` source closure, Python and original environment.
  `host_directory` must exactly equal `cli.environment.NOODLE_HOST_DIR`.
- `slots`: exactly two entries in the evaluator's precommitted order, each with a
  distinct opaque `run_id`, `variant: "baseline" | "candidate"`, and full
  `RequestTemplate`. Use separate fresh App controls. Each template has its own
  fixed single-stage `composition` and the same `producer_handoff` pointing to A.
  Original handoff verifies accepted A and full derived skill assets before enter.
- `output_root`: a fresh pair evidence directory, `poll_s`, `always_on_max`.
- `initial_paid_spent_usd`: original current cumulative paid-key usage, not zero
  and not a new allowance. `billing`: existing `environment_file` private locator,
  `key_name`, `timeout_s`, `exclusive_key: true`, `observations`, `interval_s`.
  The existing GET-only supplier reads that locator only during a paid call.

Both templates must share Noodle/profile FileRefs, write/delivery/acceptance scope,
skill aliases and identical complete `skill_bases`. Only control directory and
composition selection can differ. The original composition verifier allows only
the selected SKILL bytes to differ from the common immutable assets. All sources
must remain unchanged throughout the pair. New heldout bindings are not usable
as replacements for old training bindings.

The root Session invokes the entry from its own callback script:

```python
from heldout_driver import preflight_pair, run_pair
from soodles_bridge import decode, read_ref

config = decode(read_ref(original_config_ref))
preflight_pair(config)  # no effects; missing actual sources refuse
receipt = run_pair(config, original_session_review_callback)
```

The callback follows the original `review_input`. It must wait for candidate
approval **and integrated acceptance PASS**, not merely submission of approve.
Safe artifact delivery is separate from benchmark quality: clipping or other
quality failures remain visible to the later independent evaluator and must not
cause the callback to demand a perfect regenerated result.

Any unknown owner effect ends this pair and retains its intent, request, journal,
budget hold and original result evidence. The caller cannot automatically reopen
an output directory, resend enter, allocate a replacement control or reset usage.
Session reconciliation must inspect the same original call using the existing
bridge APIs before any separately authorized continuation. This entry deliberately
does not invent a second recovery state machine.

After both original outputs settle, use `ReviewedRun` and `evaluate.py pair` with
independent observed reports. The pair receipt claims neither quality PASS nor
adoption. Whole-workflow cost/time remain null until original Stage A, training,
evaluation, repair and review evidence are accounted for. Codex USD null is not
zero. The same paid key's total hard limit remains $5 across all calls; all work
must remain serial/exclusive while settlement observes it.
