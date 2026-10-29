# OpenShell custody and native identity

## Shared control-root handover

The custody format is v4. Ordinary reservation requires an explicit Host control root and refuses a v3 ledger without changing it. Host passes its original pool control root; profile and dispatch fingerprints keep their original representation. The v3-to-v4 step belongs to the confirmed Host maintenance journal and preserves a settled before-image, empty-resource observation, source identity and immutable handover references. Schema-2 release acquires the original custody lock and writes v4 before releasing that lock. The inspected 119036b6 client rejects v4 before native initialization, so source process exit is unnecessary for this transfer. Another control root cannot reserve the resulting custody. Schema-1 historical release still requires its original process-exit proof. A snapshot copies its handover reference rather than exposing mutable owner data.

Gateway handover and Host retirement have separate acceptance. Completed-context handovers send no process signal. Unknown cleanup preserves its native status/error; it does not prove every task is unfinished. The original journal reader must recognize handover phases and cleanup-intent, while refusing same-root pause/checkpoint phases for a handover. Otherwise a saved transaction cannot resume even though its writer succeeded. Fixtures prove only these scoped transitions, not actual private Host release or R3 admission.

## Qualified failed-compute cold handover

The separate storage-failure path extends original `host switch --maintenance-plan --handover-root` with `--failure-recovery-file`. It reads the qualified Stop receipt and its independent original inputs from the pinned 19402a60 reader. It does not send another Stop. It accepts only the inspected 119 storage-persistence fault with one failed worker and its dedicated gateway responsibility. Every known VM must have fresh same-ID/main Stopped evidence. Other work, unknown Create/Stop, foreign leases and unbound actors refuse retirement.

The confirmed maintenance journal fences new admission before exact-generation control. It saves each intent before the kernel call. An unknown result only reads that generation back. For a source already absent, `--source-plan-file` binds the original CLI's observed kernel identity and the original recovery resource/history snapshots. The source Host lock prevents another local owner during maintenance. Historical counters remain nonzero archival data. They do not claim the departed Host completed normal cleanup.

Release schema 3 binds the original failure, before-image, native inventories, actor intents and observed exits. A compatible shared receiver is required before this release. Custody remains v4. The exclusive original custody lock protects its upgrade; no history, command or missing Exec result is rewritten. Fresh native reads may add Stopped observations. Only the exact failed record can use the cold proof for resource Quiesce. Create retains the unresolved-effect refusal for a new attempt of that failed order. The new R3 is a separate task and still needs normal admission, execution and source readback.

The cold release exception does not change the ordinary service's force-after-grace policy or the completed-context release-2 path. Source tests and native read-only inventory are separate from real retirement, R3 admission and task acceptance.

The explicit retained-initialization repair remains able to repair its exact v3 pre-start rejection without upgrading its version. This local repair does not establish native inventory, grant a shared lease or bypass the v4 handover. The completed-owner handover compatibility contract covers the inspected clean 119036b6 source; the earlier same-root bootstrap contract remains separate. Source controls, fixture-process exit, deployed Host identity and real task acceptance remain different evidence.

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
- `status=observed` means model execution, a retained candidate and original stop evidence are present. A changed candidate has a published commit. A read-only candidate retains its original HEAD through `workspaceartifacts.Publish`, with an empty patch, unchanged HEAD/index/worktree and a `publish.json` whose phase is `unchanged`. It does not create an empty commit. Canonical status is reported independently, and canonical `attempt_id` differs from runtime `native_attempt_id`. This does not make a failed local task accepted, enable process-only recovery for OpenShell, or authorize merge/continuation.
- Reports expose source paths/digests and selected observation fields, never whole custody command/environment records; `effects=[]` applies to the readback operation. The running Host may independently checkpoint, and source drift causes refusal.

- Readback cannot use normal `git status` as a side-effect-free cleanliness check: fsmonitor hooks and clean filters may execute even with `--no-optional-locks`. Use `workspaceartifacts.ReadCleanHead`, which compares HEAD/index/raw worktree bytes through the existing isolated plumbing and leaves the real index untouched. Inherited `GIT_*` selectors must never redirect the observed repository; normal publication hooks remain enabled.

## Frozen guest instructions and completion proof

- The admitted local-task prompt supplies an explicit manifest reference and reasoning selection before worktree creation. OpenShell local tasks cannot omit the reference; ordinary bare runtime calls remain a separate, explicit capability.
- The materialization consumer copies the complete enumerated frozen files outside the returned repository, preserves bytes/modes, and rejects path ambiguity, private files and unlisted entries. Original-source paths are provenance only, never fallback reads. A pinned Linux verifier checks the transported tree before replacing itself with the harness in the original native Exec.
- A completion reader must compare the persisted command against a SHA-256 of the original custody owner's in-memory command. Matching only native request/attempt IDs would let edited disk argv invent verifier execution. Return the digest, never command environment values.
- Missing manifest/mapping cannot downgrade a verified execution into a bare harness execution. The original command must either bind the exact verifier/order/manifest or directly select the configured bare harness. Recheck transported manifest/files/modes and bounded mapping/input refs around completion readback.
- Test mock Terminate only requests termination; it does not close Done. Watcher cleanup tests must use the existing mock completion signal before waiting, just as real cleanup must wait for original native completion.

## Shared deployment and per-context worker policy

- `gateway.identity_source` explicitly names the canonical native configuration file in `gateway.config_files`. Gateway custody binds all shared configuration/TLS/client/Resident fields, not the task profile path, payload or route policy. Existing combined custody bindings are not migrated automatically.
- Host must pass each attached context its selected profile, even when the gateway owner is reused. Reusing the first owner's profile silently dispatches the wrong network policy. Native Create still persists and audits the complete per-worker specification.
- File profiles retain the original dependency paths and byte digests; verification must not re-decode an edited profile and follow newly introduced paths. Runtime clones route protobufs at construction and binds the complete dispatch digest into completion readback.

## Stopped progress and explicit new attempts

- Progress output must stay inside the native CLI's `/sandbox` download workspace and outside both archived input trees. A permissive transfer mock hid the former `/tmp` mismatch: the guest drained and archived successfully, then the Host retained `progress-unknown` because the CLI refused the download. Keep the workspace restriction in the transfer fixture. External copies of the archives do not create native Stop evidence or a canonical ProgressHead.
- An interrupted helper result is not a completed candidate. The original Runtime waits for both owned native exec results, checks the two archive digests and contents, returns them while the VM is Ready, then obtains Stop ACK and fresh Stopped. Saved failed/cancelled work is `ProgressHead`, never `CandidateHead` or task acceptance.
- `InspectSavedProgress` requires the latest cancelled/failed local-task review, done initial admission, settled effects and original Runtime evidence. It rebinds the original transported materialization, reasoning, role and canonical prompt. The consent digest excludes only the checkpoint clock; all semantic state and runtime source refs remain bound.
- `QueueSavedProgressAttempt` appends one exact version to the original control queue. The Loop rechecks it after resource deferral, persists and syncs a one-shot offer in Stage.Extra, then uses original dispatch with a new attempt/session and the saved worktree. Paused scheduling remains paused; no native conversation resume, fallback, adoption, automatic retry or generic requeue is implied.
- Existing `spawnCookAdmitted` can record a local preflight failure and return nil. The explicit new-attempt handler must also observe the registered successor before returning a successful control ACK. An offer with no known successor remains inspectable original history and cannot be replayed.
- Focused tests distinguish queued from started, stale consent, unknown native stop, changed task inputs, memory deferral and one-shot refusal. Fixture repairs used canonical path equivalence on macOS and registered the original local-task skill; otherwise the general registry refresh drops the artificial fixture order. The earlier native v3 helper proof does not establish this later Host/Loop integration.

## Review-only tool transport integration

The final-review set uses manifest schema 2 `tool_files` through the original
instruction transfer and verifier. Ordinary schema 1 budgets are unchanged.
`openshell_role_tool_payload` and `openshell_saved_progress_attempt` advertise
different capabilities; integrating saved-progress Host code retained both.
The active older Host has not acquired either behavior from local commits.

The first progress import omitted its shared-gateway identity prerequisite.
That conflicted series was aborted before importing the prerequisite and the
original dependent commits. The integration retained unchanged publication
for both successful read-only candidates and stopped progress. Only the former
can satisfy completed-session readback. The added unchanged-progress fixture
initially failed because the test variable was not in the explicit profile
environment; binding it there made both changed and unchanged cases pass.

The Host CLI import overlapped the request struct and capability fixture.
Integration preserved the existing fields, added the original expected version
and retained both capabilities. Unrelated RequiredHostInstanceID changes were
not imported without their producer. Focused Loop controls, Host race controls
and the normal build passed. Native guest review and live new-attempt acceptance
remain unobserved; no process fallback or parallel Host was used.

## Completed-context cleanup

- A drained archive-transfer failure needs an explicit post-dispatch handoff; the pre-offer runtime successor cannot recover effects that already happened. Maintenance binds the original command, both native exec exits, original archive receipt and immutable before-images. Its fixed session reservation prevents another plan directory from repeating Stop, including a lost ACK. Register the Host startup fence before any Stop effect.
- Release the original gateway through its living child owner before stopping the old Host. Preserve the original launcher waiter result separately from PID absence; neither reconstructs a child capability. Offline restoration holds Host, project and custody locks through the final handoff marker, publishes only cancelled ProgressHead, and preserves the original failed dispatch and full Host history. Native completion readback rechecks the entire proof chain before the existing Loop parks the same attempt for review. These source controls do not establish live recovery or candidate acceptance.

- Native session tar files contain guest-local absolute executable links. Retain and hash the exact opaque archive; only repository output needs safe extraction. Weakening the extractor or deleting session entries would change the original evidence. Session readback and cleanup must both verify the retained raw archives.
- A restore that fails during transfer can continue only through explicit inspection of the same plan: original failed intent and custody/Host before-images remain intact; repository HEAD, index and bytes still match the original baseline; retained extraction exactly matches the original repository tar; no delivery, completion or prior continuation exists. Independent original native Host responses must account for the full Host before-image; matching live and snapshot files alone does not establish history. The continuation reserves a separate one-shot intent and never repeats native Stop or gateway release.
- Startup invokes recovery for every registered runtime. The process scanner must leave a session with a verified explicit native maintenance handoff to its registered OpenShell owner instead of demanding or creating `process.json`. A native display tag alone is insufficient; conflicting process records, changed original spawn or incomplete handoff remain errors. The native Recover path freshly verifies stopped progress even after the canonical attempt has already reached cancelled review, and missing native runtime registration still blocks admission.
- A successor can successfully park maintained progress, then fail a later startup check. Its original Host may normally close all resources while preserving a failed receipt and request. `openshell progress-maintenance retire-successor` explicitly binds the original maintenance history plus that one captured failed startup response, exact request, normal exit-zero launch/wait pair, released generation and zero accounting. Offline inspection/apply holds Host and project locks, verifies present absence and the unchanged cancelled review, and publishes only a separate resource-retirement marker. Startup revalidates every marker and its complete historical prefix, allowing legitimate appended rows; it never clears failed task outcome or admits the old request. The archived evidence bytes stay immutable; later normal Host saves preserve the supported row semantics rather than whole-file JSON formatting.

- Generic offline admission/recovery remains process-only; the explicit progress maintenance above has its own bound handoff. Cleanup runs inside the quiescent original Loop and pairs every canonical attempt with exactly one local session; missing, orphan, duplicate or unresolved sessions refuse release.
- OpenShell cleanup borrows the existing Runtime and original gateway lease for fresh per-session native Get. Completed publication and saved cancelled/failed progress retain distinct outcome checks. A failed pre-start attempt without complete retained progress is not assumed safe.
- Accepted worktrees can already be removed or reused. Cleanup verifies the exact published commit's parent/tree against retained baseline and returned bytes through read-only Git plumbing, plus both saved archive digests. Normal session readback still requires its own clean worktree and exact HEAD.
- Cleanup compares the stable terminal metadata projection before/after observation, excluding only non-authoritative monitor detail fields from its source refs. Session/provider/model/runtime/status/alive/exit-pending changes still refuse. It does not claim an atomic full metadata snapshot or relax normal readback.
- Do not Quiesce a shared gateway while checking one context: that seals admission for other leases. Each session is freshly observed; final lease Release retains the existing whole-inventory Quiesce and shutdown boundary.

## Cleanup after candidate review rejection

A local task's old attempt can be canonical `failed` after request-changes while its original OpenShell dispatch remains a completed published candidate. Cleanup reads the typed StageFailed/OrderFailed request-changes events before the same order's requeue, then verifies completed-runtime evidence for that session. A later canonical attempt must exist. Missing history, foreign session/reason, changed source, unstopped execution or incomplete publication still refuses cleanup. Canonical attempt history is unchanged; this read grants neither retry nor acceptance. `loop/cleanup_review_history.go` and `TestCleanupReviewRejectionUsesOriginalTypedRequeue` cover this distinction. Deploying this fix to the shared pool does not modify or release another private Host.
