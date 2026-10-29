package loop

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	loopruntime "github.com/poteto/noodle/runtime"
)

func completedAcceptanceFixture(t *testing.T) (*Loop, completedTaskAcceptanceRequest) {
	t.Helper()
	l, cook, cmd, rt := openShellLocalTaskFixture(t, "codex")
	localTaskRealWorktree(t, l)
	cmd.Action = "merge-local-task"
	if err := l.dispatchControlCommand(cmd); err != nil {
		t.Fatal(err)
	}
	reader := &cleanupReadbackRuntime{mockRuntime: newMockRuntime()}
	reader.read = func(b loopruntime.CompletedSessionBinding, _ string, status loopruntime.SessionStatus) (loopruntime.CleanupSessionReadback, error) {
		canonical, err := filepath.EvalSymlinks(cook.worktreePath)
		// Cleanup canonicalizes the project; the review fixture also retains its
		// equivalent /var spelling for the initial native completion binding.
		if err != nil || b.WorktreePath != canonical {
			t.Fatalf("cleanup target differs: %v", err)
		}
		b.WorktreePath = cook.worktreePath
		execution, err := rt.read(b)
		return loopruntime.CleanupSessionReadback{OutcomeStatus: status, PublishedHead: execution.CandidateHead, Execution: execution}, err
	}
	l.deps.Runtimes["openshell"] = reader
	var merge localTaskMergeBinding
	if err := json.Unmarshal(l.canonical.Orders[cook.orderID].Stages[0].Extra[localTaskMergeKey], &merge); err != nil {
		t.Fatal(err)
	}
	evidence, err := loopruntime.ReadSessionReadbackFile(merge.Candidate.Path, nil)
	if err != nil {
		t.Fatal(err)
	}
	value := loopruntime.TaskDeliveryAcceptance{Schema: 1, OrderID: cook.orderID, SessionID: cook.session.ID(), CandidateHead: merge.Head, Status: "PASS", Conditions: []loopruntime.TaskDeliveryCondition{{Condition: "all original delivery requirements", Status: "PASS", Evidence: []loopruntime.SessionReadbackFileRef{evidence}}}}
	path := filepath.Join(l.runtimeDir, "outer-acceptance.json")
	recoveryWriteJSON(t, path, value)
	ref, err := loopruntime.ReadSessionReadbackFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	return l, completedTaskAcceptanceRequest{OrderID: cook.orderID, SessionID: cook.session.ID(), Acceptance: ref}
}

func TestCompletedMergeWaitsForOriginalOuterAcceptance(t *testing.T) {
	l, request := completedAcceptanceFixture(t)
	if err := l.checkCompletedTasks(); !errors.Is(err, ErrReleaseBusy) {
		t.Fatalf("merge alone released: %v", err)
	}
	if _, err := os.Stat(l.executionClosurePath(request.OrderID)); !os.IsNotExist(err) {
		t.Fatal("inspection manufactured acceptance")
	}
	if err := l.acceptCompletedTask(request); err != nil {
		t.Fatal(err)
	}
	if err := l.checkCompletedTasks(); err != nil {
		t.Fatal(err)
	}
	if err := l.dispatchControlCommand(ControlCommand{Action: "requeue", OrderID: request.OrderID}); err == nil {
		t.Fatal("accepted closed task requeued")
	}
	if err := l.acceptCompletedTask(request); err == nil {
		t.Fatal("duplicate acceptance replayed")
	}
	if err := os.WriteFile(request.Acceptance.Path, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := l.checkCompletedTasks(); err == nil {
		t.Fatal("changed acceptance remained eligible")
	}
}

func TestOuterAcceptanceRejectsUnresolvedOrForeignEvidence(t *testing.T) {
	for _, variant := range []string{"head", "session", "order", "unknown", "empty", "duplicate", "source", "policy"} {
		t.Run(variant, func(t *testing.T) {
			l, request := completedAcceptanceFixture(t)
			var value loopruntime.TaskDeliveryAcceptance
			if _, err := loopruntime.ReadSessionReadbackFile(request.Acceptance.Path, &value); err != nil {
				t.Fatal(err)
			}
			switch variant {
			case "head":
				value.CandidateHead = strings.Repeat("1", 40)
			case "session":
				value.SessionID = "another-session"
			case "order":
				value.OrderID = "another-order"
			case "unknown":
				value.Conditions[0].Status = "UNKNOWN"
			case "empty":
				value.Conditions = nil
			case "duplicate":
				value.Conditions = append(value.Conditions, value.Conditions[0])
			case "source":
				value.Conditions[0].Evidence[0].SHA256 = strings.Repeat("0", 64)
			case "policy":
				l.effectLedger = nil
			}
			recoveryWriteJSON(t, request.Acceptance.Path, value)
			ref, err := loopruntime.ReadSessionReadbackFile(request.Acceptance.Path, nil)
			if err != nil {
				t.Fatal(err)
			}
			request.Acceptance = ref
			if err := l.acceptCompletedTask(request); err == nil {
				t.Fatal("invalid outer acceptance accepted")
			}
			if _, err := os.Stat(l.executionClosurePath(request.OrderID)); !os.IsNotExist(err) {
				t.Fatal("refusal wrote closure")
			}
		})
	}
}
