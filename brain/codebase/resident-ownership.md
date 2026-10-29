# Resident ownership and shared admission

A repeated process name is a diagnostic candidate, not proof that its state or connections can be shared. Private context-owned network services retain their original leases and admission budget. Native sharing admits only verified stateless test fixtures; transport or a repeated name does not establish safe sharing. The Host starts foreground services in an owned process group. The original live child handle authorizes graceful termination of that group. The last lease requests termination. Native Wait reaps the launcher; the allocation is released only after the original process and group are absent. Services that daemonize or leave the group require their own lifecycle interface and are outside this contract.

Persist starting before spawn. A lost Host handle leaves needs_owner_readback; reconciliation can prove a recorded PID/start identity absent but cannot adopt a process. Keep stdio MCP and REPL lifecycle with their original owner. A released context lease is terminal so a lost response cannot replay a service launch.

Controls: internal/admission/residents_test.go and cmd_host_residents_test.go. RSS inventory is not physical footprint or reclaimable memory. Worker, resident and total allocation counts remain distinct.

Resident startup and last-lease release also save sampled descendant identities.
These records preserve the observed PID, start time, parent and group. They do
not authorize signals to an escaped descendant. When a recorded descendant
remains alive after its original launcher and group exit, the Host retains the
allocation and reports `needs_owner_readback`. Reconcile only observes absence.
An unobserved descendant can still exist: these lifecycle samples are not a
complete process launch history. Old records are not retroactively populated.
The escaped-group control is in `internal/admission/resident_descendants_test.go`.


Resident mutations require the original `--request-file`. The client reads its
native status; the Host then checks request, context, instance and binary again
before acquire or release. A delayed request for the same project cannot borrow
a successor context's lease. The capability is `resident_request_binding=1`.
Unbound mutation operations refuse. Old Hosts refuse the new bound operation;
clients must not retry through the older project-only operation.
The existing context lease remains the unit of idempotence, including duplicate
acquire and release. External stdio ownership is unchanged and cannot be adopted.

If the launcher exits while its group remains, the record stays `needs_owner_readback` and retains its allocation. Reconcile observes absence without signalling or adopting a PID. After Host restart, unresolved resident records block new worker and resident admission until absence is confirmed. Missing start identity or a legacy record without process-group identity remains unknown.

Host-managed process workers also retain their allocation until the original
launcher and process group are absent. Session completion waits for this evidence.
Terminate, ForceKill and cancellation use the original spawned group, including
after its launcher exits. Recovery refuses a live or unconfirmed group. This
contract does not adopt foreign processes. Observed descendant generations retain
separate exit evidence; escaped or unsampled descendants remain unknown.


## Native consumer fixture leases

`host resources residents acquire --native-consumer` derives its caller from the
Unix socket peer, the original MCP parent generation and its native registry.
The caller cannot supply a Session, PID or worktree identity. The original
request binding must still match. Independent App chats require the bound
parent's explicit membership record; native agents require their own captured
assignment. Same-repository worktrees retain distinct roots, gitdirs and leases.

Each acquire returns one `consumer_lease`. Release requires that exact lease and
native connection. One consumer's release leaves every other active lease on the
same child. The existing Host mutex owns acquisition and release; the original
single Host Budget charges one allocation per resident. No generic I/O proxy or
cross-generation Host pool is introduced by this adapter.

The existing Host lifecycle checks native MCP generations. Explicit release or
confirmed original generation exit ends a lease; turn completion, Stop, low CPU,
missing observations and elapsed idle time do not. Last release normally stops
the verified fixture child. Accounting waits for native reap, its group and all
observed descendants to exit. Unknown control results remain readback-only.

Stateless fixtures register their helper, dedicated test purpose, native owner
and child generation before recovery. Historical recovery uses an exact frozen
manifest through the bound `recover` operation. Name and PID alone never grant
control. Existing recovery intents are not repeated. The supported native
platform uses generation-bound controls; unsupported access remains unknown.
The Host tick projects confirmed recovery exit back into the original resident
ledger. Native fixture sharing remains a single live owner contract. The separately
confirmed [[swap-used-host-maintenance]] boundary can preserve retired Host
metadata and migrate an empty quota; it does not adopt live resident handles.


The resident owner may declare `force_after_grace` only for services with no
state to preserve after the last lease. The default is false. The Host waits
two seconds after graceful termination, then rechecks the original child and
leases before a force signal. Signals and the sole Unix waiter share a mutex;
the child cannot be reaped and its PID reused during signalling. Unknown
identity retains the allocation. Windows reports this force capability as
unsupported. Records retain previous generations and failure reasons.

Reconciliation waits until the original completion has saved its evidence.
An old waiter cannot remove a newer generation's child or release its quota.
Tests in `internal/admission/resident_escalation_test.go` cover the shared
lease, force-disabled, unknown identity and completion/replacement boundaries.

Completed-context cleanup is separate from migration detach. Cleanup runs
through the original Loop actor and control lock. It requires completed
canonical business orders, no pending controls/reviews, and resolved original
effects and sessions. A local-task completion without its accepted merge
binding remains available for recovery. A new admission can bind automatic
cleanup; legacy requests remain unchanged and manual migration holds remain.
The existing Cycle and resident-release notification apply this policy.

After a successful Run exit, the Loop supplies its terminal cause to the
original Host receipt: `auto-completed`, `explicit-cleanup`, or
`migration-detach`. The two automatic triggers share one cause. Original
`host status` reads that saved cause with the context and request identity.
Busy inspection, failed exits and unresolved receipt persistence do not report
a successful cause. Missing causes in historical receipts and `once` runs
remain unknown; status does not infer them from policy or replay release.
This cause identifies the runtime release path, not task acceptance, physical
memory recovery or Parent Goal completion.

Task admission may select `host attach --config-file` (also supported by
`host once`) when `external_task_config=1` is present. The original request and
receipt bind the absolute regular source path and its byte digest. The Host
checks both before constructing the runtime; the repository configuration is
not overwritten. Request-based status and detach retain this identity even if
the source later becomes unreadable. Reattaching cannot change the source.

Under the project lock, the Host saves the explicit source reference in
`.noodle/host-config.json`. Offline review, completion recovery and worktree
retention use `config.LoadRuntime` to read those same bytes. Missing or changed
source files refuse rather than select repository defaults. Another Host
request can use the same source, but cannot replace its path or digest. This
file is a configuration reference, not a second task or ownership record.
Projects without this reference still use their repository configuration.
Deleting runtime state (including `reset`) destroys the reference; it does not
establish continuation of the original task. Original admission readback is
still required. These checks do not recover deleted owner evidence.

An original merge failure with observed Git conflicts must close its uniquely
matching effect as failed. Generic command errors remain unresolved because
they do not prove that the ref update failed. Otherwise an approved retry creates a second unresolved
effect for the same stage and success cannot establish unique custody. Keep
the original failure result when the retry succeeds; historical labels and
ambiguous effects still cannot establish a result. The conflict-resolution
integration test exercises failure, approval and retry through the real loop.

Two producers had hidden this completion evidence. Canonical projection
updates now retain omitted terminal orders. A native MergeCompleted transition
completes only its uniquely matching merge effect in the same checkpoint.
Historical completed labels do not repair unknown effects. Their original
recovery evidence is still required. Cleanup tests do not prove full machine
memory recovery, native task acceptance or Parent Goal completion.

Stateless test fixture recovery is a separate operation on the existing Host.
The original request binds `host resources residents recover --manifest-file`.
Historical input is a frozen explicit list; future fixtures are registered by
their launching owner. The journal preserves source evidence, owner history,
kernel generation, each control intent/result, and exit readback. Inspect never
schedules or persists recovery. Existing lifecycle ticks and bound reconcile
process eligible registered fixtures without creating another daemon.

Darwin cgo recovery obtains an opaque audit token through task-name access.
`proc_signal_with_audittoken` targets one kernel PID version, never a group.
Frozen UID/start/argv/cwd/log/image/group evidence is checked before TERM and
again before KILL after two seconds. Unsupported native access, changed identity
and ambiguous effects remain unknown; existing control intents are never replayed
after restart. Other platforms have no PID-signal fallback. Original child
fixtures retain their unreaped-child protection and are signalled individually.

Exact helper entry points also have a test-only parent lifetime and cooperative
exit marker. This prevents new orphan helpers when normal teardown loses its
release result. A deliberately bounded legacy helper mode exists only to test
the recovery path. Tests cover owner preservation, orphan recovery, TERM/KILL,
read-only snapshots, interrupted intents and kernel refusal after exec.
