# Resident ownership and shared admission

A repeated process name is a diagnostic candidate, not proof that its state or connections can be shared. Private context-owned network services retain their original leases and admission budget. Native sharing admits only verified stateless test fixtures; transport or a repeated name does not establish safe sharing. The Host starts foreground services in an owned process group. The original live child handle authorizes graceful termination of that group. The last lease requests termination. Native Wait reaps the launcher; the allocation is released only after the original process and group are absent. Services that daemonize or leave the group require their own lifecycle interface and are outside this contract.

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
