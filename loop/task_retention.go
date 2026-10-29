package loop

import (
	"slices"

	"github.com/poteto/noodle/internal/reducer"
)

const taskDataRetention = reducer.TaskDataRetention

func retainedTaskData(records []reducer.EffectLedgerRecord, orderID string) bool {
	return reducer.RetainedTaskData(records, orderID)
}

func (l *Loop) retainTaskData(orderID string) bool {
	if slices.Contains(l.config.Runtime.AdmittedOrders, orderID) || slices.Contains(l.config.Runtime.Process.AdmittedOrders, orderID) {
		return true
	}
	return l.effectLedger != nil && retainedTaskData(l.effectLedger.All(), orderID)
}
