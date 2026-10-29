package main

import (
	"encoding/json"
	"errors"

	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/spf13/cobra"
)

func newCustodyRetirementMigrationCmd() *cobra.Command {
	var profilePath, expected string
	var apply bool
	cmd := &cobra.Command{Use: "migrate-retirement-schema", Short: "Inspect or migrate stopped original custody to retain native retirement receipts", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		profile, err := loopruntime.LoadOpenShellProfile(profilePath)
		if err != nil {
			return err
		}
		result, err := loopruntime.MigrateOpenShellCustodyRetirement(profile.Gateway.Binding, expected, apply)
		if result.Status != "" {
			err = errors.Join(err, json.NewEncoder(cmd.OutOrStdout()).Encode(result))
		}
		return err
	}}
	cmd.Flags().StringVar(&profilePath, "profile", "", "Original OpenShell profile")
	cmd.Flags().StringVar(&expected, "expected-owner-sha256", "", "Reviewed SHA256 of original owner.json")
	cmd.Flags().BoolVar(&apply, "apply", false, "Preserve before-image and migrate the stopped ledger")
	_ = cmd.MarkFlagRequired("profile")
	_ = cmd.MarkFlagRequired("expected-owner-sha256")
	return cmd
}
