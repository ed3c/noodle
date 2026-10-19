package admission

import (
	"errors"
	"os"
	"testing"
	"time"
)

func TestResidentDuplicateReleaseRetainsPersistenceFailure(t *testing.T) {
	r, budget, spec := residentFixture(t)
	resident, err := r.Acquire(spec, "a", budget)
	if err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	child := r.children[resident.ID]
	r.mu.Unlock()

	// Fail the original atomic rename without replacing its process owner.
	backup := r.path + ".before-failure"
	if err := os.Rename(r.path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(r.path, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Remove(r.path); err != nil {
			t.Error(err)
		}
		if err := os.Rename(backup, r.path); err != nil {
			t.Error(err)
		}
		if err := signalResident(&resident, child, false); err != nil && !errors.Is(err, os.ErrProcessDone) {
			t.Error(err)
		}
		select {
		case <-child.done:
		case <-time.After(3 * time.Second):
			t.Error("original fixture child exit not observed")
		}
	})

	_, firstErr := r.Release(resident.ID, "a")
	if firstErr == nil {
		t.Fatal("release accepted an unpersisted stop intent")
	}
	retry, retryErr := r.Release(resident.ID, "a")
	if retryErr == nil || !errors.Is(retryErr, firstErr) {
		t.Fatalf("duplicate release hid the original persistence failure: %v", retryErr)
	}
	if retry.State != "stopping" || budget.Snapshot().Allocations != 1 {
		t.Fatal("unconfirmed stop lost its state or allocation", retry)
	}
	select {
	case <-child.done:
		t.Fatal("unpersisted intent stopped the original child")
	default:
	}
}
