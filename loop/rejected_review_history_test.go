package loop

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/poteto/noodle/event"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
)

func TestRejectedReviewHistoryRequiresBothOriginalRejections(t *testing.T) {
	l, cook, original := newRequestChangesRecoveryOutcome(t, event.StageOutcomeCompleted)
	if err := l.controlEditItem(ControlCommand{OrderID: cook.orderID, Prompt: "revise the original attempt"}); err != nil {
		t.Fatal(err)
	}
	if err := l.controlRequeue(cook.orderID); err != nil {
		t.Fatal(err)
	}
	snapshotPath := filepath.Join(l.runtimeDir, "state.snapshot.json")
	snapshot, err := reducer.ReadSnapshot(snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	order, review := original.Order, original.Review
	stage := &order.Stages[review.StageIndex]
	stage.Runtime, stage.TaskKey, review.Runtime = "openshell", "local-task", "openshell"
	current := stage.Attempts[0]
	current.AttemptID, current.SessionID, current.Error = "revision-attempt", "revision-session", "changes requested: revise the successor"
	review.SessionID = current.SessionID
	stage.Attempts = append(stage.Attempts, current)
	binding := original.Binding
	binding.AttemptID, binding.SessionID, binding.Attempt = current.AttemptID, current.SessionID, 1
	stage.Extra[state.RequestChangesKey], _ = json.Marshal(binding)
	snapshot.State.Orders[order.OrderID] = order
	snapshot.State.PendingReviews[order.OrderID] = review
	for i, row := range snapshot.EffectLedger {
		if row.EffectID != original.Admission.EffectID {
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(row.Effect.Payload, &payload); err != nil {
			t.Fatal(err)
		}
		payload["task_data_retention"] = reducer.TaskDataRetention
		snapshot.EffectLedger[i].Effect.Payload, _ = json.Marshal(payload)
	}
	recoveryWriteJSON(t, snapshotPath, snapshot)
	eventsPath := filepath.Join(l.runtimeDir, "loop-events.ndjson")
	events := recoveryRead(t, eventsPath)
	mistake := newCookMistakeEnvelope(CookMistakeReasonRequestChanges, order.OrderID, stage.StageIndex)
	failure := eventFailureMetadataForLoop(CycleFailureClassOrderHard, OrderFailureClassStageTerminal, &mistake)
	stageFailure := StageFailedPayload{OrderID: order.OrderID, StageIndex: stage.StageIndex, Reason: current.Error,
		SessionID: &current.SessionID, AgentMistake: &mistake, Failure: &failure}
	orderFailure := OrderFailedPayload{OrderID: order.OrderID, Reason: current.Error, AgentMistake: &mistake, Failure: &failure}
	stagePayload, _ := json.Marshal(stageFailure)
	orderPayload, _ := json.Marshal(orderFailure)
	for _, record := range []event.LoopEvent{{Type: event.LoopEventStageFailed, Payload: stagePayload},
		{Type: event.LoopEventOrderFailed, Payload: orderPayload}} {
		raw, err := json.Marshal(record)
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, append(raw, '\n')...)
	}
	requestChangesWrite(t, eventsPath, events)
	history, err := ReadRejectedReviewHistory(l.runtimeDir, order.OrderID)
	if err != nil || len(history.Attempts) != 2 || history.Attempts[0].SessionID != original.Binding.SessionID ||
		history.Attempts[1].SessionID != current.SessionID {
		t.Fatalf("two original rejections unavailable: %#v %v", history.Attempts, err)
	}
	for _, kind := range []event.LoopEventType{event.LoopEventOrderRequeued, event.LoopEventOrderFailed} {
		var collected []byte
		for index, line := range bytes.Split(bytes.TrimSpace(events), []byte{'\n'}) {
			var record event.LoopEvent
			if err := json.Unmarshal(line, &record); err != nil {
				t.Fatal(err)
			}
			if record.Type == kind && (kind == event.LoopEventOrderRequeued || index == bytes.Count(bytes.TrimSpace(events), []byte{'\n'})) {
				continue
			}
			collected = append(collected, append(line, '\n')...)
		}
		if err := os.WriteFile(eventsPath, collected, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadRejectedReviewHistory(l.runtimeDir, order.OrderID); err == nil {
			t.Fatalf("accepted history without the selected %s witness", kind)
		}
	}
}
