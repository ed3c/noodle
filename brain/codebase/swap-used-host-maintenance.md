# Swap used capacity and restricted Host maintenance

Worker admission uses `host-workers-v1`: one Host shares nine worker slots
across its generations. Residents retain separate leases without consuming worker
slots. Pressure, Swap and sampling remain diagnostic observations. Unresolved
custody and quota returns still block admission; active permits remain owned.

The restricted legacy handoff is an explicitly authorized compatibility boundary.
It freezes accepted request/config/source generations in a digest-bound plan,
persists each intent before its effect, and reads back uncertain outcomes without
repeating controls. Pause ACK must confirm dispatch is paused; the original actor
must then export its canonical checkpoint. ACK alone is not checkpoint proof.

Only native retirement of the source process/group and original runtime locks
allows empty schema-1 quota migration. Schema 3 fixes the nine-worker policy
and preserves original bytes separately,
including the retired capacity/window and unknown fields. Policy disagreement
still refuses admission. Historical owners and version records are
custody metadata. All original work remains held; no queue runner or context
rebuild is present in this scoped delivery. Execution profiles and context inputs
remain opaque JSON rather than pulling unrelated dispatcher/recovery code into
maintenance.

When porting an already validated candidate, audit its dependency closure against
remote main. Do not merge unrelated candidate history to satisfy missing types.
Verify the reduced integration with a fresh, controlled native fixture as well as
the full repository check. Never replay production maintenance as a source check.

Controls: cmd_host_restricted*_test.go, cmd_host_generation_test.go,
internal/admission/budget_test.go and quota_migration_test.go.
