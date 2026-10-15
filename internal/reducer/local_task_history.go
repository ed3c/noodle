package reducer

import (
	"encoding/json"
	"github.com/poteto/noodle/internal/state"
)

// A recovered merge completes an order without rewriting its failed writer.
func preservedLocalMergeAttempt(stage state.StageNode, attempt state.AttemptNode) bool {
	if stage.TaskKey != "local-task" || attempt.Status != state.AttemptFailed {
		return false
	}
	var binding struct {
		SessionID     string `json:"session_id"`
		AttemptID     string `json:"attempt_id"`
		OriginalError string `json:"original_error"`
	}
	return json.Unmarshal(stage.Extra[state.LocalTaskMergeRecoveryKey], &binding) == nil && binding.SessionID == attempt.SessionID && binding.AttemptID == attempt.AttemptID && binding.OriginalError != "" && binding.OriginalError == attempt.Error
}
