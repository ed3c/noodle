# Settle a retained call after original owner closure

`stopped_settlement.settle_stopped_call(bridge, gate, call, migration_ref,
billing_receipt=None)` consumes an explicit Soodles stopped-reader migration.
Session first uses the pinned reader's `inspect` and `accept` operations to
record the original integrated acceptance. The bridge invokes only its
`inspect`, requires PASS on that same candidate, and settles the existing
unknown journal row. It never calls the original CLI, review callback, native
optimizer, Host, or model.

Construct the bridge from the exact original runner/config with `reopen=True`
and its unchanged serialized template. Today's additional template fields are
not defaults for an old call. For a frozen training runner, cherry-pick this
narrow seam onto that runner's source instead of migrating its request shape.
The new reader has its own pinned source closure in the migration; it never
claims to be the original active execution owner.

The migration is the Soodles `stopped-original-task-integration` schema. It
binds the original request/candidate, actual Noodle stopped reconciliation,
original capture, verified closure, native offline reader and new Soodles
reader sources. New evidence is saved under
`CALL/stopped-settlement-<migration-sha>-<unique>/`; original operations, result tree,
failed native cache and unknown journal remain untouched. Reconciliation
archives the previous journal and adds its full prior row to history.
Each explicit read gets a new evidence directory; an earlier refused read or
timeout is retained. This does not automatically retry the reader or model.

Without a verified original billing receipt, cost/time remain null and the
paid hold remains reserved. A receipt may settle the original bill through the
existing OpenRouter verifier; there is no new provider request in this seam.
Elapsed time continues from the original journal start. Only the already
accepted result is cached; independent benchmark quality is unchanged.

This entrypoint settles one retained call only. A caller may later consume the
existing same-namespace native cache through the original runner. Settlement
does not start another optimization round or silently repeat a model call.
