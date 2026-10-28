package loop

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/poteto/noodle/config"
)

func TestBoundTaskRejectRetainsWorktreeAndSession(t *testing.T) {
	l, cook, _ := localTaskReviewFixture(t)
	localTaskRealWorktree(t, l)
	// The original admission keeps the policy even after config changes.
	l.config.Runtime.AdmittedOrders = nil
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
			cook := &cookHandle{cookIdentity: cookIdentity{orderID: "task", stage: Stage{Runtime: "openshell"}}, worktreeName: "task-worktree"}
			if bound {
				l.config.Runtime.AdmittedOrders = []string{"task"}
			}
			l.cleanupCookWorktree(cook)
			if got := len(wt.cleaned); (bound && got != 0) || (!bound && got != 1) {
				t.Fatalf("cleanup count=%d, bound=%v", got, bound)
			}
		})
	}
}

func TestStoppedBoundTaskRejectRetainsWorktree(t *testing.T) {
	for _, external := range []bool{false, true} {
		t.Run(fmt.Sprintf("external=%v", external), func(t *testing.T) {
			project, wt := stoppedReviewFixture(t)
			data := []byte("[runtime]\nadmitted_orders = ['order-1']\n")
			if external {
				dir, err := filepath.EvalSymlinks(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dir, "task.toml")
				recoveryWrite(t, path, data)
				recoveryWrite(t, filepath.Join(project, ".noodle.toml"), []byte("mode = 'supervised'\n"))
				if _, err := config.SaveRuntimeSource(project, config.RuntimeSource{Path: path, SHA256: fmt.Sprintf("%x", sha256.Sum256(data))}); err != nil {
					t.Fatal(err)
				}
			} else {
				recoveryWrite(t, filepath.Join(project, ".noodle.toml"), data)
			}
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
		})
	}
}

func TestBoundTaskStopKeepsOriginalWatcherUntilExit(t *testing.T) {
	wt := &fakeWorktree{}
	l := newControlTestLoop(t, wt, newMockRuntime())
	l.config.Runtime.AdmittedOrders = []string{"task"}
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
