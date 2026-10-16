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
