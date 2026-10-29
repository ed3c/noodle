package runtime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"

	"github.com/poteto/noodle/internal/filex"
)

// The local owner lock is already held. Resume only the exact durable original
// transition; this never replays a native operation or creates a new migration.
func resumeCustodyRetirementMigration(binding OpenShellCustodyBinding, expected string, current, journal []byte, apply bool) (OpenShellCustodyMigration, error) {
	result := OpenShellCustodyMigration{}
	var audit custodyMigration
	if json.Unmarshal(journal, &audit) != nil || audit.BeforeSHA256 != expected || retainedRecoveryHash(audit.Before) != expected || retainedRecoveryHash(audit.After) != audit.AfterSHA256 {
		return result, fmt.Errorf("original custody migration journal digest differs")
	}
	var before, after OpenShellCustodyState
	if json.Unmarshal(audit.Before, &before) != nil || json.Unmarshal(audit.After, &after) != nil || before.Version != 3 || before.Binding != binding || before.MigrationBeforeSHA256 != "" {
		return result, fmt.Errorf("original custody migration journal binding differs")
	}
	if err := validateCustodyMigrationOriginal(before, binding); err != nil {
		return result, err
	}
	before.Version = openShellCustodyVersion
	before.MigrationBeforeSHA256 = expected
	if !reflect.DeepEqual(before, after) {
		return result, fmt.Errorf("custody migration successor differs from original transition")
	}
	result = OpenShellCustodyMigration{Status: "eligible", BeforeSHA256: expected, AfterSHA256: audit.AfterSHA256, EvidencePath: filepath.Join(binding.StateRoot, custodyMigrationFile)}
	if !bytes.Equal(current, audit.Before) {
		var observed OpenShellCustodyState
		if json.Unmarshal(current, &observed) != nil || observed.Binding != binding {
			return result, fmt.Errorf("custody migration owner differs from original before or successor")
		}
		if err := verifyCustodyMigration(observed); err != nil {
			return result, err
		}
		result.Status = "migrated"
		return result, nil
	}
	if !apply {
		return result, nil
	}
	result.Status = "unknown"
	if err := filex.WriteFileAtomicDurable(filepath.Join(binding.StateRoot, "owner.json"), audit.After); err != nil {
		return result, err
	}
	if err := verifyCustodyMigration(after); err != nil {
		return result, err
	}
	result.Status = "migrated"
	return result, nil
}
