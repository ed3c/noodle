package main

import (
	"context"
	"encoding/json"
	"time"

	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/spf13/cobra"
)

func newApplyOpenShellArchiveRecoveryCmd() *cobra.Command {
	var review, expected string
	cmd := &cobra.Command{Use: "apply-workspace-capture", Short: "Adopt the stopped original workspace as a local recovery review candidate", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
		defer cancel()
		candidate, err := runOriginalArchiveRecoveryControl(ctx, loopruntime.SessionReadbackFileRef{Path: review, SHA256: expected}, nil, true)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"schema": 1, "status": "archive_recovery_candidate_observed", "candidate": candidate, "original_return": "UNKNOWN", "authorizes_landing": false, "next": nil})
	}}
	cmd.Flags().StringVar(&review, "review-file", "", "Original archive recovery review evidence")
	cmd.Flags().StringVar(&expected, "expected-sha256", "", "Exact reviewed FileRef SHA256")
	_ = cmd.MarkFlagRequired("review-file")
	_ = cmd.MarkFlagRequired("expected-sha256")
	return cmd
}
