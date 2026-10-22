package worktree

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
)

func TestRetainedTaskPublicCleanupAndMergeRequireExplicitDeletion(t *testing.T) {
	for _, ledgerPolicy := range []bool{false, true} {
		root := setupTestRepo(t)
		app := &App{Root: root, Quiet: true}
		if err := app.Create("retained"); err != nil {
			t.Fatal(err)
		}
		path := WorktreePath(root, "retained")
		if err := os.WriteFile(filepath.Join(path, "result.txt"), []byte("retained work"), 0600); err != nil {
			t.Fatal(err)
		}
		runGitIn(t, path, "add", "result.txt")
		runGitIn(t, path, "commit", "-m", "candidate")
		if err := os.MkdirAll(filepath.Join(root, ".noodle"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, ".git/info/exclude"), []byte(".noodle/\n.noodle.toml\n"), 0600); err != nil {
			t.Fatal(err)
		}
		order := state.OrderNode{OrderID: "task", Stages: []state.StageNode{{Attempts: []state.AttemptNode{{WorktreeName: "retained"}}}}}
		snapshot := reducer.BuildSnapshot(state.State{Orders: map[string]state.OrderNode{"task": order}}, nil, time.Time{})
		if ledgerPolicy {
			payload, _ := json.Marshal(map[string]string{"order_id": "task", "task_data_retention": "explicit_delete"})
			effect := reducer.Effect{EffectID: reducer.InitialAdmissionEffectID("task"), Payload: payload}
			snapshot.EffectLedger = []reducer.EffectLedgerRecord{{EffectID: effect.EffectID, Effect: effect}}
		} else if err := os.WriteFile(filepath.Join(root, ".noodle.toml"), []byte("[runtime.process]\nadmitted_orders=['task']\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := reducer.WriteSnapshotAtomic(filepath.Join(root, ".noodle/state.snapshot.json"), snapshot); err != nil {
			t.Fatal(err)
		}
		if err := app.Merge("retained"); err != nil {
			t.Fatal(err)
		}
		if err := app.Prune(); err != nil {
			t.Fatal(err)
		}
		if err := app.Cleanup("retained", CleanupOpts{Force: true}); err == nil {
			t.Fatal("ordinary cleanup bypassed retention")
		}
		if raw, err := os.ReadFile(filepath.Join(path, "result.txt")); err != nil || string(raw) != "retained work" {
			t.Fatalf("public cleanup deleted retained work: %v", err)
		}
		if err := app.Cleanup("retained", CleanupOpts{ExplicitDelete: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("explicit deletion did not remove selected worktree")
		}
	}
}
