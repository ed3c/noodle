package admission

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/lockfile"
)

// A new policy cannot adopt an old ledger while its original leases may be live.
const sharedQuotaSchema = 3

type quotaLease struct {
	Owner       string `json:"owner"`
	Context     string `json:"context"`
	Resident    bool   `json:"resident"`
	ResidentKey string `json:"resident_key,omitempty"`
}
type quotaState struct {
	Schema  int                   `json:"schema"`
	Maximum int                   `json:"maximum"`
	Policy  string                `json:"worker_admission_policy"`
	Leases  map[string]quotaLease `json:"leases"`
}
type sharedQuota struct {
	dir, owner string
}

type quotaWriteUncertain struct{ error }
type SharedQuotaSnapshot struct {
	Maximum     int    `json:"maximum"`
	Allocations int    `json:"allocations"`
	Residents   int    `json:"residents"`
	Owned       int    `json:"owned_allocations"`
	Error       string `json:"error,omitempty"`
}

// BindSharedQuota joins the original Host budget to one cross-generation ledger.
// A dead Host does not release its leases: worker exit still needs native proof.
func (b *Budget) BindSharedQuota(dir, owner string) error {
	if !filepath.IsAbs(dir) || owner == "" {
		return errors.New("shared quota requires an absolute custody path and native Host identity")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	q := &sharedQuota{dir: dir, owner: owner}
	if err := q.access(b.maximum, true, nil); err != nil {
		return err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.active != 0 || b.shared != nil {
		return errors.New("shared quota must bind before any allocation")
	}
	b.shared = q
	return nil
}

func (q *sharedQuota) update(maximum int, apply func(*quotaState) error) error {
	return q.access(maximum, true, apply)
}
func (q *sharedQuota) access(maximum int, write bool, apply func(*quotaState) error) error {
	_, priorLockErr := os.Stat(filepath.Join(q.dir, "quota.lock"))

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	lock, err := lockfile.Acquire(ctx, filepath.Join(q.dir, "quota.lock"))
	if err != nil {
		return fmt.Errorf("shared quota lock: %w", err)
	}
	defer lock.Close()
	path := filepath.Join(q.dir, "quota.json")
	raw, err := os.ReadFile(path)
	var state quotaState
	if err == nil {
		if err = json.Unmarshal(raw, &state); err != nil {
			return fmt.Errorf("shared quota read: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	} else if !write || maximum <= 0 || !os.IsNotExist(priorLockErr) {
		return errors.New("shared quota state missing; native owner readback required")
	} else {
		state = quotaState{Schema: sharedQuotaSchema, Maximum: maximum, Policy: WorkerAdmissionPolicy, Leases: map[string]quotaLease{}}
	}

	if state.Schema != sharedQuotaSchema || state.Leases == nil || state.Policy != WorkerAdmissionPolicy || state.Maximum <= 0 || state.Maximum > DefaultWorkerLimit {
		return errors.New("shared quota policy or schema differs")
	}
	if maximum > 0 {
		if state.Maximum != maximum {
			return errors.New("Host generations disagree on the Host worker ceiling")
		}
	}
	if apply != nil {
		if err = apply(&state); err != nil {
			return err
		}
	}
	// Binding only validates an existing ledger. Preserve its exact bytes and
	// opaque metadata; only a genuinely new ledger needs initialization.
	if !write || (apply == nil && raw != nil) {
		return nil
	}
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if err = filex.WriteFileAtomicDurable(path, data); err != nil {
		return &quotaWriteUncertain{err}
	}
	return nil
}

func (q *sharedQuota) acquire(context string, maximum int, resident bool, residentKey string, retiring bool) (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(nonce[:])
	err := q.update(maximum, func(s *quotaState) error {
		// The old round retains service-start priority until it finishes. Otherwise
		// a new service could strand the old worker behind a cross-Host lease.
		if resident && !retiring {
			for _, lease := range s.Leases {
				if !lease.Resident && lease.Owner != q.owner {
					return ErrDeferred
				}
			}
		}
		if resident && residentKey != "" {
			for _, lease := range s.Leases {
				if lease.Resident && lease.ResidentKey == residentKey {
					return ErrDeferred
				}
			}
		}
		workers := 0
		for _, lease := range s.Leases {
			if !lease.Resident {
				workers++
			}
		}
		if !resident && workers >= s.Maximum {
			return ErrDeferred
		}
		s.Leases[id] = quotaLease{Owner: q.owner, Context: context, Resident: resident, ResidentKey: residentKey}

		return nil
	})
	return id, err
}
func (q *sharedQuota) replacement(id string) error {
	return q.update(0, func(s *quotaState) error {
		lease, ok := s.Leases[id]
		if !ok || lease.Owner != q.owner {
			return errors.New("original shared allocation is missing")
		}
		return nil
	})
}
func (q *sharedQuota) release(id string) error {
	return q.update(0, func(s *quotaState) error {
		lease, ok := s.Leases[id]
		if !ok {
			return nil
		} // A validated ledger confirms this return already applied.
		if lease.Owner != q.owner {
			return errors.New("shared allocation ownership changed")
		}
		delete(s.Leases, id)
		return nil
	})
}
func (q *sharedQuota) workerContext(context string) bool {
	found := false
	err := q.access(0, false, func(s *quotaState) error {
		for _, lease := range s.Leases {
			if lease.Owner == q.owner && lease.Context == context && !lease.Resident {
				found = true
			}
		}
		return nil
	})
	return err == nil && found
}
func (q *sharedQuota) snapshot() SharedQuotaSnapshot {
	var result SharedQuotaSnapshot
	err := q.access(0, false, func(s *quotaState) error {
		result.Maximum = s.Maximum
		result.Allocations = len(s.Leases)
		for _, lease := range s.Leases {
			if lease.Owner == q.owner {
				result.Owned++
			}
			if lease.Resident {
				result.Residents++
			}
		}
		return nil
	})
	if err != nil {
		result.Error = err.Error()
	}
	return result
}
