---
name: noodles-issue-execute
description: Implement one source-authorized ed3c/noodle Issue and hand its verified commit to the exact provider PR. Do not use for ad-hoc work or provider landing.
schedule: "Only when the target schedule selects an authorized ed3c/noodle Issue"
---

# Target Issue Execute

The stage prompt is the exact JSON backlog row selected by the target schedule. Treat it as a source-authorized target Issue projection, not as permission to infer missing scope. Before editing:

1. Parse the row and require its exact `id`, Issue body, target repository, execution skill, and authorization declaration.
2. Require the subject and target to be `ed3c/noodle`, the execution skill to be `noodles-issue-execute`, and the current checkout to be the Noodle-created worktree for that atom.
3. Revalidate the authorization binding and declared write boundary using the repository-owned adapter or contract. Missing, stale, or inconsistent input is a blocked outcome, never permission to infer or widen scope.

Implement only the smallest independently useful behavior inside the declared boundary. Read the nearest source and test, run focused RED/GREEN verification, then run the repository's required check once at the exact candidate head. Do not weaken checks or modify unrelated files.

Commit the candidate in the current worktree. Recheck that the committed candidate is clean and exact, then invoke the target-owned carrier with the exact subject:

```bash
go run ./adapters/noodles-github handoff 'ed3c/noodle#N'
```

The carrier must successfully read back the deterministic provider branch, exact PR, and the Issue at `awaiting_land`. Only then emit the completed typed outcome and the supported handoff event, then stop:

```bash
payload="$(printf '{"message":"Implemented and verified the exact target Issue; candidate committed for supervised provider handoff.","blocking":false,"outcome":"completed","order_id":"%s","stage_index":%s}' "$NOODLE_ORDER_ID" "$NOODLE_STAGE_INDEX")"
noodle event emit --session "$NOODLE_SESSION_ID" stage_message --payload "$payload"
noodle event emit --session "$NOODLE_SESSION_ID" stage_yield --payload '{"message":"Implemented and verified the exact target Issue; candidate committed for supervised provider handoff."}'
```

Every terminal path emits exactly one final typed `stage_message` bound to the existing `NOODLE_SESSION_ID`, `NOODLE_ORDER_ID`, and `NOODLE_STAGE_INDEX` values. Use `{"outcome":"blocked"}` with `"blocking":true` when exact input, authorization, or a required capability is unavailable. Use `{"outcome":"failed"}` with `"blocking":true` when implementation or verification fails. Emit the non-blocking `{"outcome":"completed"}` shape above only after the verified provider handoff. Do not infer any identity value, emit more than one typed outcome, or treat final prose or process exit as completion.

This route does not merge locally, merge the provider PR, close the Issue, or grant landing authority. Exact-head landing, closure, and reconciliation remain outside this route.
