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
	// Real attached contexts already retain their original schedule order.
	if err := l.ensureScheduleOrderPresent(); err != nil {
		t.Fatal(err)
	}
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
				if req.ResumeSessionID != cook.session.ID() {
					t.Fatalf("revision selected wrong native predecessor: %q", req.ResumeSessionID)
				}
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

func completionRefreshMeta(t *testing.T, l *Loop, cook *cookHandle, field string, value any) {
	t.Helper()
	path := filepath.Join(l.runtimeDir, "sessions", cook.session.ID(), "meta.json")
	var meta map[string]any
	if err := json.Unmarshal(recoveryRead(t, path), &meta); err != nil {
		t.Fatal(err)
	}
	meta[field] = value
	recoveryWriteJSON(t, path, meta)
}

func TestLocalTaskCompletionRecoveryIgnoresOnlyMonitorProjection(t *testing.T) {
	for _, field := range []string{"updated_at", "idle_seconds", "session_id", "status", "alive", "provider", "model", "runtime"} {
		t.Run(field, func(t *testing.T) {
			l, cook, path := localTaskCompletedFixture(t)
			result, err := PrepareLocalTaskCompletionRecovery(l.projectDir, l.config, cook.orderID, path)
			if err != nil {
				t.Fatal(err)
			}
			ref := result["packet"].(localTaskFileRef)
			var value any = "changed"
			if field == "alive" {
				value = true
			}
			if field == "idle_seconds" {
				value = 999
			}
			completionRefreshMeta(t, l, cook, field, value)
			meta := recoveryRead(t, filepath.Join(l.runtimeDir, "sessions", cook.session.ID(), "meta.json"))
			before, _ := json.Marshal(l.canonical)
			err = l.dispatchControlCommand(ControlCommand{Action: "recover-local-task-completion", OrderID: cook.orderID, Value: ref.Path, Target: ref.SHA256})
			if field == "updated_at" || field == "idle_seconds" {
				if err != nil {
					t.Fatal(err)
				}
				if l.canonical.Orders[cook.orderID].Stages[0].Status != state.StageReview {
					t.Fatal("missing review")
				}
			} else {
				if err == nil {
					t.Fatal("accepted changed terminal identity")
				}
				after, _ := json.Marshal(l.canonical)
				if string(before) != string(after) {
					t.Fatal("refusal mutated state")
				}
			}
			if string(meta) != string(recoveryRead(t, filepath.Join(l.runtimeDir, "sessions", cook.session.ID(), "meta.json"))) {
				t.Fatal("rewrote native metadata")
			}
		})
	}
}

func TestLocalTaskCompletionRecoverySuccessorRequiresKnownErrorAndSameCustody(t *testing.T) {
	for _, change := range []string{"projection", "unknown_ack", "wrong_error", "duplicate_ack", "bytes", "prior_sha", "events"} {
		t.Run(change, func(t *testing.T) {
			l, cook, path := localTaskCompletedFixture(t)
			old, err := l.captureLocalTaskCompletion(l.canonical.Orders[cook.orderID])
			if err != nil {
				t.Fatal(err)
			}
			old.Schema, old.Terminal = 1, nil
			old.Binding.Files["meta.json"] = recoveryDigest(recoveryRead(t, filepath.Join(l.runtimeDir, "sessions", cook.session.ID(), "meta.json")))
			priorPath := filepath.Join(t.TempDir(), "original.json")
			recoveryWriteJSON(t, priorPath, old)
			priorBytes := recoveryRead(t, priorPath)
			priorSHA := recoveryDigest(priorBytes)
			ack := ControlAck{ID: "original-recovery", Action: "recover-local-task-completion", Status: "error", Message: "local task completion recovery custody changed"}
			if change == "wrong_error" {
				ack.Message = "different refusal"
			}
			ackBytes, _ := json.Marshal(ack)
			ackBytes = append(ackBytes, '\n')
			if change == "unknown_ack" {
				ackBytes = nil
			}
			if change == "duplicate_ack" {
				ackBytes = append(ackBytes, ackBytes...)
			}
			requestChangesWrite(t, filepath.Join(l.runtimeDir, "control-ack.ndjson"), ackBytes)
			completionRefreshMeta(t, l, cook, "updated_at", "later")
			completionRefreshMeta(t, l, cook, "idle_seconds", 999)
			if change == "bytes" {
				requestChangesWrite(t, filepath.Join(cook.worktreePath, "candidate.txt"), []byte("drift"))
			}
			if change == "prior_sha" {
				priorSHA = "bad"
			}
			if change == "events" {
				requestChangesWrite(t, filepath.Join(l.runtimeDir, "loop-events.ndjson"), append(recoveryRead(t, filepath.Join(l.runtimeDir, "loop-events.ndjson")), []byte("\n")...))
			}
			if change == "projection" {
				if err := l.releaseIfQuiescent(); err != nil {
					t.Fatal(err)
				}
				l = New(l.projectDir, "noodle", l.config, l.deps)
				if err := l.reconcile(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			result, err := PrepareLocalTaskCompletionRecovery(l.projectDir, l.config, cook.orderID, path, priorPath, priorSHA, ack.ID)
			if change != "projection" {
				if err == nil {
					t.Fatal("accepted unsupported successor")
				}
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("refusal wrote packet")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			ref := result["packet"].(localTaskFileRef)
			completionRefreshMeta(t, l, cook, "updated_at", "later again")
			cmd := ControlCommand{Action: "recover-local-task-completion", OrderID: cook.orderID, Value: ref.Path, Target: ref.SHA256}
			if err := l.dispatchControlCommand(cmd); err != nil {
				t.Fatal(err)
			}
			if err := l.dispatchControlCommand(cmd); err != nil {
				t.Fatal(err)
			}
			if string(priorBytes) != string(recoveryRead(t, priorPath)) || string(ackBytes) != string(recoveryRead(t, filepath.Join(l.runtimeDir, "control-ack.ndjson"))) {
				t.Fatal("rewrote failed history")
			}
		})
	}
}
