package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/spf13/cobra"
)

type archiveRecoveryPausePlan struct {
	Schema    int                                `json:"schema"`
	Review    loopruntime.SessionReadbackFileRef `json:"review"`
	Capture   loopruntime.SessionReadbackFileRef `json:"capture"`
	Request   loopruntime.SessionReadbackFileRef `json:"request"`
	Host      string                             `json:"host"`
	ContextID string                             `json:"context_id"`
	AttemptID string                             `json:"attempt_id"`
}

type archiveRecoveryPauseReadback struct {
	Schema         int                                      `json:"schema"`
	Status         string                                   `json:"status"`
	Plan           loopruntime.SessionReadbackFileRef       `json:"plan"`
	DispatchHold   *loopruntime.ArchiveRecoveryDispatchHold `json:"dispatch_hold"`
	OriginalReturn string                                   `json:"original_return"`
}

func newPrepareOpenShellArchiveRecoveryPauseCmd() *cobra.Command {
	var review, expected string
	cmd := &cobra.Command{Use: "prepare-workspace-capture-stop", Short: "Request the original Loop pause once and observe its native acknowledgement", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
		defer cancel()
		var pause archiveRecoveryPauseReadback
		if _, err := runOriginalArchiveRecoveryControl(ctx, loopruntime.SessionReadbackFileRef{Path: review, SHA256: expected}, &pause, false); err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(pause)
	}}
	cmd.Flags().StringVar(&review, "review-file", "", "Original archive recovery review evidence")
	cmd.Flags().StringVar(&expected, "expected-sha256", "", "Exact reviewed FileRef SHA256")
	_ = cmd.MarkFlagRequired("review-file")
	_ = cmd.MarkFlagRequired("expected-sha256")
	return cmd
}

func prepareArchiveRecoveryPauseLocked(p loopruntime.ProgressMaintenance, review, capture loopruntime.SessionReadbackFileRef, attempt string) (archiveRecoveryPauseReadback, error) {
	result := archiveRecoveryPauseReadback{Schema: 1, Status: "archive_recovery_pause_ack_unknown", OriginalReturn: "UNKNOWN"}
	runtimeDir := filepath.Dir(filepath.Dir(p.SessionDirectory))
	plan := archiveRecoveryPausePlan{Schema: 1, Review: review, Capture: capture, Request: p.Request, Host: p.HostInstance, ContextID: p.ContextID, AttemptID: attempt}
	data, err := json.Marshal(plan)
	if err != nil {
		return result, err
	}
	planPath := filepath.Join(p.SessionDirectory, "archive-recovery-pause-plan.json")
	if err = writeAdmissionOnce(planPath, data); err != nil {
		return result, fmt.Errorf("archive recovery pause plan cannot be rebound: %w", err)
	}
	result.Plan, err = loopruntime.ReadSessionReadbackFile(planPath, nil)
	if err != nil {
		return result, err
	}
	intentPath := filepath.Join(p.SessionDirectory, "archive-recovery-pause-intent.json")
	if _, err = os.Lstat(intentPath); os.IsNotExist(err) {
		if hold, err := archiveRecoveryPaused(runtimeDir); err == nil {
			result.DispatchHold, result.Status = &hold, "archive_recovery_pause_observed"
			return result, nil
		}
		if info, err := os.Lstat(filepath.Join(runtimeDir, "orders-next.json")); err == nil && info.Size() != 0 {
			return result, fmt.Errorf("archive recovery original Loop has pending orders")
		} else if err != nil && !os.IsNotExist(err) {
			return result, err
		}
	}
	observed, err := legacyPauseCaptureLocked(runtimeDir, intentPath, false)
	if err != nil || !observed {
		return result, err
	}
	hold, err := archiveRecoveryPaused(runtimeDir)
	if err != nil {
		return result, err
	}
	result.DispatchHold, result.Status = &hold, "archive_recovery_pause_observed"
	return result, nil
}
