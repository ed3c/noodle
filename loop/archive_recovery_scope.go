package loop

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
	"github.com/poteto/noodle/internal/statever"
	"github.com/poteto/noodle/internal/workspaceartifacts"
)

// ArchiveRecoveryScope reads the original admitted task from its checkpoint. A
// retained archive can qualify for review while the original effect stays open.
func ArchiveRecoveryScope(cfg config.Config, snapshot reducer.DurableSnapshot, orderID, sessionID string) (workspaceartifacts.LocalTaskScope, state.AttemptNode, error) {
	var scope workspaceartifacts.LocalTaskScope
	var attempt state.AttemptNode
	order, ok := snapshot.State.Orders[orderID]
	if !ok || snapshot.State.SchemaVersion != statever.Current || order.OrderID != orderID || order.Status != state.OrderActive || len(order.Stages) != 1 {
		return scope, attempt, fmt.Errorf("archive inspection original active task is unavailable")
	}
	stage := order.Stages[0]
	if stage.Runtime != "openshell" || !(&Loop{config: cfg}).isAdmittedLocalTask(order, stage) || len(stage.Attempts) != 1 || stage.Merge != nil {
		return scope, attempt, fmt.Errorf("archive inspection requires the original admitted OpenShell attempt")
	}
	attempt = stage.Attempts[0]
	if attempt.SessionID != sessionID || attempt.AttemptID == "" || attempt.WorktreeName != cookBaseName(orderID, 0, stage.TaskKey) || filepath.Base(attempt.WorktreeName) != attempt.WorktreeName {
		return scope, attempt, fmt.Errorf("archive inspection canonical attempt differs")
	}
	if stage.Status != state.StageRunning || attempt.Status != state.AttemptRunning {
		return scope, attempt, fmt.Errorf("archive inspection requires the retained original running checkpoint")
	}
	var prompt struct {
		WritePaths     []string          `json:"write_paths"`
		Role           *taskRoleFile     `json:"task_role"`
		PriorCandidate *localTaskFileRef `json:"prior_candidate"`
	}
	if err := json.Unmarshal([]byte(stage.Prompt), &prompt); err != nil || len(prompt.WritePaths) == 0 || prompt.Role != nil || prompt.PriorCandidate != nil {
		return scope, attempt, fmt.Errorf("archive inspection requires the original initial task scope")
	}
	if err := validateSavedProgressLedger(snapshot, orderID); err != nil {
		return scope, attempt, err
	}
	scope.WritePaths = prompt.WritePaths
	return scope, attempt, nil
}
