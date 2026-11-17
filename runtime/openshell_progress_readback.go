package runtime

import (
	"context"
	"fmt"
	"path/filepath"
)

// SavedProgressReadback is a stopped interrupted execution, never task
// acceptance or original session resume. A new attempt needs separate consent.
type SavedProgressReadback struct {
	BaselineHead          string                            `json:"baseline_head,omitempty"`
	OriginalError         string                            `json:"original_error,omitempty"`
	PreparedManifest      *SessionReadbackFileRef           `json:"prepared_manifest,omitempty"`
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
	if b.SessionID == "" || filepath.Base(b.SessionID) != b.SessionID || b.SessionID == "." || b.SessionID == ".." {
		return SavedProgressReadback{}, fmt.Errorf("session readback binding incomplete")
	}
	var receipt openShellDispatchReceipt
	if _, err := ReadSessionReadbackFile(filepath.Join(r.config.RuntimeDir, "sessions", b.SessionID, "openshell-dispatch.json"), &receipt); err != nil {
		return SavedProgressReadback{}, err
	}
	if receipt.Error != "" {
		return r.readNotStartedSession(ctx, b)
	}
	result, err := r.readSettledSession(ctx, b, func(ctx context.Context, identity OpenShellIdentity) (OpenShellCompletedObservation, error) {
		return r.config.Gateway.ObserveSavedProgress(ctx, r.config.ContextID, identity)
	}, true, nil)
	if err != nil {
		return SavedProgressReadback{}, err
	}
	e := result.completed
	return SavedProgressReadback{Status: "saved", OutcomeStatus: result.status, HarnessExitCode: result.harnessExitCode, ProgressHead: result.progressHead, ProfileSHA256: e.ProfileSHA256, CommandSHA256: e.CommandSHA256, NativeAttemptID: e.NativeAttemptID, NativePhase: e.NativePhase, StopAcknowledged: e.StopAcknowledged, MainProcessInstanceID: e.MainProcessInstanceID, Instructions: e.Instructions, Sources: e.Sources}, nil
}
