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

func newTypedOutcomeTestLoop(t *testing.T) (*Loop, *fakeAdapterRunner, *cookHandle) {
	t.Helper()
	projectDir := t.TempDir()
	runtimeDir := filepath.Join(projectDir, ".noodle")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatalf("mkdir runtime: %v", err)
	}
	ordersPath := filepath.Join(runtimeDir, "orders.json")
	if err := writeOrdersAtomic(ordersPath, OrdersFile{Orders: []Order{{
		ID:     "order-1",
		Status: OrderStatusActive,
		Stages: []Stage{{
			TaskKey:  "execute",
			Skill:    "execute",
			Provider: "codex",
			Model:    "gpt-5.4",
			Status:   StageStatusActive,
		}},
	}}}); err != nil {
		t.Fatalf("write orders: %v", err)
	}

	cfg := config.DefaultConfig()
	cfg.Routing.Defaults.Provider = "codex"
	cfg.Agents.Codex.RequireTypedOutcome = true
	adapterRunner := &fakeAdapterRunner{}
	l := New(projectDir, "noodle", cfg, Dependencies{
		Runtimes:   map[string]loopruntime.Runtime{"process": newMockRuntime()},
		Worktree:   &fakeWorktree{hasUnmergedCommits: map[string]bool{"order-1-0-execute": false}},
		Adapter:    adapterRunner,
		Mise:       &fakeMise{},
		Monitor:    fakeMonitor{},
		Registry:   testLoopRegistry(),
		Now:        time.Now,
		OrdersFile: ordersPath,
	})
	cook := &cookHandle{
		cookIdentity: cookIdentity{
			orderID:    "order-1",
			stageIndex: 0,
			stage: Stage{
				TaskKey:  "execute",
				Skill:    "execute",
				Provider: "codex",
				Model:    "gpt-5.4",
			},
		},
		session:      &mockSession{id: "session-1", status: "completed", done: make(chan struct{})},
		worktreeName: "order-1-0-execute",
		worktreePath: filepath.Join(projectDir, ".worktrees", "order-1-0-execute"),
	}
	return l, adapterRunner, cook
}

func appendTypedOutcome(t *testing.T, l *Loop, cook *cookHandle, outcome event.StageOutcome, blocking bool, orderID string, stageIndex int) {
	t.Helper()
	writer, err := event.NewEventWriter(l.runtimeDir, cook.session.ID())
	if err != nil {
		t.Fatalf("new event writer: %v", err)
	}
	payload, err := json.Marshal(event.StageMessagePayload{
		Message:    "bounded result",
		Blocking:   &blocking,
		Outcome:    outcome,
		OrderID:    orderID,
		StageIndex: &stageIndex,
	})
	if err != nil {
		t.Fatalf("marshal outcome: %v", err)
	}
	if err := writer.Append(context.Background(), event.Event{Type: event.EventStageMessage, Payload: payload}); err != nil {
		t.Fatalf("append outcome: %v", err)
	}
}

func appendStageMessage(t *testing.T, l *Loop, cook *cookHandle, payload string) {
	t.Helper()
	writer, err := event.NewEventWriter(l.runtimeDir, cook.session.ID())
	if err != nil {
		t.Fatalf("new event writer: %v", err)
	}
	if err := writer.Append(context.Background(), event.Event{Type: event.EventStageMessage, Payload: json.RawMessage(payload)}); err != nil {
		t.Fatalf("append stage message: %v", err)
	}
}

func TestTypedOutcomeMissingParksInsteadOfCompleting(t *testing.T) {
	l, adapterRunner, cook := newTypedOutcomeTestLoop(t)

	if err := l.handleCompletion(context.Background(), cook, StageResultCompleted, "completed"); err != nil {
		t.Fatalf("handle completion: %v", err)
	}

	reviews, err := ReadPendingReview(l.runtimeDir)
	if err != nil {
		t.Fatalf("read pending review: %v", err)
	}
	if len(reviews) != 1 || !strings.Contains(reviews[0].Reason, "missing stage_message") {
		t.Fatalf("pending reviews = %#v, want missing-outcome refusal", reviews)
	}
	if len(adapterRunner.doneCalls) != 0 {
		t.Fatalf("backlog done calls = %#v, want none", adapterRunner.doneCalls)
	}
}

func TestTypedOutcomeRoutesExplicitStatuses(t *testing.T) {
	tests := []struct {
		name       string
		outcome    event.StageOutcome
		blocking   bool
		wantReview bool
		wantFailed bool
	}{
		{name: "completed", outcome: event.StageOutcomeCompleted, blocking: false},
		{name: "blocked", outcome: event.StageOutcomeBlocked, blocking: true, wantReview: true},
		{name: "failed", outcome: event.StageOutcomeFailed, blocking: true, wantFailed: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			l, _, cook := newTypedOutcomeTestLoop(t)
			appendTypedOutcome(t, l, cook, test.outcome, test.blocking, cook.orderID, cook.stageIndex)

			if err := l.handleCompletion(context.Background(), cook, StageResultCompleted, "completed"); err != nil {
				t.Fatalf("handle completion: %v", err)
			}
			reviews, err := ReadPendingReview(l.runtimeDir)
			if err != nil {
				t.Fatalf("read pending review: %v", err)
			}
			if got := len(reviews) == 1; got != test.wantReview {
				t.Fatalf("pending review = %v, want %v (%#v)", got, test.wantReview, reviews)
			}
			orders, err := readOrders(l.deps.OrdersFile)
			if err != nil {
				t.Fatalf("read orders: %v", err)
			}
			if test.wantFailed {
				if len(orders.Orders) != 1 || orders.Orders[0].Status != OrderStatusFailed {
					t.Fatalf("orders = %#v, want failed order", orders.Orders)
				}
			} else if test.outcome == event.StageOutcomeCompleted {
				if len(orders.Orders) != 1 || orders.Orders[0].Status == OrderStatusFailed {
					t.Fatalf("orders = %#v, completed outcome must not fail", orders.Orders)
				}
				loopEvents, err := os.ReadFile(filepath.Join(l.runtimeDir, "loop-events.ndjson"))
				if err != nil {
					t.Fatalf("read loop events: %v", err)
				}
				if !strings.Contains(string(loopEvents), `"type":"stage.completed"`) {
					t.Fatalf("loop events = %s, want stage.completed", loopEvents)
				}
			}
		})
	}
}

func TestTypedOutcomeIdentityMismatchParks(t *testing.T) {
	l, adapterRunner, cook := newTypedOutcomeTestLoop(t)
	appendTypedOutcome(t, l, cook, event.StageOutcomeCompleted, false, "foreign-order", cook.stageIndex)

	if err := l.handleCompletion(context.Background(), cook, StageResultCompleted, "completed"); err != nil {
		t.Fatalf("handle completion: %v", err)
	}
	reviews, err := ReadPendingReview(l.runtimeDir)
	if err != nil {
		t.Fatalf("read pending review: %v", err)
	}
	if len(reviews) != 1 || !strings.Contains(reviews[0].Reason, "does not match") {
		t.Fatalf("pending reviews = %#v, want identity refusal", reviews)
	}
	if len(adapterRunner.doneCalls) != 0 {
		t.Fatalf("backlog done calls = %#v, want none", adapterRunner.doneCalls)
	}
}

func TestTypedOutcomeRefusesDuplicateReorderedAndInvalidShapes(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(*testing.T, *Loop, *cookHandle)
		want    string
	}{
		{
			name: "duplicate",
			arrange: func(t *testing.T, l *Loop, cook *cookHandle) {
				appendTypedOutcome(t, l, cook, event.StageOutcomeCompleted, false, cook.orderID, cook.stageIndex)
				appendTypedOutcome(t, l, cook, event.StageOutcomeCompleted, false, cook.orderID, cook.stageIndex)
			},
			want: "duplicate typed stage outcomes",
		},
		{
			name: "reordered",
			arrange: func(t *testing.T, l *Loop, cook *cookHandle) {
				appendTypedOutcome(t, l, cook, event.StageOutcomeCompleted, false, cook.orderID, cook.stageIndex)
				appendStageMessage(t, l, cook, `{"message":"late legacy message","blocking":false}`)
			},
			want: "not the final stage_message",
		},
		{
			name: "unknown status",
			arrange: func(t *testing.T, l *Loop, cook *cookHandle) {
				appendTypedOutcome(t, l, cook, event.StageOutcome("partial"), true, cook.orderID, cook.stageIndex)
			},
			want: "unknown typed stage outcome",
		},
		{
			name: "stage mismatch",
			arrange: func(t *testing.T, l *Loop, cook *cookHandle) {
				appendTypedOutcome(t, l, cook, event.StageOutcomeCompleted, false, cook.orderID, cook.stageIndex+1)
			},
			want: "stage does not match",
		},
		{
			name: "session mismatch",
			arrange: func(t *testing.T, l *Loop, cook *cookHandle) {
				blocking := false
				stageIndex := cook.stageIndex
				payload, err := json.Marshal(event.StageMessagePayload{
					Message:    "bounded result",
					Blocking:   &blocking,
					Outcome:    event.StageOutcomeCompleted,
					OrderID:    cook.orderID,
					StageIndex: &stageIndex,
				})
				if err != nil {
					t.Fatalf("marshal payload: %v", err)
				}
				recorded, err := json.Marshal(event.Event{Type: event.EventStageMessage, Payload: payload, SessionID: "foreign-session"})
				if err != nil {
					t.Fatalf("marshal event: %v", err)
				}
				sessionDir := filepath.Join(l.runtimeDir, "sessions", cook.session.ID())
				if err := os.MkdirAll(sessionDir, 0o755); err != nil {
					t.Fatalf("mkdir session: %v", err)
				}
				if err := os.WriteFile(filepath.Join(sessionDir, "events.ndjson"), append(recorded, '\n'), 0o644); err != nil {
					t.Fatalf("write forged event: %v", err)
				}
			},
			want: "session \"foreign-session\" does not match",
		},
		{
			name: "malformed",
			arrange: func(t *testing.T, l *Loop, cook *cookHandle) {
				sessionDir := filepath.Join(l.runtimeDir, "sessions", cook.session.ID())
				if err := os.MkdirAll(sessionDir, 0o755); err != nil {
					t.Fatalf("mkdir session: %v", err)
				}
				if err := os.WriteFile(filepath.Join(sessionDir, "events.ndjson"), []byte(`{"type":"stage_message","payload":`), 0o644); err != nil {
					t.Fatalf("write truncated event log: %v", err)
				}
			},
			want: "read stage messages",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			l, _, cook := newTypedOutcomeTestLoop(t)
			test.arrange(t, l, cook)
			_, err := l.readRequiredStageOutcome(cook)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestTypedOutcomeRequirementIsOptIn(t *testing.T) {
	l, _, cook := newTypedOutcomeTestLoop(t)
	l.config.Agents.Codex.RequireTypedOutcome = false
	if l.requiresTypedOutcome(cook) {
		t.Fatal("typed outcome requirement should be disabled by default")
	}
}
