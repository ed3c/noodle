package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/poteto/noodle/cmdmeta"
	"github.com/poteto/noodle/loop"
	"github.com/spf13/cobra"
)

var inspectPublicationClaim = loop.InspectPublicationClaim

func newPublicationCmd(app *App) *cobra.Command {
	command := &cobra.Command{
		Use:   "publication",
		Short: cmdmeta.Short("publication"),
	}
	command.AddCommand(newPublicationClaimCmd(app))
	command.AddCommand(&cobra.Command{
		Use:   "reconcile CLAIM_PATH CLAIM_SHA256 MERGE_HEAD",
		Short: cmdmeta.Short("publication", "reconcile"),
		Args:  exactTrimmedArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			project, err := app.ProjectDir()
			if err != nil {
				return err
			}
			r, reconcileErr := loop.ReconcilePublication(project, args[0], args[1], args[2])
			if err := json.NewEncoder(cmd.OutOrStdout()).Encode(r); err != nil {
				return err
			}
			return reconcileErr
		},
	})
	return command
}

func newPublicationClaimCmd(app *App) *cobra.Command {
	var output string
	command := &cobra.Command{
		Use:   "claim ORDER_ID SUBJECT",
		Short: cmdmeta.Short("publication", "claim"),
		Args:  exactTrimmedArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			projectDir, err := app.ProjectDir()
			if err != nil {
				return err
			}
			claim, err := inspectPublicationClaim(projectDir, args[0], args[1])
			if err != nil {
				return err
			}
			encoded, err := json.MarshalIndent(claim, "", "  ")
			if err != nil {
				return err
			}
			encoded = append(encoded, '\n')
			if strings.TrimSpace(output) != "" {
				if !filepath.IsAbs(output) {
					return fmt.Errorf("publication claim output must be absolute")
				}
				file, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
				if err != nil {
					return fmt.Errorf("create publication claim output: %w", err)
				}
				if _, err := file.Write(encoded); err != nil {
					_ = file.Close()
					return fmt.Errorf("write publication claim output: %w", err)
				}
				if err := file.Close(); err != nil {
					return fmt.Errorf("close publication claim output: %w", err)
				}
			}
			_, err = cmd.OutOrStdout().Write(encoded)
			return err
		},
	}
	command.Flags().StringVar(&output, "output", "", "write the claim to a fresh absolute path as well as stdout")
	return command
}
