package reducer

import (
	"encoding/json"
	"github.com/poteto/noodle/internal/ingest"
	"github.com/poteto/noodle/internal/state"
	"strings"
	"testing"
)

func TestUpgradeReturnIntentReentry(t *testing.T) {
	current := state.State{Orders: map[string]state.OrderNode{"o": {OrderID: "o", Status: state.OrderActive, Stages: []state.StageNode{{StageIndex: 0, Status: state.StageRunning}}}}}
	intent := state.ReturnHandoffIntent{BindingPath: "/fixture/binding", BindingSHA256: strings.Repeat("a", 64), Status: "unknown"}
	payload := map[string]any{"order_id": "o", "stage_index": 0, "attempt_id": "a", "session_id": "s", "return_handoff": intent}
	parked, _, err := Reduce(current, mustStateEvent(42, ingest.EventStageReviewParked, payload, fixtureTime()))
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(parked)
	for _, variant := range []string{"same", "omitted", "foreign"} {
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
			got, effects, err := Reduce(parked, mustStateEvent(43, ingest.EventStageReviewParked, p, fixtureTime()))
			after, _ := json.Marshal(got)
			if string(before) != string(after) || len(effects) != 0 {
				t.Fatal("reentry replaced durable original intent")
			}
			if err == nil || !strings.Contains(err.Error(), "original Owner readback required") {
				t.Fatal("saved intent requires owner readback", err)
			}
		})
	}
}
