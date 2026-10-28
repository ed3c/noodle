package loop

import (
	"context"
	"errors"
	"testing"

	"github.com/poteto/noodle/internal/state"
	loopruntime "github.com/poteto/noodle/runtime"
)

type maintainedRuntime struct {
	*savedRuntime
	read func(loopruntime.CompletedSessionBinding) (bool, error)
}

func (r *maintainedRuntime) ReadMaintainedProgress(_ context.Context, b loopruntime.CompletedSessionBinding) (bool, error) {
	return r.read(b)
}

func TestMaintainedProgressParksOnlyTheOriginalStoppedAttempt(t *testing.T) {
	for _, kind := range []string{"saved", "ordinary", "unknown"} {
		t.Run(kind, func(t *testing.T) {
			l, id, session, rt := savedProgressFixture(t)
			o := l.canonical.Orders[id]
			o.Stages[0].Status = state.StageRunning
			o.Stages[0].Attempts[0].Status = state.AttemptRunning
			l.canonical.Orders[id] = o
			delete(l.canonical.PendingReviews, id)
			delete(l.cooks.pendingReview, id)
			if err := l.persistCanonicalCheckpoint(); err != nil {
				t.Fatal(err)
			}
			calls := 0
			l.deps.Runtimes["openshell"] = &maintainedRuntime{savedRuntime: rt, read: func(b loopruntime.CompletedSessionBinding) (bool, error) {
				calls++
				if b.OrderID != id || b.SessionID != session || b.RetryCount != 0 {
					t.Fatalf("wrong original maintenance binding: %+v", b)
				}
				if kind == "unknown" {
					return false, errors.New("native stop unresolved")
				}
				return kind == "saved", nil
			}}
			before := recoveryRead(t, l.canonicalSnapshotPath())
			err := l.reconcileMaintainedProgress(t.Context())
			if (err != nil) != (kind == "unknown") {
				t.Fatalf("recovery result: %v", err)
			}
			if calls != 1 || len(rt.mockRuntime.calls) != 0 {
				t.Fatal("maintenance dispatched or skipped original readback")
			}
			if kind != "saved" {
				if string(before) != string(recoveryRead(t, l.canonicalSnapshotPath())) {
					t.Fatal("unverified maintenance changed canonical state")
				}
				return
			}
			after := l.canonical.Orders[id].Stages[0]
			if after.Status != state.StageReview || len(after.Attempts) != 1 || after.Attempts[0].SessionID != session || after.Attempts[0].Status != state.AttemptCancelled || l.canonical.PendingReviews[id].SessionID != session {
				t.Fatalf("maintenance rewrote original history: %+v", after)
			}
			if err := l.reconcileMaintainedProgress(t.Context()); err != nil {
				t.Fatal(err)
			}
			if calls != 1 || len(l.canonical.Orders[id].Stages[0].Attempts) != 1 {
				t.Fatal("maintenance completion replayed")
			}
		})
	}
}
