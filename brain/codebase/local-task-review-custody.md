# Local task review custody

Local tasks use terminal evidence from their original runtime. They do not produce the Issue
`stage_message` contract. `loop/local_task_review.go` accepts this path only for
an admitted order with one `local-task` stage and a matching original
initial-admission record. Supported routes are Codex/process and Codex or Pi/OpenShell.
Other tasks retain the typed outcome check.

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

Local-task `write_paths` are literal file/subtree boundaries. Directory scope
requires `host task review-capabilities` to advertise
`local_task_directory_scope=1`. Git-visible descendants, including tracked
removals, receive individual candidate hashes; ignored build output stays out.
The native owner rechecks coverage, index and bytes, stages only those reviewed
files, and rejects additions introduced by commit hooks before integration.
Sibling prefixes, symlinks and traversal never expand a scope. Issue atom
contracts retain their own exact-file semantics. Frozen task inputs stay intact;
an older Host must use the existing safe runtime successor before directory
review effects. New Soodles admission checks this capability before dispatch.

Accepted control HEAD is frozen: even a later documentation commit changes the
source-bound acceptance subject. Record follow-up notes in the next work unit;
matching runtime bytes alone cannot preserve acceptance of a changed HEAD.

The on-demand OpenShell gateway owner reserves canonical custody before asking
Residents to spawn. Retained initialization sources survive failed startup and
readback. Native readiness follows only the original live child capability;
health, matching names and PIDs cannot reconstruct it after Host restart.
Transient native reads retain obligations; confirmed configuration or inventory
conflicts remain blocked. Only Residents' confirmed local exit permits owner
Close, independently of remote VM settlement. The TCP/mTLS tests require the
managed helper itself to serve; in-process coverage is supplementary.

A successful quiesce can seal dispatch before the release guard receives its
result. If delivery times out, retain the original lease and permit a fresh
native release check. Dispatch readiness is not release eligibility; successful
readback of sealed custody must not reopen dispatch or erase initialization.
The managed-child TLS fixture transfers the issuing CA, not the client leaf,
to its trust pool. Installed macOS VM-driver probes also require a short Unix
socket path; validate the prepared path before starting the original child.

OpenShell review requires the original runtime completion reader: published clean
candidate HEAD, successful native execution, original VM/main identity, stop ACK,
fresh Stopped observation and unchanged evidence sources. A missing gateway is
unknown; host process absence is not a substitute. Damaged-candidate merge
restoration remains unsupported for OpenShell and refuses before effects.
Explicit request-changes/requeue preserves prior attempts and starts a new
OpenShell session; process-only native continuation must not be inferred for it.
Tests in `loop/local_task_openshell_test.go` exercise both harnesses and refusal
without process.json. They do not prove Soodles outer-loop integration.

Host context reconstruction consumes the original released round and its saved
mode/dispatch state. It preserves the request and native attempt history while
creating a new context and allocation owner. A snapshot without its original
native session files or retained local-task worktree is incomplete custody;
reconstruction refuses before creating a runtime. A Git snapshot of tracked
source does not restore ignored native evidence. On macOS, the Git root is
compared with the execution context's canonical path, so `/var` and `/private/var`
do not produce a false mismatch. Native HOME comes from the task environment.

First legacy migration records the effective startup mode separately from
canonical mode. The original status is saved before a mode command. Missing
effective-mode evidence parks that task in the maintenance hold. An ACK alone
does not prove checkpoint persistence. The producer checks its transition,
epoch and ledger, then captures final dispatch state after workers finish.
The stopping journal retains unknown signal effects without repeating SIGTERM.

Durable worker bytes also need their Git admin owner. Moving only `.worktrees`
leaves its index vulnerable to prune after an App alias disappears. The native
Git owner repairs each registration to physical storage and preserves the
original alias binding. Storage preparation holds the original writer lock
through partial-alias recovery and validates Git ownership overrides before
any storage effect. This covers worker/runtime bytes, not an archive of every
dirty file in the control checkout.


The 2026-10-27 OpenShell readback restored its pending review on the shared Host.
The first controller had no dispatch capability. Explicit current-profile
selection then verified the original process thread metadata and worker path;
a repeated selection returned the same successor context. The order retained one
attempt and its candidate bytes/index. Soodles could materialize its candidate
through the original request. This was infrastructure recovery, not candidate
acceptance or a new writer execution.

Two custody defects were identified during recovery. Internal host-config reads
used an external-source symlink rule despite the Host's durable runtime alias.
The internal path now requires its storage source binding and rejects a missing
alias target. Separately, a saved typed refusal left admission in its prior
reconciliation state; historical receipts could hide that known nonacceptance.
Rebuild now distinguishes original/previous/current history from new acceptance,
while unknown and contradictory effects retain their original readback.
