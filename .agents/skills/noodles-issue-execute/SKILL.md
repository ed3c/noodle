---
name: noodles-issue-execute
description: Implement one source-authorized ed3c/noodle Issue in an existing Noodle worktree and stop at supervised handoff. Do not use for ad-hoc work or provider landing.
schedule: "Only when the target schedule selects an authorized ed3c/noodle Issue"
---

# Target Issue Execute

The stage prompt is the exact JSON backlog row selected by the target schedule. Treat it as a source-authorized target Issue projection, not as permission to infer missing scope. Before editing:

1. Parse the row and require its exact `id`, Issue body, target repository, execution skill, and authorization declaration.
2. Require the subject and target to be `ed3c/noodle`, the execution skill to be `noodles-issue-execute`, and the current checkout to be the Noodle-created worktree for that atom.
3. Revalidate the authorization binding and declared write boundary using the repository-owned adapter or contract. Missing, stale, or inconsistent input is a blocked outcome, never permission to infer or widen scope.

Implement only the smallest independently useful behavior inside the declared boundary. Read the nearest source and test, run focused RED/GREEN verification, then run the repository's required check once at the exact candidate head. Do not weaken checks or modify unrelated files.

Commit the candidate in the current worktree. Emit the supported handoff event, then stop:

```bash
noodle event emit --session "$NOODLE_SESSION_ID" stage_yield --payload '{"message":"Implemented and verified the exact target Issue; candidate committed for supervised provider handoff."}'
```

This route does not merge locally, close the Issue, mutate provider lifecycle state, or grant landing authority. Provider PR creation, exact-head verification, merge, closure, and reconciliation remain outside this route until target-owned carriers provide them.
