package admission

import (
	"errors"
	"path/filepath"

	"github.com/poteto/noodle/internal/procx"
)

// The Host supplies a journal inherited from a known pool generation. Returning
// this exact allocation changes accounting only; it never ends a consumer lease
// or signals a process. Native absence is checked again before the atomic write.
func (b *Budget) ReleaseRecoveredFixtureQuota(owner string, row FixtureReclamation) error {
	c := row.Candidate
	if owner == "" || row.State != "exited" || row.ExitAt == "" || !c.ConsumerManaged || c.Native == nil || c.Owner == nil || row.ID != fixtureKey(c) ||
		filepath.Dir(c.LogPath) != filepath.Dir(row.OriginJournal) || filepath.Base(filepath.Dir(row.OriginJournal)) != owner {
		return errors.New("original inherited consumer fixture and exit evidence required for quota return")
	}
	p, err := procx.ObserveNativeIdentity(c.Owner.PID)
	ownerGone := errors.Is(err, procx.ErrProcessGone) || (err == nil && (p.Zombie || p.StartSec != c.Owner.StartSec || p.StartUsec != c.Owner.StartUsec))
	if !ownerGone {
		return errors.New("original quota owner is live or unobserved")
	}
	if gone, err := fixtureGenerationGone(c, row.Evidence); err != nil || !gone {
		return errors.New("original fixture generation exit unconfirmed")
	}
	b.mu.Lock()
	q := b.shared
	b.mu.Unlock()
	if q == nil {
		return errors.New("original shared quota unavailable")
	}
	if q.owner == owner {
		return errors.New("live owner must return its own original permit")
	}
	return q.releaseRecoveredResident(owner, c.ResidentID)
}

func (q *sharedQuota) releaseRecoveredResident(owner, key string) error {
	return q.update(0, func(s *quotaState) error {
		for id, lease := range s.Leases {
			if lease.Owner == owner && lease.Resident && lease.ResidentKey == key {
				delete(s.Leases, id)
			}
		}
		return nil
	})
}
