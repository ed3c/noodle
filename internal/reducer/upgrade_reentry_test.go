package reducer

import (
	"encoding/json"
	"github.com/poteto/noodle/internal/ingest"
	"github.com/poteto/noodle/internal/state"
	"reflect"
	"strings"
	"testing"
)

func TestUpgradeReturnIntentReentry(t *testing.T) {
	current := state.State{Orders: map[string]state.OrderNode{"o": {OrderID: "o", Status: state.OrderActive, Stages: []state.StageNode{{StageIndex: 0, Status: state.StageRunning, Attempts: []state.AttemptNode{{AttemptID: "a", SessionID: "s", ReturnHandoffUnattempted: true}}}}}}}
	intent := state.ReturnHandoffIntent{BindingPath: "/fixture/binding", BindingSHA256: strings.Repeat("a", 64), Status: "unknown"}
	payload := map[string]any{"order_id": "o", "stage_index": 0, "attempt_id": "a", "session_id": "s", "return_handoff": intent}
	parked, _, err := Reduce(current, mustStateEvent(42, ingest.EventStageReviewParked, payload, fixtureTime()))
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(parked)
	for _, variant := range []string{"same", "omitted", "foreign", "session"} {
		t.Run(variant, func(t *testing.T) {
			p := map[string]any{"order_id": "o", "stage_index": 0, "attempt_id": "a", "session_id": "s"}
			if variant != "omitted" {
				p["return_handoff"] = intent
			}
			if variant == "foreign" {
				foreign := intent
				foreign.BindingPath = "/foreign/binding"
				p["return_handoff"] = foreign
			}
			if variant == "session" {
				p["session_id"] = "foreign"
			}
			got, effects, err := Reduce(parked, mustStateEvent(43, ingest.EventStageReviewParked, p, fixtureTime()))
			after, _ := json.Marshal(got)
			if variant == "foreign" || variant == "session" {
				if string(before) != string(after) || len(effects) != 0 || err == nil {
					t.Fatal("foreign reentry altered original intent", err)
				}
				return
			}
			if err != nil || len(effects) != 0 || !reflect.DeepEqual(got.PendingReviews["o"].ReturnHandoff, parked.PendingReviews["o"].ReturnHandoff) || got.LastEventID != "43" {
				t.Fatal("same-attempt native repark failed or changed callback", err)
			}

		})
	}
}
