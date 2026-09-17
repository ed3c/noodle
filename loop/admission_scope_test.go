package loop

import (
	"encoding/json"
	"testing"

	"github.com/poteto/noodle/internal/reducer"
)

func TestAdmissionRecoveryHistoricalEvidenceScope(t *testing.T) {
	for _, name := range []string{"unrelated_pending_effect", "stale_running_metadata"} {
		t.Run(name, func(t *testing.T) {
			tc, _ := recoveryFixture(t)
			switch name {
			case "unrelated_pending_effect":
				recoverySnapshot(t, tc, func(s *reducer.DurableSnapshot) {
					s.EffectLedger = append(s.EffectLedger, reducer.EffectLedgerRecord{
						EffectID: "historical-dispatch", Status: reducer.EffectLedgerPending,
						Effect: reducer.Effect{EffectID: "historical-dispatch", Type: reducer.EffectDispatch, Payload: json.RawMessage(`{"order_id":"schedule","stage_index":0,"attempt_id":"old-schedule"}`)},
					})
				})
			case "stale_running_metadata":
				// Fresh kernel observation proves both PID and process group absent.
				// Stale derived metadata does not authorize dispatch or complete a session.
				recoverySession(t, tc, "running", 99999999)
			}
			r := InspectAdmission(tc.projectDir, "noodle")
			if r.Status != "recoverable" {
				t.Fatalf("unrelated/stale evidence blocked exact unpromoted subject: %+v", r)
			}
		})
	}
}
