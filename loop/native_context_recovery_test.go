package loop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/poteto/noodle/internal/state"
	"github.com/poteto/noodle/internal/taskreg"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/poteto/noodle/skill"
)

type missingNativeSession struct{ *mockSession }

func (s *missingNativeSession) Outcome() loopruntime.SessionOutcome {
	return loopruntime.SessionOutcome{Status: loopruntime.StatusFailed, ExitCode: 1, Reason: "no events emitted"}
}

func TestMissingNativeContextDegradesOnceAndPreservesOriginalTask(t *testing.T) {
	l, original, _ := localTaskReviewFixture(t)
	localTaskRealWorktree(t, l)
	if err := l.controlRequestChanges(original.orderID, "correct result"); err != nil {
		t.Fatal(err)
	}
	if err := l.controlRequeue(original.orderID); err != nil {
		t.Fatal(err)
	}
	l.registry = taskreg.NewFromSkills([]skill.SkillMeta{{Name: "local-task", Path: "/skills/local-task", Frontmatter: skill.Frontmatter{Schedule: "admitted task"}}})
	rt := newMockRuntime()
	calls := 0
	const uuid = "01a1622d-1cdf-7c92-be2f-3613a06ecd1d"
	rt.dispatchHook = func(req loopruntime.DispatchRequest) (loopruntime.SessionHandle, error) {
		calls++
		if calls == 1 {
			if req.ResumeSessionID != original.session.ID() {
				t.Fatal("missing original resume identity")
			}
			dir := filepath.Join(l.runtimeDir, "sessions", "missing-native")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			recoveryWriteJSON(t, filepath.Join(dir, "spawn.json"), map[string]any{"session_id": "missing-native", "provider": "codex", "resume_session_id": req.ResumeSessionID, "resume_thread_id": uuid, "task_key": req.TaskKey, "order_id": req.EnvVars["NOODLE_ORDER_ID"], "stage_index": req.EnvVars["NOODLE_STAGE_INDEX"], "worktree_path": req.WorktreePath})
			recoveryWriteJSON(t, filepath.Join(dir, "codex-startup.json"), map[string]any{"session_id": "missing-native"})
			recoveryWriteJSON(t, filepath.Join(dir, "meta.json"), map[string]any{"session_id": "missing-native", "status": "exited", "alive": false})
			recoveryWriteJSON(t, filepath.Join(dir, "process.json"), map[string]any{"session_id": "missing-native", "pid": 99999999})
			requestChangesWrite(t, filepath.Join(dir, "raw.ndjson"), nil)
			requestChangesWrite(t, filepath.Join(dir, "stderr.log"), []byte("Error: thread/resume: thread/resume failed: no rollout found for thread id "+uuid+" (code -32600)\n"))
			done := make(chan struct{})
			close(done)
			return &missingNativeSession{&mockSession{id: "missing-native", status: "failed", done: done}}, nil
		}
		if calls != 2 || req.ResumeSessionID != "" || req.DegradedFromSessionID != original.session.ID() || req.DegradationReason == "" {
			t.Fatalf("wrong degraded dispatch: %+v", req)
		}
		if !strings.Contains(req.Prompt, "DEGRADED") || !strings.Contains(req.Prompt, filepath.Join(l.runtimeDir, "sessions", original.session.ID())) || !strings.Contains(req.Prompt, "fix candidate") {
			t.Fatal("degraded conversation omitted its label, original evidence or current goal")
		}
		if string(recoveryRead(t, filepath.Join(req.WorktreePath, "candidate.txt"))) != "reviewed candidate\n" {
			t.Fatal("degraded dispatch lost original data")
		}
		return &mockSession{id: "degraded-worker", status: "running", done: make(chan struct{})}, nil
	}
	l.deps.Runtimes["process"] = rt
	spawn := func(attempt int) {
		t.Helper()
		orders, err := l.currentOrders()
		if err != nil {
			t.Fatal(err)
		}
		order := orders.Orders[0]
		if err := l.spawnCook(context.Background(), dispatchCandidate{OrderID: order.ID, StageIndex: 0, Stage: order.Stages[0]}, order, spawnOptions{attempt: attempt}); err != nil {
			t.Fatal(err)
		}
	}
	spawn(1)
	cook := l.cooks.activeCooksByOrder[original.orderID]
	if err := l.controlPause(); err != nil {
		t.Fatal(err)
	}
	if err := l.applyStageResult(context.Background(), StageResult{OrderID: cook.orderID, SessionID: cook.session.ID(), Generation: cook.generation, Status: StageResultFailed}); err != nil {
		t.Fatal(err)
	}
	order := l.canonical.Orders[cook.orderID]
	if l.state != StatePaused || order.Status != state.OrderActive || order.Stages[0].Status != state.StagePending || calls != 1 {
		t.Fatal("recovery lifted hold or dispatched early")
	}
	if len(order.Stages[0].Attempts) != 2 || order.Stages[0].Attempts[1].Status != state.AttemptFailed {
		t.Fatal("original resume refusal history lost")
	}
	// A crash after the saved decision and StageFailed must finish the same
	// requeue, while leaving the persisted pause and native refusal untouched.
	order.Status = state.OrderFailed
	order.Stages[0].Status = state.StageFailed
	l.canonical.Orders[cook.orderID] = order
	if err := l.persistCanonicalCheckpoint(); err != nil {
		t.Fatal(err)
	}
	restarted := New(l.projectDir, "noodle", l.config, l.deps)
	if err := restarted.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := restarted.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	recovered := restarted.canonical.Orders[cook.orderID]
	if recovered.Status != state.OrderActive || recovered.Stages[0].Status != state.StagePending || calls != 1 || len(recovered.Stages[0].Attempts) != 2 {
		t.Fatal("restart lost continuation or replayed dispatch")
	}
	l.canonical = restarted.canonical
	if err := l.projectRecoveredOrder(cook.orderID); err != nil {
		t.Fatal(err)
	}
	if err := l.controlResume(); err != nil {
		t.Fatal(err)
	}
	spawn(2)
	stage := l.canonical.Orders[cook.orderID].Stages[0]
	req := loopruntime.DispatchRequest{ResumeSessionID: "missing-native", WorktreePath: original.worktreePath, TaskKey: stage.TaskKey}
	if _, err := l.degradedTaskDispatch(dispatchCandidate{OrderID: cook.orderID, StageIndex: 0}, 2, req); !errors.Is(err, errDispatchUnknown) {
		t.Fatalf("duplicate degradation not refused: %v", err)
	}
	records, err := nativeDegradations(stage)
	if err != nil || len(records) != 1 || !records["missing-native"].Offered {
		t.Fatal("single degraded successor not saved")
	}
	if calls != 2 {
		t.Fatalf("dispatches=%d, want resume + one degraded successor", calls)
	}
	if err := l.cooks.activeCooksByOrder[original.orderID].session.ForceKill(); err != nil {
		t.Fatal(err)
	}
}
