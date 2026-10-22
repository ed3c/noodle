package loop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/poteto/noodle/internal/admission"
	loopruntime "github.com/poteto/noodle/runtime"
)

type stopFailureSession struct {
	*mockSession
	err error
}

func (s *stopFailureSession) ForceKill() error { return s.err }

func TestStopAllPausesAndReleasesOnlyJoinedWorkers(t *testing.T) {
	l := newControlTestLoop(t, &fakeWorktree{}, newMockRuntime())
	b := resourceBudget(l, 2)
	b.Observe(admission.Sample{At: time.Now(), Unsupported: true}, nil)
	stopped := &mockSession{id: "stopped", done: make(chan struct{})}
	failed := &stopFailureSession{mockSession: &mockSession{id: "unresolved", done: make(chan struct{})}, err: errors.New("original process identity unavailable")}
	for _, session := range []loopruntime.SessionHandle{stopped, failed} {
		if err := l.withAdmission(context.Background(), "process", func(ctx context.Context) error {
			l.cooks.activeCooksByOrder[session.ID()] = &cookHandle{session: l.retainDispatched(ctx, session)}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = failed.mockSession.ForceKill() })
	err := l.dispatchControlCommand(ControlCommand{Action: "stop-all"})
	if !errors.Is(err, failed.err) {
		t.Errorf("stop failure reported success: %v", err)
	}
	if l.state != StatePaused {
		t.Errorf("stopped loop can still dispatch: %s", l.state)
	}
	select {
	case <-l.cooks.activeCooksByOrder[stopped.ID()].session.Done():
	case <-time.After(time.Second):
		t.Fatal("stopped worker did not join")
	}
	if got := b.Snapshot().Active; got != 1 {
		t.Fatalf("active workers = %d, want only unresolved worker retained", got)
	}
	if len(l.cooks.pendingReview) != 1 || len(l.cooks.activeCooksByOrder) != 2 {
		t.Fatal("stop erased original recovery or review custody")
	}
	if err := failed.mockSession.ForceKill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-l.cooks.activeCooksByOrder[failed.ID()].session.Done():
	case <-time.After(time.Second):
		t.Fatal("recovered worker did not join")
	}
	if got := b.Snapshot().Active; got != 0 {
		t.Fatalf("joined workers retained %d resource permits", got)
	}
}

func TestStopAllWithoutWorkersStillPauses(t *testing.T) {
	l := newControlTestLoop(t, &fakeWorktree{}, newMockRuntime())
	if err := l.dispatchControlCommand(ControlCommand{Action: "stop-all"}); err != nil {
		t.Fatal(err)
	}
	if l.state != StatePaused || len(l.cooks.pendingReview) != 1 {
		t.Fatal("stop-all must prevent pending work from starting and retain review")
	}
	if _, _, dispatch, _, err := l.buildCycleBrief(context.Background()); err != nil || dispatch {
		t.Fatalf("stopped loop still admits a dispatch cycle: %v", err)
	}
	if err := l.dispatchControlCommand(ControlCommand{Action: "resume"}); err != nil || l.state != StateRunning {
		t.Fatalf("explicit resume failed: %v", err)
	}
	if _, _, dispatch, _, err := l.buildCycleBrief(context.Background()); err != nil || !dispatch {
		t.Fatalf("explicit resume did not restore dispatch: %v", err)
	}
}

func TestCancelledAdmittedLocalTaskRetainsDirtyWorktreeForRecovery(t *testing.T) {
	l, cook, _ := localTaskReviewFixture(t)
	localTaskRealWorktree(t, l)
	path := filepath.Join(cook.worktreePath, "candidate.txt")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.handleCompletion(context.Background(), cook, StageResultCancelled, "cancelled"); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("interrupted task lost candidate bytes: %v", err)
	}
	if _, ok := l.cooks.pendingReview[cook.orderID]; !ok {
		t.Fatal("interrupted task lost native recovery custody")
	}
}
