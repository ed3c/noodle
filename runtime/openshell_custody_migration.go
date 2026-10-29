package runtime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"

	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/lockfile"
)

const custodyMigrationFile = "retirement-schema-migration.json"

type custodyMigration struct {
	Before, After             []byte
	BeforeSHA256, AfterSHA256 string
}
type OpenShellCustodyMigration struct {
	Status       string `json:"status"`
	BeforeSHA256 string `json:"before_sha256"`
	AfterSHA256  string `json:"after_sha256"`
	EvidencePath string `json:"evidence_path"`
}

// MigrateOpenShellCustodyRetirement is offline and local only. Holding original
// custody excludes a living gateway owner. It changes only schema 3 -> 4 and
// retains the exact before-image; no native RPC, replay or retention change.
func MigrateOpenShellCustodyRetirement(binding OpenShellCustodyBinding, expected string, apply bool) (OpenShellCustodyMigration, error) {
	result := OpenShellCustodyMigration{}
	root, err := filepath.EvalSymlinks(binding.StateRoot)
	if err != nil {
		return result, err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return result, err
	}
	binding.StateRoot = root
	lock, err := lockfile.TryLock(filepath.Join(root, "owner.lock"))
	if err != nil {
		return result, fmt.Errorf("original custody is still owned: %w", err)
	}
	defer lock.Close()
	path := filepath.Join(root, "owner.json")
	before, err := regularMigrationFile(path)
	if err != nil {
		return result, err
	}
	if len(expected) != 64 {
		return result, fmt.Errorf("reviewed original custody digest missing")
	}
	auditPath := filepath.Join(root, custodyMigrationFile)
	if raw, err := regularMigrationFile(auditPath); err == nil {
		return resumeCustodyRetirementMigration(binding, expected, before, raw, apply)
	} else if !os.IsNotExist(err) {
		return result, err
	}
	if retainedRecoveryHash(before) != expected {
		return result, fmt.Errorf("reviewed original custody digest differs")
	}
	var original OpenShellCustodyState
	decoder := json.NewDecoder(bytes.NewReader(before))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&original); err != nil {
		return result, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return result, fmt.Errorf("custody migration source has trailing JSON")
	}
	if err := validateCustodyMigrationOriginal(original, binding); err != nil {
		return result, err
	}

	successor := original
	successor.Version = openShellCustodyVersion
	successor.MigrationBeforeSHA256 = expected
	after, err := json.Marshal(successor)
	if err != nil {
		return result, err
	}
	audit := custodyMigration{Before: before, After: after, BeforeSHA256: expected, AfterSHA256: retainedRecoveryHash(after)}
	result = OpenShellCustodyMigration{Status: "eligible", BeforeSHA256: expected, AfterSHA256: audit.AfterSHA256, EvidencePath: auditPath}
	if _, err := os.Lstat(auditPath); !os.IsNotExist(err) {
		return result, fmt.Errorf("original custody migration evidence already exists; inspect original state")
	}
	if !apply {
		return result, nil
	}
	raw, err := json.Marshal(audit)
	if err != nil {
		return result, err
	}
	if err := filex.WriteFileAtomicDurable(auditPath, raw); err != nil {
		return result, err
	}
	current, err := regularMigrationFile(path)
	if err != nil {
		return result, err
	}
	if !bytes.Equal(current, before) {
		return result, fmt.Errorf("original custody changed during migration")
	}
	result.Status = "unknown"
	if err := filex.WriteFileAtomicDurable(path, after); err != nil {
		return result, err
	}
	if err := verifyCustodyMigration(successor); err != nil {
		return result, err
	}
	result.Status = "migrated"
	return result, nil
}

func regularMigrationFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 64<<20 {
		return nil, fmt.Errorf("custody migration evidence is not a bounded regular file")
	}
	return os.ReadFile(path)
}

func verifyCustodyMigration(current OpenShellCustodyState) error {
	raw, err := regularMigrationFile(filepath.Join(current.Binding.StateRoot, custodyMigrationFile))
	if os.IsNotExist(err) {
		if current.MigrationBeforeSHA256 == "" {
			return nil
		}
		return fmt.Errorf("original custody migration journal is missing")
	}
	if err != nil {
		return err
	}
	var audit custodyMigration
	if json.Unmarshal(raw, &audit) != nil || retainedRecoveryHash(audit.Before) != audit.BeforeSHA256 || retainedRecoveryHash(audit.After) != audit.AfterSHA256 {
		return fmt.Errorf("custody migration before-image or successor digest differs")
	}
	var before, after OpenShellCustodyState
	if json.Unmarshal(audit.Before, &before) != nil || json.Unmarshal(audit.After, &after) != nil || before.Version != 3 || after.Version != openShellCustodyVersion {
		return fmt.Errorf("custody migration schema differs")
	}
	before.Version = openShellCustodyVersion
	before.MigrationBeforeSHA256 = audit.BeforeSHA256
	if !reflect.DeepEqual(before, after) || current.Version != after.Version || current.Binding != after.Binding || len(current.Records) < len(after.Records) {
		return fmt.Errorf("custody migration changed original history or binding")
	}
	if current.MigrationBeforeSHA256 != audit.BeforeSHA256 {
		return fmt.Errorf("custody migration original digest binding differs")
	}
	for i, r := range after.Records {
		if !reflect.DeepEqual(current.Records[i], r) {
			return fmt.Errorf("custody migrated retained history changed")
		}
	}
	return nil
}

func validateCustodyMigrationOriginal(original OpenShellCustodyState, binding OpenShellCustodyBinding) error {
	if original.Version != 3 || original.MigrationBeforeSHA256 != "" || original.Binding != binding || original.Blocked != "" || len(original.PendingRetained) != 0 {
		return fmt.Errorf("original schema 3 custody is not settled or binding differs")
	}
	if err := (&OpenShellCustody{state: original}).validateLedger(); err != nil {
		return err
	}
	for _, r := range original.Records {
		if r.Retirement != nil || r.Stopped == nil || (!r.Retained && (!r.CreateAcknowledged || r.Worker.StopPending || !r.StopAcknowledged || r.StopRequestID == "" || (r.Exec != nil && !r.Exec.Result.ExitObserved) || (r.ProgressStop != nil && !r.ProgressStop.Result.ExitObserved))) {
			return fmt.Errorf("original native operation is unresolved: %s", r.Name)
		}
	}
	return nil
}
