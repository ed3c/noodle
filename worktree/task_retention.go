package worktree

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/reducer"
)

func (a *App) retainsTaskWorktree(name string) (bool, error) {
	cfg, _, err := config.LoadRuntime(a.Root)
	if err != nil {
		return false, err
	}
	snapshot, err := reducer.ReadSnapshot(filepath.Join(a.Root, ".noodle", "state.snapshot.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && len(cfg.Runtime.Process.AdmittedOrders) == 0 {
			return false, nil
		}
		return false, fmt.Errorf("task retention readback required: %w", err)
	}
	target := canonicalPath(a.resolveWorktreePath(name))
	matches := func(n string) bool { return n != "" && (n == name || canonicalPath(WorktreePath(a.Root, n)) == target) }
	for id, order := range snapshot.State.Orders {
		if !slices.Contains(cfg.Runtime.Process.AdmittedOrders, id) && !reducer.RetainedTaskData(snapshot.EffectLedger, id) {
			continue
		}
		for _, stage := range order.Stages {
			for _, attempt := range stage.Attempts {
				if matches(attempt.WorktreeName) {
					return true, nil
				}
			}
			if stage.Merge != nil && matches(stage.Merge.WorktreeName) {
				return true, nil
			}
		}
		if review, ok := snapshot.State.PendingReviews[id]; ok && (matches(review.WorktreeName) || (review.WorktreePath != "" && canonicalPath(review.WorktreePath) == target)) {
			return true, nil
		}
	}
	return false, nil
}
