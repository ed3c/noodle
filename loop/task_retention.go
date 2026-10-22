package loop

import (
	"encoding/json"
	"slices"

	"github.com/poteto/noodle/internal/reducer"
)

const taskDataRetention = "explicit_delete"

func retainedTaskData(records []reducer.EffectLedgerRecord, orderID string) bool {
	for _, record := range records {
		if record.EffectID != initialAdmissionEffectID(orderID) {
			continue
		}
		var payload struct {
			OrderID   string `json:"order_id"`
			Retention string `json:"task_data_retention"`
		}
		if json.Unmarshal(record.Effect.Payload, &payload) == nil && payload.OrderID == orderID && payload.Retention == taskDataRetention {
			return true
		}
	}
	return false
}

func (l *Loop) retainTaskData(orderID string) bool {
	if slices.Contains(l.config.Runtime.Process.AdmittedOrders, orderID) {
		return true
	}
	return l.effectLedger != nil && retainedTaskData(l.effectLedger.All(), orderID)
}
