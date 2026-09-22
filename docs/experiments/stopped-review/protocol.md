# Noodle stopped-review correction — fixed supervisor protocol

Source 3732a02f9994c23bbb2be30c31801b7a2da586af; target ed3c/noodle#92.
This is deterministic CLI correction, not a fresh Agent behavior comparison.
No P-class improvement is claimed. No unchanged failed run retries.

Supervisor fixes before product edits: one happy-path fixture with real local
Git worktree, one table of nearest refusal cases (live lock/process, moved or
dirty Git, foreign subject, stale digest), and one table with fault cuts after
intent, canonical persistence, and cleanup. Require exact rejection through
existing reducer, candidate archive recoverability, no unrelated file/provider
effects, exact cleanup readback, idempotent terminal re-entry. A refusal must
not mutate the owner or destroy candidate/source. All controls use disposable
local fixtures. PIDs are observed, never signalled. Test cases are not samples
of natural Agent mistakes. Do not weaken rules for treatment.

Allow effects: this Issue/body; existing owner worktree creation and dependency
installation; candidate source/tests/generated CLI skill in that worktree;
local test fixtures and evidence. Publication follows the existing target
authorization and exact-head Test/landing workflow; missing admission blocks
that effect alone. No direct merge, branch protection changes, or live #145
state transition before independent qualification.

Baseline binary lacks review inspect/reject; save raw command exit and owner
readback. Candidate controls and existing admission/publication controls run
together. Final candidate requires pnpm check, generated skill consistency and
Linux exact-head checks. Record source diff, executable digest and raw outputs.
Fault-cut coverage does not claim power-loss durability beyond filesystem APIs.
The live original Soodles checkpoint remains separately owned and unfinished.
