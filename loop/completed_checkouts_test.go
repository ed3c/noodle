package loop

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/poteto/noodle/internal/state"
	"github.com/poteto/noodle/worktree/archive"
)

func completedCheckoutLoopPolicy(t *testing.T, l *Loop, cook *cookHandle) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("original lossless archive metadata requires macOS")
	}
	admin, err := recoveryGit(cook.worktreePath, "rev-parse", "--absolute-git-dir")
	if err != nil {
		t.Fatal(err)
	}
	// This fixture uses a raw linked checkout. Creator enrollment is exercised
	// through the separate real App.Create controls.
	recoveryWriteJSON(t, filepath.Join(admin, "noodle-incarnation.json"), strings.Repeat("a", 64))
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	codec, err := exec.LookPath("zstd")
	if err != nil {
		t.Fatal(err)
	}
	physical, err := filepath.EvalSymlinks(cook.worktreePath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := archive.Config{Roots: []string{filepath.Dir(physical)}, ArchiveRoot: filepath.Join(base, "archive"), BlobRoot: filepath.Join(base, "cas"), StateDir: filepath.Join(base, "owner"), Codec: codec, MaxItems: 1, ItemTimeoutSeconds: 30}
	for _, path := range []string{cfg.ArchiveRoot, cfg.BlobRoot, cfg.StateDir} {
		if err = os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.WriteFile(filepath.Join(cfg.StateDir, "owner.key"), []byte(strings.Repeat("k", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(base, "policy.json")
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	hash, err := archive.FileHash(path)
	if err != nil {
		t.Fatal(err)
	}
	l.deps.CheckoutArchivePolicy = &archive.FileRef{Path: path, SHA256: hash}
}

func TestCompletedCheckoutOriginalLoopConsumesBothMainDispositions(t *testing.T) {
	for _, action := range []string{"retain-local-task", "merge-local-task"} {
		t.Run(action, func(t *testing.T) {
			l, cook, cmd := localTaskReviewFixture(t)
			localTaskRealWorktree(t, l)
			completedCheckoutLoopPolicy(t, l, cook)
			cmd.Action = action
			if err := l.dispatchControlCommand(cmd); err != nil {
				t.Fatal(err)
			}
			_, ack, _ := l.controlPaths()
			if err := appendAcks(ack, []ControlAck{{ID: cmd.ID, Action: action, Status: "ok"}}); err != nil {
				t.Fatal(err)
			}
			before, err := json.Marshal(l.canonical)
			if err != nil {
				t.Fatal(err)
			}
			if err = l.cleanupCompleted(false); err != nil {
				t.Fatal("original cleanup readiness", err)
			}
			if _, err = l.verifyCompletedCheckoutReclamation(); err == nil {
				t.Fatal("readiness was mistaken for known reclamation")
			}
			if _, err = os.Stat(cook.worktreePath); err != nil {
				t.Fatal("inspect removed accepted checkout", err)
			}
			if err = l.cleanupCompleted(true); err != nil {
				t.Fatal("original cleanup apply", err)
			}
			if _, err = os.Stat(cook.worktreePath); !os.IsNotExist(err) {
				t.Fatal("original cleanup did not remove checkout", err)
			}
			if _, err = recoveryGit(l.projectDir, "rev-parse", "refs/heads/"+cook.worktreeName); err != nil {
				t.Fatal("original cleanup deleted accepted branch", err)
			}
			after, _ := json.Marshal(l.canonical)
			if string(after) != string(before) || l.canonical.Orders[cook.orderID].Status != state.OrderCompleted {
				t.Fatal("cleanup rewrote original task or failed history")
			}
			if err = l.cleanupCompleted(false); err != nil {
				t.Fatal("original cleanup could not read its completed removal", err)
			}
			rows, err := l.verifyCompletedCheckoutReclamation()
			if err != nil {
				t.Fatal("known original checkout reclamation proof failed", err)
			}
			attempt := l.canonical.Orders[cook.orderID].Stages[0].Attempts[0]
			if len(rows) != 1 || rows[0].OrderID != cook.orderID || rows[0].AttemptID != attempt.AttemptID || rows[0].SessionID != attempt.SessionID || rows[0].ControlID != cmd.ID || rows[0].ControlAction != action || rows[0].Archive.VerificationRelease.Path == "" || rows[0].MainACK.Path != ack {
				t.Fatal("reclamation proof lost original identity or sources", rows)
			}
			l.deps.CheckoutArchivePolicy = nil
			if _, err = l.verifyCompletedCheckoutReclamation(); err == nil {
				t.Fatal("legacy release was mistaken for qualified reclamation")
			}
		})
	}
}
