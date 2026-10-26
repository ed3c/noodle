package admission

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ResidentGuard is a native owner's capability, never a JSON/shell check. Reuse
// this exact handle for shared acquisitions. Identity survives process restart;
// the callback and original child handle do not transfer to another owner.
type ResidentGuard struct {
	id      string
	timeout time.Duration
	check   func(context.Context) error
}

func NewResidentGuard(id string, timeout time.Duration, check func(context.Context) error) (*ResidentGuard, error) {
	if strings.TrimSpace(id) == "" || strings.ContainsRune(id, 0) || timeout <= 0 || timeout > 30*time.Second || check == nil {
		return nil, errors.New("resident guard identity, callback or bounded timeout invalid")
	}
	return &ResidentGuard{id: id, timeout: timeout, check: check}, nil
}

func guardedFingerprint(fingerprint, guardID string) string {
	sum := sha256.Sum256([]byte(fingerprint + "\x00" + guardID))
	return hex.EncodeToString(sum[:])
}

func (r *Residents) AcquireGuarded(spec ResidentSpec, contextID string, budget *Budget, guard *ResidentGuard) (Resident, error) {
	if guard == nil || guard.check == nil || guard.id == "" || guard.timeout <= 0 || guard.timeout > 30*time.Second {
		return Resident{}, errors.New("resident release guard missing")
	}
	return r.acquire(spec, contextID, budget, guard)
}

// Called with r.mu held and returns with it held. Never hold Residents.mu while
// calling the owner: custody takes its own mutex. Reentrant mutations refuse
// while checking; a callback must not wait for another Release on this service.
func (r *Residents) checkRelease(v *Resident) error {
	child := r.children[v.ID]
	if child == nil || child.guard == nil || child.guard.id != v.GuardID {
		return errors.New("resident original child release guard unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), child.guard.timeout)
	defer cancel()
	result := make(chan error, 1)
	child.checking = true
	r.mu.Unlock()
	go func() { result <- child.guard.check(ctx) }()
	var err error
	select {
	case err = <-result:
		err = errors.Join(err, ctx.Err())
		r.mu.Lock()
		child.checking = false
	case <-ctx.Done():
		err = ctx.Err()
		r.mu.Lock()
		// Even a callback ignoring cancellation cannot stall Host or create an
		// unbounded retry stream. Keep the original capability busy until return.
		go func() {
			<-result
			r.mu.Lock()
			child.checking = false
			r.mu.Unlock()
		}()
	}
	if r.children[v.ID] != child || v.State != "running" {
		return errors.Join(err, fmt.Errorf("resident original child changed during release guard"))
	}
	return errors.Join(err, r.failure)
}
