package admission

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/poteto/noodle/internal/procx"
)

// Inherit reads the journal of a known prior Host generation. Only after its
// original owner lifetime is proven absent can the new Host become its writer.
// Prior action intents are copied as readback-only history, never replayed.
func (m *FixtureRecovery) Inherit(directory string) error {
	path := filepath.Join(directory, "fixture-recovery.json")
	if path == m.path {
		return nil
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	prior, err := OpenFixtureRecovery(directory)
	if err != nil {
		return err
	}
	eligible := []*FixtureReclamation{}
	for _, row := range prior.rows {
		original := row.RecoveryOwner
		if original == nil {
			original = row.Candidate.Owner
		}
		if original == nil {
			continue
		}
		owner, readErr := procx.ObserveNativeIdentity(original.PID)
		if readErr != nil && !errors.Is(readErr, procx.ErrProcessGone) {
			return readErr
		}
		if readErr == nil && !owner.Zombie && owner.StartSec == original.StartSec && owner.StartUsec == original.StartUsec {
			continue
		}
		eligible = append(eligible, row)
	}
	if len(eligible) == 0 {
		return nil
	}
	currentOwner, err := procx.ObserveNativeIdentity(os.Getpid())
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failure != nil {
		return m.failure
	}
	changed := false
	for _, row := range eligible {
		if m.rows[row.ID] != nil {
			continue
		}
		row.OriginJournal = path
		row.RecoveryOwner = &currentOwner
		m.rows[row.ID] = row
		changed = true
	}
	if changed {
		return m.saveLocked()
	}
	return nil
}

func (m *FixtureRecovery) RecordFailure(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failure == nil {
		m.failure = fmt.Errorf("fixture recovery lifecycle observation failed: %w", err)
	}
}
