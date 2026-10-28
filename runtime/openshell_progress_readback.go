package runtime

import "context"

// SavedProgressReadback is a stopped interrupted execution, never task
// acceptance or original session resume. A new attempt needs separate consent.
type SavedProgressReadback struct {
	Status                string                            `json:"status"`
	OutcomeStatus         SessionStatus                     `json:"outcome_status"`
	HarnessExitCode       int                               `json:"harness_exit_code"`
	ProgressHead          string                            `json:"progress_head"`
	ProfileSHA256         string                            `json:"profile_sha256"`
	CommandSHA256         string                            `json:"command_sha256"`
	NativeAttemptID       string                            `json:"native_attempt_id"`
	NativePhase           string                            `json:"native_phase"`
	StopAcknowledged      bool                              `json:"stop_acknowledged"`
	MainProcessInstanceID string                            `json:"main_process_instance_id"`
	Instructions          *CompletedInstructionEvidence     `json:"instructions"`
	Sources               map[string]SessionReadbackFileRef `json:"sources"`
}

func (r *OpenShellRuntime) ReadSavedProgress(ctx context.Context, b CompletedSessionBinding) (SavedProgressReadback, error) {
	result, err := r.readSettledSession(ctx, b, func(ctx context.Context, identity OpenShellIdentity) (OpenShellCompletedObservation, error) {
		return r.config.Gateway.ObserveSavedProgress(ctx, r.config.ContextID, identity)
	}, true)
	if err != nil {
		return SavedProgressReadback{}, err
	}
	e := result.completed
	return SavedProgressReadback{Status: "saved", OutcomeStatus: result.status, HarnessExitCode: result.harnessExitCode, ProgressHead: result.progressHead, ProfileSHA256: e.ProfileSHA256, CommandSHA256: e.CommandSHA256, NativeAttemptID: e.NativeAttemptID, NativePhase: e.NativePhase, StopAcknowledged: e.StopAcknowledged, MainProcessInstanceID: e.MainProcessInstanceID, Instructions: e.Instructions, Sources: e.Sources}, nil
}
