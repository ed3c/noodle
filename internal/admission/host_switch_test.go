package admission

import (
	"errors"
	"testing"
)

func TestHostSwitchHoldWithoutSharedProofBlocksNewAllocations(t *testing.T) {
	b, sample := testBudget(2)
	b.Register("a")
	b.Register("b")
	for i := 0; i < 4; i++ {
		sample(Normal, 0, nil)
	}
	p, err := b.TryAcquire("a")
	if err != nil {
		t.Fatal(err)
	}
	b.HoldForSwitch()
	b.HoldForSwitch()
	sample(Normal, 0, nil)
	if _, err = b.TryAcquire("b"); !errors.Is(err, ErrDeferred) {
		t.Fatal("new worker admitted")
	}
	if _, err = p.ReserveReplacement("a"); !errors.Is(err, ErrDeferred) {
		t.Fatal("replacement admitted")
	}
	if _, err = b.TryAcquireResident("b"); !errors.Is(err, ErrDeferred) {
		t.Fatal("resident admitted")
	}
	if got := b.Snapshot(); got.Allocations != 1 || got.Limit != 0 || got.Reason != "host_switch_draining" {
		t.Fatalf("hold: %+v", got)
	}
	p.Release()
	if b.Snapshot().Allocations != 0 {
		t.Fatal("allocation leaked")
	}
	sample(Normal, 0, nil)
	if _, err = b.TryAcquire("b"); !errors.Is(err, ErrDeferred) {
		t.Fatal("old generation resumed dispatch instead of retiring")
	}
}
