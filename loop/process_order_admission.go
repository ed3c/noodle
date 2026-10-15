package loop

import (
	"fmt"
	"github.com/poteto/noodle/internal/stringx"
	"slices"
)

// The context configuration is bound by the original Host attach request.
// This dispatch boundary does not grant an order lifecycle or merge authority.
func (l *Loop) requireProcessOrder(runtimeName, orderID string) error {
	runtimeName = stringx.Normalize(nonEmpty(runtimeName, nonEmpty(l.config.Runtime.Default, "process")))
	admitted := l.config.Runtime.Process.AdmittedOrders
	if runtimeName != "process" || len(admitted) == 0 || (orderID != "" && slices.Contains(admitted, orderID)) {
		return nil
	}
	return fmt.Errorf("process order %q is outside runtime.process.admitted_orders", orderID)
}
