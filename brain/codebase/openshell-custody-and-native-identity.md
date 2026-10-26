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
