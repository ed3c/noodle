# Task warning grants

- `host resources grant-warning --request-file PATH --order-id ID` allows one explicitly admitted task launch during stable memory warning; capability `task_warning_grant=1`.
- This is permission to launch, not additional RAM or a memory limit. The ordinary pressure snapshot stays truthful: warning and effective limit zero.
- Bind the original Host instance, context, request, binary, configuration path and digest. Read exact config bytes and require the selected order in `runtime.process.admitted_orders`.
- Grant requires three fresh warning samples with zero swapout, no active allocation, and no unresolved resident ownership. Critical, stale, failed, gap and reset samples still block.
- The grant expires after ten minutes, is consumed once, and never renews on duplicate requests. Detach revokes an unused grant; Host exit discards all grants. A normal launch also consumes a pending grant so a later retry cannot reuse it.
- Scheduler, bootstrap and resident admission have no task identity and cannot consume grants. Automatic replacement retains the normal pressure gate.
- A selected warning task may pass a blocked ordinary waiter, but only when the shared budget has zero active allocations. Other tasks remain deferred.
- Original process/group absence still controls allocation release. A launcher exit or model completion event is insufficient.
- Read `warning_grants` in `host resources` to distinguish available, consumed, expired and revoked permissions.
- Controls: `internal/admission/warning_grant_test.go`, `cmd_host_warning_grant_test.go`, the warning variant of the admitted-order loop cycle, and the real process-group retention test.
- Root Go tests require built UI assets. Run `pnpm build` first, or `pnpm check`; do not treat a missing `ui/dist/client` embed as a feature failure.
