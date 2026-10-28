package admission

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/poteto/noodle/internal/procx"
)

func (m *FixtureRecovery) BindClaimDirectory(directory string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.claims == directory {
		return nil
	}
	if m.running {
		return errors.New("cannot change fixture claim directory during recovery")
	}
	for _, row := range m.rows {
		if row.ClaimPath != "" && filepath.Dir(row.ClaimPath) != directory {
			return errors.New("cannot relocate an existing fixture claim")
		}
	}
	m.claims = directory
	return nil
}

// O_EXCL coordinates all Host generations. Claims survive crashes and ambiguous
// responses. Another journal may observe the original effect, never repeat TERM.
func (m *FixtureRecovery) claimLocked(v *FixtureReclamation, p procx.NativeIdentity) error {
	if err := os.MkdirAll(m.claims, 0700); err != nil {
		return err
	}
	parent, err := os.Open(filepath.Dir(m.claims))
	if err != nil {
		return err
	}
	if err = errors.Join(parent.Sync(), parent.Close()); err != nil {
		return err
	}
	path := filepath.Join(m.claims, v.ID+".json")
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return fmt.Errorf("fixture recovery claim unavailable; readback only at %s: %w", path, err)
	}
	v.ClaimPath = path
	data := map[string]any{"id": v.ID, "journal": m.path, "binding": v.Binding, "intent_at": time.Now().UTC().Format(time.RFC3339Nano), "writer_pid": os.Getpid(), "native_identity": p, "evidence": v.Evidence}
	err = json.NewEncoder(f).Encode(data)
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err == nil {
		dir, openErr := os.Open(m.claims)
		if openErr != nil {
			return openErr
		}
		err = errors.Join(dir.Sync(), dir.Close())
	}
	return err
}
