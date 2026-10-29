package loop

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/state"
	loopruntime "github.com/poteto/noodle/runtime"
)

func TestNativeCodexAdmissionUsesOriginalOrderScope(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Runtime.Default = "codex-app-server"
	cfg.Runtime.Process.AdmittedOrders = []string{"original-order"}
	l := &Loop{config: cfg}
	if err := l.requireProcessOrder("codex-app-server", "foreign-order"); err == nil {
		t.Fatal("native route bypassed original task scope")
	}
	if err := l.requireProcessOrder("", "original-order"); err != nil {
		t.Fatal(err)
	}
}

func TestNativeCodexUnavailableNeverFallsBackToExec(t *testing.T) {
	native, local := newMockRuntime(), newMockRuntime()
	native.dispatchErr = errors.New("original native daemon unavailable")
	l := New(t.TempDir(), "noodle", config.DefaultConfig(), Dependencies{
		Runtimes: map[string]loopruntime.Runtime{"codex-app-server": native, "process": local}, Registry: testLoopRegistry(),
	})
	_, _, err := l.dispatchSession(context.Background(), loopruntime.DispatchRequest{Runtime: "codex-app-server"})
	if err == nil || len(native.calls) != 1 || len(local.calls) != 0 {
		t.Fatalf("native failure changed execution owner: native=%d local=%d error=%v", len(native.calls), len(local.calls), err)
	}
}

func TestNativeLocalTaskReviewUsesOriginalReleaseAndRetainsCustody(t *testing.T) {
	for _, nativeState := range []string{"released", "waiting_native_unload", "needs_owner_readback"} {
		t.Run(nativeState, func(t *testing.T) {
			l, cook, _ := localTaskReviewFixture(t)
			cook.stage.Runtime = "codex-app-server"
			order := l.canonical.Orders[cook.orderID]
			order.Stages[0].Runtime = cook.stage.Runtime
			l.canonical.Orders[cook.orderID] = order
			pending := l.cooks.pendingReview[cook.orderID]
			pending.stage.Runtime = cook.stage.Runtime
			review := l.canonical.PendingReviews[cook.orderID]
			review.Runtime = cook.stage.Runtime
			l.canonical.PendingReviews[cook.orderID] = review
			dir := filepath.Join(l.runtimeDir, "sessions", cook.session.ID())
			var spawn map[string]any
			if err := json.Unmarshal(recoveryRead(t, filepath.Join(dir, "spawn.json")), &spawn); err != nil {
				t.Fatal(err)
			}
			spawn["runtime"] = cook.stage.Runtime
			recoveryWriteJSON(t, filepath.Join(dir, "spawn.json"), spawn)
			if err := os.Remove(filepath.Join(dir, "process.json")); err != nil {
				t.Fatal(err)
			}
			recoveryWriteJSON(t, filepath.Join(dir, "native.json"), map[string]any{
				"schema": 2, "session_id": cook.session.ID(), "native_home": t.TempDir(), "worktree": cook.worktreePath,
				"foreground": map[string]any{"pid": 99999999, "started": "Mon Oct 26 00:00:00 2026"},
				"state":      nativeState, "terminal": "completed", "thread_id": "11111111-1111-4111-8111-111111111111",
				"order_id": cook.orderID, "task_key": "local-task", "model": cook.stage.Model,
				"server_exit": 0, "unsubscribe_ack": map[string]any{"status": "unsubscribed"}, "unload_readback": map[string]any{"data": []string{}, "nextCursor": nil},
			})
			if !l.isAdmittedLocalTask(order, order.Stages[0]) {
				t.Fatal("native task bypasses local task review")
			}
			b, err := l.captureRequestChanges(pending)
			if err != nil {
				t.Fatal(err)
			}
			if b.Files["native.json"] == "" || b.Files["process.json"] != "" {
				t.Fatal("native custody replaced with legacy process record")
			}
			err = l.validateRecovery(order, review, b)
			if nativeState == "released" {
				if err != nil {
					t.Fatal(err)
				}
				if err := l.handleCompletion(context.Background(), cook, StageResultCompleted, "completed"); err != nil {
					t.Fatal(err)
				}
				got := l.canonical.Orders[cook.orderID]
				if got.Stages[0].Status != state.StageReview {
					t.Fatal("native completion skipped candidate review")
				}
			} else if err == nil {
				t.Fatal("incomplete native release accepted")
			}
		})
	}
}
