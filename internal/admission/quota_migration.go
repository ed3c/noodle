package admission

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/lockfile"
)

// MigrateEmptySharedQuota is called only by the maintenance owner after native
// retirement of every old writer. It preserves exact old bytes separately.
// A repeat reads the same result, never empties live leases.
func MigrateEmptySharedQuota(ctx context.Context, dir, custody string, original []byte) error {
	return migrateEmptySharedQuota(ctx, dir, custody, original, 1, "quota-schema1.json")
}

// MigrateEmptySourceQuotaV2 is the explicit Source maintenance continuation.
// The original controller must prove every old writer retired and settle only
// the selected retired resident allocation before this empty-ledger migration.
// It does not expand the original schema-1 entry's accepted inputs.
func MigrateEmptySourceQuotaV2(ctx context.Context, dir, custody string, original []byte) error {
	return migrateEmptySharedQuota(ctx, dir, custody, original, 2, "quota-source-schema2.json")
}

func migrateEmptySharedQuota(ctx context.Context, dir, custody string, original []byte, schema int, backupName string) error {
	var before struct {
		Schema    int                   `json:"schema"`
		Maximum   int                   `json:"maximum"`
		SwapLimit uint64                `json:"swap_used_limit_bytes"`
		Leases    map[string]quotaLease `json:"leases"`
	}
	if err := json.Unmarshal(original, &before); err != nil {
		return err
	}
	if before.Schema != schema || before.Maximum <= 0 || before.SwapLimit != 15_000_000_000 || before.Leases == nil || len(before.Leases) != 0 {
		if schema == 1 {
			return errors.New("migration requires an original empty schema-1 quota with the selected ceiling")
		}
		return errors.New("Source migration requires an original empty schema-2 quota with the selected ceiling")
	}
	next, err := json.Marshal(quotaState{Schema: sharedQuotaSchema, Policy: WorkerAdmissionPolicy,
		Maximum: DefaultWorkerLimit, Leases: map[string]quotaLease{}})
	if err != nil {
		return err
	}
	lock, err := lockfile.Acquire(ctx, filepath.Join(dir, "quota.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	path := filepath.Join(dir, "quota.json")
	actual, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	backup := filepath.Join(custody, backupName)
	intent := filepath.Join(custody, "quota-migration.json")
	if prior, readErr := os.ReadFile(intent); readErr == nil {
		var saved struct{ Before, After string }
		if json.Unmarshal(prior, &saved) != nil || saved.Before != quotaBytesDigest(original) || saved.After != quotaBytesDigest(next) {
			return errors.New("original quota migration intent changed")
		}
		old, backupErr := os.ReadFile(backup)
		if backupErr != nil || !bytes.Equal(old, original) {
			return errors.New("original quota backup changed")
		}
		if bytes.Equal(actual, next) {
			return nil
		}
		return errors.New("quota migration outcome unresolved; readback only, no rewrite")
	} else if !os.IsNotExist(readErr) {
		return readErr
	}
	if !bytes.Equal(actual, original) {
		return errors.New("original quota bytes changed; no migration performed")
	}
	if err = os.MkdirAll(custody, 0700); err != nil {
		return err
	}
	if old, readErr := os.ReadFile(backup); readErr == nil {
		if !bytes.Equal(old, original) {
			return errors.New("quota backup already belongs to another source")
		}
	} else if !os.IsNotExist(readErr) {
		return readErr
	} else if err = filex.WriteFileAtomicDurable(backup, original); err != nil {
		return err
	}
	raw, err := json.Marshal(struct{ Before, After string }{quotaBytesDigest(original), quotaBytesDigest(next)})
	if err != nil {
		return err
	}
	if err = filex.WriteFileAtomicDurable(intent, raw); err != nil {
		return err
	}
	return filex.WriteFileAtomicDurable(path, next)
}

func quotaBytesDigest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
