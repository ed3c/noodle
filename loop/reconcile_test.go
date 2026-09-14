package loop

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/event"
	loopruntime "github.com/poteto/noodle/runtime"
)

func TestReconcileConsumesLateTypedFailedBeforeStaleReset(t *testing.T) {
	projectDir := t.TempDir()
	runtimeDir := filepath.Join(projectDir, ".noodle")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatalf("mkdir runtime: %v", err)
	}
	ordersPath := filepath.Join(runtimeDir, "orders.json")
	if err := writeOrdersAtomic(ordersPath, OrdersFile{Orders: []Order{
		{ID: "schedule", Status: OrderStatusActive, Stages: []Stage{{TaskKey: "schedule", Skill: "schedule", Status: StageStatusPending}}},
		{ID: "late-failed", Title: "late failure", Status: OrderStatusActive, Stages: []Stage{{TaskKey: "execute", Skill: "execute", Provider: "codex", Status: StageStatusActive}}},
	}}); err != nil {
		t.Fatalf("write orders: %v", err)
	}
	review := pendingReviewFile{Items: []PendingReviewItem{{
		OrderID: "late-failed", StageIndex: 0, TaskKey: "execute", Provider: "codex",
		SessionID: "session-late", WorktreeName: "late-failed-0-execute", Reason: "unrelated prose",
	}}}
	reviewData, err := json.Marshal(review)
	if err != nil {
		t.Fatalf("marshal pending review: %v", err)
	}
	if err := os.WriteFile(pendingReviewFilePath(runtimeDir), reviewData, 0o644); err != nil {
		t.Fatalf("write pending review: %v", err)
	}
	writer, err := event.NewEventWriter(runtimeDir, "session-late")
	if err != nil {
		t.Fatalf("new event writer: %v", err)
	}
	stageIndex := 0
	payload, err := json.Marshal(event.StageMessagePayload{Message: "tests failed", Blocking: boolPointer(true), Outcome: event.StageOutcomeFailed, OrderID: "late-failed", StageIndex: &stageIndex})
	if err != nil {
		t.Fatalf("marshal typed outcome: %v", err)
	}
	if err := writer.Append(context.Background(), event.Event{Type: event.EventStageMessage, Payload: payload}); err != nil {
		t.Fatalf("append typed outcome: %v", err)
	}

	l := New(projectDir, "noodle", config.DefaultConfig(), Dependencies{
		Runtimes: map[string]loopruntime.Runtime{"process": newMockRuntime()}, Worktree: &fakeWorktree{},
		Adapter: &fakeAdapterRunner{}, Mise: &fakeMise{}, Monitor: fakeMonitor{}, Registry: testLoopRegistry(), Now: time.Now, OrdersFile: ordersPath,
	})
	if err := l.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if len(l.reconciledFailures) != 1 || l.reconciledFailures[0].OrderID != "late-failed" || !strings.Contains(l.reconciledFailures[0].Reason, "tests failed") {
		t.Fatalf("reconciled failures = %#v, want typed late failure", l.reconciledFailures)
	}
	reviews, err := ReadPendingReview(runtimeDir)
	if err != nil {
		t.Fatalf("read pending review: %v", err)
	}
	if len(reviews) != 0 {
		t.Fatalf("pending reviews = %#v, want empty", reviews)
	}
	events := findEvents(readNDJSON(t, filepath.Join(runtimeDir, "loop-events.ndjson")), LoopEventStageFailed)
	if len(events) != 1 {
		t.Fatalf("stage.failed events = %d, want 1", len(events))
	}
	var failed StageFailedPayload
	if err := json.Unmarshal(events[0].Payload, &failed); err != nil {
		t.Fatalf("parse stage.failed: %v", err)
	}
	if failed.SessionID == nil || *failed.SessionID != "session-late" || !strings.Contains(failed.Reason, "tests failed") {
		t.Fatalf("stage.failed = %#v, want typed message and exact session", failed)
	}
	if err := l.reconcile(context.Background()); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if got := len(findEvents(readNDJSON(t, filepath.Join(runtimeDir, "loop-events.ndjson")), LoopEventStageFailed)); got != 1 {
		t.Fatalf("stage.failed events after second reconcile = %d, want 1", got)
	}
}

func boolPointer(value bool) *bool { return &value }

func TestReconcileLateTypedFailedRejectsIneligibleEvidenceWithoutMutation(t *testing.T) {
	valid := func(outcome event.StageOutcome, orderID string, stageIndex int) json.RawMessage {
		payload, err := json.Marshal(event.StageMessagePayload{
			Message: "terminal evidence", Blocking: boolPointer(outcome != event.StageOutcomeCompleted),
			Outcome: outcome, OrderID: orderID, StageIndex: &stageIndex,
		})
		if err != nil {
			t.Fatalf("marshal typed outcome: %v", err)
		}
		return payload
	}
	tests := []struct {
		name          string
		messages      []json.RawMessage
		reviewSession string
		attemptDone   bool
		adopted       bool
	}{
		{name: "no stage message", attemptDone: true},
		{name: "malformed", messages: []json.RawMessage{json.RawMessage(`"not an object"`)}, attemptDone: true},
		{name: "duplicate typed", messages: []json.RawMessage{valid(event.StageOutcomeFailed, "late-failed", 0), valid(event.StageOutcomeFailed, "late-failed", 0)}, attemptDone: true},
		{name: "non-final typed", messages: []json.RawMessage{valid(event.StageOutcomeFailed, "late-failed", 0), json.RawMessage(`{"message":"later prose"}`)}, attemptDone: true},
		{name: "mismatched session", messages: []json.RawMessage{valid(event.StageOutcomeFailed, "late-failed", 0)}, reviewSession: "different-session", attemptDone: true},
		{name: "mismatched order", messages: []json.RawMessage{valid(event.StageOutcomeFailed, "other", 0)}, attemptDone: true},
		{name: "mismatched stage", messages: []json.RawMessage{valid(event.StageOutcomeFailed, "late-failed", 1)}, attemptDone: true},
		{name: "blocked", messages: []json.RawMessage{valid(event.StageOutcomeBlocked, "late-failed", 0)}, attemptDone: true},
		{name: "completed", messages: []json.RawMessage{valid(event.StageOutcomeCompleted, "late-failed", 0)}, attemptDone: true},
		{name: "attempt not completed", messages: []json.RawMessage{valid(event.StageOutcomeFailed, "late-failed", 0)}},
		{name: "still live adopted session", messages: []json.RawMessage{valid(event.StageOutcomeFailed, "late-failed", 0)}, attemptDone: true, adopted: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			projectDir := t.TempDir()
			runtimeDir := filepath.Join(projectDir, ".noodle")
			if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
				t.Fatalf("mkdir runtime: %v", err)
			}
			ordersPath := filepath.Join(runtimeDir, "orders.json")
			if err := writeOrdersAtomic(ordersPath, OrdersFile{Orders: []Order{{
				ID: "late-failed", Status: OrderStatusActive,
				Stages: []Stage{{TaskKey: "execute", Provider: "codex", Status: StageStatusActive}},
			}}}); err != nil {
				t.Fatalf("write orders: %v", err)
			}
			sessionID := "session-late"
			if tt.reviewSession != "" {
				sessionID = tt.reviewSession
			}
			reviewData, err := json.Marshal(pendingReviewFile{Items: []PendingReviewItem{{
				OrderID: "late-failed", StageIndex: 0, TaskKey: "execute", Provider: "codex",
				SessionID: sessionID, WorktreeName: "late-failed-0-execute", Reason: "arbitrary prose",
			}}})
			if err != nil {
				t.Fatalf("marshal review: %v", err)
			}
			if err := os.WriteFile(pendingReviewFilePath(runtimeDir), reviewData, 0o644); err != nil {
				t.Fatalf("write review: %v", err)
			}
			for _, message := range tt.messages {
				writer, err := event.NewEventWriter(runtimeDir, "session-late")
				if err != nil {
					t.Fatalf("new event writer: %v", err)
				}
				if err := writer.Append(context.Background(), event.Event{Type: event.EventStageMessage, Payload: message}); err != nil {
					t.Fatalf("append stage message: %v", err)
				}
			}
			wt := &fakeWorktree{}
			adapter := &fakeAdapterRunner{}
			l := New(projectDir, "noodle", config.DefaultConfig(), Dependencies{
				Runtimes: map[string]loopruntime.Runtime{"process": newMockRuntime()}, Worktree: wt, Adapter: adapter,
				Mise: &fakeMise{}, Monitor: fakeMonitor{}, Registry: testLoopRegistry(), Now: time.Now, OrdersFile: ordersPath,
			})
			if err := l.loadOrBootstrapCanonical(); err != nil {
				t.Fatalf("load canonical: %v", err)
			}
			if err := l.loadPendingReview(); err != nil {
				t.Fatalf("load review: %v", err)
			}
			if !tt.attemptDone {
				order := l.canonical.Orders["late-failed"]
				order.Stages[0].Attempts[0].Status = "running"
				l.canonical.Orders["late-failed"] = order
			}
			if tt.adopted {
				l.cooks.adoptedTargets["late-failed"] = "session-late"
			}
			before, err := json.Marshal(l.canonical)
			if err != nil {
				t.Fatalf("marshal before state: %v", err)
			}
			if err := l.reconcileLateTypedFailed(context.Background()); err != nil {
				t.Fatalf("reconcile late outcome: %v", err)
			}
			after, err := json.Marshal(l.canonical)
			if err != nil {
				t.Fatalf("marshal after state: %v", err)
			}
			if string(after) != string(before) {
				t.Fatal("ineligible evidence mutated canonical state")
			}
			if len(wt.created)+len(wt.merged)+len(wt.remoteMerged)+len(wt.cleaned) != 0 || len(adapter.doneCalls) != 0 {
				t.Fatalf("ineligible evidence mutated external state: worktree=%#v adapter=%#v", wt, adapter.doneCalls)
			}
		})
	}
}

func TestReconcileFailedOrdersArchivesAndSummarizes(t *testing.T) {
	projectDir := t.TempDir()
	runtimeDir := filepath.Join(projectDir, ".noodle")
	if err := os.MkdirAll(filepath.Join(runtimeDir, "sessions"), 0o755); err != nil {
		t.Fatalf("mkdir runtime: %v", err)
	}
	ordersPath := filepath.Join(runtimeDir, "orders.json")

	orders := OrdersFile{
		Orders: []Order{
			{
				ID:     "schedule",
				Title:  "scheduling tasks",
				Status: OrderStatusActive,
				Stages: []Stage{{TaskKey: "schedule", Status: StageStatusPending}},
			},
			{
				ID:     "abc-123",
				Title:  "fix auth bug",
				Status: OrderStatusFailed,
				Stages: []Stage{
					{TaskKey: "execute", Status: StageStatusFailed},
					{TaskKey: "quality", Status: StageStatusPending},
				},
			},
			{
				ID:     "def-456",
				Title:  "add logging",
				Status: OrderStatusActive,
				Stages: []Stage{{TaskKey: "execute", Status: StageStatusActive}},
			},
		},
	}
	if err := writeOrdersAtomic(ordersPath, orders); err != nil {
		t.Fatalf("write orders: %v", err)
	}

	cfg := config.DefaultConfig()
	l := New(projectDir, "noodle", cfg, Dependencies{
		Runtimes:   map[string]loopruntime.Runtime{"process": newMockRuntime()},
		Worktree:   &fakeWorktree{},
		Adapter:    &fakeAdapterRunner{},
		Mise:       &fakeMise{},
		Monitor:    fakeMonitor{},
		Registry:   testLoopRegistry(),
		Now:        time.Now,
		OrdersFile: ordersPath,
	})

	if err := l.loadOrdersState(); err != nil {
		t.Fatalf("loadOrdersState: %v", err)
	}
	if err := l.reconcileFailedOrders(); err != nil {
		t.Fatalf("reconcileFailedOrders: %v", err)
	}

	// Failed order should be removed from orders.json.
	updated, err := readOrders(ordersPath)
	if err != nil {
		t.Fatalf("read orders: %v", err)
	}
	for _, o := range updated.Orders {
		if o.ID == "abc-123" {
			t.Fatal("failed order abc-123 should have been removed from orders.json")
		}
	}
	if len(updated.Orders) != 2 {
		t.Fatalf("expected 2 orders remaining, got %d", len(updated.Orders))
	}

	// Reconciled failures should be populated.
	if len(l.reconciledFailures) != 1 {
		t.Fatalf("expected 1 reconciled failure, got %d", len(l.reconciledFailures))
	}
	f := l.reconciledFailures[0]
	if f.OrderID != "abc-123" {
		t.Fatalf("failure OrderID = %q, want %q", f.OrderID, "abc-123")
	}
	if f.Title != "fix auth bug" {
		t.Fatalf("failure Title = %q, want %q", f.Title, "fix auth bug")
	}
	if f.TaskKey != "execute" {
		t.Fatalf("failure TaskKey = %q, want %q", f.TaskKey, "execute")
	}
}

func TestReconcileFailedOrdersSkipsScheduleOrder(t *testing.T) {
	projectDir := t.TempDir()
	runtimeDir := filepath.Join(projectDir, ".noodle")
	if err := os.MkdirAll(filepath.Join(runtimeDir, "sessions"), 0o755); err != nil {
		t.Fatalf("mkdir runtime: %v", err)
	}
	ordersPath := filepath.Join(runtimeDir, "orders.json")

	orders := OrdersFile{
		Orders: []Order{
			{
				ID:     "schedule",
				Title:  "scheduling tasks",
				Status: OrderStatusFailed,
				Stages: []Stage{{TaskKey: "schedule", Status: StageStatusFailed}},
			},
		},
	}
	if err := writeOrdersAtomic(ordersPath, orders); err != nil {
		t.Fatalf("write orders: %v", err)
	}

	cfg := config.DefaultConfig()
	l := New(projectDir, "noodle", cfg, Dependencies{
		Runtimes:   map[string]loopruntime.Runtime{"process": newMockRuntime()},
		Worktree:   &fakeWorktree{},
		Adapter:    &fakeAdapterRunner{},
		Mise:       &fakeMise{},
		Monitor:    fakeMonitor{},
		Registry:   testLoopRegistry(),
		Now:        time.Now,
		OrdersFile: ordersPath,
	})

	if err := l.loadOrdersState(); err != nil {
		t.Fatalf("loadOrdersState: %v", err)
	}
	if err := l.reconcileFailedOrders(); err != nil {
		t.Fatalf("reconcileFailedOrders: %v", err)
	}

	// Schedule order should not be archived.
	updated, err := readOrders(ordersPath)
	if err != nil {
		t.Fatalf("read orders: %v", err)
	}
	if len(updated.Orders) != 1 {
		t.Fatalf("expected 1 order remaining, got %d", len(updated.Orders))
	}
	if len(l.reconciledFailures) != 0 {
		t.Fatalf("expected 0 reconciled failures, got %d", len(l.reconciledFailures))
	}
}

func TestReconcileFailedOrdersNoopWhenNoFailures(t *testing.T) {
	projectDir := t.TempDir()
	runtimeDir := filepath.Join(projectDir, ".noodle")
	if err := os.MkdirAll(filepath.Join(runtimeDir, "sessions"), 0o755); err != nil {
		t.Fatalf("mkdir runtime: %v", err)
	}
	ordersPath := filepath.Join(runtimeDir, "orders.json")

	orders := OrdersFile{
		Orders: []Order{
			{
				ID:     "active-order",
				Title:  "doing stuff",
				Status: OrderStatusActive,
				Stages: []Stage{{TaskKey: "execute", Status: StageStatusPending}},
			},
		},
	}
	if err := writeOrdersAtomic(ordersPath, orders); err != nil {
		t.Fatalf("write orders: %v", err)
	}

	cfg := config.DefaultConfig()
	l := New(projectDir, "noodle", cfg, Dependencies{
		Runtimes:   map[string]loopruntime.Runtime{"process": newMockRuntime()},
		Worktree:   &fakeWorktree{},
		Adapter:    &fakeAdapterRunner{},
		Mise:       &fakeMise{},
		Monitor:    fakeMonitor{},
		Registry:   testLoopRegistry(),
		Now:        time.Now,
		OrdersFile: ordersPath,
	})

	if err := l.loadOrdersState(); err != nil {
		t.Fatalf("loadOrdersState: %v", err)
	}
	if err := l.reconcileFailedOrders(); err != nil {
		t.Fatalf("reconcileFailedOrders: %v", err)
	}

	if len(l.reconciledFailures) != 0 {
		t.Fatalf("expected 0 reconciled failures, got %d", len(l.reconciledFailures))
	}
}

func TestFullReconcileArchivesFailedOrders(t *testing.T) {
	projectDir := t.TempDir()
	runtimeDir := filepath.Join(projectDir, ".noodle")
	if err := os.MkdirAll(filepath.Join(runtimeDir, "sessions"), 0o755); err != nil {
		t.Fatalf("mkdir runtime: %v", err)
	}
	ordersPath := filepath.Join(runtimeDir, "orders.json")

	orders := OrdersFile{
		Orders: []Order{
			{
				ID:     "schedule",
				Title:  "scheduling tasks",
				Status: OrderStatusActive,
				Stages: []Stage{{TaskKey: "schedule", Skill: "schedule", Status: StageStatusPending}},
			},
			{
				ID:     "failed-order",
				Title:  "broken thing",
				Status: OrderStatusFailed,
				Stages: []Stage{{TaskKey: "execute", Status: StageStatusFailed}},
			},
		},
	}
	if err := writeOrdersAtomic(ordersPath, orders); err != nil {
		t.Fatalf("write orders: %v", err)
	}

	cfg := config.DefaultConfig()
	l := New(projectDir, "noodle", cfg, Dependencies{
		Runtimes:   map[string]loopruntime.Runtime{"process": newMockRuntime()},
		Worktree:   &fakeWorktree{},
		Adapter:    &fakeAdapterRunner{},
		Mise:       &fakeMise{},
		Monitor:    fakeMonitor{},
		Registry:   testLoopRegistry(),
		Now:        time.Now,
		OrdersFile: ordersPath,
	})

	if err := l.loadOrdersState(); err != nil {
		t.Fatalf("loadOrdersState: %v", err)
	}
	if err := l.reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// Failed order should be removed after full reconcile.
	updated, err := readOrders(ordersPath)
	if err != nil {
		t.Fatalf("read orders: %v", err)
	}
	for _, o := range updated.Orders {
		if o.ID == "failed-order" {
			t.Fatal("failed order should have been removed by reconcile()")
		}
	}

	// Summaries should be stored for the scheduler.
	if len(l.reconciledFailures) != 1 {
		t.Fatalf("expected 1 reconciled failure, got %d", len(l.reconciledFailures))
	}
	if l.reconciledFailures[0].OrderID != "failed-order" {
		t.Fatalf("failure OrderID = %q, want %q", l.reconciledFailures[0].OrderID, "failed-order")
	}
}
