package monitor

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMonitorUnstartedSessionPreservesDispatchIdentity(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "sessions", "unstarted")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	// A transfer failure has original spawn provenance but no model event log,
	// heartbeat or previous metadata. Only the formal Monitor may create meta.
	if err := os.WriteFile(filepath.Join(dir, "spawn.json"), []byte(`{"session_id":"unstarted","provider":"codex","model":"original-model","runtime":"openshell"}`), 0600); err != nil {
		t.Fatal(err)
	}
	rows, err := NewMonitor(root).RunOnce(t.Context())
	if err != nil || len(rows) != 1 {
		t.Fatalf("formal Monitor: %+v, %v", rows, err)
	}
	got := rows[0]
	if got.SessionID != "unstarted" || got.Provider != "codex" || got.Model != "original-model" || got.Runtime != "openshell" || got.Status != SessionStatusFailed || got.Alive || got.ExitPending {
		t.Fatalf("unstarted terminal metadata lost dispatch provenance: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "canonical.ndjson")); !os.IsNotExist(err) {
		t.Fatalf("monitor invented model events: %v", err)
	}
	stored, err := readSessionMeta(filepath.Join(dir, "meta.json"))
	if err != nil || stored != got {
		t.Fatalf("formal metadata differs: %+v, %v", stored, err)
	}
}
