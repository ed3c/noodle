package loop

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/poteto/noodle/event"
	"github.com/poteto/noodle/internal/state"
	"github.com/poteto/noodle/internal/taskreg"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/poteto/noodle/skill"
)

func TestLocalTaskCompletionAlwaysRequiresReview(t *testing.T) {
	for _, kind := range []string{"dirty", "committed", "empty"} {
		t.Run(kind, func(t *testing.T) {
			l, cook, _ := localTaskReviewFixture(t)
			localTaskRealWorktree(t, l)
			if kind == "committed" {
				runGitInRepo(t, cook.worktreePath, "add", "candidate.txt")
				runGitInRepo(t, cook.worktreePath, "commit", "-m", "candidate")
			}
			if kind == "empty" {
				if err := os.Remove(filepath.Join(cook.worktreePath, "candidate.txt")); err != nil {
					t.Fatal(err)
				}
			}
			before := recoverySessionBytes(t, l, cook.session.ID())
			if err := l.handleCompletion(context.Background(), cook, StageResultCompleted, "completed"); err != nil {
				t.Fatal(err)
			}
			order := l.canonical.Orders[cook.orderID]
			if order.Status != state.OrderActive || order.Stages[0].Status != state.StageReview || l.canonical.PendingReviews[cook.orderID].SessionID != cook.session.ID() {
				t.Fatalf("missing review: %+v", order)
			}
			if !recoveryJSONEqual(before, recoverySessionBytes(t, l, cook.session.ID())) {
				t.Fatal("completion rewrote session")
			}
		})
	}
}

func localTaskCompletedFixture(t *testing.T) (*Loop, *cookHandle, string) {
	t.Helper()
	l, cook, _ := localTaskReviewFixture(t)
	localTaskRealWorktree(t, l)
	order := l.canonical.Orders[cook.orderID]
	order.Status = state.OrderCompleted
	order.Stages[0].Status = state.StageCompleted
	l.canonical.Orders[cook.orderID] = order
	delete(l.canonical.PendingReviews, cook.orderID)
	delete(l.cooks.pendingReview, cook.orderID)
	if err := l.persistCanonicalCheckpoint(); err != nil {
		t.Fatal(err)
	}
	if err := l.projectRecoveredOrder(cook.orderID); err != nil {
		t.Fatal(err)
	}
	if err := l.syncPendingReviewProjection(); err != nil {
		t.Fatal(err)
	}
	if err := l.events.Emit(event.LoopEventStageCompleted, StageCompletedPayload{OrderID: cook.orderID, StageIndex: 0, TaskKey: "local-task", SessionID: sessionIDPtr(cook)}); err != nil {
		t.Fatal(err)
	}
	if err := l.events.Emit(event.LoopEventOrderCompleted, OrderCompletedPayload{OrderID: cook.orderID}); err != nil {
		t.Fatal(err)
	}
	return l, cook, filepath.Join(t.TempDir(), "completion-recovery.json")
}

func TestLocalTaskCompletionRecoveryRetainsHistoryAndReviewsExactBytes(t *testing.T) {
	l, cook, path := localTaskCompletedFixture(t)
	before := l.canonical.Orders[cook.orderID].Stages[0].Attempts
	session := recoverySessionBytes(t, l, cook.session.ID())
	events := recoveryRead(t, filepath.Join(l.runtimeDir, "loop-events.ndjson"))
	snapshot := recoveryRead(t, l.canonicalSnapshotPath())
	result, err := PrepareLocalTaskCompletionRecovery(l.projectDir, l.config, cook.orderID, path)
	if err != nil {
		t.Fatal(err)
	}
	if !recoveryJSONEqual(snapshot, recoveryRead(t, l.canonicalSnapshotPath())) {
		t.Fatal("producer wrote native state")
	}
	ref := result["packet"].(localTaskFileRef)
	cmd := ControlCommand{ID: "recover-completion", Action: "recover-local-task-completion", OrderID: cook.orderID, Value: ref.Path, Target: ref.SHA256}
	if ack := recoveryControl(t, l, cmd); ack.Status != "ok" {
		t.Fatal(ack)
	}
	l = New(l.projectDir, "noodle", l.config, l.deps)
	if err := l.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := l.dispatchControlCommand(cmd); err != nil {
		t.Fatal(err)
	}
	order := l.canonical.Orders[cook.orderID]
	if order.Status != state.OrderActive || order.Stages[0].Status != state.StageReview || !recoveryJSONEqual(before, order.Stages[0].Attempts) {
		t.Fatal("recovery changed history or lost review")
	}
	if !recoveryJSONEqual(session, recoverySessionBytes(t, l, cook.session.ID())) || string(events) != string(recoveryRead(t, filepath.Join(l.runtimeDir, "loop-events.ndjson"))) {
		t.Fatal("recovery changed original evidence")
	}
	if string(recoveryRead(t, filepath.Join(cook.worktreePath, "candidate.txt"))) != "reviewed candidate\n" {
		t.Fatal("recovery changed bytes")
	}
}

func TestLocalTaskCompletionRecoveryRejectsInvalidCustody(t *testing.T) {
	for _, change := range []string{"empty", "bytes", "index", "base", "foreign", "meta", "event", "scope", "packet", "attempt", "process"} {
		t.Run(change, func(t *testing.T) {
			l, cook, path := localTaskCompletedFixture(t)
			result, err := PrepareLocalTaskCompletionRecovery(l.projectDir, l.config, cook.orderID, path)
			if err != nil {
				t.Fatal(err)
			}
			ref := result["packet"].(localTaskFileRef)
			cmd := ControlCommand{Action: "recover-local-task-completion", OrderID: cook.orderID, Value: ref.Path, Target: ref.SHA256}
			switch change {
			case "empty":
				os.Remove(filepath.Join(cook.worktreePath, "candidate.txt"))
			case "bytes":
				requestChangesWrite(t, filepath.Join(cook.worktreePath, "candidate.txt"), []byte("drift"))
			case "index":
				runGitInRepo(t, cook.worktreePath, "add", "candidate.txt")
			case "base":
				runGitInRepo(t, l.projectDir, "commit", "--allow-empty", "-m", "moved")
			case "foreign":
				requestChangesWrite(t, filepath.Join(cook.worktreePath, "foreign.txt"), []byte("foreign"))
			case "meta":
				recoveryWriteJSON(t, filepath.Join(l.runtimeDir, "sessions", cook.session.ID(), "meta.json"), map[string]any{"alive": true, "status": "running"})
			case "event":
				requestChangesWrite(t, filepath.Join(l.runtimeDir, "loop-events.ndjson"), []byte(""))
			case "scope":
				l.config.Runtime.Process.AdmittedOrders = nil
			case "packet":
				cmd.Target = "bad"
			case "attempt":
				o := l.canonical.Orders[cook.orderID]
				o.Stages[0].Attempts[0].SessionID = "foreign"
				l.canonical.Orders[cook.orderID] = o
			case "process":
				recoveryWriteJSON(t, filepath.Join(l.runtimeDir, "sessions", cook.session.ID(), "process.json"), map[string]any{"session_id": cook.session.ID(), "pid": os.Getpid()})
			}
			before, _ := json.Marshal(l.canonical)
			if err := l.dispatchControlCommand(cmd); err == nil {
				t.Fatal("accepted invalid custody")
			}
			after, _ := json.Marshal(l.canonical)
			if string(before) != string(after) {
				t.Fatal("refusal mutated canonical")
			}
		})
	}
}

func TestLocalTaskRevisionDispatchPreservesBoundDirtyBytes(t *testing.T) {
	for _, drift := range []bool{false, true} {
		t.Run(map[bool]string{false: "preserved", true: "refused_drift"}[drift], func(t *testing.T) {
			l, cook, _ := localTaskReviewFixture(t)
			if err := l.controlRequestChanges(cook.orderID, "correct result"); err != nil {
				t.Fatal(err)
			}
			if err := l.controlRequeue(cook.orderID); err != nil {
				t.Fatal(err)
			}
			if drift {
				requestChangesWrite(t, filepath.Join(cook.worktreePath, "candidate.txt"), []byte("drift"))
			}
			dispatched := false
			rt := newMockRuntime()
			rt.dispatchHook = func(req loopruntime.DispatchRequest) (loopruntime.SessionHandle, error) {
				dispatched = true
				if string(recoveryRead(t, filepath.Join(req.WorktreePath, "candidate.txt"))) != "reviewed candidate\n" {
					t.Fatal("dispatch discarded retained dirty candidate")
				}
				return &mockSession{id: "revision-session", status: "running", done: make(chan struct{})}, nil
			}
			l.deps.Runtimes["process"] = rt
			l.registry = taskreg.NewFromSkills([]skill.SkillMeta{{Name: "local-task", Path: "/skills/local-task", Frontmatter: skill.Frontmatter{Schedule: "admitted task"}}})
			orders, err := l.currentOrders()
			if err != nil {
				t.Fatal(err)
			}
			o := orders.Orders[0]
			err = l.spawnCook(context.Background(), dispatchCandidate{OrderID: o.ID, StageIndex: 0, Stage: o.Stages[0]}, o, spawnOptions{attempt: 1})
			if drift {
				if err == nil || dispatched {
					t.Fatal("dispatched changed custody")
				}
			} else if err != nil || !dispatched {
				t.Fatalf("dispatch %v %v", dispatched, err)
			}
		})
	}
}
