# Native execution storage lifecycle

User decisions: original owner closes failures explicitly (reason + retained diagnostics); accepted successes reclaim automatically; unresolved/recoverable attempts remain; preserve designated historical hosts/VMs. Required evidence may grow, full execution copies must not.

1. Native custody retirement: persist closure + same-ID stopped observation + one-shot Delete intent, require original response identity and acknowledged completion/absence, keep tombstone/history; never equate lost ACK with success or externally delete VM files.
2. Compact original workspace copies into verified manifests + pinned original Git objects, retain original instructions/raw archives/telemetry/receipts; strict original owner worktree deletion.
3. Wire original Loop accepted-task / explicitly-closed-failure eligibility to runtime retirement, admission retention policies and automatic cleanup. No change to old explicit-retention requests.
4. Formal offline custody-version migration + versioned Host/Soodles rollout, verify repeated native no-model tasks plateau in allocated VM execution data; retain historical evidence.
5. Resume accepted source / Pi then Codex optimization, fixed checker, two unseen pairs per group, final delivery. Original cumulative USD5 cap remains.

No new model or VM dispatch until lifecycle implementation validated. Each unit needs tests/build and diff review before commit. App-managed control worktrees belong to App, not worker cleanup.

Implementation detail: new admissions freeze after_task_closure. Existing explicit_delete ledgers remain protected. Native custody schema 4 is mandatory because old binaries must refuse tombstones they cannot interpret. A formal offline migration will retain v3 before-image and hold original locks; no automatic in-place startup upgrade. Native Delete is name-based (documented limitation); response must bind exact UUID and ACK, all-workspace audit and original lease remain required.

Progress 2026-10-29: outer acceptance is a separate original-owner receipt. Inner merge no longer issues successful reclamation authority. New after_task_closure admissions are only the supported OpenShell local-task flow; generic/process/multistage requests retain existing explicit policy. Focused tests cover success waiting on outer acceptance, wrong session/head/conditions/evidence, explicit failed closure, unknown deletion ACK and ordering. Full final suite and real VM plateau remain pending.

Original shared Stage-A source independently integrated and accepted PASS. Frozen public task bundle: harness2-quality-20261029/training-bundle-01/task.json (af4453bfa88eba0cb46da6d49b3b1b96a60595ea098c4aae8afe5a3067171794). No optimization/unseen dispatch yet. Checker stays 42798104 / freeze-03.

Known created-worker failures before model submission now have a separate original diagnostic readback: exact Create ACK, no Exec, original Stop ACK and fresh same-generation Stopped. They retain failed/not_started and require explicit owner closure. Unknown operations, pre-worker failures and recoverable execution remain retained. Host acceptance response/detachment race and post-retirement original-owner readback are implemented; physical validation remains pending.

Final engineering check passed 2026-10-29: Node 22.17.1, pnpm 10.30.3, GOFLAGS=-p=2, pnpm check (build, all Go packages, vet, architecture checks, loop and hash fixtures). Log /tmp/h2-storage-final-check-01.log. Original Soodles Test Manager passed 21 selected controls; source committed c6c6d267eabf5cb75f8027d060b31424d0f0d203 and frozen separately. No physical VM controls or new model calls have run yet.
