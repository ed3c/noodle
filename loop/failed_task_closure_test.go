package loop

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/poteto/noodle/internal/state"
	loopruntime "github.com/poteto/noodle/runtime"
)

func failedClosureFixture(t *testing.T) (*Loop, *cookHandle, failedTaskClosureRequest) {
	t.Helper()
	l, cook, _ := localTaskReviewFixtureRoute(t, "candidate.txt", "candidate.txt", "openshell", "codex")
	order := l.canonical.Orders[cook.orderID]
	order.Stages[0].Attempts[0].Status = state.AttemptFailed
	l.canonical.Orders[order.OrderID] = order
	source := filepath.Join(l.runtimeDir, "sessions", cook.session.ID(), "diagnostics.json")
	recoveryJSON(t, source, map[string]any{"reason": "diagnosed unrecoverable result", "preserved": true})
	ref, err := loopruntime.ReadSessionReadbackFile(source, nil)
	if err != nil {
		t.Fatal(err)
	}
	reader := &cleanupReadbackRuntime{mockRuntime: newMockRuntime()}
	reader.read = func(b loopruntime.CompletedSessionBinding, project string, status loopruntime.SessionStatus) (loopruntime.CleanupSessionReadback, error) {
		return loopruntime.CleanupSessionReadback{OutcomeStatus: status, PublishedHead: "published-progress", Execution: loopruntime.CompletedSessionReadback{NativeExecExitCode: 0, NativePhase: "Stopped", StopAcknowledged: true, Sources: map[string]loopruntime.SessionReadbackFileRef{"original": ref}}}, nil
	}
	l.deps.Runtimes = map[string]loopruntime.Runtime{"openshell": reader}
	return l, cook, failedTaskClosureRequest{OrderID: cook.orderID, SessionID: cook.session.ID(), Reason: "original flow explicitly abandons recovery after diagnosis", Diagnostics: ref}
}

func TestOriginalFailureClosureDoesNotPromoteFailureToSuccess(t *testing.T) {
	l, cook, request := failedClosureFixture(t)
	if err := l.checkCompletedTasks(t.Context()); err == nil {
		t.Fatal("unclosed failure was eligible")
	}
	if err := l.closeFailedTask(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	order := l.canonical.Orders[cook.orderID]
	if order.Status != state.OrderFailed || order.Stages[0].Status != state.StageFailed || order.Stages[0].Attempts[0].Status != state.AttemptFailed {
		t.Fatal("closure changed failed outcome")
	}
	if _, exists := l.canonical.PendingReviews[cook.orderID]; exists {
		t.Fatal("closed failure still resumable from review")
	}
	if _, err := l.failedExecutionClosure(order); err != nil {
		t.Fatal(err)
	}
	if err := l.checkCompletedTasks(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := l.dispatchControlCommand(ControlCommand{Action: "requeue", OrderID: cook.orderID}); err == nil {
		t.Fatal("closed failure requeued")
	}
}

func TestFailureClosureRefusesUnknownOrChangedEvidence(t *testing.T) {
	for _, variant := range []string{"reason", "diagnostic", "wrong-session", "still-running", "unknown-native", "retained-policy"} {
		t.Run(variant, func(t *testing.T) {
			l, cook, request := failedClosureFixture(t)
			switch variant {
			case "reason":
				request.Reason = ""
			case "diagnostic":
				request.Diagnostics.SHA256 = "changed"
			case "wrong-session":
				request.SessionID = "another-session"
			case "still-running":
				order := l.canonical.Orders[cook.orderID]
				order.Stages[0].Attempts[0].Status = state.AttemptRunning
				l.canonical.Orders[order.OrderID] = order
			case "unknown-native":
				l.deps.Runtimes["openshell"].(*cleanupReadbackRuntime).read = func(loopruntime.CompletedSessionBinding, string, loopruntime.SessionStatus) (loopruntime.CleanupSessionReadback, error) {
					return loopruntime.CleanupSessionReadback{}, errors.New("native stop unknown")
				}
			case "retained-policy":
				l.effectLedger = nil
			}
			if err := l.closeFailedTask(t.Context(), request); err == nil {
				t.Fatal("closure without original settled evidence accepted")
			}
			if _, err := os.Lstat(l.executionClosurePath(cook.orderID)); !os.IsNotExist(err) {
				t.Fatal("refused closure saved authority")
			}
		})
	}
}

func TestFailureClosureDetectsLaterMutation(t *testing.T) {
	l, cook, request := failedClosureFixture(t)
	if err := l.closeFailedTask(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	order := l.canonical.Orders[cook.orderID]
	order.Stages[0].Attempts[0].SessionID = "changed"
	if _, err := l.failedExecutionClosure(order); err == nil {
		t.Fatal("closure accepted changed original attempt")
	}
	if err := os.WriteFile(request.Diagnostics.Path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := l.failedExecutionClosure(l.canonical.Orders[cook.orderID]); err == nil {
		t.Fatal("closure accepted changed diagnosis")
	}
}
