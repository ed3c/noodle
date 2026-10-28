package main

import (
	"encoding/json"
	"errors"

	"github.com/poteto/noodle/cmdmeta"
	"github.com/poteto/noodle/loop"
	"github.com/spf13/cobra"
)

func newLandedLocalMergeCmd(app *App, apply bool) *cobra.Command {
	name := "inspect-landed-local-merge"
	if apply {
		name = "reconcile-landed-local-merge"
	}
	var expected string
	cmd := &cobra.Command{Use: name + " PROOF_PATH PROOF_SHA256", Short: cmdmeta.Short("review", name), Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		var result loop.LandedLocalMergeResult
		var err error
		if apply {
			result, err = loop.ReconcileLandedLocalMerge(app.projectDir, args[0], args[1], expected)
		} else {
			result, err = loop.InspectLandedLocalMerge(app.projectDir, args[0], args[1])
		}
		return errors.Join(err, json.NewEncoder(cmd.OutOrStdout()).Encode(result))
	}}
	if apply {
		cmd.Flags().StringVar(&expected, "expected-version", "", "Exact version returned by stopped inspection")
		_ = cmd.MarkFlagRequired("expected-version")
	}
	return cmd
}
