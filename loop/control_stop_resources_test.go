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

func TestNativeStopUsesOriginalInterruptAndWaitsForActualDone(t *testing.T) {
	for _, action := range []string{"stop", "stop-all", "kill"} {
		t.Run(action, func(t *testing.T) {
			l := newControlTestLoop(t, &fakeWorktree{}, newMockRuntime())
			b := resourceBudget(l, 1)
			healthyResourceBudget(b)
			raw := newShutdownTestSession("original-native")
			var handle loopruntime.SessionHandle
			if err := l.withAdmission(context.Background(), "codex-app-server", "", func(ctx context.Context) error { handle = l.retainDispatched(ctx, raw); return nil }); err != nil {
				t.Fatal(err)
			}
			l.cooks.activeCooksByOrder["original"] = &cookHandle{session: handle, dispatchedRuntime: "codex-app-server", worktreeName: "original-worktree"}
			if err := l.dispatchControlCommand(ControlCommand{Action: action, Name: "original-native"}); err != nil {
				t.Fatal(err)
			}
			if raw.terminateCalls.Load() != 1 || raw.forceKillCalls.Load() != 0 || b.Snapshot().Allocations != 1 {
				t.Fatal("native interrupt ACK released the slot or sent an OS force-stop")
			}
			close(raw.done)
			select {
			case <-handle.Done():
			case <-time.After(time.Second):
				t.Fatal("native completion was not joined")
			}
			if b.Snapshot().Allocations != 0 {
				t.Fatal("native Done did not return original quota")
			}
		})
	}
}

func (s *stopFailureSession) ForceKill() error { return s.err }

func TestStopAllPausesAndReleasesOnlyJoinedWorkers(t *testing.T) {
	l := newControlTestLoop(t, &fakeWorktree{}, newMockRuntime())
	b := resourceBudget(l, 2)
	healthyResourceBudget(b)
	stopped := &mockSession{id: "stopped", done: make(chan struct{})}
	failed := &stopFailureSession{mockSession: &mockSession{id: "unresolved", done: make(chan struct{})}, err: errors.New("original process identity unavailable")}
	for _, session := range []loopruntime.SessionHandle{stopped, failed} {
		b.Observe(admission.Sample{At: time.Now(), Pressure: admission.Normal}, nil)
		if err := l.withAdmission(context.Background(), "process", "", func(ctx context.Context) error {
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

func TestPauseKeepsCurrentWorkerAndReleasesAcrossInnerRounds(t *testing.T) {
	l := newControlTestLoop(t, &fakeWorktree{}, newMockRuntime())
	b := resourceBudget(l, 1)
	healthyResourceBudget(b)
	for _, id := range []string{"round-one", "round-two", "round-three"} {
		b.Observe(admission.Sample{At: time.Now(), Pressure: admission.Normal}, nil)
		session := &mockSession{id: id, status: "running", done: make(chan struct{})}
		if err := l.withAdmission(context.Background(), "process", "", func(ctx context.Context) error {
			l.cooks.activeCooksByOrder[id] = &cookHandle{session: l.retainDispatched(ctx, session)}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if err := l.dispatchControlCommand(ControlCommand{Action: "pause"}); err != nil {
			t.Fatal(err)
		}
		if session.Status() != "running" || b.Snapshot().Active != 1 {
			t.Fatal("pause stopped or prematurely released the current worker")
		}
		for _, mode := range []string{"manual", "supervised"} {
			if err := l.dispatchControlCommand(ControlCommand{Action: "mode", Value: mode}); err != nil {
				t.Fatal(err)
			}
			if _, _, dispatch, _, err := l.buildCycleBrief(context.Background()); err != nil || dispatch {
				t.Fatalf("outer mode change resumed paused dispatch: %v", err)
			}
		}
		session.complete("completed")
		select {
		case <-l.cooks.activeCooksByOrder[id].session.Done():
		case <-time.After(time.Second):
			t.Fatal("completed worker did not join")
		}
		delete(l.cooks.activeCooksByOrder, id)
		if b.Snapshot().Active != 0 || l.state != StatePaused || len(l.cooks.pendingReview) != 1 {
			t.Fatal("round completion leaked permits, resumed dispatch or lost outer review")
		}
		if err := l.dispatchControlCommand(ControlCommand{Action: "resume"}); err != nil {
			t.Fatal(err)
		}
		if _, _, dispatch, _, err := l.buildCycleBrief(context.Background()); err != nil || !dispatch {
			t.Fatalf("explicit resume did not enable next round: %v", err)
		}
	}
}

func TestDispatchHoldSurvivesRestartWithoutReplayingControls(t *testing.T) {
	l := newControlTestLoop(t, &fakeWorktree{}, newMockRuntime())
	for _, tc := range []struct {
		command ControlCommand
		want    State
	}{
		{ControlCommand{ID: "paused", Action: "pause"}, StatePaused},
		{ControlCommand{ID: "outer-release", Action: "mode", Value: "supervised"}, StatePaused},
		{ControlCommand{ID: "continued", Action: "resume"}, StateRunning},
		{ControlCommand{ID: "paused", Action: "pause"}, StateRunning},
		{ControlCommand{ID: "drained", Action: "drain"}, StateDraining},
		{ControlCommand{ID: "stopped", Action: "stop-all"}, StatePaused},
		{ControlCommand{ID: "resumed-again", Action: "resume"}, StateRunning},
		{ControlCommand{ID: "normalized-pause", Action: " PAUSE "}, StatePaused},
	} {
		if ack := recoveryControl(t, l, tc.command); ack.Status != "ok" {
			t.Fatal(ack)
		}
		l = New(l.projectDir, "noodle", l.config, l.deps)
		if err := l.hydrateProcessedCommands(); err != nil {
			t.Fatal(err)
		}
		if l.state != tc.want {
			t.Fatalf("after %s restart state = %s, want %s", tc.command.ID, l.state, tc.want)
		}
		if len(l.deps.Runtimes["process"].(*mockRuntime).calls) != 0 {
			t.Fatal("control hydration dispatched a worker")
		}
	}
}

func TestFailedStopAllRetainsDispatchHoldAfterRestart(t *testing.T) {
	l := newControlTestLoop(t, &fakeWorktree{}, newMockRuntime())
	session := &stopFailureSession{mockSession: &mockSession{id: "unresolved", done: make(chan struct{})}, err: errors.New("original process identity unavailable")}
	l.cooks.activeCooksByOrder[session.ID()] = &cookHandle{session: session}
	if ack := recoveryControl(t, l, ControlCommand{ID: "partial-stop", Action: "stop-all"}); ack.Status != "error" {
		t.Fatal("partial stop did not preserve failure")
	}
	l = New(l.projectDir, "noodle", l.config, l.deps)
	if err := l.hydrateProcessedCommands(); err != nil {
		t.Fatal(err)
	}
	if l.state != StatePaused {
		t.Fatal("restart resumed dispatch after a partial stop failure")
	}
	select {
	case <-session.Done():
		t.Fatal("hydration claimed the unresolved worker exited")
	default:
	}
}

func TestSingleBoundStopHoldSurvivesRestartEvenWhenKillFails(t *testing.T) {
	for _, fail := range []bool{false, true} {
		l := newControlTestLoop(t, &fakeWorktree{}, newMockRuntime())
		l.config.Runtime.AdmittedOrders = []string{"bound-task"}
		session := &stopFailureSession{mockSession: &mockSession{id: "worker", done: make(chan struct{})}}
		if fail {
			session.err = errors.New("exit not confirmed")
		}
		l.cooks.activeCooksByOrder["bound-task"] = &cookHandle{cookIdentity: cookIdentity{orderID: "bound-task"}, session: session, worktreeName: "bound-worktree"}
		ack := recoveryControl(t, l, ControlCommand{ID: "stop-one", Action: "stop", Name: "worker"})
		if ack.DispatchState != StatePaused {
			t.Fatalf("stop lost actual hold: %+v", ack)
		}
		restarted := New(l.projectDir, "noodle", l.config, l.deps)
		if err := restarted.hydrateProcessedCommands(); err != nil {
			t.Fatal(err)
		}
		if restarted.state != StatePaused {
			t.Fatal("single stop resumed dispatch on restart")
		}
		if ack := recoveryControl(t, restarted, ControlCommand{ID: "continue", Action: "resume"}); ack.Status != "ok" {
			t.Fatal(ack)
		}
		recoveryControl(t, restarted, ControlCommand{ID: "stop-one", Action: "stop", Name: "worker"})
		final := New(l.projectDir, "noodle", l.config, l.deps)
		if err := final.hydrateProcessedCommands(); err != nil {
			t.Fatal(err)
		}
		if final.state != StateRunning {
			t.Fatal("duplicate stop overrode later resume")
		}
	}
}
