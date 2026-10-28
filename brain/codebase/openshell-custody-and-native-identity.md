# OpenShell custody and native identity

- `runtime/openshell_custody*.go` provides a custody primitive, not a registered OpenShell Runtime or a gateway process supervisor. Formal dispatch and artifact application still need integration.
- Assign exactly one canonical state root to each dedicated gateway. The OS lock excludes cooperating local owners; it does not exclude an external CLI or add an atomic expected-ID condition to OpenShell's name-based API.
- Persist task, name, mutation UUID and intent before Create, exec or Stop. Keep unknown effects and never recycle historical names. Reopening reads state; it does not replay commands.
- Audit all workspaces without a label filter and consume every page. Unknown resources, missing IDs, replacements, specification drift or changed configuration binding block new dispatch.
- Bind the first nonempty gateway-owned `main_process_instance_id`, regardless of phase. A canonical main can complete before its first Ready observation. Binding only after Stopped missed active-runtime replacement; binding only at Ready also misses early completion.
- Persist that identity before using the observation. A later changed or missing canonical identity blocks custody. Driver FD/resource-version changes are not canonical identity changes; a stopped driver may legitimately clear its optional receipt.
- Historical fresh Get Stopped evidence detects later lifecycle drift, but never replaces a new recovery read. Stop ACK and VM Stopped are separate evidence: a lost ACK stays pending even when Get later reports Stopped.
- A new explicit attempt may retain its order only after all previous attempts have observed Stopped and have no unresolved create, stop or exec. New attempt/session identifiers and a fresh name are required; this is not automatic retry or native session resume.
- Initial retained resources require explicit same-ID Stopped receipts and remain read-only. They are not adopted as new workers.
- Close cancels local calls before waiting for the mutation mutex. It does not stop remote compute. Waiting for the mutex before cancelling can deadlock behind an outstanding RPC.
- The configuration owner supplies `ConfigFingerprint`; custody does not read private configuration or credentials. The public v0.1.5 protocol has provider mutation status APIs, but no generic read-only VM Create/Stop UUID receipt API was found. Do not substitute provider status for a VM operation ACK.
- Regression coverage includes `TestOpenShellCustodyActiveInstanceReplacement`, `TestOpenShellCustodyFirstReadyInstance`, `TestOpenShellCustodyStoppedHistory`, `TestOpenShellCustodyExplicitNewAttempt`, `TestOpenShellCustodyStoppedReadMustPersist` and `TestOpenShellCustodyCloseCancelsBlockedCreate`.
- Restricted worker TCP/process-query failures are not PASS. Preserve them and verify the same candidate bytes in the supervising environment; in-process mTLS tests supplement rather than replace TCP acceptance.

See also [[codebase/owned-worker-exit-and-recovery-evidence]] and [[codebase/runtime-routing-owned-by-orders]].

## Formal dispatch and artifact return

- `OpenShellRuntime` now reuses the local Codex/Pi protocol parser through native streams. Its session completion still waits for the original VM stop acknowledgement and a fresh same-ID Stopped read.
- OpenShell 0.1.5 cannot SSH-download from Stopped VMs. Guest launchers archive the workspace and native session before exiting; download those individual archive files while Ready, validate all archive headers, then stop and apply locally.
- Native directory download is not a security/completeness check: its tar library skips `..` members and permits outward symlink entries. `workspaceartifacts.Extract` rejects these instead of silently dropping them.
- Even `git apply --check` can execute a target clean filter. Artifact application uses an isolated Git directory plus highest-precedence attributes disabling text/filter/ident/working-tree-encoding; normal candidate commits must still execute repository hooks.
- Persist a local delivery intent before applying or publishing. A stopped VM cannot settle a partially applied local patch or an uncertain Git commit.
- OpenShell `stop`, `kill`, and `stop-all` must retain the original watcher and worktree until Session.Done. Returning from a stop request is not proof of stopping.
- Active native Runtime readback is not the same as cross-Host recovery. The existing shared-admission original-owner guard still refuses unresolved sessions from a lost Host; do not describe that guard as a working UI resume path.

## Reopening retained history after pre-start refusal

- A fixed profile may repeat its complete retained set. Compare exact name/ID/workspace membership against pending or initialized retained history, ignoring ordering and rejecting duplicate/add/drop/replace changes. Reopening never replays native effects.
- The earlier local error `retained initialization supplied for existing custody` can exist before any worker record. `noodle openshell recover-retained-init --profile <original-path> --expected-owner-sha256 <reviewed-hash>` only inspects; `--apply` additionally requires the exact binding, nonempty matching pending set and empty Records, preserving original bytes and before/after hashes before clearing that one reason.
- Recovery uses the original owner lock and makes no native calls. A pending or invalid recovery journal prevents future custody reservation. Only a completed durable receipt permits normal native readback; retained entries remain pending until that readback succeeds.
- Other block reasons, changed configuration, nonempty worker history, or existing recovery evidence refuse recovery. File-write failures stop immediately and retain evidence; this is not a general unblock, adoption or automatic retry facility.
