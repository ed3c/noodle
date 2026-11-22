package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/spf13/cobra"
)

const (
	archivePauseOperation = "prepare-workspace-capture-stop"
	archiveStopOperation  = "stop-workspace-capture"
	archiveApplyOperation = "apply-workspace-capture"
)

type archiveRecoveryNext struct {
	Owner      string                             `json:"owner"`
	Kind       string                             `json:"kind"`
	Operation  string                             `json:"operation"`
	Argv       []string                           `json:"argv"`
	Executable loopruntime.SessionReadbackFileRef `json:"executable"`
}

type archiveRecoveryContinuation struct {
	Next    *archiveRecoveryNext `json:"next"`
	WaitFor string               `json:"wait_for,omitempty"`
}

type archiveRecoveryPlanReadback struct {
	Schema                int                                      `json:"schema"`
	Status                string                                   `json:"status"`
	Review                loopruntime.SessionReadbackFileRef       `json:"review"`
	OriginalReview        *loopruntime.SessionReadbackFileRef      `json:"original_review,omitempty"`
	ReviewSuccessor       *loopruntime.SessionReadbackFileRef      `json:"review_successor,omitempty"`
	Capture               loopruntime.SessionReadbackFileRef       `json:"capture"`
	Request               loopruntime.SessionReadbackFileRef       `json:"request"`
	ContextID             string                                   `json:"context_id"`
	OrderID               string                                   `json:"order_id"`
	SessionID             string                                   `json:"session_id"`
	AttemptID             string                                   `json:"attempt_id"`
	NativeAttemptID       string                                   `json:"native_attempt_id"`
	SandboxID             string                                   `json:"sandbox_id"`
	MainProcessInstanceID string                                   `json:"main_process_instance_id"`
	DispatchHold          *loopruntime.ArchiveRecoveryDispatchHold `json:"dispatch_hold,omitempty"`
	OriginalReturn        string                                   `json:"original_return"`
	AuthorizesLanding     bool                                     `json:"authorizes_landing"`
	Effects               []string                                 `json:"effects"`
	archiveRecoveryContinuation
}

func newPlanOpenShellArchiveRecoveryCmd() *cobra.Command {
	var review, expected string
	cmd := &cobra.Command{Use: "plan-workspace-capture-recovery", Short: "Read the original archive recovery Owner's verified next action", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
		defer cancel()
		var plan archiveRecoveryPlanReadback
		if _, err := runOriginalArchiveRecoveryControl(ctx, loopruntime.SessionReadbackFileRef{Path: review, SHA256: expected}, archiveRecoveryControl{plan: &plan}); err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(plan)
	}}
	cmd.Flags().StringVar(&review, "review-file", "", "Original archive recovery review evidence")
	cmd.Flags().StringVar(&expected, "expected-sha256", "", "Exact reviewed FileRef SHA256")
	_ = cmd.MarkFlagRequired("review-file")
	_ = cmd.MarkFlagRequired("expected-sha256")
	return cmd
}

func archiveRecoveryNextAction(operation string, review loopruntime.SessionReadbackFileRef) (*archiveRecoveryNext, error) {
	path, err := os.Executable()
	if err != nil {
		return nil, err
	}
	binary, err := hostReadbackBinaryRef(path)
	if err != nil {
		return nil, err
	}
	return &archiveRecoveryNext{Owner: "Noodle", Kind: "executable", Operation: operation, Executable: binary,
		Argv: []string{binary.Path, "openshell", "progress-maintenance", operation,
			"--review-file", review.Path, "--expected-sha256", review.SHA256}}, nil
}

func archiveRecoveryEvidenceExists(path string) (bool, error) {
	file, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !file.Mode().IsRegular() {
		return false, fmt.Errorf("archive recovery original evidence is not a regular file: %s", path)
	}
	return true, nil
}

// An action that has already been reserved can only be reconciled by the Owner.
func archiveRecoveryOfferAfter(session string, review loopruntime.SessionReadbackFileRef, phase, operation string) (archiveRecoveryContinuation, error) {
	reserved, err := archiveRecoveryEvidenceExists(filepath.Join(session, "archive-recovery-"+phase+"-intent.json"))
	if err != nil {
		return archiveRecoveryContinuation{}, err
	}
	if reserved {
		return archiveRecoveryContinuation{WaitFor: "original_archive_recovery_" + phase + "_readback"}, nil
	}
	next, err := archiveRecoveryNextAction(operation, review)
	return archiveRecoveryContinuation{Next: next}, err
}

func archiveRecoveryPlanStage(session, runtimeDir string, expected archiveRecoveryPausePlan) (operation, wait string, stopped *loopruntime.ArchiveRecoveryDispatchHold, err error) {
	for _, phase := range []struct{ name, receipt string }{
		{"apply", "archive-recovery-candidate.json"}, {"stop", "archive-recovery-stop/archive-recovery-stop.json"},
	} {
		intent, err := archiveRecoveryEvidenceExists(filepath.Join(session, "archive-recovery-"+phase.name+"-intent.json"))
		if err != nil {
			return "", "", nil, err
		}
		receipt, err := archiveRecoveryEvidenceExists(filepath.Join(session, phase.receipt))
		if err != nil {
			return "", "", nil, err
		}
		if receipt && !intent {
			return "", "", nil, fmt.Errorf("archive recovery %s evidence lacks an original intent", phase.name)
		}
		if intent && phase.name == "apply" && receipt {
			return "", "original_archive_recovery_candidate_readback", nil, nil
		}
		if intent && !receipt {
			return "", "original_archive_recovery_" + phase.name + "_readback", nil, nil
		}
		if receipt {
			hold, err := archiveRecoveryPaused(runtimeDir)
			return archiveApplyOperation, "", &hold, err
		}
	}
	var plan archiveRecoveryPausePlan
	_, planErr := loopruntime.ReadSessionReadbackFile(filepath.Join(session, "archive-recovery-pause-plan.json"), &plan)
	if planErr == nil && plan != expected {
		return "", "", nil, fmt.Errorf("archive recovery original pause plan differs")
	}
	if planErr != nil && !os.IsNotExist(planErr) {
		return "", "", nil, planErr
	}
	var pause legacyModeIntent
	_, err = loopruntime.ReadSessionReadbackFile(filepath.Join(session, "archive-recovery-pause-intent.json"), &pause)
	if err == nil {
		if planErr != nil {
			return "", "", nil, fmt.Errorf("archive recovery pause intent lacks its original plan")
		}
		ack, err := os.ReadFile(filepath.Join(runtimeDir, "control-ack.ndjson"))
		if os.IsNotExist(err) {
			ack, err = nil, nil
		}
		if err != nil {
			return "", "", nil, err
		}
		observed, err := legacyPauseObserved(runtimeDir, pause, ack)
		if err != nil || !observed {
			return "", "original_archive_recovery_pause_readback", nil, err
		}
		hold, err := archiveRecoveryPaused(runtimeDir)
		if err != nil || hold.PauseCommandID != pause.Command.ID {
			return "", "original_archive_recovery_pause_readback", nil, err
		}
		return archiveStopOperation, "", &hold, nil
	}
	if !os.IsNotExist(err) {
		return "", "", nil, err
	}
	if hold, err := archiveRecoveryPaused(runtimeDir); err == nil {
		return archiveStopOperation, "", &hold, nil
	}
	status, err := os.ReadFile(filepath.Join(runtimeDir, "status.json"))
	if err != nil {
		return "", "", nil, err
	}
	_, state, err := readMaintenanceStatus(status)
	if err != nil || state != "running" {
		return "", "original_archive_recovery_pause_readback", nil, err
	}
	for _, name := range []string{"control.ndjson", "orders-next.json"} {
		data, err := os.ReadFile(filepath.Join(runtimeDir, name))
		if err != nil && !os.IsNotExist(err) {
			return "", "", nil, err
		}
		if len(data) != 0 {
			return "", "original_archive_recovery_pause_readback", nil, nil
		}
	}
	return archivePauseOperation, "", nil, nil
}

func inspectArchiveRecoveryPlan(ctx context.Context, p loopruntime.ProgressMaintenance, profile loopruntime.OpenShellProfile, reviewRef loopruntime.SessionReadbackFileRef, review archiveRecoveryReview, capture loopruntime.OpenShellWorkspaceCapture, verifyOwner func() error) (archiveRecoveryPlanReadback, error) {
	r := archiveRecoveryPlanReadback{Schema: 1, Status: "archive_recovery_waiting", Review: reviewRef,
		Capture: review.Capture, Request: p.Request, ContextID: p.ContextID, OrderID: capture.OrderID,
		SessionID: capture.SessionID, AttemptID: review.AttemptID, NativeAttemptID: capture.NativeAttemptID,
		SandboxID: capture.SandboxID, MainProcessInstanceID: capture.MainProcessInstanceID, OriginalReturn: "UNKNOWN", Effects: []string{}}
	runtimeDir := filepath.Dir(filepath.Dir(p.SessionDirectory))
	pausePlan := archiveRecoveryPausePlan{Schema: 1, Review: reviewRef, Capture: review.Capture, Request: p.Request,
		Host: p.HostInstance, ContextID: p.ContextID, AttemptID: review.AttemptID}
	if refreshed, handled, err := inspectExistingArchiveRecoveryRefresh(ctx, r, p, profile, pausePlan); handled || err != nil {
		return refreshed, err
	}
	operation, wait, hold, err := archiveRecoveryPlanStage(p.SessionDirectory, runtimeDir, pausePlan)
	if err != nil || operation == "" {
		r.WaitFor = wait
		return r, err
	}
	var stopped *loopruntime.ArchiveRecoveryDispatchHold
	if operation == archiveApplyOperation {
		stopped = hold
		if err := verifyRetainedArchiveRecoveryTree(ctx, review, loopruntime.InspectStoppedOpenShellWorkspaceTree); err != nil {
			return r, err
		}
	} else if err := verifyArchiveRecoveryReview(ctx, review); err != nil {
		if operation == archivePauseOperation && errors.Is(err, loopruntime.ErrWorkspaceCaptureCustodyChanged) {
			return planArchiveRecoveryRefresh(ctx, r, p, profile, reviewRef, review, pausePlan, verifyOwner)
		}
		return r, err
	}
	if err := verifyOwner(); err != nil {
		return r, err
	}
	if err := loopruntime.ObserveOpenShellArchiveRecovery(ctx, p, profile, reviewRef, review.Capture, stopped); err != nil {
		return r, err
	}
	if err := verifyOwner(); err != nil {
		return r, err
	}
	// Close the race between read-only observation and a concurrent reservation.
	fresh, wait, freshHold, err := archiveRecoveryPlanStage(p.SessionDirectory, runtimeDir, pausePlan)
	if err != nil || fresh != operation || ((hold == nil) != (freshHold == nil)) || (hold != nil && *freshHold != *hold) {
		r.WaitFor = wait
		if r.WaitFor == "" {
			r.WaitFor = "original_archive_recovery_stage_readback"
		}
		return r, err
	}
	r.Status = "archive_recovery_ready"
	r.DispatchHold = hold
	r.Next, err = archiveRecoveryNextAction(operation, reviewRef)
	return r, err
}

func planArchiveRecoveryRefresh(ctx context.Context, r archiveRecoveryPlanReadback, p loopruntime.ProgressMaintenance, profile loopruntime.OpenShellProfile, reviewRef loopruntime.SessionReadbackFileRef, review archiveRecoveryReview, pausePlan archiveRecoveryPausePlan, verifyOwner func() error) (archiveRecoveryPlanReadback, error) {
	if _, err := verifyArchiveRecoveryRefresh(ctx, p, profile, reviewRef, review, verifyOwner); err != nil {
		return r, err
	}
	fresh, wait, _, err := archiveRecoveryPlanStage(p.SessionDirectory, filepath.Dir(filepath.Dir(p.SessionDirectory)), pausePlan)
	if err != nil || fresh != archivePauseOperation {
		r.WaitFor = wait
		return r, err
	}
	if _, err := os.Lstat(archiveRecoveryRefreshIntentPath(p.SessionDirectory)); !os.IsNotExist(err) {
		r.WaitFor = archiveRefreshWait
		return r, nil
	}
	r.Status = "archive_recovery_ready"
	r.Next, err = archiveRecoveryNextAction(archiveRefreshOperation, reviewRef)
	return r, err
}
