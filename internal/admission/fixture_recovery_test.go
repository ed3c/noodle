package admission

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/poteto/noodle/internal/procx"
)

// Only the recovery regression explicitly reproduces the old orphan lifetime.
// It remains cooperative and bounded even if the controller test fails.
func waitLegacyRecoveryProbe(t *testing.T) bool {
	if os.Getenv("NOODLE_RECOVERY_LEGACY_PROBE") != "1" {
		return false
	}
	if err := os.WriteFile("ready", nil, 0600); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat("exit"); err == nil {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return true
}

func TestFixtureRecoveryUnknownIntentAndReadonlySnapshot(t *testing.T) {
	r, b, spec := residentFixture(t)
	v, err := r.Acquire(spec, "a", b)
	if err != nil {
		t.Fatal(err)
	}
	m := r.FixtureRecoveries()
	rows, err := m.Snapshot()
	if err != nil || len(rows) != 1 {
		t.Fatalf("registration: %+v %v", rows, err)
	}
	id := rows[0].ID
	m.mu.Lock()
	m.rows[id].Actions = []FixtureAction{{Kind: "term", IntentAt: "2026-10-27T00:00:00Z"}}
	m.rows[id].State = "unknown"
	err = m.saveLocked()
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(m.path)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenFixtureRecovery(filepath.Dir(m.path))
	if err != nil {
		t.Fatal(err)
	}
	reopened.RunPending(context.Background())
	rows, err = reopened.Snapshot()
	if err != nil || len(rows) != 1 || rows[0].State != "unknown" || len(rows[0].Actions) != 1 || rows[0].Actions[0].Result != "" {
		t.Fatalf("unknown control replayed: %+v %v", rows, err)
	}
	after, err := os.ReadFile(m.path)
	if err != nil || string(before) != string(after) {
		t.Fatalf("readback mutated journal: %v", err)
	}
	current, _ := r.Snapshot()
	if len(current) != 1 || current[0].PID != v.PID || current[0].State != "running" || len(current[0].Contexts) != 1 {
		t.Fatal("readback affected original live lease", current)
	}
	var journal map[string]FixtureReclamation
	if err = json.Unmarshal(after, &journal); err != nil {
		t.Fatal(err)
	}
	changed := rows[0].Candidate
	changed.Directory += "-changed"
	if err = reopened.Register(FixtureManifest{Schema: 1, EvidenceSHA256: rows[0].EvidenceSHA256, Candidates: []FixtureCandidate{changed}}, "different-binding"); err == nil {
		t.Fatal("conflicting frozen evidence accepted")
	}
}

func TestFixtureRecoveryKeepsActiveOwner(t *testing.T) {
	r, b, spec := residentFixture(t)
	if _, err := r.Acquire(spec, "a", b); err != nil {
		t.Fatal(err)
	}
	m := r.FixtureRecoveries()
	m.RunPending(context.Background())
	deadline := time.Now().Add(time.Second)
	for {
		rows, err := m.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) == 1 && (rows[0].State == "waiting_owner" || rows[0].State == "unknown") {
			if len(rows[0].Actions) != 0 {
				t.Fatal("active owner authorized control", rows)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("owner decision unobserved", rows)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestFixtureRecoveryPriorClaimCannotResend(t *testing.T) {
	r, b, spec := residentFixture(t)
	if _, err := r.Acquire(spec, "a", b); err != nil {
		t.Fatal(err)
	}
	m := r.FixtureRecoveries()
	rows, err := m.Snapshot()
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	c := rows[0].Candidate
	if c.Native == nil {
		t.Skip("native process generation unavailable")
	}
	claims := t.TempDir()
	if err = m.BindClaimDirectory(claims); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(claims, rows[0].ID+".json"), []byte(`{"prior_effect":"unknown"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err = m.act(rows[0].ID, *c.Native, false); err == nil {
		t.Fatal("prior cross-generation claim allowed another control")
	}
	current, err := procx.ObserveNativeIdentity(c.PID)
	if err != nil || current.Zombie || !c.Native.SameGeneration(current) {
		t.Fatalf("prior claim affected original fixture: %+v %v", current, err)
	}
	rows, err = m.Snapshot()
	if err != nil || len(rows[0].Actions) != 0 || rows[0].State != "unknown" {
		t.Fatal(rows, err)
	}
}

func TestFixtureRecoveryIdentityDriftIsNotExit(t *testing.T) {
	r, b, spec := residentFixture(t)
	if _, err := r.Acquire(spec, "a", b); err != nil {
		t.Fatal(err)
	}
	rows, err := r.FixtureRecoveries().Snapshot()
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	c := rows[0].Candidate
	if c.Native == nil {
		t.Skip("native process generation unavailable")
	}
	changed := *c.Native
	changed.Version++
	c.Native = &changed
	if gone, err := fixtureGenerationGone(c, nil); err == nil || gone {
		t.Fatalf("identity drift reported as proven exit: %v %v", gone, err)
	}
}
