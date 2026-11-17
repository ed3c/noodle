package main

import (
	"context"
	"encoding/json"
	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/worktree"
	"github.com/spf13/cobra"
)

func newHostControlCmd() *cobra.Command {
	var taskFile string
	cmd := &cobra.Command{Use: "control", Short: "Prepare the original task admission checkout without changing its source", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			root, err := existingHostDirectory()
			if err != nil {
				return err
			}
			result, err := worktree.PrepareTaskControlWithAdmission(cmd.Context(), root, taskFile, func(ctx context.Context, plan worktree.TaskControl) (admission.PreparationRef, error) {
				return reserveTaskControlPreparation(ctx, root, plan)
			})
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
		}}
	cmd.Flags().StringVar(&taskFile, "task-file", "", "Original accepted task source")
	_ = cmd.MarkFlagRequired("task-file")
	return cmd
}
