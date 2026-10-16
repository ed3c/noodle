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

The local-task caller always reaches the bound merge owner. A same-named branch
or patch-equivalence preview cannot prove that the reviewed commit was integrated.
The reviewed path does not stash or rebase candidate files. If another writer
changes them after validation, non-force cleanup preserves that worktree.
After restart, recovery checks the bound commit and tree on the original control
branch. A removed candidate branch does not erase evidence that its commit landed.

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

Commit-only completion detection skipped review for dirty local-task outputs.
Local-task success now parks review before that detection, including empty outputs.
`prepare-local-completion-recovery` captures retained completed custody into an
external immutable packet. Its control returns that same terminal attempt to
review after checking bytes, index, base, process absence and completion events.
It preserves the original session and events and does not dispatch a worker.
Request-changes dispatch validates its saved binding before retaining dirty work;
the generic retry cleanup must not delete that reviewed revision input.

Terminal `meta.json` remains a monitor projection: `updated_at` and `idle_seconds`
change after process exit. Completion recovery schema 2 binds the stable session,
status, alive, provider, model and runtime fields instead of the whole file digest.
All other session files, candidate bytes, index, base and completion events remain
bound. A schema 1 packet can have a new immutable successor only after its exact
known custody-error ACK and matching remaining custody. The successor references
the failed packet and control; it never rewrites that packet or ACK. Unknown ACKs
require readback. The corrected control requires Host6, not only a new producer.
