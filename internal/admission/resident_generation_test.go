package admission

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestResidentGenerationCapability(t *testing.T) {
	r, err := OpenResidents(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	original := &Resident{ID: "gateway", Name: "gateway", State: "running", PID: 123, Started: "original", Fingerprint: "bound", GuardID: "guard", Contexts: []string{"a"}}
	r.records[original.ID] = original
	guard, err := NewResidentGuard("guard", time.Second, func(_ context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.OriginalGeneration(*original, guard); err == nil {
		t.Fatal("disk record supplied original child capability")
	}
	child := &residentChild{guard: guard}
	r.children[original.ID] = child
	generation, err := r.OriginalGeneration(*original, guard)
	if err != nil {
		t.Fatal(err)
	}
	if err := generation.Running(); err != nil {
		t.Fatal(err)
	}
	if err := r.CheckServiceClosed("gateway"); err == nil {
		t.Fatal("live child obligation discarded")
	}
	wrongGuard, err := NewResidentGuard("guard", time.Second, func(_ context.Context) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.OriginalGeneration(*original, wrongGuard); err == nil {
		t.Fatal("same guard identity replaced original capability")
	}
	child.reaped = true
	if err := generation.Running(); err == nil {
		t.Fatal("reaped child reported running")
	}
	child.reaped = false
	replacement := *original
	r.records[original.ID] = &replacement // same PID/config still cannot transfer capability
	if err := generation.Running(); err == nil {
		t.Fatal("same PID successor adopted")
	}
	if _, err := generation.Release("a"); err == nil {
		t.Fatal("old capability released successor")
	}
	r.records[original.ID] = original
	original.State = "needs_owner_readback"
	if err := generation.Running(); err == nil {
		t.Fatal("unresolved generation reported running")
	}
	if err := r.CheckServiceClosed("gateway"); err == nil {
		t.Fatal("unresolved local obligations discarded")
	}
	delete(r.children, original.ID)
	original.State = "exited"
	original.Contexts = nil
	if err := r.CheckServiceClosed("gateway"); err != nil {
		t.Fatal(err)
	}
	r.failure = errors.New("persist failed")
	if err := r.CheckServiceClosed("gateway"); err == nil {
		t.Fatal("failed persistence allowed close")
	}
}
