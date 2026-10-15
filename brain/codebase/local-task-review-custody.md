# Local task review custody

Local tasks use terminal native process evidence. They do not produce the Issue
`stage_message` contract. `loop/local_task_review.go` accepts this path only for
an admitted order with one `local-task` Codex process stage and a matching original
initial-admission record. Other tasks retain the typed outcome check.

Request-changes retains the original attempt, worktree bytes, index and native
session files. `recover-local-task-review` repairs the older failed-order path
from its original candidate, request-changes ACK and matching failure events.
It restores custody without launching a worker. Prompt editing and requeue stay
separate existing controls.

`merge-local-task` binds the candidate source and checks HEAD, index, file bytes,
executable modes and admitted paths before committing the reviewed changes.
The worktree merge owner checks the resulting HEAD and tree under its merge lock.
It uses a fast-forward into the context's recorded control branch. Branch drift,
new unreviewed files and a changed integration base cause refusal.

The previous generic merge-error path deleted dirty candidates. Merge errors now
retain their worktree and pending review. For a candidate already lost through
that path, `review prepare-local-merge-recovery` binds exact retained backup bytes.
`recover-local-task-merge` validates that packet, the original failed merge,
terminal session and reconstructible index before restoring the worktree.
It records recovery separately and preserves the failed attempt. The owner
cancels only matching obsolete pending dispatch, merge and cleanup effects.
A running or foreign effect remains unresolved. No provider write is involved.

Controls in `loop/local_task_review_test.go` and `loop/local_task_merge_test.go`
cover recovery, dirty candidates, refusal, fixed-branch integration and retained
failure history. These local tests do not establish live Host acceptance.

See also [[codebase/merge-recovery-synthetic-cooks-must-preserve-task-key]].
