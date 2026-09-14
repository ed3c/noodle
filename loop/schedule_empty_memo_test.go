package loop

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/poteto/noodle/adapter"
	"github.com/poteto/noodle/event"
	"github.com/poteto/noodle/internal/ingest"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/mise"
)

func TestScheduleDecisionDigestIgnoresScheduleLifecycleAndOrdering(t *testing.T) {
	logger, _ := newTestLogger()
	tc := newTestLoop(t, logger)
	brief := mise.Brief{
		GeneratedAt: time.Unix(10, 0),
		Backlog: []adapter.BacklogItem{
			{ID: "2", Title: "second", Status: adapter.BacklogStatusOpen},
			{ID: "1", Title: "first", Status: adapter.BacklogStatusOpen},
		},
		ActiveSummary: mise.ActiveSummary{Total: 1, ByTaskKey: map[string]int{"schedule": 1}},
		Resources:     mise.ResourceSnapshot{MaxConcurrency: 3, Active: 1, Available: 2},
		Routing: mise.RoutingSnapshot{
			Defaults:          mise.RoutingPolicy{Provider: "codex", Model: "gpt-5"},
			AvailableRuntimes: []string{"sprites", "process"},
		},
		TaskTypes: []mise.TaskTypeSummary{
			{Key: "review", Schedule: "after execute"},
			{Key: "execute", Schedule: "ready work"},
		},
		RecentHistory: []mise.HistoryItem{{SessionID: "schedule-1", TaskKey: scheduleOrderID, Status: "completed"}},
	}
	orders := OrdersFile{Orders: []Order{
		testOrder("work-2", "execute", "execute", "codex", "gpt-5"),
		testOrder(scheduleOrderID, scheduleOrderID, scheduleOrderID, "codex", "gpt-5"),
		testOrder("work-1", "review", "review", "codex", "gpt-5"),
	}}

	before, err := tc.loop.scheduleDecisionDigest(brief, orders)
	if err != nil {
		t.Fatalf("initial digest: %v", err)
	}
	if err := tc.loop.events.Emit(event.LoopEventScheduleCompleted, ScheduleCompletedPayload{SessionID: "schedule-1"}); err != nil {
		t.Fatalf("emit schedule completion: %v", err)
	}

	brief.GeneratedAt = time.Unix(20, 0)
	brief.ActiveSummary = mise.ActiveSummary{Total: 0}
	brief.Resources.Active = 0
	brief.Resources.Available = 3
	brief.RecentHistory = nil
	brief.Backlog[0], brief.Backlog[1] = brief.Backlog[1], brief.Backlog[0]
	brief.Routing.AvailableRuntimes[0], brief.Routing.AvailableRuntimes[1] = brief.Routing.AvailableRuntimes[1], brief.Routing.AvailableRuntimes[0]
	brief.TaskTypes[0], brief.TaskTypes[1] = brief.TaskTypes[1], brief.TaskTypes[0]
	orders.Orders[0], orders.Orders[2] = orders.Orders[2], orders.Orders[0]
	orders.Orders[1].Stages[0].Status = StageStatusActive

	after, err := tc.loop.scheduleDecisionDigest(brief, orders)
	if err != nil {
		t.Fatalf("digest after schedule-only changes: %v", err)
	}
	if after != before {
		t.Fatalf("schedule-only lifecycle or input ordering changed digest:\n before %s\n after  %s", before, after)
	}
}

func TestScheduleDecisionDigestChangesForEveryAdmittedInput(t *testing.T) {
	logger, _ := newTestLogger()
	tc := newTestLoop(t, logger)
	baseBrief := mise.Brief{
		Backlog:   []adapter.BacklogItem{{ID: "1", Title: "first", Status: adapter.BacklogStatusOpen}},
		Resources: mise.ResourceSnapshot{MaxConcurrency: 2},
		Routing:   mise.RoutingSnapshot{Defaults: mise.RoutingPolicy{Provider: "codex", Model: "gpt-5"}},
		TaskTypes: []mise.TaskTypeSummary{{Key: "execute", Schedule: "ready work"}},
		Warnings:  []string{"one warning"},
	}
	baseOrders := OrdersFile{Orders: []Order{testOrder("work-1", "execute", "execute", "codex", "gpt-5")}}
	base, err := tc.loop.scheduleDecisionDigest(baseBrief, baseOrders)
	if err != nil {
		t.Fatalf("base digest: %v", err)
	}

	assertChanged := func(name string, brief mise.Brief, orders OrdersFile) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			got, err := tc.loop.scheduleDecisionDigest(brief, orders)
			if err != nil {
				t.Fatalf("digest: %v", err)
			}
			if got == base {
				t.Fatalf("%s did not change decision digest", name)
			}
		})
	}

	changedBacklog := baseBrief
	changedBacklog.Backlog = []adapter.BacklogItem{{ID: "1", Title: "changed", Status: adapter.BacklogStatusOpen}}
	assertChanged("provider backlog", changedBacklog, baseOrders)

	changedOrder := baseOrders
	changedOrder.Orders = []Order{testOrder("work-2", "execute", "execute", "codex", "gpt-5")}
	assertChanged("active non-schedule order", baseBrief, changedOrder)

	changedRouting := baseBrief
	changedRouting.Routing.Defaults.Model = "gpt-6"
	assertChanged("routing", changedRouting, baseOrders)

	changedTaskTypes := baseBrief
	changedTaskTypes.TaskTypes = []mise.TaskTypeSummary{{Key: "execute", Schedule: "new scheduling rule"}}
	assertChanged("task types", changedTaskTypes, baseOrders)

	if err := tc.loop.events.Emit("ci.failed", map[string]string{"branch": "main", "reason": "lint error"}); err != nil {
		t.Fatalf("emit external event: %v", err)
	}
	assertChanged("external event", baseBrief, baseOrders)
}

func TestEmptyScheduleMemoSuppressesUnchangedCyclesAndAdmitsBacklogChange(t *testing.T) {
	logger, handler := newTestLogger()
	brief := mise.Brief{
		Backlog:   []adapter.BacklogItem{{ID: "1", Title: "first", Status: adapter.BacklogStatusOpen}},
		Resources: mise.ResourceSnapshot{MaxConcurrency: 1},
	}
	tc := newTestLoop(t, logger, func(opts *testLoopOpts) { opts.brief = &brief })
	if err := writeOrdersAtomic(tc.ordersPath, bootstrapScheduleOrder(tc.loop.config)); err != nil {
		t.Fatalf("seed schedule order: %v", err)
	}
	digest, err := tc.loop.scheduleDecisionDigest(brief, bootstrapScheduleOrder(tc.loop.config))
	if err != nil {
		t.Fatalf("capture dispatch digest: %v", err)
	}
	tc.loop.scheduleDispatchDigest = digest
	if err := os.WriteFile(tc.loop.deps.OrdersNextFile, []byte(`{"orders":[]}`), 0o644); err != nil {
		t.Fatalf("write empty proposal: %v", err)
	}

	if _, _, err := tc.loop.prepareOrdersForCycle(brief, nil, true); err != nil {
		t.Fatalf("promote empty decision: %v", err)
	}
	if _, exists, err := tc.loop.readScheduleEmptyMemo(); err != nil || !exists {
		t.Fatalf("empty memo exists=%v err=%v", exists, err)
	}
	if err := tc.loop.writeOrdersState(OrdersFile{}); err != nil {
		t.Fatalf("simulate schedule completion: %v", err)
	}
	if err := tc.loop.events.Emit(event.LoopEventScheduleCompleted, ScheduleCompletedPayload{SessionID: "schedule-1"}); err != nil {
		t.Fatalf("emit schedule completion: %v", err)
	}

	for cycle := 0; cycle < 3; cycle++ {
		_, shouldContinue, err := tc.loop.prepareOrdersForCycle(brief, nil, true)
		if err != nil {
			t.Fatalf("unchanged cycle %d: %v", cycle, err)
		}
		if shouldContinue {
			t.Fatalf("unchanged cycle %d continued, want idle", cycle)
		}
	}
	if got := handler.countMessage("orders empty, bootstrapping schedule"); got != 0 {
		t.Fatalf("unchanged decision spawned %d schedules, want 0", got)
	}

	changed := brief
	changed.Backlog = []adapter.BacklogItem{
		{ID: "1", Title: "first", Status: adapter.BacklogStatusOpen},
		{ID: "2", Title: "new work", Status: adapter.BacklogStatusOpen},
	}
	_, shouldContinue, err := tc.loop.prepareOrdersForCycle(changed, nil, true)
	if err != nil {
		t.Fatalf("changed backlog cycle: %v", err)
	}
	if !shouldContinue {
		t.Fatal("changed backlog stayed idle, want one schedule order")
	}
	orders, err := readOrders(tc.ordersPath)
	if err != nil {
		t.Fatalf("read changed orders: %v", err)
	}
	if len(orders.Orders) != 1 || !isScheduleOrder(orders.Orders[0]) {
		t.Fatalf("changed backlog orders = %#v, want one schedule order", orders.Orders)
	}
	if got := handler.countMessage("orders empty, bootstrapping schedule"); got != 1 {
		t.Fatalf("changed decision spawned %d schedules, want exactly 1", got)
	}
}

func TestEmptyScheduleMemoUsesSchedulerDispatchState(t *testing.T) {
	logger, handler := newTestLogger()
	dispatchedBrief := mise.Brief{
		Backlog:   []adapter.BacklogItem{{ID: "1", Title: "first", Status: adapter.BacklogStatusOpen}},
		Resources: mise.ResourceSnapshot{MaxConcurrency: 1},
	}
	tc := newTestLoop(t, logger, func(opts *testLoopOpts) { opts.brief = &dispatchedBrief })
	if err := writeOrdersAtomic(tc.ordersPath, bootstrapScheduleOrder(tc.loop.config)); err != nil {
		t.Fatalf("seed schedule order: %v", err)
	}

	if err := tc.loop.Cycle(context.Background()); err != nil {
		t.Fatalf("dispatch scheduler: %v", err)
	}
	if got := len(tc.runtime.calls); got != 1 {
		t.Fatalf("initial scheduler dispatches = %d, want 1", got)
	}

	changedBrief := dispatchedBrief
	changedBrief.Backlog = append(changedBrief.Backlog,
		adapter.BacklogItem{ID: "2", Title: "new ready work", Status: adapter.BacklogStatusOpen},
	)
	tc.mise.brief = changedBrief
	if err := os.WriteFile(tc.loop.deps.OrdersNextFile, []byte(`{"orders":[]}`), 0o644); err != nil {
		t.Fatalf("write empty proposal: %v", err)
	}
	defer tc.runtime.sessions[0].ForceKill()
	delete(tc.loop.cooks.activeCooksByOrder, scheduleOrderID)
	if err := tc.loop.writeOrdersState(OrdersFile{}); err != nil {
		t.Fatalf("simulate schedule completion: %v", err)
	}

	orders, shouldContinue, err := tc.loop.prepareOrdersForCycle(changedBrief, nil, true)
	if err != nil {
		t.Fatalf("promote stale empty decision: %v", err)
	}
	if !shouldContinue || len(orders.Orders) != 1 || !isScheduleOrder(orders.Orders[0]) {
		t.Fatalf("changed backlog orders = %#v continue=%v, want one schedule order", orders.Orders, shouldContinue)
	}
	if got := handler.countMessage("orders empty, bootstrapping schedule"); got != 1 {
		t.Fatalf("changed backlog spawned %d replacement schedules, want exactly 1", got)
	}
	candidates, err := tc.loop.planCycleSpawns(orders, changedBrief, tc.loop.config.Concurrency.MaxConcurrency)
	if err != nil {
		t.Fatalf("plan replacement scheduler: %v", err)
	}
	if err := tc.loop.spawnPlannedCandidates(context.Background(), candidates, orders, changedBrief); err != nil {
		t.Fatalf("spawn replacement scheduler: %v", err)
	}
	if got := len(tc.runtime.calls); got != 2 {
		t.Fatalf("total scheduler dispatches = %d, want initial plus exactly one replacement", got)
	}
}

func TestStaleEmptyScheduleOutputDoesNotMemoizeCurrentDecision(t *testing.T) {
	logger, _ := newTestLogger()
	brief := mise.Brief{
		Backlog:   []adapter.BacklogItem{{ID: "1", Title: "new ready work", Status: adapter.BacklogStatusOpen}},
		Resources: mise.ResourceSnapshot{MaxConcurrency: 1},
	}
	tc := newTestLoop(t, logger, func(opts *testLoopOpts) { opts.brief = &brief })
	if err := writeOrdersAtomic(tc.ordersPath, OrdersFile{}); err != nil {
		t.Fatalf("seed empty orders: %v", err)
	}
	if err := os.WriteFile(tc.loop.deps.OrdersNextFile, []byte(`{"orders":[]}`), 0o644); err != nil {
		t.Fatalf("write stale empty output: %v", err)
	}

	orders, shouldContinue, err := tc.loop.prepareOrdersForCycle(brief, nil, true)
	if err != nil {
		t.Fatalf("promote stale empty output: %v", err)
	}
	if !shouldContinue || len(orders.Orders) != 1 || !isScheduleOrder(orders.Orders[0]) {
		t.Fatalf("current decision orders = %#v continue=%v, want one schedule order", orders.Orders, shouldContinue)
	}
	if _, exists, err := tc.loop.readScheduleEmptyMemo(); err != nil || exists {
		t.Fatalf("stale output memo exists=%v err=%v, want absent", exists, err)
	}
	if _, err := os.Stat(tc.loop.deps.OrdersNextFile); !os.IsNotExist(err) {
		t.Fatalf("stale output was not consumed: %v", err)
	}
	if len(brief.Backlog) != 1 || brief.Backlog[0].ID != "1" {
		t.Fatalf("current backlog changed during promotion: %#v", brief.Backlog)
	}
	if got := len(tc.runtime.calls); got != 0 {
		t.Fatalf("promotion produced %d provider effects, want 0", got)
	}
	for _, order := range orders.Orders {
		if !isScheduleOrder(order) {
			t.Fatalf("promotion produced duplicate non-schedule order: %#v", order)
		}
	}
}

func TestRestartDefersScheduleUntilMemoCanBeComparedWithProviderState(t *testing.T) {
	logger, _ := newTestLogger()
	brief := mise.Brief{
		Backlog:   []adapter.BacklogItem{{ID: "1", Title: "first", Status: adapter.BacklogStatusOpen}},
		Resources: mise.ResourceSnapshot{MaxConcurrency: 1},
	}
	tc := newTestLoop(t, logger, func(opts *testLoopOpts) { opts.brief = &brief })
	if err := tc.loop.writeScheduleEmptyMemo(brief, OrdersFile{}); err != nil {
		t.Fatalf("write memo: %v", err)
	}
	if err := writeOrdersAtomic(tc.ordersPath, OrdersFile{}); err != nil {
		t.Fatalf("write empty orders: %v", err)
	}

	restarted := New(tc.projectDir, "noodle", tc.loop.config, Dependencies{
		Runtimes:   tc.loop.deps.Runtimes,
		Worktree:   tc.worktree,
		Adapter:    tc.adapter,
		Mise:       tc.mise,
		Monitor:    fakeMonitor{},
		Registry:   tc.loop.registry,
		Now:        time.Now,
		OrdersFile: tc.ordersPath,
	})
	if err := restarted.loadOrdersState(); err != nil {
		t.Fatalf("load orders: %v", err)
	}
	if err := restarted.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile restart: %v", err)
	}
	orders, err := readOrders(tc.ordersPath)
	if err != nil {
		t.Fatalf("read orders after reconcile: %v", err)
	}
	if hasScheduleOrder(orders) {
		t.Fatalf("restart injected schedule before provider comparison: %#v", orders.Orders)
	}

	_, shouldContinue, err := restarted.prepareOrdersForCycle(brief, nil, true)
	if err != nil {
		t.Fatalf("prepare after restart: %v", err)
	}
	if shouldContinue {
		t.Fatal("unchanged restart decision continued, want idle")
	}
	if _, err := os.Stat(filepath.Join(tc.runtimeDir, "schedule-empty-decision.json")); err != nil {
		t.Fatalf("memo not preserved across restart: %v", err)
	}
}

func TestScheduleEmptyMemoRestartDispatchesPendingScheduleAfterPriorCanonicalAttemptCompleted(t *testing.T) {
	logger, _ := newTestLogger()
	brief := mise.Brief{
		Backlog:   []adapter.BacklogItem{{ID: "1", Title: "new ready work", Status: adapter.BacklogStatusOpen}},
		Resources: mise.ResourceSnapshot{MaxConcurrency: 1},
	}
	tc := newTestLoop(t, logger, func(opts *testLoopOpts) { opts.brief = &brief })
	prior := bootstrapScheduleOrder(tc.loop.config).Orders[0]
	if err := tc.loop.writeOrdersState(OrdersFile{Orders: []Order{prior}}); err != nil {
		t.Fatalf("seed prior schedule: %v", err)
	}
	tc.loop.emitEvent(ingest.EventDispatchRequested, map[string]any{
		"order_id": scheduleOrderID, "stage_index": 0,
		"attempt_id": "schedule-0-attempt-0",
	})
	tc.loop.emitEvent(ingest.EventDispatchCompleted, map[string]any{
		"order_id": scheduleOrderID, "stage_index": 0,
		"attempt_id": "schedule-0-attempt-0", "session_id": "schedule-prior",
	})
	tc.loop.emitEvent(ingest.EventStageCompleted, map[string]any{
		"order_id": scheduleOrderID, "stage_index": 0,
		"attempt_id": "schedule-0-attempt-0", "session_id": "schedule-prior",
	})
	tc.loop.emitEvent(ingest.EventOrderCompleted, map[string]any{"order_id": scheduleOrderID})

	// Reproduce the replay shape: the public schedule ID is pending in the
	// legacy projection while its prior canonical incarnation is terminal.
	replacement := bootstrapScheduleOrder(tc.loop.config)
	if err := writeOrdersAtomic(tc.ordersPath, replacement); err != nil {
		t.Fatalf("write replacement schedule: %v", err)
	}

	restarted := New(tc.projectDir, "noodle", tc.loop.config, Dependencies{
		Runtimes:   tc.loop.deps.Runtimes,
		Worktree:   tc.worktree,
		Adapter:    tc.adapter,
		Mise:       tc.mise,
		Monitor:    fakeMonitor{},
		Registry:   tc.loop.registry,
		Logger:     logger,
		Now:        time.Now,
		OrdersFile: tc.ordersPath,
	})
	if err := restarted.loadOrdersState(); err != nil {
		t.Fatalf("load replacement orders: %v", err)
	}
	if err := restarted.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile restart: %v", err)
	}
	if err := restarted.Cycle(context.Background()); err != nil {
		t.Fatalf("restart cycle: %v", err)
	}
	t.Cleanup(restarted.Shutdown)

	if got := len(tc.runtime.calls); got != 1 {
		t.Fatalf("replacement scheduler dispatches = %d, want exactly 1", got)
	}
	stage := restarted.canonical.Orders[scheduleOrderID].Stages[0]
	if len(stage.Attempts) != 2 || stage.Attempts[1].SessionID == "schedule-prior" {
		t.Fatalf("replacement canonical attempt was not distinct: %#v", stage.Attempts)
	}
	if stage.Attempts[1].AttemptID == "schedule-0-attempt-0" {
		t.Fatalf("replacement canonical attempt identity was reused: %#v", stage.Attempts)
	}
	dispatchEffectIDs := make([]string, 0, 2)
	for _, record := range restarted.effectLedger.All() {
		if record.Effect.Type == reducer.EffectDispatch {
			dispatchEffectIDs = append(dispatchEffectIDs, record.EffectID)
		}
	}
	if len(dispatchEffectIDs) != 2 || dispatchEffectIDs[0] == dispatchEffectIDs[1] {
		t.Fatalf("dispatch effect identities = %#v, want two distinct effects", dispatchEffectIDs)
	}

	if err := restarted.Cycle(context.Background()); err != nil {
		t.Fatalf("live scheduler control cycle: %v", err)
	}
	if got := len(tc.runtime.calls); got != 1 {
		t.Fatalf("live scheduler dispatches = %d, want exactly 1", got)
	}
}

func TestChefSteerBypassesMatchingEmptyDecisionMemo(t *testing.T) {
	logger, _ := newTestLogger()
	brief := mise.Brief{
		Backlog:   []adapter.BacklogItem{{ID: "1", Title: "first", Status: adapter.BacklogStatusOpen}},
		Resources: mise.ResourceSnapshot{MaxConcurrency: 1},
	}
	tc := newTestLoop(t, logger, func(opts *testLoopOpts) { opts.brief = &brief })
	if err := tc.loop.writeScheduleEmptyMemo(brief, OrdersFile{}); err != nil {
		t.Fatalf("write memo: %v", err)
	}
	if err := tc.loop.writeOrdersState(OrdersFile{}); err != nil {
		t.Fatalf("write empty orders: %v", err)
	}

	if err := tc.loop.steer(ScheduleTaskKey(), "prioritize the outage"); err != nil {
		t.Fatalf("chef steer: %v", err)
	}
	orders, err := tc.loop.currentOrders()
	if err != nil {
		t.Fatalf("read orders: %v", err)
	}
	if len(orders.Orders) != 1 || !isScheduleOrder(orders.Orders[0]) {
		t.Fatalf("chef steer orders = %#v, want one schedule order", orders.Orders)
	}
	if orders.Orders[0].Rationale != "Chef steer: prioritize the outage" {
		t.Fatalf("chef steer rationale = %q", orders.Orders[0].Rationale)
	}
	if _, exists, err := tc.loop.readScheduleEmptyMemo(); err != nil || exists {
		t.Fatalf("chef steer left empty memo exists=%v err=%v", exists, err)
	}
	candidates, err := tc.loop.planCycleSpawns(orders, brief, tc.loop.config.Concurrency.MaxConcurrency)
	if err != nil {
		t.Fatalf("plan steered scheduler: %v", err)
	}
	if err := tc.loop.spawnPlannedCandidates(context.Background(), candidates, orders, brief); err != nil {
		t.Fatalf("spawn steered scheduler: %v", err)
	}
	if got := len(tc.runtime.calls); got != 1 {
		t.Fatalf("steered scheduler dispatches = %d, want exactly 1", got)
	}
}
