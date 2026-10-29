package reducer

import (
	"encoding/json"
	"testing"
)

func TestReclamationRequiresOriginalCompletedAdmissionPolicy(t *testing.T) {
	for _, variant := range []string{"new", "historical", "pending", "failed-result", "foreign-order", "foreign-effect"} {
		t.Run(variant, func(t *testing.T) {
			id := InitialAdmissionEffectID("task")
			policy, order := TaskDataAfterClosure, "task"
			if variant == "historical" {
				policy = TaskDataRetention
			}
			if variant == "foreign-order" {
				order = "foreign"
			}
			payload, err := json.Marshal(map[string]string{"order_id": order, "task_data_retention": policy})
			if err != nil {
				t.Fatal(err)
			}
			ledger := NewEffectLedger()
			kind := EffectInitialAdmission
			if variant == "foreign-effect" {
				kind = EffectDispatch
			}
			ledger.Record(Effect{EffectID: id, Type: kind, Payload: payload})
			if variant != "pending" {
				if err := ledger.MarkRunning(id); err != nil {
					t.Fatal(err)
				}
				status := EffectResultCompleted
				if variant == "failed-result" {
					status = EffectResultFailed
				}
				if err := ledger.MarkDone(id, EffectResult{EffectID: id, Status: status}); err != nil {
					t.Fatal(err)
				}
			}
			if ReclaimableTaskData(ledger.All(), "task") != (variant == "new") {
				t.Fatal("reclamation eligibility crossed original admission boundary")
			}
			if variant == "historical" && !RetainedTaskData(ledger.All(), "task") {
				t.Fatal("historical retention lost")
			}
		})
	}
}
