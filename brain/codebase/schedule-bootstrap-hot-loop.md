# Schedule Bootstrap Hot Loop

- Root cause: `prepareOrdersForCycle` bootstrapped a schedule order whenever there were no non-schedule orders, even if a schedule order already existed.
- This caused `orders.json` rewrites every cycle → fsnotify triggers → rapid log spam.
- Fix: only bootstrap when a schedule order is absent (`!hasScheduleOrder(orders)`).
- Schedule must be exempt from sticky failure semantics — dispatch planning ignores failure blocks for the schedule order.
- Startup invariant: scheduler must be dispatchable immediately after `noodle start`.
  - Reconcile injects a schedule order if missing.
  - Reconcile resets stale schedule stage status `active -> pending` when no live session was recovered.

## Canonical projection feedback

The v0.1.19 loop has another feedback path. After `spawnPlannedCandidates`
succeeds, `Cycle` calls `flushState`. This atomically replaces the projected
`orders.json`, even when its content is unchanged. The runtime watcher treated
that output as an input and queued another cycle.

The runtime watcher now accepts only `orders-next.json` and `control.ndjson`.
`orders.json` remains a persisted output and a startup input. The running Loop
uses cached order state. The sandbox script seeds the file before startup.

`TestRuntimeWatcherIgnoresProjectionWrites` uses a real filesystem watcher.
It checks that unchanged and changed projections do not queue another cycle,
that changed content is persisted, and that both runtime inputs still queue a
cycle. The test failed before the trigger change and passed afterward. This
test does not measure CPU usage in a running Noodle process.

See also [[principles/fix-root-causes]], [[principles/make-operations-idempotent]]
