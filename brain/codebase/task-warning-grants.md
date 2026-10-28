# Task warning grants

- `host resources grant-warning --request-file PATH --order-id ID` allows one explicitly admitted task launch during stable memory warning; capability `task_warning_grant=1`.
- This is permission to launch, not additional RAM or a memory limit. The ordinary pressure snapshot stays truthful: warning and effective limit zero.
- Bind the original Host instance, context, request, binary, configuration path and digest. Read exact config bytes and require the selected order in `runtime.admitted_orders`.
- Grant requires three fresh warning samples with zero swapout, no active allocation, and no unresolved resident ownership. Critical, stale, failed, gap and reset samples still block.
- The grant expires after ten minutes, is consumed once, and never renews on duplicate requests. Detach revokes an unused grant; Host exit discards all grants. A normal launch also consumes a pending grant so a later retry cannot reuse it.
- Scheduler, bootstrap and resident admission have no task identity and cannot consume grants. Automatic replacement retains the normal pressure gate.
- A selected warning task may pass a blocked ordinary waiter, but only when the shared budget has zero active allocations. Other tasks remain deferred.
- Original process/group absence still controls allocation release. A launcher exit or model completion event is insufficient.
- Read `warning_grants` in `host resources` to distinguish available, consumed, expired and revoked permissions.
- Controls: `internal/admission/warning_grant_test.go`, `cmd_host_warning_grant_test.go`, the warning variant of the admitted-order loop cycle, and the real process-group retention test.
- Root Go tests require built UI assets. Run `pnpm build` first, or `pnpm check`; do not treat a missing `ui/dist/client` embed as a feature failure.

## Explicit OpenShell gateway permission

- Capability `openshell_warning_grant=1` is separate from the original process-only grant. `host attach --warning-order-id ORDER --require-existing-instance HOST` binds one manual admitted OpenShell order before gateway startup; native request bytes retain that selection.
- Its two phases admit the exact resident scope, then one worker while that sole gateway allocation remains. `gateway_warning_grants` retains the pressure evidence and permission status. Another resident, active worker, critical pressure, swapout or unknown observation still refuses admission.
- `host resources grant-warning --openshell-gateway --previous-revision REV` explicitly advances a terminal permission. Repeating the previous revision reads the existing permission; it does not authorize another launch. A normal worker launch also consumes the permission.
- A pre-offer Soodles runtime successor is distinct from post-offer recovery: require the original known deferred receipt, no proposal/order/session/mailbox effect, unchanged task inputs and a held exact new context. Adoption itself offers no task and dispatches no model.
- Same-card source migration uses `host contexts inspect --id ID` and `register --expected-version VERSION`; retain the original request and old source archive. The registration migration archive records intent, not proof of execution or acceptance.
