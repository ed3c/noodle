package loop

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBoundTaskRejectRetainsWorktreeAndSession(t *testing.T) {
	l, cook, _ := localTaskReviewFixture(t)
	localTaskRealWorktree(t, l)
	// The original admission keeps the policy even after config changes.
	l.config.Runtime.Process.AdmittedOrders = nil
	path := filepath.Join(cook.worktreePath, "candidate.txt")
	before := recoveryRead(t, path)
	if err := l.controlReject(cook.orderID); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != string(before) {
		t.Fatalf("reject deleted bound task data: %v", err)
	}
	if _, err := os.Stat(filepath.Join(l.runtimeDir, "sessions", cook.session.ID(), "spawn.json")); err != nil {
		t.Fatal(err)
	}
}

func TestBoundTaskAutomaticCleanupRetainsOnlyBoundWorktrees(t *testing.T) {
	for _, bound := range []bool{false, true} {
		t.Run(map[bool]string{false: "standalone", true: "bound"}[bound], func(t *testing.T) {
			wt := &fakeWorktree{}
			l := newControlTestLoop(t, wt, newMockRuntime())
			cook := &cookHandle{cookIdentity: cookIdentity{orderID: "task"}, worktreeName: "task-worktree"}
			if bound {
				l.config.Runtime.Process.AdmittedOrders = []string{"task"}
			}
			l.cleanupCookWorktree(cook)
			if got := len(wt.cleaned); (bound && got != 0) || (!bound && got != 1) {
				t.Fatalf("cleanup count=%d, bound=%v", got, bound)
			}
		})
	}
}

func TestStoppedBoundTaskRejectRetainsWorktree(t *testing.T) {
	project, wt := stoppedReviewFixture(t)
	recoveryWrite(t, filepath.Join(project, ".noodle.toml"), []byte("[runtime.process]\nadmitted_orders = ['order-1']\n"))
	r := InspectStoppedReview(project, "/exact/noodle", "order-1", "example/project#7")
	if r.Status != "recoverable" {
		t.Fatalf("inspect: %+v", r)
	}
	r = RejectStoppedReview(project, "/exact/noodle", "order-1", "example/project#7", r.Digest)
	if r.Status != "rejected" {
		t.Fatalf("reject: %+v", r)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("offline rejection deleted worktree: %v", err)
	}
	if again := InspectStoppedReview(project, "/exact/noodle", "order-1", "example/project#7"); again.Status != "rejected" {
		t.Fatalf("repeated readback: %+v", again)
	}
}

func TestBoundTaskStopKeepsOriginalWatcherUntilExit(t *testing.T) {
	wt := &fakeWorktree{}
	l := newControlTestLoop(t, wt, newMockRuntime())
	l.config.Runtime.Process.AdmittedOrders = []string{"task"}
	session := &mockSession{id: "bound-worker", done: make(chan struct{})}
	cook := &cookHandle{cookIdentity: cookIdentity{orderID: "task"}, session: session, worktreeName: "bound-worktree"}
	l.cooks.activeCooksByOrder["task"] = cook
	if err := l.controlStopKill(cook); err != nil {
		t.Fatal(err)
	}
	if l.state != StatePaused || l.cooks.activeCooksByOrder["task"] != cook || len(wt.cleaned) != 0 {
		t.Fatal("stop erased original watcher, hold or retained worktree")
	}
}
