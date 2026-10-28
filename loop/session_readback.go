package loop

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
	"github.com/poteto/noodle/internal/statever"
	loopruntime "github.com/poteto/noodle/runtime"
)

type SessionReadback struct {
	SchemaVersion int                                  `json:"schema_version"`
	Status        string                               `json:"status"`
	OrderID       string                               `json:"order_id"`
	StageIndex    int                                  `json:"stage_index"`
	AttemptID     string                               `json:"attempt_id"`
	SessionID     string                               `json:"session_id"`
	OrderStatus   string                               `json:"order_status"`
	StageStatus   string                               `json:"stage_status"`
	AttemptStatus string                               `json:"attempt_status"`
	Runtime       string                               `json:"runtime"`
	Provider      string                               `json:"provider"`
	Model         string                               `json:"model"`
	WorktreePath  string                               `json:"worktree_path"`
	Execution     loopruntime.CompletedSessionReadback `json:"execution"`
	Effects       []string                             `json:"effects"`
}
type completedSessionReader interface {
	ReadCompletedSession(context.Context, loopruntime.CompletedSessionBinding) (loopruntime.CompletedSessionReadback, error)
}

// SessionReadback observes execution evidence through the existing runtime.
// Canonical failure/review status remains visible; observation grants no task
// acceptance, continuation, review, merge or publication authority.
func (l *Loop) SessionReadback(ctx context.Context, orderID, sessionID string) (result SessionReadback, err error) {
	if orderID == "" || sessionID == "" {
		return result, fmt.Errorf("exact order and session identity missing")
	}
	var snapshot reducer.DurableSnapshot
	ref, err := loopruntime.ReadSessionReadbackFile(l.canonicalSnapshotPath(), &snapshot)
	if err != nil {
		return result, err
	}
	if snapshot.State.SchemaVersion != statever.Current {
		return result, fmt.Errorf("canonical snapshot schema differs")
	}
	var order state.OrderNode
	var stage state.StageNode
	var attempt state.AttemptNode
	count, retry := 0, 0
	for key, o := range snapshot.State.Orders {
		for stageIndex, s := range o.Stages {
			for i, a := range s.Attempts {
				if a.SessionID == sessionID {
					if key != o.OrderID || stageIndex != s.StageIndex {
						return result, fmt.Errorf("canonical order/stage location differs")
					}
					order, stage, attempt, retry = o, s, a, i
					count++
				}
			}
		}
	}
	if count != 1 || order.OrderID != orderID || stage.StageIndex < 0 || attempt.AttemptID == "" || attempt.WorktreeName == "" || filepath.Base(attempt.WorktreeName) != attempt.WorktreeName || attempt.WorktreeName == "." || attempt.WorktreeName == ".." || stage.Runtime != "openshell" {
		return result, fmt.Errorf("canonical order/stage/attempt/session binding missing or ambiguous")
	}
	reader, ok := l.deps.Runtimes[stage.Runtime].(completedSessionReader)
	if !ok {
		return result, fmt.Errorf("original runtime has no session readback capability")
	}
	target, err := canonicalDirectory(filepath.Join(l.projectDir, ".worktrees", attempt.WorktreeName))
	if err != nil {
		return result, err
	}
	execution, err := reader.ReadCompletedSession(ctx, loopruntime.CompletedSessionBinding{OrderID: orderID, SessionID: sessionID, WorktreePath: target, Provider: stage.Provider, Model: stage.Model, StageIndex: stage.StageIndex, RetryCount: retry})
	if err != nil {
		return result, err
	}
	if execution.Sources == nil {
		return result, fmt.Errorf("original runtime returned no source evidence")
	}
	execution.Sources["canonical_snapshot"] = ref
	if err = loopruntime.VerifySessionReadbackFiles(execution.Sources); err != nil {
		return result, err
	}
	return SessionReadback{SchemaVersion: 1, Status: "observed", OrderID: orderID, StageIndex: stage.StageIndex, AttemptID: attempt.AttemptID, SessionID: sessionID, OrderStatus: string(order.Status), StageStatus: string(stage.Status), AttemptStatus: string(attempt.Status), Runtime: stage.Runtime, Provider: stage.Provider, Model: stage.Model, WorktreePath: target, Execution: execution, Effects: []string{}}, nil
}
