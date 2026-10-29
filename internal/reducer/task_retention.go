package reducer

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

const TaskDataRetention = "explicit_delete"
const TaskDataAfterClosure = "after_task_closure"

func InitialAdmissionEffectID(orderID string) string {
	return fmt.Sprintf("initial-admission-%x", sha256.Sum256([]byte(orderID)))
}

func RetainedTaskData(records []EffectLedgerRecord, orderID string) bool {
	for _, record := range records {
		if record.EffectID != InitialAdmissionEffectID(orderID) {
			continue
		}
		var payload struct {
			OrderID   string `json:"order_id"`
			Retention string `json:"task_data_retention"`
		}
		if json.Unmarshal(record.Effect.Payload, &payload) == nil && payload.OrderID == orderID && (payload.Retention == TaskDataRetention || payload.Retention == TaskDataAfterClosure) {
			return true
		}
	}
	return false
}

// ReclaimableTaskData is frozen by original admission; historical explicit
// retention never inherits a new binary's automatic reclamation policy.
func ReclaimableTaskData(records []EffectLedgerRecord, orderID string) bool {
	for _, record := range records {
		if record.EffectID != InitialAdmissionEffectID(orderID) || record.Status != EffectLedgerDone || record.Effect.Type != EffectInitialAdmission || record.Result == nil || record.Result.EffectID != record.EffectID || record.Result.Status != EffectResultCompleted {
			continue
		}
		var payload struct {
			OrderID   string `json:"order_id"`
			Retention string `json:"task_data_retention"`
		}
		return json.Unmarshal(record.Effect.Payload, &payload) == nil && payload.OrderID == orderID && payload.Retention == TaskDataAfterClosure
	}
	return false
}
