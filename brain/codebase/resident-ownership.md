# Resident ownership and shared admission

A repeated process name is a diagnostic candidate, not proof that its state or connections can be shared. Host-managed network services use context leases and the existing admission budget. The Host starts foreground services in an owned process group. The original live child handle authorizes graceful termination of that group. The last lease requests termination. Native Wait reaps the launcher; the allocation is released only after the original process and group are absent. Services that daemonize or leave the group require their own lifecycle interface and are outside this contract.

Persist starting before spawn. A lost Host handle leaves needs_owner_readback; reconciliation can prove a recorded PID/start identity absent but cannot adopt a process. Keep stdio MCP and REPL lifecycle with their original owner. A released context lease is terminal so a lost response cannot replay a service launch.

Controls: internal/admission/residents_test.go and cmd_host_residents_test.go. RSS inventory is not physical footprint or reclaimable memory. Worker, resident and total allocation counts remain distinct.


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
contract does not adopt foreign processes or cover descendants that leave the group.

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

Two producers had hidden this completion evidence. Canonical projection
updates now retain omitted terminal orders. A native MergeCompleted transition
completes only its uniquely matching merge effect in the same checkpoint.
Historical completed labels do not repair unknown effects. Their original
recovery evidence is still required. Cleanup tests do not prove full machine
memory recovery, native task acceptance or Parent Goal completion.
