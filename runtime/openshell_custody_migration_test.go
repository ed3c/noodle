package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenShellCustodyMigrationRetainsExactStoppedHistory(t *testing.T) {
	_, c, b, _ := completedObservationFixture(t)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	original := c.Snapshot()
	original.Version = 3
	raw, err := json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(b.StateRoot, "owner.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	inspected, err := MigrateOpenShellCustodyRetirement(b, retainedRecoveryHash(raw), false)
	if err != nil || inspected.Status != "eligible" {
		t.Fatal(err)
	}
	afterInspect, err := os.ReadFile(path)
	if err != nil || string(afterInspect) != string(raw) {
		t.Fatal("inspection changed owner")
	}
	result, err := MigrateOpenShellCustodyRetirement(b, retainedRecoveryHash(raw), true)
	if err != nil || result.Status != "migrated" {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var migrated OpenShellCustodyState
	if json.Unmarshal(data, &migrated) != nil || migrated.Version != 4 {
		t.Fatal("schema not advanced")
	}
	if err := verifyCustodyMigration(migrated); err != nil {
		t.Fatal(err)
	}
	migrated.Records[0].CreateRequestID = "changed"
	if err := verifyCustodyMigration(migrated); err == nil {
		t.Fatal("changed original history accepted")
	}
}

func TestOpenShellCustodyMigrationRejectsActiveOrChangedState(t *testing.T) {
	for _, variant := range []string{"locked", "digest", "unknown-stop", "unknown-exec", "retirement"} {
		t.Run(variant, func(t *testing.T) {
			_, c, b, _ := completedObservationFixture(t)
			original := c.Snapshot()
			original.Version = 3
			switch variant {
			case "unknown-stop":
				original.Records[0].StopAcknowledged = false
			case "unknown-exec":
				original.Records[0].Exec.Result.ExitObserved = false
			case "retirement":
				original.Records[0].Retirement = &OpenShellRetirement{}
			}
			raw, err := json.Marshal(original)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(b.StateRoot, "owner.json")
			if err := os.WriteFile(path, raw, 0600); err != nil {
				t.Fatal(err)
			}
			if variant != "locked" {
				if err := c.Close(); err != nil {
					t.Fatal(err)
				}
			}
			digest := retainedRecoveryHash(raw)
			if variant == "digest" {
				digest = retainedRecoveryHash([]byte("other"))
			}
			if _, err := MigrateOpenShellCustodyRetirement(b, digest, true); err == nil {
				t.Fatal("unsettled migration accepted")
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(raw) {
				t.Fatal("refusal changed owner")
			}
		})
	}
}
