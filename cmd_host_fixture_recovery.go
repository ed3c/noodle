package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

// Reuse the original Host lifecycle event. Separate generation journals avoid
// concurrent writers; only records whose original owner exited are inherited.
func (h *noodleHost) reconcileFixtureLifecycle(ctx context.Context) {
	if h.residents == nil {
		return
	}
	if err := h.residents.ReconcileConsumerLeases(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "consumer lease readback:", err)
	}
	m := h.residents.FixtureRecoveries()
	if h.rolling != nil {
		if err := m.BindClaimDirectory(filepath.Join(h.rolling.root, "fixture-recovery-claims")); err != nil {
			m.RecordFailure(err)
			fmt.Fprintln(os.Stderr, "fixture recovery claims:", err)
			return
		}
		pool, err := readHostPool(h.rolling.root)
		if err == nil {
			for _, generation := range pool.Generations {
				if pool.Active != h.instanceID {
					break
				}
				if generation.ID == h.instanceID {
					continue
				}
				if err = m.Inherit(generation.Directory); err != nil {
					break
				}
			}
		}
		if err != nil {
			m.RecordFailure(err)
			fmt.Fprintln(os.Stderr, "fixture recovery lifecycle:", err)
			return
		}
	}
	h.residents.ReconcileFixtures(ctx)
	if _, err := h.residents.Reconcile(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "resident recovery readback:", err)
	}
	if h.rolling == nil {
		return
	}
	rows, err := m.Snapshot()
	if err != nil {
		return
	}
	pool, err := readHostPool(h.rolling.root)
	if err != nil {
		m.RecordFailure(err)
		return
	}
	for _, row := range rows {
		if !row.Candidate.ConsumerManaged || row.State != "exited" || row.OriginJournal == "" {
			continue
		}
		for _, generation := range pool.Generations {
			if generation.ID == h.instanceID || row.OriginJournal != filepath.Join(generation.Directory, "fixture-recovery.json") {
				continue
			}
			if err := h.budget.ReleaseRecoveredFixtureQuota(generation.ID, row); err != nil {
				m.RecordFailure(err)
				fmt.Fprintln(os.Stderr, "recovered fixture quota:", err)
				return
			}
		}
	}
}
