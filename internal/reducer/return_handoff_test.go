package reducer

import (
	"github.com/poteto/noodle/internal/ingest"
	"github.com/poteto/noodle/internal/state"
	"strings"
	"testing"
)

func TestReturnHandoffCheckpointAndClone(t *testing.T) {
	current := state.State{Orders: map[string]state.OrderNode{"o": {OrderID: "o", Status: state.OrderActive, Stages: []state.StageNode{{StageIndex: 0, Status: state.StageRunning, Attempts: []state.AttemptNode{{AttemptID: "a", SessionID: "s", ReturnHandoffUnattempted: true}}}}}}}
	intent := state.ReturnHandoffIntent{BindingPath: "/fixture/binding", BindingSHA256: strings.Repeat("a", 64), Status: "unknown"}
	payload := map[string]any{"order_id": "o", "stage_index": 0, "attempt_id": "a", "session_id": "s", "return_handoff": intent}
	event := mustStateEvent(42, ingest.EventStageReviewParked, payload, fixtureTime())
	next, effects, err := Reduce(current, event)
	if err != nil {
		t.Fatal(err)
	}
	got := next.PendingReviews["o"].ReturnHandoff
	if got == nil || got.EventID != "42" || got.Status != "unknown" || len(effects) != 0 {
		t.Fatalf("missing durable intent: %+v, effects %v", got, effects)
	}
	clone := next.Clone()
	clone.PendingReviews["o"].ReturnHandoff.Status = "mutated"
	clone.Orders["o"].Stages[0].Attempts[0].ReturnHandoff.Status = "mutated attempt"
	if got.Status != "unknown" {
		t.Fatal("clone changed original intent")
	}
	if next.Orders["o"].Stages[0].Attempts[0].ReturnHandoff.Status != "unknown" || next.Orders["o"].Stages[0].Attempts[0].ReturnHandoffUnattempted {
		t.Fatal("clone changed durable attempt intent or left first return armed")
	}
	intent.Status = "acknowledged"
	payload["return_handoff"] = intent
	if _, _, err := Reduce(current, mustStateEvent(43, ingest.EventStageReviewParked, payload, fixtureTime())); err == nil {
		t.Fatal("producer cannot invent ACK")
	}
	intent.Status = "unknown"
	intent.EventID = "previous-unknown-event"
	payload["return_handoff"] = intent
	if _, _, err := Reduce(current, mustStateEvent(43, ingest.EventStageReviewParked, payload, fixtureTime())); err == nil {
		t.Fatal("new return overwrote an existing unknown event")
	}
	delete(payload, "return_handoff")
	legacy, _, err := Reduce(current, mustStateEvent(44, ingest.EventStageReviewParked, payload, fixtureTime()))
	if err != nil || legacy.PendingReviews["o"].ReturnHandoff != nil {
		t.Fatal("legacy opt-out changed", err)
	}
}
