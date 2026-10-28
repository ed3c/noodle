package loop

import (
	"fmt"
	"slices"
)

// The context configuration is bound by the original Host attach request.
// This dispatch boundary does not grant an order lifecycle or merge authority.
func (l *Loop) requireAdmittedOrder(orderID string) error {
	admitted := l.config.Runtime.AdmittedOrders
	if len(admitted) == 0 || (orderID != "" && slices.Contains(admitted, orderID)) {
		return nil
	}
	return fmt.Errorf("order %q is outside runtime.admitted_orders", orderID)
}
