# OpenShell custody and native identity

- `runtime/openshell_custody*.go` owns native identity and operation receipts. `OpenShellGateway` manages the foreground service; `OpenShellRuntime` is wired through Host/Loop for Codex/Pi and validated artifact publication. Keep source/unit-test completion separate from real model acceptance and Soodles outer-loop integration.
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

## Original Host session readback

- `noodle --project-dir CONTROL host task session-readback --request-file ORIGINAL --order-id EXACT --session-id EXACT` connects only to the original active Host, with exact binary/request/context/config bindings. It does not launch a missing Host, consume Loop completion, or rewrite the original request.
- The original Loop binds the canonical order/stage/attempt/session and delegates to its existing OpenShell Runtime. Dispatch, spawn, terminal metadata, delivery/publication, native custody and candidate HEAD must agree and remain stable around one fresh original-gateway Get.
- `status=observed` means model execution, published candidate and original stop evidence are present. Canonical status is reported independently, and canonical `attempt_id` differs from runtime `native_attempt_id`. This does not make a failed local task accepted, enable process-only recovery for OpenShell, or authorize merge/continuation.
- Reports expose source paths/digests and selected observation fields, never whole custody command/environment records; `effects=[]` applies to the readback operation. The running Host may independently checkpoint, and source drift causes refusal.

- Readback cannot use normal `git status` as a side-effect-free cleanliness check: fsmonitor hooks and clean filters may execute even with `--no-optional-locks`. Use `workspaceartifacts.ReadCleanHead`, which compares HEAD/index/raw worktree bytes through the existing isolated plumbing and leaves the real index untouched. Inherited `GIT_*` selectors must never redirect the observed repository; normal publication hooks remain enabled.

## Frozen guest instructions and completion proof

- The admitted local-task prompt supplies an explicit manifest reference and reasoning selection before worktree creation. OpenShell local tasks cannot omit the reference; ordinary bare runtime calls remain a separate, explicit capability.
- The materialization consumer copies the complete enumerated frozen files outside the returned repository, preserves bytes/modes, and rejects path ambiguity, private files and unlisted entries. Original-source paths are provenance only, never fallback reads. A pinned Linux verifier checks the transported tree before replacing itself with the harness in the original native Exec.
- A completion reader must compare the persisted command against a SHA-256 of the original custody owner's in-memory command. Matching only native request/attempt IDs would let edited disk argv invent verifier execution. Return the digest, never command environment values.
- Missing manifest/mapping cannot downgrade a verified execution into a bare harness execution. The original command must either bind the exact verifier/order/manifest or directly select the configured bare harness. Recheck transported manifest/files/modes and bounded mapping/input refs around completion readback.
- Test mock Terminate only requests termination; it does not close Done. Watcher cleanup tests must use the existing mock completion signal before waiting, just as real cleanup must wait for original native completion.

## Terminal projection stability

- The monitor previously rewrote terminal `meta.json` every poll solely for `updated_at` and `idle_seconds`. A source-bound completion readback could correctly refuse even after VM shutdown and clean publication, because this display churn raced its evidence hashes.
- Keep terminal metadata bytes unchanged when every substantive observation/claim field is unchanged. Continue observing and publish late cost/log/claim changes or renewed liveness; never freeze a still-alive or exit-pending session merely because a completion event exists.
- A diagnostic direct readback is not a substitute for the outer owner accepting its own full packet. Retain transient refusals; any bounded read-only re-observation must preserve all owner, command, source and native-stop checks and must not dispatch another model attempt.

## Shared deployment and per-context worker policy

- `gateway.identity_source` explicitly names the canonical native configuration file in `gateway.config_files`. Gateway custody binds all shared configuration/TLS/client/Resident fields, not the task profile path, payload or route policy. Existing combined custody bindings are not migrated automatically.
- Host must pass each attached context its selected profile, even when the gateway owner is reused. Reusing the first owner's profile silently dispatches the wrong network policy. Native Create still persists and audits the complete per-worker specification.
- File profiles retain the original dependency paths and byte digests; verification must not re-decode an edited profile and follow newly introduced paths. Runtime clones route protobufs at construction and binds the complete dispatch digest into completion readback.

## Stopped progress and explicit new attempts

- An interrupted helper result is not a completed candidate. The original Runtime waits for both owned native exec results, checks the two archive digests and contents, returns them while the VM is Ready, then obtains Stop ACK and fresh Stopped. Saved failed/cancelled work is `ProgressHead`, never `CandidateHead` or task acceptance.
- `InspectSavedProgress` requires the latest cancelled/failed local-task review, done initial admission, settled effects and original Runtime evidence. It rebinds the original transported materialization, reasoning, role and canonical prompt. The consent digest excludes only the checkpoint clock; all semantic state and runtime source refs remain bound.
- `QueueSavedProgressAttempt` appends one exact version to the original control queue. The Loop rechecks it after resource deferral, persists and syncs a one-shot offer in Stage.Extra, then uses original dispatch with a new attempt/session and the saved worktree. Paused scheduling remains paused; no native conversation resume, fallback, adoption, automatic retry or generic requeue is implied.
- Existing `spawnCookAdmitted` can record a local preflight failure and return nil. The explicit new-attempt handler must also observe the registered successor before returning a successful control ACK. An offer with no known successor remains inspectable original history and cannot be replayed.
- Focused tests distinguish queued from started, stale consent, unknown native stop, changed task inputs, memory deferral and one-shot refusal. Fixture repairs used canonical path equivalence on macOS and registered the original local-task skill; otherwise the general registry refresh drops the artificial fixture order. The earlier native v3 helper proof does not establish this later Host/Loop integration.

## Completed-context cleanup

- Offline admission/recovery remains process-only. Cleanup runs inside the quiescent original Loop and pairs every canonical attempt with exactly one local session; missing, orphan, duplicate or unresolved sessions refuse release.
- OpenShell cleanup borrows the existing Runtime and original gateway lease for fresh per-session native Get. Completed publication and saved cancelled/failed progress retain distinct outcome checks. A failed pre-start attempt without complete retained progress is not assumed safe.
- Accepted worktrees can already be removed or reused. Cleanup verifies the exact published commit's parent/tree against retained baseline and returned bytes through read-only Git plumbing, plus both saved archive digests. Normal session readback still requires its own clean worktree and exact HEAD.
- Cleanup compares the stable terminal metadata projection before/after observation, excluding only non-authoritative monitor detail fields from its source refs. Session/provider/model/runtime/status/alive/exit-pending changes still refuse. It does not claim an atomic full metadata snapshot or relax normal readback.
- Do not Quiesce a shared gateway while checking one context: that seals admission for other leases. Each session is freshly observed; final lease Release retains the existing whole-inventory Quiesce and shutdown boundary.
