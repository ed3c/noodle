# Initial admission retirement

The stopped Noodle owner exposes `admission inspect` and an exact `next.argv`
for retirement. CLI help and generated Noodle skill consume the same guidance
from `cmdmeta.AdmissionRecoveryGuide`.

Retirement binds original bytes and current canonical revision, keeps an intent
before removing the mailbox, and discovers that intent after process interruption.
It changes no canonical ownership, effect ledger, worktree or provider state.
An empty mailbox ends recovery; it is not permission to start the next task.

The legacy orders projection excludes completed/cancelled orders. Compare that
projection against its inclusion rule while retaining all canonical and ledger
ownership when rejecting duplicate initial admission. A completed A must not
prevent a fresh B or make A admissible again.

Historical pending dispatch/projection effects can outlive their sessions. Their
subject remains owned; an unrelated subject can be retired only with the stopped
owner lock and fresh absence of every recorded process and process group. Stale
derived session metadata is not proof of a live process or a successful outcome.

Observed limitation outside this correction: retry dispatch currently preserves
the worktree path but discards uncommitted files. Do not describe typed-blocked
requeue as preserving dirty progress. Preserve evidence and a verified commit
before requeue; no successful retirement control proves dirty-worktree recovery.
