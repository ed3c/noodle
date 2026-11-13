package admission

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"

	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/lockfile"
)

// SourceResidentQuotaReturn binds one original row, not native retirement or
// permission to change accounting. Only the original full maintenance owner
// may consume it after qualifying compute and every original physical actor.
type SourceResidentQuotaReturn struct {
	Owner, Context, ResidentKey, LeaseID string
	// Bytes survive the JSON plan carrier without RawMessage compaction.
	Lease []byte
}

func sourceQuotaDocument(raw []byte) (map[string]json.RawMessage, map[string]json.RawMessage, error) {
	var q struct {
		Schema    int                        `json:"schema"`
		Maximum   int                        `json:"maximum"`
		SwapLimit uint64                     `json:"swap_used_limit_bytes"`
		Leases    map[string]json.RawMessage `json:"leases"`
	}
	if err := json.Unmarshal(raw, &q); err != nil {
		return nil, nil, err
	}
	if q.Schema != 2 || q.Maximum <= 0 || q.SwapLimit != 15_000_000_000 || q.Leases == nil {
		return nil, nil, errors.New("Source quota return requires the original schema2 ledger and policy")
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(raw, &document); err != nil {
		return nil, nil, err
	}
	return document, q.Leases, nil
}

// PrepareSourceResidentQuotaReturn reads only. The nonce row key comes from the
// original quota, never from a caller-generated replacement allocation.
func PrepareSourceResidentQuotaReturn(original []byte, owner string, contexts []string, key string) (SourceResidentQuotaReturn, error) {
	var result SourceResidentQuotaReturn
	if owner == "" || key == "" || len(contexts) == 0 {
		return result, errors.New("original Source generation, contexts and resident key required")
	}
	_, leases, err := sourceQuotaDocument(original)
	if err != nil {
		return result, err
	}
	for id, raw := range leases {
		var row quotaLease
		if err = json.Unmarshal(raw, &row); err != nil {
			return result, err
		}
		if row.Owner != owner || !row.Resident || row.ResidentKey != key || row.Context == "" || !slices.Contains(contexts, row.Context) {
			continue
		}
		if id == "" || result.LeaseID != "" {
			return SourceResidentQuotaReturn{}, errors.New("original Source resident allocation is missing identity or ambiguous")
		}
		result = SourceResidentQuotaReturn{Owner: owner, Context: row.Context, ResidentKey: key, LeaseID: id, Lease: slices.Clone(raw)}
	}
	if result.LeaseID == "" {
		return result, errors.New("original Source resident allocation not found")
	}
	return result, nil
}

func sourceQuotaAfterReturn(original []byte, selected SourceResidentQuotaReturn) ([]byte, error) {
	document, leases, err := sourceQuotaDocument(original)
	if err != nil {
		return nil, err
	}
	derived, err := PrepareSourceResidentQuotaReturn(original, selected.Owner, []string{selected.Context}, selected.ResidentKey)
	if err != nil || derived.LeaseID != selected.LeaseID || !bytes.Equal(derived.Lease, selected.Lease) {
		return nil, errors.New("Source quota return differs from the original exact resident row")
	}
	delete(leases, selected.LeaseID)
	document["leases"], err = json.Marshal(leases)
	if err != nil {
		return nil, err
	}
	return json.Marshal(document)
}

// ReturnSourceResidentQuota changes only accounting under the existing quota
// lock. Its caller must already own the full maintenance epoch and prove the
// original writer, resident and driver retired with qualified compute. This
// does not perform that qualification or certify drain/business completion.
// Existing intent permits exact-after readback only, never another write.
func ReturnSourceResidentQuota(ctx context.Context, dir, epoch string, original []byte, selected SourceResidentQuotaReturn) error {
	if !filepath.IsAbs(dir) || !filepath.IsAbs(epoch) {
		return errors.New("Source quota and original maintenance epoch must be absolute")
	}
	next, err := sourceQuotaAfterReturn(original, selected)
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
	intent := filepath.Join(epoch, "quota-source-resident-return.json")
	backup := filepath.Join(epoch, "quota-source-resident-before.json")
	want, err := json.Marshal(struct {
		Directory, Before, After string
		Selected                 SourceResidentQuotaReturn
	}{filepath.Clean(dir), quotaBytesDigest(original), quotaBytesDigest(next), selected})
	if err != nil {
		return err
	}
	if prior, readErr := os.ReadFile(intent); readErr == nil {
		old, backupErr := os.ReadFile(backup)
		if !bytes.Equal(prior, want) || backupErr != nil || !bytes.Equal(old, original) {
			return errors.New("original Source quota return intent or before-image changed")
		}
		if bytes.Equal(actual, next) {
			return nil
		}
		return errors.New("Source quota return outcome unresolved; readback only, no rewrite")
	} else if !os.IsNotExist(readErr) {
		return readErr
	}
	if !bytes.Equal(actual, original) {
		return errors.New("original Source quota bytes changed; no allocation returned")
	}
	if err = os.MkdirAll(epoch, 0700); err != nil {
		return err
	}
	if old, readErr := os.ReadFile(backup); readErr == nil {
		if !bytes.Equal(old, original) {
			return errors.New("Source quota return backup belongs to another before-image")
		}
	} else if !os.IsNotExist(readErr) {
		return readErr
	} else if err = filex.WriteFileAtomicDurable(backup, original); err != nil {
		return err
	}
	if err = filex.WriteFileAtomicDurable(intent, want); err != nil {
		return err
	}
	return filex.WriteFileAtomicDurable(path, next)
}
