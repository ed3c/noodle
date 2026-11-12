# Worker admission after warning grants

The current source uses `host-workers-v1` and quota schema 3. Each Host has 9 worker slots; rolling generations of the same Host share those slots. Different Goal Hosts have separate ledgers. Gateway residents retain their own allocations and lifecycle evidence without consuming worker slots.

Memory pressure, Swap usage, swapout rate and sample availability are observations. They do not gate admission or grant launch credit. The warning grant commands, attach flag, capabilities and grant readbacks were removed. Historical grant receipts describe the earlier policy, not current launch permission.

Unresolved resident ownership, shared-quota errors and uncertain returns still block admission. A full Host retains its original queue. Original process/group absence or native OpenShell stop evidence still controls release; model completion or a disconnected client does not return a slot.

`internal/admission/budget.go` and `shared_quota.go` own this policy. Controls include `budget_test.go`, `shared_quota_test.go`, `cmd_host_swap_used_test.go` and `cmd_host_gateway_warning_test.go`. Some historical test filenames retain the old policy name. Source or fixture success does not establish deployment.

The restricted migration accepts an unchanged empty schema-1 ledger only after retirement of its old writers. It retains the original bytes and creates a schema-3 ledger. Earlier schema-2 or warning-policy observations remain historical evidence; they do not prove the new migration has run.

Known pre-offer refusal and post-offer recovery remain separate. A missing owner record is not proof of nonacceptance. The current admission transfer requires the original Host's durable pre-admission refusal; sealed readback only reads an existing transfer.

A pre-offer Soodles runtime successor retains the original known deferred receipt, no proposal/order/session/mailbox effect, unchanged task inputs and a held exact new context. Adoption itself offers no task and dispatches no model. Same-card source migration uses `host contexts inspect --id ID` and `register --expected-version VERSION`; it retains the original request and old source archive. Its archive records intent, not execution or acceptance.

Root Go tests require built UI assets. A missing `ui/dist/client` embed is a build prerequisite failure, not evidence about this admission policy; see [[codebase/root-tests-require-ui-dist-assets]].

CLI details and evidence limits: [Goal Host](../../docs/reference/goal-hosts.md) and [shared resources](../../docs/reference/cli.md#共享資源額度swap-manager).
