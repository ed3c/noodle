package loop

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/poteto/noodle/internal/state"
	loopruntime "github.com/poteto/noodle/runtime"
)

type maintainedProgressReader interface {
	ReadMaintainedProgress(context.Context, loopruntime.CompletedSessionBinding) (bool, error)
}

func (l *Loop) reconcileMaintainedProgress(ctx context.Context) error {
	reader, ok := l.deps.Runtimes["openshell"].(maintainedProgressReader)
	if !ok {
		return nil
	}
	for _, order := range l.canonical.Orders {
		if len(order.Stages) != 1 || order.Status != state.OrderActive {
			continue
		}
		stage := order.Stages[0]
		if stage.Runtime != "openshell" || !l.isAdmittedLocalTask(order, stage) || len(stage.Attempts) == 0 || (stage.Status != state.StageRunning && stage.Status != state.StageDispatching) {
			continue
		}
		index := len(stage.Attempts) - 1
		a := stage.Attempts[index]
		if a.Status != state.AttemptRunning && a.Status != state.AttemptLaunching {
			return fmt.Errorf("maintenance canonical attempt is not the original unfinished attempt")
		}
		if a.WorktreeName == "" || filepath.Base(a.WorktreeName) != a.WorktreeName {
			return fmt.Errorf("maintenance original worktree identity differs")
		}
		bound := loopruntime.CompletedSessionBinding{OrderID: order.OrderID, SessionID: a.SessionID, WorktreePath: l.worktreePath(a.WorktreeName), Provider: stage.Provider, Model: stage.Model, StageIndex: 0, RetryCount: index}
		maintained, err := reader.ReadMaintainedProgress(ctx, bound)
		if err != nil {
			return err
		}
		if !maintained {
			continue
		}
		cook := &cookHandle{cookIdentity: cookIdentity{orderID: order.OrderID, stageIndex: 0, stage: Stage{TaskKey: stage.TaskKey, Prompt: stage.Prompt, Skill: stage.Skill, Provider: stage.Provider, Model: stage.Model, Runtime: stage.Runtime}}, session: &adoptedSession{id: a.SessionID, status: "cancelled"}, attempt: index, worktreeName: a.WorktreeName, worktreePath: bound.WorktreePath}
		if err := l.parkPendingReviewAttempt(cook, "OpenShell interrupted; original archived progress restored through explicit maintenance", state.AttemptCancelled); err != nil {
			return err
		}
	}
	return nil
}
