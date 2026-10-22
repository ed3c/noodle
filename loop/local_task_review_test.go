package loop

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/poteto/noodle/internal/state"
)

func localTaskReviewFixture(t *testing.T) (*Loop, *cookHandle, ControlCommand) {
	t.Helper()
	l, _, cook := newTypedOutcomeTestLoop(t)
	l.config.Agents.Codex.RequireTypedOutcome = false
	l.config.Runtime.Process.AdmittedOrders = []string{cook.orderID}
	cook.stage = Stage{TaskKey: "local-task", Skill: "local-task", Provider: "codex", Model: "gpt-5.4", Runtime: "process", Prompt: `{"kind":"local_task","goal":"fix candidate","write_paths":["candidate.txt"]}`}
	cook.worktreeName = cookBaseName(cook.orderID, 0, "local-task")
	cook.worktreePath = l.worktreePath(cook.worktreeName)
	if err := writeOrdersAtomic(l.deps.OrdersFile, OrdersFile{Orders: []Order{{ID: cook.orderID, Status: OrderStatusActive, Stages: []Stage{cook.stage}}}}); err != nil {
		t.Fatal(err)
	}
	runGitInRepo(t, l.projectDir, "init", "-b", "control")
	runGitInRepo(t, l.projectDir, "config", "user.email", "test@noodle.dev")
	runGitInRepo(t, l.projectDir, "config", "user.name", "Noodle Test")
	requestChangesWrite(t, filepath.Join(l.projectDir, ".gitignore"), []byte(".noodle/\n.worktrees/\n"))
	runGitInRepo(t, l.projectDir, "add", ".gitignore")
	runGitInRepo(t, l.projectDir, "commit", "-m", "base")
	runGitInRepo(t, l.projectDir, "worktree", "add", "-b", cook.worktreeName, cook.worktreePath)
	requestChangesWrite(t, filepath.Join(cook.worktreePath, "candidate.txt"), []byte("reviewed candidate\n"))
	dir := filepath.Join(l.runtimeDir, "sessions", cook.session.ID())
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"events.ndjson": "", "prompt.txt": cook.stage.Prompt} {
		requestChangesWrite(t, filepath.Join(dir, name), []byte(data))
	}
	recoveryWriteJSON(t, filepath.Join(dir, "spawn.json"), map[string]any{"session_id": cook.session.ID(), "worktree_path": cook.worktreePath, "retry_count": 0, "skill": "local-task", "provider": "codex", "model": "gpt-5.4", "runtime": "process"})
	recoveryWriteJSON(t, filepath.Join(dir, "meta.json"), map[string]any{"session_id": cook.session.ID(), "status": "exited", "alive": false, "provider": "codex", "model": "gpt-5.4", "runtime": "process"})
	recoveryWriteJSON(t, filepath.Join(dir, "process.json"), map[string]any{"session_id": cook.session.ID(), "pid": 99999999})
	if err := l.parkPendingReview(cook, "supervised mode requires merge approval"); err != nil {
		t.Fatal(err)
	}
	if err := l.recordInitialAdmissions([]string{cook.orderID}); err != nil {
		t.Fatal(err)
	}
	b, err := l.captureRequestChanges(l.cooks.pendingReview[cook.orderID])
	if err != nil {
		t.Fatal(err)
	}
	index, err := boundGitCommand(l.deps.Execution, cook.worktreePath, "-C", cook.worktreePath, "ls-files", "--stage", "-z").Output()
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(t.TempDir(), "candidate.json")
	recoveryWriteJSON(t, source, map[string]any{"schema": 1, "order_id": cook.orderID, "session_id": b.SessionID, "attempt_id": b.AttemptID, "worktree": b.WorktreePath, "head": b.Head, "spawn_sha256": b.Files["spawn.json"], "index_sha256": recoveryDigest(index), "files": map[string]any{"candidate.txt": map[string]any{"sha256": recoveryDigest([]byte("reviewed candidate\n")), "executable": false}}})
	return l, cook, ControlCommand{ID: "recover-local", Action: "recover-local-task-review", OrderID: cook.orderID, Value: source, Target: recoveryDigest(recoveryRead(t, source)), Name: "original-review"}
}

func TestLocalTaskRequestChangesPreservesDirtyCandidateAndRequeues(t *testing.T) {
	l, cook, _ := localTaskReviewFixture(t)
	if ack := recoveryControl(t, l, ControlCommand{ID: "original-review", Action: "request-changes", OrderID: cook.orderID, Prompt: "correct result"}); ack.Status != "ok" {
		t.Fatalf("request changes: %+v", ack)
	}
	if _, err := l.requestChangesReview(cook.orderID); err != nil {
		t.Fatal(err)
	}
	l = New(l.projectDir, "noodle", l.config, l.deps)
	if err := l.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := l.controlPause(); err != nil {
		t.Fatal(err)
	}
	if err := l.controlEditItem(ControlCommand{OrderID: cook.orderID, Prompt: `{"kind":"local_task","goal":"fix candidate","write_paths":["candidate.txt"],"correction":"correct result"}`}); err != nil {
		t.Fatal(err)
	}
	order := l.canonical.Orders[cook.orderID]
	priorMerge := json.RawMessage(`{"head":"previous-reviewed-head","tree":"previous-reviewed-tree"}`)
	order.Stages[0].Extra[localTaskMergeKey] = priorMerge
	l.canonical.Orders[cook.orderID] = order
	if err := l.controlRequeue(cook.orderID); err != nil {
		t.Fatal(err)
	}
	stage := l.canonical.Orders[cook.orderID].Stages[0]
	if stage.Status != state.StagePending || len(stage.Attempts) != 1 || stage.Attempts[0].Status != state.AttemptFailed {
		t.Fatalf("history changed: %+v", stage)
	}
	if _, exists := stage.Extra[localTaskMergeKey]; exists {
		t.Fatal("requeued writer retained stale merge authority")
	}
	var requeued requestChangesRequeued
	if err := json.Unmarshal(stage.Extra[requestChangesRequeuedKey], &requeued); err != nil || !recoveryJSONEqual(requeued.Merge, priorMerge) {
		t.Fatal("requeue lost previous merge evidence")
	}
	if string(recoveryRead(t, filepath.Join(cook.worktreePath, "candidate.txt"))) != "reviewed candidate\n" {
		t.Fatal("candidate lost")
	}
	if err := l.controlMode("supervised"); err != nil {
		t.Fatal(err)
	}
	if _, _, dispatch, _, err := l.buildCycleBrief(context.Background()); err != nil || dispatch || l.state != StatePaused {
		t.Fatalf("outer review resumed a paused revision: %v", err)
	}
}

func oldLocalTaskFailure(t *testing.T, l *Loop, cook *cookHandle) {
	t.Helper()
	if ack := recoveryControl(t, l, ControlCommand{ID: "original-review", Action: "request-changes", OrderID: cook.orderID, Prompt: "correct result"}); ack.Status != "ok" {
		t.Fatal(ack)
	}
	order := l.canonical.Orders[cook.orderID]
	delete(order.Stages[0].Extra, requestChangesKey)
	l.canonical.Orders[cook.orderID] = order
	delete(l.canonical.PendingReviews, cook.orderID)
	if err := l.persistCanonicalCheckpoint(); err != nil {
		t.Fatal(err)
	}
	if err := l.syncPendingReviewProjection(); err != nil {
		t.Fatal(err)
	}
}

func TestLocalTaskRecoveryUsesOriginalACKCandidateAndFailureHistory(t *testing.T) {
	l, cook, cmd := localTaskReviewFixture(t)
	oldLocalTaskFailure(t, l, cook)
	before := l.canonical.Orders[cook.orderID].Stages[0].Attempts[0]
	l = New(l.projectDir, "noodle", l.config, l.deps)
	if err := l.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := l.dispatchControlCommand(cmd); err != nil {
		t.Fatal(err)
	}
	if err := l.dispatchControlCommand(cmd); err != nil {
		t.Fatal(err)
	}
	if !recoveryJSONEqual(before, l.canonical.Orders[cook.orderID].Stages[0].Attempts[0]) {
		t.Fatal("failed history changed")
	}
	if _, err := l.requestChangesReview(cook.orderID); err != nil {
		t.Fatal(err)
	}
}

func TestLocalTaskRecoveryRejectsForeignOrChangedEvidence(t *testing.T) {
	for _, which := range []string{"candidate", "ack", "bytes", "meta", "scope", "typed"} {
		t.Run(which, func(t *testing.T) {
			l, cook, cmd := localTaskReviewFixture(t)
			oldLocalTaskFailure(t, l, cook)
			switch which {
			case "candidate":
				cmd.Target = "bad"
			case "ack":
				cmd.Name = "foreign"
			case "bytes":
				requestChangesWrite(t, filepath.Join(cook.worktreePath, "candidate.txt"), []byte("changed"))
			case "meta":
				recoveryWriteJSON(t, filepath.Join(l.runtimeDir, "sessions", cook.session.ID(), "meta.json"), map[string]any{"status": "running", "alive": true})
			case "scope":
				l.config.Runtime.Process.AdmittedOrders = nil
			case "typed":
				l.config.Agents.Codex.RequireTypedOutcome = true
			}
			before, _ := json.Marshal(l.canonical)
			if err := l.dispatchControlCommand(cmd); err == nil {
				t.Fatal("accepted drift")
			}
			after, _ := json.Marshal(l.canonical)
			if string(before) != string(after) {
				t.Fatal("refusal mutated canonical")
			}
		})
	}
}
