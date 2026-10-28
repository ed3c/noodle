package runtime

import (
	"context"
	"encoding/json"

	"github.com/poteto/noodle/internal/workspaceartifacts"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestOpenShellCleanupObservesOriginalOwnerAfterWorktreeRemoval(t *testing.T) {
	r, b, f := completedReadbackFixture(t)
	control := filepath.Join(t.TempDir(), "control")
	cmd := exec.Command("git", "clone", "--quiet", "--no-hardlinks", b.WorktreePath, control)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone fixture: %v %s", err, out)
	}
	if err := os.RemoveAll(b.WorktreePath); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(f.root, "events"))
	got, err := r.ReadCleanupSession(t.Context(), b, control, StatusCompleted)
	if err != nil {
		t.Fatal(err)
	}
	if got.OutcomeStatus != StatusCompleted || got.PublishedHead == "" || !got.Execution.StopAcknowledged || got.Execution.NativePhase != "Stopped" {
		t.Fatal("incomplete cleanup evidence")
	}
	after, _ := os.ReadFile(filepath.Join(f.root, "events"))
	if string(after[len(before):]) != "get\n" {
		t.Fatalf("cleanup native effects: %s", after[len(before):])
	}
	if _, err := r.ReadCompletedSession(t.Context(), b); err == nil {
		t.Fatal("normal readback accepted missing worktree")
	}
	if _, err := r.ReadCleanupSession(t.Context(), b, control, StatusCancelled); err == nil {
		t.Fatal("completed session mislabeled as cancelled")
	}
	dir := filepath.Join(r.config.RuntimeDir, "sessions", b.SessionID)
	archives, err := filepath.Glob(filepath.Join(dir, "progress-archive-*", "*.tar"))
	if err != nil || len(archives) != 2 {
		t.Fatalf("archives: %v %v", archives, err)
	}
	file, err := os.OpenFile(archives[0], os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.Write([]byte("changed"))
	if closeErr := file.Close(); err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
	if _, err := r.ReadCleanupSession(t.Context(), b, control, StatusCompleted); err == nil {
		t.Fatal("changed saved archive accepted")
	}
}

func TestOpenShellCleanupMetadataProjection(t *testing.T) {
	for _, variant := range []string{"clock", "identity", "alive", "status", "missing-identity", "normal-clock"} {
		t.Run(variant, func(t *testing.T) {
			r, b, _ := completedReadbackFixture(t)
			observe := func(ctx context.Context, id OpenShellIdentity) (OpenShellCompletedObservation, error) {
				got, err := r.config.Gateway.ObserveCompleted(ctx, r.config.ContextID, id)
				if err != nil {
					return got, err
				}
				path := filepath.Join(r.config.RuntimeDir, "sessions", b.SessionID, "meta.json")
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var meta map[string]any
				if err = json.Unmarshal(data, &meta); err != nil {
					t.Fatal(err)
				}
				switch variant {
				case "clock", "normal-clock":
					meta["updated_at"] = "2099-01-01T00:00:00Z"
					meta["cpu_percent"] = 5
				case "identity":
					meta["session_id"] = "foreign"
				case "alive":
					meta["alive"] = true
				case "status":
					meta["status"] = "running"
				case "missing-identity":
					delete(meta, "session_id")
				}
				writeReadbackJSON(t, path, meta)
				return got, nil
			}
			retained := func(ctx context.Context, receipt openShellDispatchReceipt, head string) error {
				if err := verifyCleanupProgressArchives(ctx, filepath.Join(r.config.RuntimeDir, "sessions", b.SessionID), receipt); err != nil {
					return err
				}
				return workspaceartifacts.VerifyRetainedPublication(ctx, b.WorktreePath, receipt.Baseline, receipt.Delivery, head)
			}
			if variant == "normal-clock" {
				retained = nil
			}
			result, err := r.readSettledSession(t.Context(), b, observe, false, retained)
			if variant == "clock" {
				if err != nil {
					t.Fatal(err)
				}
				if _, ok := result.completed.Sources["meta.json"]; ok {
					t.Fatal("derived metadata advertised as atomic cleanup source")
				}
			} else if err == nil {
				t.Fatalf("accepted %s drift", variant)
			}
		})
	}
}
