package loop

import (
	"context"
	"errors"
	"testing"
)

type delayedOpenShellStop struct {
	*mockSession
	requests int
	stopErr  error
}

func (s *delayedOpenShellStop) ForceKill() error { s.requests++; return s.stopErr }

func TestOpenShellStopWaitsForOriginalSessionBeforeCleanup(t *testing.T) {
	for _, action := range []string{"stop", "kill", "stop-all"} {
		t.Run(action, func(t *testing.T) {
			wt := &fakeWorktree{}
			l := newControlTestLoop(t, wt, newMockRuntime())
			session := &delayedOpenShellStop{mockSession: &mockSession{id: "vm-worker", status: "running", done: make(chan struct{})}}
			cook := &cookHandle{cookIdentity: cookIdentity{orderID: "42", stageIndex: 0, stage: Stage{TaskKey: "execute", Skill: "execute", Provider: "codex", Runtime: "openshell"}}, session: session, dispatchedRuntime: "openshell", worktreeName: "42-0-execute", generation: l.nextDispatchGeneration()}
			if l.retainTaskData(cook.orderID) {
				t.Fatal("fixture is not an ordinary task")
			}
			budget := resourceBudget(l, 1)
			healthyResourceBudget(budget)
			if err := l.withAdmission(context.Background(), "openshell", cook.orderID, func(ctx context.Context) error { cook.session = l.retainDispatched(ctx, session); return nil }); err != nil {
				t.Fatal(err)
			}
			l.cooks.activeCooksByOrder[cook.orderID] = cook
			l.trackCookStarted(cook)
			l.startSessionWatcher(context.Background(), cook, false)
			t.Cleanup(func() { session.complete("cancelled"); l.watcherWG.Wait() })
			if err := l.dispatchControlCommand(ControlCommand{Action: action, Name: session.ID()}); err != nil {
				t.Fatal(err)
			}
			if session.requests != 1 || l.cooks.activeCooksByOrder[cook.orderID] != cook || len(wt.cleaned) != 0 || l.watcherCount.Load() != 1 || budget.Snapshot().Active != 1 {
				t.Fatal("stop request discarded the original worker before native termination")
			}
			if err := l.drainCompletions(context.Background()); err != nil {
				t.Fatal(err)
			}
			if l.cooks.activeCooksByOrder[cook.orderID] != cook || len(wt.cleaned) != 0 {
				t.Fatal("unconfirmed stop produced completion")
			}
			if action != "kill" && l.state != StatePaused {
				t.Fatal("stop did not hold subsequent dispatch")
			}
			session.complete("cancelled")
			l.watcherWG.Wait()
			if err := l.drainCompletions(context.Background()); err != nil {
				t.Fatal(err)
			}
			if l.cooks.activeCooksByOrder[cook.orderID] != nil || len(wt.cleaned) != 1 || wt.cleaned[0] != cook.worktreeName || budget.Snapshot().Active != 0 {
				t.Fatal("original native completion did not finish interruption handling")
			}
			if action != "kill" && l.state != StatePaused {
				t.Fatal("native termination resumed dispatch")
			}
		})
	}
}

func TestOpenShellStopFailureRetainsHoldAndOriginalWorker(t *testing.T) {
	wt := &fakeWorktree{}
	l := newControlTestLoop(t, wt, newMockRuntime())
	session := &delayedOpenShellStop{mockSession: &mockSession{id: "vm-worker", done: make(chan struct{})}, stopErr: errors.New("original native stop acknowledgement unknown")}
	cook := &cookHandle{cookIdentity: cookIdentity{orderID: "42"}, session: session, dispatchedRuntime: "openshell", worktreeName: "42-0-execute"}
	l.cooks.activeCooksByOrder[cook.orderID] = cook
	ack := recoveryControl(t, l, ControlCommand{ID: "explicit-vm-stop", Action: "stop", Name: session.ID()})
	if ack.Status != "error" || ack.DispatchState != StatePaused || l.cooks.activeCooksByOrder[cook.orderID] != cook || len(wt.cleaned) != 0 {
		t.Fatalf("failed stop discarded custody or hold: %+v", ack)
	}
	restarted := New(l.projectDir, "noodle", l.config, l.deps)
	if err := restarted.hydrateProcessedCommands(); err != nil {
		t.Fatal(err)
	}
	if restarted.state != StatePaused || session.requests != 1 {
		t.Fatal("restart replayed an uncertain stop or resumed dispatch")
	}
}
