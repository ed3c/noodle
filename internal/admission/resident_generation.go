package admission

import (
	"errors"
	"os"
)

// ValidateResidentSpec uses the same identity validation as Acquire, without
// admission, persistence or spawn. Owners can validate before reserving control.
func ValidateResidentSpec(spec ResidentSpec) error {
	_, _, err := residentIdentity(spec, "")
	if err != nil {
		return err
	}
	dir, err := os.Stat(spec.Directory)
	if err != nil {
		return err
	}
	if !dir.IsDir() {
		return errors.New("resident directory is not a directory")
	}
	return nil
}

// ResidentGeneration is a local capability to one original child, not a PID
// receipt. It cannot be reconstructed from disk or native health responses.
type ResidentGeneration struct {
	residents *Residents
	record    *Resident
	child     *residentChild
}

func (r *Residents) OriginalGeneration(v Resident, guard *ResidentGuard) (*ResidentGeneration, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	original, child := r.records[v.ID], r.children[v.ID]
	if original == nil || child == nil || child.guard != guard || original.PID != v.PID || original.Started != v.Started || original.Fingerprint != v.Fingerprint || original.State != "running" {
		return nil, errors.New("resident original running child unavailable")
	}
	if r.failure != nil {
		return nil, r.failure
	}
	return &ResidentGeneration{r, original, child}, nil
}

func (g *ResidentGeneration) Running() error {
	r := g.residents
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failure != nil {
		return r.failure
	}
	if r.records[g.record.ID] != g.record || r.children[g.record.ID] != g.child || g.record.State != "running" {
		return errors.New("resident original running generation unavailable")
	}
	g.child.mu.Lock()
	defer g.child.mu.Unlock()
	if g.child.reaped {
		return errors.New("resident original child already reaped")
	}
	return nil
}

// CheckServiceClosed proves only local obligations absent. An exited launcher
// with an unresolved group, a lost Host handle, or failed persistence refuses.
// This says nothing about the service's remote effects.
func (r *Residents) CheckServiceClosed(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failure != nil {
		return r.failure
	}
	for id, v := range r.records {
		if v.Name == name && (v.State != "exited" || r.children[id] != nil || len(v.Contexts) != 0) {
			return errors.New("resident local child obligations unresolved")
		}
	}
	return nil
}

// Release cannot be redirected to a same-name successor between owner readback
// and the mutation. The existing last-lease guard remains the sole barrier.
func (g *ResidentGeneration) Release(contextID string) (Resident, error) {
	r := g.residents
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.records[g.record.ID] != g.record || (r.children[g.record.ID] != g.child && g.record.State != "exited") {
		return Resident{}, errors.New("resident original generation changed; release refused")
	}
	return r.release(g.record.ID, contextID)
}
