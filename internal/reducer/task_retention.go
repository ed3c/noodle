package reducer

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

const TaskDataRetention = "explicit_delete"

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
		if json.Unmarshal(record.Effect.Payload, &payload) == nil && payload.OrderID == orderID && payload.Retention == TaskDataRetention {
			return true
		}
	}
	return false
}
