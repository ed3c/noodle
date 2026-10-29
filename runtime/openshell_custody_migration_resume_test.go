package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCustodyMigrationResumesOriginalJournalWithoutNativeReplay(t *testing.T) {
	_, c, b, _ := completedObservationFixture(t)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	before := c.Snapshot()
	before.Version = 3
	raw, err := json.Marshal(before)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(b.StateRoot, "owner.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	digest := retainedRecoveryHash(raw)
	if _, err := MigrateOpenShellCustodyRetirement(b, digest, true); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Crash after durable intent but before owner replacement: retain journal.
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	inspection, err := MigrateOpenShellCustodyRetirement(b, digest, false)
	if err != nil || inspection.Status != "eligible" {
		t.Fatalf("original intent cannot be inspected: %v", err)
	}
	current, err := os.ReadFile(path)
	if err != nil || string(current) != string(raw) {
		t.Fatal("inspection changed before image")
	}
	resumed, err := MigrateOpenShellCustodyRetirement(b, digest, true)
	if err != nil || resumed.Status != "migrated" {
		t.Fatalf("original intent cannot resume: %v", err)
	}
	current, err = os.ReadFile(path)
	if err != nil || string(current) != string(after) {
		t.Fatal("resume did not preserve exact successor")
	}
	observed, err := MigrateOpenShellCustodyRetirement(b, digest, false)
	if err != nil || observed.Status != "migrated" {
		t.Fatalf("completed migration readback failed: %v", err)
	}
	if _, err := MigrateOpenShellCustodyRetirement(b, retainedRecoveryHash([]byte("foreign")), true); err == nil {
		t.Fatal("foreign migration adopted")
	}
	var state OpenShellCustodyState
	if err := json.Unmarshal(after, &state); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(b.StateRoot, custodyMigrationFile)); err != nil {
		t.Fatal(err)
	}
	if err := verifyCustodyMigration(state); err == nil {
		t.Fatal("migrated custody silently lost its journal")
	}
}
