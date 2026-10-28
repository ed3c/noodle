package loop

import (
	"encoding/json"
	"fmt"
	loopruntime "github.com/poteto/noodle/runtime"

	"github.com/poteto/noodle/internal/state"
)

// Canonical attempts select the predecessor; shared worktree names do not.
func (l *Loop) resumeTaskSessionID(cand dispatchCandidate, attempt int) (string, error) {
	if !l.retainTaskData(cand.OrderID) || cand.Stage.Provider != "codex" || nonEmpty(cand.Stage.Runtime, "process") != "process" || attempt == 0 {
		return "", nil
	}
	order, ok := l.canonical.Orders[cand.OrderID]
	if !ok || cand.StageIndex < 0 || cand.StageIndex >= len(order.Stages) || attempt != len(order.Stages[cand.StageIndex].Attempts) {
		return "", fmt.Errorf("native continuation current attempt differs")
	}
	return l.resumeTaskPredecessor(cand, attempt)
}

func (l *Loop) resumeTaskPredecessor(cand dispatchCandidate, attempt int) (string, error) {
	if !l.retainTaskData(cand.OrderID) || cand.Stage.Provider != "codex" || nonEmpty(cand.Stage.Runtime, "process") != "process" || attempt == 0 {
		return "", nil
	}
	order, ok := l.canonical.Orders[cand.OrderID]
	if !ok || cand.StageIndex < 0 || cand.StageIndex >= len(order.Stages) {
		return "", fmt.Errorf("native continuation original stage missing")
	}
	stage := order.Stages[cand.StageIndex]
	if stage.TaskKey != cand.Stage.TaskKey || stage.Skill != cand.Stage.Skill || stage.Provider != cand.Stage.Provider || stage.Runtime != cand.Stage.Runtime || attempt < 0 || attempt > len(stage.Attempts) {
		return "", fmt.Errorf("native continuation task type or attempt differs")
	}
	prior := stage.Attempts[attempt-1]
	if prior.SessionID == "" || (prior.Status != state.AttemptCompleted && prior.Status != state.AttemptFailed && prior.Status != state.AttemptCancelled) {
		return "", fmt.Errorf("native continuation predecessor not terminal")
	}
	var prompt struct {
		Role *taskRoleFile `json:"task_role"`
	}
	if json.Unmarshal([]byte(cand.Stage.Prompt), &prompt) == nil && prompt.Role != nil {
		role, _, err := l.taskRole(cand)
		if err != nil {
			return "", err
		}
		for index := attempt - 1; index >= 0; index-- {
			item := stage.Attempts[index]
			if item.Status != state.AttemptCompleted && item.Status != state.AttemptFailed && item.Status != state.AttemptCancelled {
				return "", fmt.Errorf("task role prior attempt not terminal")
			}
			observed, err := loopruntime.ReadSessionTaskRole(l.runtimeDir, item.SessionID)
			if err != nil {
				return "", err
			}
			if observed == role {
				return item.SessionID, nil
			}
		}
		return "", nil
	}
	return prior.SessionID, nil
}
