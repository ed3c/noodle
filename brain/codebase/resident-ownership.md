# Resident ownership and shared admission

A repeated process name is a diagnostic candidate, not proof that its state or connections can be shared. Host-managed network services use context leases and the existing admission budget. The Host starts foreground services in an owned process group. The original live child handle authorizes graceful termination of that group. The last lease requests termination. Native Wait reaps the launcher; the allocation is released only after the original process and group are absent. Services that daemonize or leave the group require their own lifecycle interface and are outside this contract.

Persist starting before spawn. A lost Host handle leaves needs_process_readback; reconciliation can prove a recorded PID/start identity absent but cannot adopt a process. Session judgment does not transfer live stdio MCP or REPL child handles. A released context lease is terminal so a lost response cannot replay a service launch.

Controls: internal/admission/residents_test.go and cmd_host_residents_test.go. RSS inventory is not physical footprint or reclaimable memory. Worker, resident and total allocation counts remain distinct.


Resident mutations use an explicitly selected native context, found with
`host status` from only a repository or workspace. `--context-id` and
`--instance-id` select the retained data target; the historical `--request-file`
remains an optional selector. Neither original Owner nor original Session is an
eligibility condition. The Host revalidates the active context and request,
instance and native binary records before acquire/release. A delayed selection
cannot borrow a successor context's lease. The context lease remains the unit
of deduplication, including repeated acquire/release. Host absence, selected receipt gaps or a live readback refusal still permit
read-only retained resident facts, marked `persisted_only` and not live verified.

Unknown process identity is `needs_process_readback`: the Session resolves
retained process/start/group facts. The label does not require finding another
actor. Opening older non-exited records maps them to this same unknown state;
process absence remains necessary before allocation can be released.

If the launcher exits while its group remains, the record stays `needs_process_readback` and retains its allocation. Reconcile observes absence without signalling or adopting a PID. After Host restart, unresolved resident records block new worker and resident admission until absence is confirmed. Missing start identity or a legacy record without process-group identity remains unknown.

Host-managed process workers also retain their allocation until the original
launcher and process group are absent. Session completion waits for this evidence.
Terminate, ForceKill and cancellation use the original spawned group, including
after its launcher exits. Recovery refuses a live or unconfirmed group. This
contract does not adopt foreign processes or cover descendants that leave the group.
