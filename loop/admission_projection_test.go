package loop

import (
	"github.com/poteto/noodle/internal/mode"
	"github.com/poteto/noodle/internal/projection"
	"github.com/poteto/noodle/internal/state"
	"testing"
)

// The actual producer removes completed A from orders.json while keeping A
// in canonical state. Initial admission must compare against that projection,
// but still refuse to admit A again.
func TestInitialAdmissionCompletedProjectionBoundary(t *testing.T) {
	tc := initialLoop(t)
	l := tc.loop
	initialProposal(t, l, ownerRevision(t, l), "A")
	promoteInitial(t, l)
	node := l.canonical.Orders["A"]
	node.Status = state.OrderCompleted
	node.Stages[0].Status = state.StageCompleted
	l.canonical.Orders["A"] = node
	if err := l.persistCanonicalCheckpoint(); err != nil {
		t.Fatal(err)
	}
	bundle, err := projection.Project(l.canonical, mode.ModeState{})
	if err != nil {
		t.Fatal(err)
	}
	if err := projection.WriteProjectionFiles(tc.runtimeDir, bundle); err != nil {
		t.Fatal(err)
	}
	if err := l.loadOrdersState(); err != nil {
		t.Fatal(err)
	}
	initialProposal(t, l, ownerRevision(t, l), "B")
	r, err := l.mergeOrdersNext()
	if err != nil || !r.Promoted {
		t.Fatalf("fresh B rejected after completed A: %v", err)
	}
	initialProposal(t, l, ownerRevision(t, l), "A")
	refusedInitial(t, tc, "order.id")
}
