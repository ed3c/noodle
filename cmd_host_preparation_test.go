package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/worktree"
)

// Existing Host tests isolate runtime/config/actor behavior. Their private
// dependency supplies no CLI capability and is absent from production Hosts.
func fixtureTaskPreparation(context.Context, hostRequest, []byte) (worktree.TaskControl, hostTaskPreparationPlan, error) {
	id, err := hostID()
	return worktree.TaskControl{Preparation: admission.PreparationRef{ID: id}}, hostTaskPreparationPlan{}, err
}

func preparationRepository(t *testing.T) string {
	t.Helper()
	root := hostCapacityRoot(t)
	for _, args := range [][]string{{"init", "-b", "main"}, {"-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-m", "Keep the original preparation source"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if raw, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("original fixture Git: %v: %s", err, raw)
		}
	}
	return root
}

func TestHostControlPublicProducerDefersMissingSourceBeforeEffects(t *testing.T) {
	root, repository := hostCapacityRoot(t), preparationRepository(t)
	task := filepath.Join(hostCapacityRoot(t), "task.json")
	raw, err := json.Marshal(map[string]string{"repository": repository, "base_ref": "main"})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(task, raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("NOODLE_HOST_DIR", root)
	cmd := newHostControlCmd()
	cmd.SetArgs([]string{"--task-file", task})
	if err = cmd.Execute(); !errors.Is(err, admission.ErrDeferred) {
		t.Fatal("missing original Source was not a named defer", err)
	}
	for _, path := range []string{filepath.Join(root, "controls"), filepath.Join(repository, ".git", "worktrees")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("deferred public producer created checkout or control effects", path, err)
		}
	}
}

func TestHostPreparationUnsupportedEntriesHaveNoHostEffects(t *testing.T) {
	root := filepath.Join(hostCapacityRoot(t), "not-created")
	t.Setenv("NOODLE_HOST_DIR", root)
	t.Setenv("NOODLE_HOST_POOL_ROOT", "")
	t.Setenv("NOODLE_HOST_GENERATION", "")
	if err := serveHostWithSampler(context.Background(), hostShutdownTestSampler{}); !errors.Is(err, admission.ErrDeferred) {
		t.Fatal("unpooled serve bypassed native preparation", err)
	}
	cmd := newStartCmd(&App{})
	if err := cmd.Execute(); !errors.Is(err, admission.ErrDeferred) {
		t.Fatal("standalone start bypassed native preparation", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("unsupported entry materialized a Host", err)
	}
}

func TestHostGoalPublicOwnerDefersMissingBudgetBeforeProfileOrProcess(t *testing.T) {
	root, goal, template, binary := goalHostFixture(t)
	launched := false
	_, err := ensureGoalHost(context.Background(), root, goal, template, binary, "", func(context.Context, string, string) error { launched = true; return nil })
	if !errors.Is(err, admission.ErrDeferred) || launched {
		t.Fatal("Goal substituted missing lifetime budget", err)
	}
	if _, err := os.Stat(filepath.Join(root, "h")); !os.IsNotExist(err) {
		t.Fatal("missing budget materialized profile or Host", err)
	}
}
