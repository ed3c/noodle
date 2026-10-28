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

// Schema 2 binds inclusive Swap ceilings to one admission policy.
// Older Hosts reject it rather than sharing allocations under different rules.
const sharedQuotaSchema = 2

type quotaLease struct {
	Owner       string `json:"owner"`
	Context     string `json:"context"`
	Resident    bool   `json:"resident"`
	ResidentKey string `json:"resident_key,omitempty"`
	Exclusive   bool   `json:"exclusive,omitempty"`
}
type quotaState struct {
	Schema    int                   `json:"schema"`
	Maximum   int                   `json:"maximum"`
	SwapLimit uint64                `json:"swap_used_limit_bytes"`
	LastStart int64                 `json:"last_start_window"`
	Leases    map[string]quotaLease `json:"leases"`
}
type sharedQuota struct {
	dir, owner string
	swapLimit  uint64
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
	q := &sharedQuota{dir: dir, owner: owner, swapLimit: b.swapUsedLimit}
	if err := q.update(0, func(*quotaState) error { return nil }); err != nil {
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
	state := quotaState{Schema: sharedQuotaSchema, SwapLimit: q.swapLimit, LastStart: -1, Leases: map[string]quotaLease{}}
	if err == nil {
		if err = json.Unmarshal(raw, &state); err != nil {
			return fmt.Errorf("shared quota read: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	} else if !write || priorLockErr == nil {
		return errors.New("shared quota state missing; native owner readback required")
	}

	if state.Schema != sharedQuotaSchema || state.Leases == nil || state.SwapLimit != q.swapLimit || state.Maximum < 0 {
		return errors.New("shared quota policy or schema differs")
	}
	if maximum > 0 {
		if state.Maximum == 0 {
			state.Maximum = maximum
		} else if state.Maximum != maximum {
			return errors.New("Host generations disagree on the machine worker ceiling")
		}
	}
	if err = apply(&state); err != nil {
		return err
	}
	if !write {
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

func (q *sharedQuota) acquire(context string, maximum int, now time.Time, limited, resident bool, residentKey string, retiring bool) (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	id := hex.EncodeToString(nonce[:])
	err := q.update(maximum, func(s *quotaState) error {
		for _, lease := range s.Leases {
			if lease.Exclusive {
				return ErrDeferred
			}
		}
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
		window := now.UnixNano() / int64(PollInterval)
		if len(s.Leases) >= s.Maximum || (limited && window <= s.LastStart) {
			return ErrDeferred
		}
		s.Leases[id] = quotaLease{Owner: q.owner, Context: context, Resident: resident, ResidentKey: residentKey}
		if limited {
			s.LastStart = window
		}
		return nil
	})
	return id, err
}
func (q *sharedQuota) replacement(id string, now time.Time, limited bool) error {
	return q.update(0, func(s *quotaState) error {
		lease, ok := s.Leases[id]
		if !ok || lease.Owner != q.owner {
			return errors.New("original shared allocation is missing")
		}
		window := now.UnixNano() / int64(PollInterval)
		if limited && window <= s.LastStart {
			return ErrDeferred
		}
		if limited {
			s.LastStart = window
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
