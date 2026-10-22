package loop

import (
	"fmt"

	"github.com/poteto/noodle/internal/state"
)

// Canonical attempts select the predecessor; shared worktree names do not.
func (l *Loop) resumeTaskSessionID(cand dispatchCandidate, attempt int) (string, error) {
	if !l.retainTaskData(cand.OrderID) || cand.Stage.Provider != "codex" || attempt == 0 {
		return "", nil
	}
	order, ok := l.canonical.Orders[cand.OrderID]
	if !ok || cand.StageIndex < 0 || cand.StageIndex >= len(order.Stages) {
		return "", fmt.Errorf("native continuation original stage missing")
	}
	stage := order.Stages[cand.StageIndex]
	if stage.TaskKey != cand.Stage.TaskKey || stage.Skill != cand.Stage.Skill || stage.Provider != cand.Stage.Provider || stage.Runtime != cand.Stage.Runtime || attempt != len(stage.Attempts) {
		return "", fmt.Errorf("native continuation task type or attempt differs")
	}
	prior := stage.Attempts[len(stage.Attempts)-1]
	if prior.SessionID == "" || (prior.Status != state.AttemptCompleted && prior.Status != state.AttemptFailed && prior.Status != state.AttemptCancelled) {
		return "", fmt.Errorf("native continuation predecessor not terminal")
	}
	return prior.SessionID, nil
}
