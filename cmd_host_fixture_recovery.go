package main

import (
	"context"
	"fmt"
	"os"
)

// The existing single Host owns lease liveness and observed fixture recovery.
func (h *noodleHost) reconcileFixtureLifecycle(ctx context.Context) {
	if h.residents == nil {
		return
	}
	if err := h.residents.ReconcileConsumerLeases(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "consumer lease readback:", err)
	}
	h.residents.ReconcileFixtures(ctx)
	// Recovered original generations return to the existing resident accounting
	// only after launcher, group and every observed descendant are confirmed gone.
	if _, err := h.residents.Reconcile(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "resident recovery readback:", err)
	}
}
