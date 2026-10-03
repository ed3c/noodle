package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/poteto/noodle/event"
	"github.com/poteto/noodle/internal/ingest"
	"github.com/poteto/noodle/internal/state"
	loopruntime "github.com/poteto/noodle/runtime"
)

func completedInterruptionReview(t *testing.T) (*Loop, *cookHandle) {
	t.Helper()
	project, wt := interruptionFixture(t)
	r := InspectInterruption(project, "/exact/noodle", "order-1", "example/project#7")
	r = PrepareInterruption(project, "/exact/noodle", "order-1", "example/project#7", r.Digest)
	if r.Status != "prepared" {
		t.Fatalf("prepare: %+v", r)
	}
	rt := newMockRuntime()
	rt.dispatchHook = func(req loopruntime.DispatchRequest) (loopruntime.SessionHandle, error) {
		return &mockSession{id: fmt.Sprintf("%s-attempt-%d", req.Name, req.RetryCount), status: "running", done: make(chan struct{})}, nil
	}
	l := interruptionLoop(t, project, rt)
	orders, _ := l.currentOrders()
	o := orders.Orders[0]
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := l.spawnCook(ctx, dispatchCandidate{OrderID: o.ID, StageIndex: 0, Stage: o.Stages[0]}, o, spawnOptions{attempt: 1}); err != nil {
		t.Fatal(err)
	}
	cook := l.cooks.activeCooksByOrder[o.ID]
	publicationRun(t, wt, "git", "add", "candidate.txt", "new.txt")
	publicationRun(t, wt, "git", "commit", "-m", "retained completed candidate")
	finishReviewFixture(t, l, cook)
	return l, cook
}

func finishReviewFixture(t *testing.T, l *Loop, cook *cookHandle) {
	t.Helper()
	appendTypedOutcome(t, l, cook, event.StageOutcomeCompleted, false, cook.orderID, cook.stageIndex)
	dir := filepath.Join(l.runtimeDir, "sessions", cook.session.ID())
	recoveryWriteJSON(t, filepath.Join(dir, "spawn.json"), map[string]any{"session_id": cook.session.ID(), "worktree_path": cook.worktreePath, "retry_count": cook.attempt})
	recoveryWriteJSON(t, filepath.Join(dir, "process.json"), map[string]any{"session_id": cook.session.ID(), "pid": 99999999})
	requestChangesWrite(t, filepath.Join(dir, "prompt.txt"), []byte(cook.stage.Prompt))
	delete(l.cooks.activeCooksByOrder, cook.orderID)
	if err := l.parkPendingReview(cook, "completed candidate"); err != nil {
		t.Fatal(err)
	}
}

func TestCompletedReviewRetiresInterruptionAndCompletesCorrection(t *testing.T) {
	l, cook := completedInterruptionReview(t)
	oldSession := recoverySessionBytes(t, l, "session-1")
	successorSession := recoverySessionBytes(t, l, cook.session.ID())
	before := l.canonical.Orders[cook.orderID].Stages[0]
	originalMarker := append([]byte(nil), before.Extra[interruptionKey]...)
	head := publicationOutput(t, cook.worktreePath, "git", "rev-parse", "HEAD")
	if ack := recoveryControl(t, l, ControlCommand{ID: "correction", Action: "request-changes", OrderID: cook.orderID, Prompt: "integrate selected base"}); ack.Status != "ok" {
		t.Fatalf("request changes: %+v", ack)
	}
	stage := l.canonical.Orders[cook.orderID].Stages[0]
	if _, active := stage.Extra[interruptionKey]; active {
		t.Fatal("active interruption authority retained")
	}
	var history []json.RawMessage
	if err := json.Unmarshal(stage.Extra[interruptionHistoryKey], &history); err != nil || len(history) != 1 || !recoveryJSONEqual(json.RawMessage(originalMarker), history[0]) {
		t.Fatalf("history: %s %v", stage.Extra[interruptionHistoryKey], err)
	}
	read := InspectInterruption(l.projectDir, "/exact/noodle", cook.orderID, "example/project#7")
	if read.Status != "dispatched" || read.Successor == nil || read.Successor.SessionID != cook.session.ID() {
		t.Fatalf("historical readback: %+v", read)
	}
	l = New(l.projectDir, "noodle", l.config, l.deps)
	if err := l.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	prompt := strings.Replace(cook.stage.Prompt, strings.Repeat("a", 64), strings.Repeat("b", 64), 1)
	if ack := recoveryControl(t, l, ControlCommand{ID: "edit", Action: "edit-item", OrderID: cook.orderID, Prompt: prompt}); ack.Status != "ok" {
		t.Fatalf("edit: %+v", ack)
	}
	if ack := recoveryControl(t, l, ControlCommand{ID: "requeue", Action: "requeue", OrderID: cook.orderID}); ack.Status != "ok" {
		t.Fatalf("requeue: %+v", ack)
	}
	l = New(l.projectDir, "noodle", l.config, l.deps)
	if err := l.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := l.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	next := l.cooks.activeCooksByOrder[cook.orderID]
	if next == nil || next.attempt != 2 || next.session.ID() == cook.session.ID() || next.worktreePath != cook.worktreePath {
		t.Fatalf("next: %+v", next)
	}
	if got := publicationOutput(t, cook.worktreePath, "git", "rev-parse", "HEAD"); got != head {
		t.Fatal("lost committed candidate")
	}
	finishReviewFixture(t, l, next)
	claim, err := InspectPublicationClaim(l.projectDir, cook.orderID, "example/project#7")
	if err != nil || claim.Head != head || claim.SessionID != next.session.ID() {
		t.Fatalf("claim: %+v %v", claim, err)
	}
	if len(l.canonical.Orders[cook.orderID].Stages[0].Attempts) != 3 {
		t.Fatal("lost prior attempts")
	}
	read = InspectInterruption(l.projectDir, "/exact/noodle", cook.orderID, "example/project#7")
	if read.Status != "dispatched" || read.Successor == nil || read.Successor.SessionID != cook.session.ID() {
		t.Fatalf("history after ordinary attempt: %+v", read)
	}
	if !reflect.DeepEqual(oldSession, recoverySessionBytes(t, l, "session-1")) || !reflect.DeepEqual(successorSession, recoverySessionBytes(t, l, cook.session.ID())) {
		t.Fatal("rewrote original sessions")
	}
	if err := l.controlRequestChanges(cook.orderID, "another ordinary correction"); err != nil {
		t.Fatal(err)
	}
	if err := l.controlRequeue(cook.orderID); err != nil {
		t.Fatal(err)
	}
}

func TestCompletedReviewRetirementRefusals(t *testing.T) {
	for _, condition := range []string{"unknown_dispatch", "foreign_successor", "dirty", "changed_original_session", "live_successor"} {
		t.Run(condition, func(t *testing.T) {
			l, cook := completedInterruptionReview(t)
			receipt := filepath.Join(interruptionPath(l.projectDir, cook.orderID, "example/project#7"), "dispatch-result.json")
			switch condition {
			case "unknown_dispatch":
				if err := os.Remove(receipt); err != nil {
					t.Fatal(err)
				}
			case "foreign_successor":
				var result interruptionDispatch
				if err := json.Unmarshal(recoveryRead(t, receipt), &result); err != nil {
					t.Fatal(err)
				}
				result.SessionID = "foreign"
				recoveryWriteJSON(t, receipt, result)
			case "dirty":
				requestChangesWrite(t, filepath.Join(cook.worktreePath, "dirty"), []byte("foreign"))
			case "changed_original_session":
				requestChangesWrite(t, filepath.Join(l.runtimeDir, "sessions", "session-1", "prompt.txt"), []byte("foreign"))
			case "live_successor":
				recoveryWriteJSON(t, filepath.Join(l.runtimeDir, "sessions", cook.session.ID(), "process.json"), map[string]any{"session_id": cook.session.ID(), "pid": os.Getpid()})
			}
			before := recoveryRefusalBytes(t, l)
			if err := l.controlRequestChanges(cook.orderID, "correction"); err == nil {
				t.Fatal("unsafe retirement accepted")
			}
			if !reflect.DeepEqual(before, recoveryRefusalBytes(t, l)) {
				t.Fatal("refusal mutated custody")
			}
		})
	}
}

func TestCompletedReviewCorrectionSurvivesCanonicalTransitionBeforeProjection(t *testing.T) {
	l, cook, p := newRequestChangesRecoveryOutcome(t, event.StageOutcomeCompleted)
	// Reproduce the canonical checkpoint boundary without relying on later log writes.
	requestChangesWrite(t, filepath.Join(l.runtimeDir, "loop-events.ndjson"), nil)
	l = New(l.projectDir, "noodle", l.config, l.deps)
	if err := l.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := l.controlEditItem(ControlCommand{OrderID: cook.orderID, Prompt: "resumed correction"}); err != nil {
		t.Fatal(err)
	}
	if err := l.controlRequeue(cook.orderID); err != nil {
		t.Fatal(err)
	}
	if got := publicationOutput(t, cook.worktreePath, "git", "rev-parse", "HEAD"); got != p.Binding.Head {
		t.Fatal("changed candidate")
	}
}

func TestCompletedReviewCorrectionReducerRetainsBoundReview(t *testing.T) {
	l, cook, _ := newRequestChangesRecoveryOutcome(t, event.StageOutcomeCompleted)
	order := l.canonical.Orders[cook.orderID]
	order.Status = state.OrderActive
	order.Stages[0].Status = state.StageReview
	order.Stages[0].Attempts[0].Status = state.AttemptCompleted
	l.canonical.Orders[cook.orderID] = order
	if err := l.emitEventChecked(ingest.EventStageReviewChangesRequested, map[string]any{"order_id": cook.orderID, "stage_index": cook.stageIndex, "reason": "changes requested: correction"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := l.canonical.PendingReviews[cook.orderID]; !ok || l.canonical.Orders[cook.orderID].Status != state.OrderFailed {
		t.Fatal("lost bound review")
	}
}

func TestCompletedReviewCorrectionControlAckInterruption(t *testing.T) {
	for _, boundary := range []string{"request-intent", "request-changes", "requeue"} {
		action := boundary
		if action == "request-intent" {
			action = "request-changes"
		}
		t.Run(boundary, func(t *testing.T) {
			l, cook := completedInterruptionReview(t)
			if action == "requeue" {
				if err := l.controlRequestChanges(cook.orderID, "correction"); err != nil {
					t.Fatal(err)
				}
			}
			cmd := ControlCommand{ID: "exact-interrupted-command", Action: action, OrderID: cook.orderID, Prompt: "correction"}
			control, _, _ := l.controlPaths()
			raw, _ := json.Marshal(cmd)
			requestChangesWrite(t, control, append(raw, '\n'))
			if boundary == "request-intent" {
				l.TestRequestChangesBarrier = func() { panic("fixture interruption after custody intent") }
			} else {
				l.TestControlAckBarrier = func() { panic("fixture interruption before ack") }
			}
			func() {
				defer func() {
					if recover() == nil {
						t.Fatal("missing boundary interruption")
					}
				}()
				_ = l.processControlCommands()
			}()
			priorAttempts := append([]state.AttemptNode(nil), l.canonical.Orders[cook.orderID].Stages[0].Attempts...)
			l = New(l.projectDir, "noodle", l.config, l.deps)
			if err := l.reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			if ack := recoveryControl(t, l, cmd); ack.Status != "ok" {
				t.Fatalf("readback retry: %+v", ack)
			}
			after := l.canonical.Orders[cook.orderID].Stages[0].Attempts
			if boundary == "request-intent" {
				priorAttempts[len(priorAttempts)-1].Status = state.AttemptFailed
				priorAttempts[len(priorAttempts)-1].Error = "changes requested: correction"
			}
			if !recoveryJSONEqual(priorAttempts, after) {
				t.Fatal("reentry changed unexpected attempt history")
			}
			if len(l.deps.Runtimes["process"].(*mockRuntime).calls) != 1 {
				t.Fatal("reentry dispatched writer")
			}
		})
	}
}

func TestCompletedReviewIntentRefusesDrift(t *testing.T) {
	for _, condition := range []string{"head", "session", "events", "reason"} {
		t.Run(condition, func(t *testing.T) {
			l, cook := completedInterruptionReview(t)
			l.TestRequestChangesBarrier = func() { panic("stop after intent") }
			func() {
				defer func() {
					if recover() == nil {
						t.Fatal("missing interruption")
					}
				}()
				_ = l.controlRequestChanges(cook.orderID, "correction")
			}()
			l = New(l.projectDir, "noodle", l.config, l.deps)
			if err := l.reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			reason := "correction"
			switch condition {
			case "head":
				publicationRun(t, cook.worktreePath, "git", "commit", "--allow-empty", "-m", "foreign")
			case "session":
				requestChangesWrite(t, filepath.Join(l.runtimeDir, "sessions", cook.session.ID(), "prompt.txt"), []byte("foreign"))
			case "events":
				requestChangesWrite(t, filepath.Join(l.runtimeDir, "sessions", cook.session.ID(), "events.ndjson"), nil)
			case "reason":
				reason = "foreign"
			}
			before := recoveryRefusalBytes(t, l)
			if err := l.controlRequestChanges(cook.orderID, reason); err == nil {
				t.Fatal("changed custody accepted")
			}
			if !reflect.DeepEqual(before, recoveryRefusalBytes(t, l)) {
				t.Fatal("refusal overwrote intent")
			}
		})
	}
}

func TestCompletedReviewCorrectionAllowsLaterBlockedRequeue(t *testing.T) {
	l, cook, _ := newRequestChangesRecoveryOutcome(t, event.StageOutcomeCompleted)
	if err := l.controlRequeue(cook.orderID); err != nil {
		t.Fatal(err)
	}
	if err := l.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	next := l.cooks.activeCooksByOrder[cook.orderID]
	if next == nil {
		t.Fatal("missing correction")
	}
	appendTypedOutcome(t, l, next, event.StageOutcomeBlocked, true, next.orderID, next.stageIndex)
	delete(l.cooks.activeCooksByOrder, next.orderID)
	if err := l.parkPendingReview(next, "blocked correction"); err != nil {
		t.Fatal(err)
	}
	if err := l.controlRequeue(cook.orderID); err != nil {
		t.Fatal(err)
	}
	stage := l.canonical.Orders[cook.orderID].Stages[0]
	if stage.Status != state.StagePending || len(stage.Attempts) != 2 {
		t.Fatal("ordinary blocked retry lost lineage")
	}
	if _, stale := stage.Extra[requestChangesRequeuedKey]; stale {
		t.Fatal("stale requeue receipt retained")
	}
}

func TestCompletedReviewRequeueReceiptCannotReactivateTerminalOrder(t *testing.T) {
	for _, status := range []state.OrderLifecycleStatus{state.OrderCompleted, state.OrderCancelled} {
		t.Run(string(status), func(t *testing.T) {
			l, cook, _ := newRequestChangesRecoveryOutcome(t, event.StageOutcomeCompleted)
			if err := l.controlRequeue(cook.orderID); err != nil {
				t.Fatal(err)
			}
			order := l.canonical.Orders[cook.orderID]
			order.Status = status
			order.Stages[0].Status = state.StageCompleted
			l.canonical.Orders[cook.orderID] = order
			if err := l.persistCanonicalCheckpoint(); err != nil {
				t.Fatal(err)
			}
			l = New(l.projectDir, "noodle", l.config, l.deps)
			if err := l.reconcile(context.Background()); err != nil {
				t.Fatal(err)
			}
			if order, exists := l.canonical.Orders[cook.orderID]; exists && order.Status != status {
				t.Fatalf("terminal order reactivated: %s", order.Status)
			}
			orders, err := l.currentOrders()
			if err != nil {
				t.Fatal(err)
			}
			for _, order := range orders.Orders {
				if order.ID == cook.orderID {
					t.Fatal("terminal order retained in dispatch projection")
				}
			}
		})
	}
}

func TestLegacyRequestChangesRequeueAckReadback(t *testing.T) {
	l, cook, packet := newRequestChangesRecovery(t)
	binding := packet.Binding
	binding.Reason = ""
	raw, _ := json.Marshal(binding)
	order := l.canonical.Orders[cook.orderID]
	order.Stages[0].Extra[requestChangesKey] = raw
	l.canonical.Orders[cook.orderID] = order
	if err := l.persistCanonicalCheckpoint(); err != nil {
		t.Fatal(err)
	}
	if err := l.projectRecoveredOrder(cook.orderID); err != nil {
		t.Fatal(err)
	}
	cmd := ControlCommand{ID: "legacy-requeue", Action: "requeue", OrderID: cook.orderID}
	control, _, _ := l.controlPaths()
	raw, _ = json.Marshal(cmd)
	requestChangesWrite(t, control, append(raw, '\n'))
	l.TestControlAckBarrier = func() { panic("stop before ack") }
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("missing interruption")
			}
		}()
		_ = l.processControlCommands()
	}()
	l = New(l.projectDir, "noodle", l.config, l.deps)
	if err := l.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ack := recoveryControl(t, l, cmd); ack.Status != "ok" {
		t.Fatalf("legacy reentry: %+v", ack)
	}
	if len(l.canonical.Orders[cook.orderID].Stages[0].Attempts) != 1 {
		t.Fatal("readback added attempt")
	}
}
