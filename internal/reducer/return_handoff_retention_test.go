package reducer

import (
	"encoding/json"
	"github.com/poteto/noodle/internal/ingest"
	"github.com/poteto/noodle/internal/state"
	"reflect"
	"strings"
	"testing"
)

func TestReturnHandoffLegacyApprovalRetainsKnownIntent(t *testing.T) {
	intent := state.ReturnHandoffIntent{BindingPath: "/original/binding", BindingSHA256: strings.Repeat("a", 64), EventID: "6", Status: "unknown"}
	current := state.State{Orders: map[string]state.OrderNode{"o": {OrderID: "o", Status: state.OrderActive, Stages: []state.StageNode{{StageIndex: 0, Status: state.StageReview, Attempts: []state.AttemptNode{{AttemptID: "a", SessionID: "s", Status: state.AttemptCompleted}}}}}}, PendingReviews: map[string]state.PendingReviewNode{"o": {OrderID: "o", StageIndex: 0, SessionID: "s", ReturnHandoff: &intent}}}
	// Legacy JSON has no attempt-level fields. Keep the saved first before-image.
	raw, _ := json.Marshal(current)
	var legacy state.State
	if err := json.Unmarshal(raw, &legacy); err != nil {
		t.Fatal(err)
	}
	approved, _, err := Reduce(legacy, mustStateEvent(7, ingest.EventStageReviewApproved, map[string]any{"order_id": "o", "stage_index": 0}, fixtureTime()))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := approved.PendingReviews["o"]; ok {
		t.Fatal("approval retained transient row")
	}
	if got := approved.Orders["o"].Stages[0].Attempts[0].ReturnHandoff; got == nil || *got != intent {
		t.Fatal("legacy approval lost original event", got)
	}
	snapshot := BuildSnapshot(approved, nil, fixtureTime())
	bytes, _ := json.Marshal(snapshot)
	var restored DurableSnapshot
	if err := json.Unmarshal(bytes, &restored); err != nil {
		t.Fatal(err)
	}
	next, _, err := Reduce(restored.State, mustStateEvent(10, ingest.EventStageReviewParked, map[string]any{"order_id": "o", "stage_index": 0, "attempt_id": "a", "session_id": "s", "reason": "merge failed"}, fixtureTime()))
	if err != nil || !reflect.DeepEqual(next.PendingReviews["o"].ReturnHandoff, &intent) {
		t.Fatal("restored approval reparks with new intent", err)
	}
}

func TestReturnHandoffMissingLegacyHistoryNeverArmsCallback(t *testing.T) {
	for _, phase := range []state.StageLifecycleStatus{state.StageReview, state.StageMerging, state.StageFailed, state.StageRunning} {
		t.Run(string(phase), func(t *testing.T) {
			current := state.State{Orders: map[string]state.OrderNode{"o": {OrderID: "o", Status: state.OrderActive, Stages: []state.StageNode{{StageIndex: 0, Status: phase, Attempts: []state.AttemptNode{{AttemptID: "a", SessionID: "s", Status: state.AttemptCompleted}}}}}}}
			payload := map[string]any{"order_id": "o", "stage_index": 0, "attempt_id": "a", "session_id": "s", "reason": "native failure readback"}
			next, _, err := Reduce(current, mustStateEvent(10, ingest.EventStageReviewParked, payload, fixtureTime()))
			if err != nil || next.PendingReviews["o"].ReturnHandoff != nil || next.Orders["o"].Stages[0].Status != state.StageReview {
				t.Fatal("legacy native park changed authority", err)
			}
			payload["return_handoff"] = &state.ReturnHandoffIntent{BindingPath: "/new/binding", BindingSHA256: strings.Repeat("a", 64), Status: "unknown"}
			refused, _, err := Reduce(current, mustStateEvent(11, ingest.EventStageReviewParked, payload, fixtureTime()))
			if err == nil || !reflect.DeepEqual(refused, current) {
				t.Fatal("missing legacy history became unsent proof")
			}
		})
	}
}

func TestReturnHandoffDispatchReplayCannotRearmKnownIntent(t *testing.T) {
	intent := &state.ReturnHandoffIntent{BindingPath: "/original/binding", BindingSHA256: strings.Repeat("a", 64), EventID: "6", Status: "unknown"}
	current := state.State{Orders: map[string]state.OrderNode{"o": {OrderID: "o", Status: state.OrderActive, Stages: []state.StageNode{{StageIndex: 0, Status: state.StageReview, Attempts: []state.AttemptNode{{AttemptID: "a", SessionID: "s", ReturnHandoff: intent}}}}}}}
	next, _, err := Reduce(current, mustStateEvent(8, ingest.EventDispatchCompleted, map[string]any{"order_id": "o", "stage_index": 0, "attempt_id": "a", "session_id": "s"}, fixtureTime()))
	if err != nil {
		t.Fatal(err)
	}
	got := next.Orders["o"].Stages[0].Attempts[0]
	if got.ReturnHandoffUnattempted || !reflect.DeepEqual(got.ReturnHandoff, intent) {
		t.Fatal("dispatch replay rearmed original callback")
	}
}

func TestReturnHandoffKnownSessionCannotBeRebound(t *testing.T) {
	for _, kind := range []ingest.EventType{ingest.EventDispatchCompleted, ingest.EventSessionAdopted} {
		t.Run(string(kind), func(t *testing.T) {
			intent := &state.ReturnHandoffIntent{BindingPath: "/original/binding", BindingSHA256: strings.Repeat("a", 64), EventID: "6", Status: "unknown"}
			current := state.State{Orders: map[string]state.OrderNode{"o": {OrderID: "o", Status: state.OrderActive, Stages: []state.StageNode{{StageIndex: 0, Status: state.StageReview, Attempts: []state.AttemptNode{{AttemptID: "a", SessionID: "s", ReturnHandoff: intent}}}}}}}
			next, _, err := Reduce(current, mustStateEvent(11, kind, map[string]any{"order_id": "o", "stage_index": 0, "attempt_id": "a", "session_id": "foreign"}, fixtureTime()))
			if err == nil || !reflect.DeepEqual(next, current) {
				t.Fatal("callback history was relabeled to foreign session")
			}
		})
	}
}
