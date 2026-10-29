package main

import (
	"encoding/json"

	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/spf13/cobra"
)

func newOpenShellCmd() *cobra.Command {
	root := &cobra.Command{Use: "openshell", Short: "Inspect and recover explicit local OpenShell configuration"}
	var profilePath, expected string
	var apply bool
	recover := &cobra.Command{Use: "recover-retained-init", Short: "Inspect the exact pre-start retained-initialization rejection; --apply repairs only that local block", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		profile, err := loopruntime.LoadOpenShellProfile(profilePath)
		if err != nil {
			return err
		}
		result, err := loopruntime.RecoverOpenShellRetainedInitialization(profile.Gateway.Binding, profile.Gateway.Retained, expected, apply)
		if result.Status != "" {
			if outputErr := json.NewEncoder(cmd.OutOrStdout()).Encode(result); outputErr != nil {
				return outputErr
			}
		}
		return err
	}}
	recover.Flags().StringVar(&profilePath, "profile", "", "Absolute original OpenShell profile path")
	recover.Flags().StringVar(&expected, "expected-owner-sha256", "", "Reviewed SHA-256 of the original owner.json bytes")
	recover.Flags().BoolVar(&apply, "apply", false, "Persist evidence and clear only the verified local rejection")
	_ = recover.MarkFlagRequired("profile")
	_ = recover.MarkFlagRequired("expected-owner-sha256")
	root.AddCommand(recover)
	root.AddCommand(newProgressMaintenanceCmd())
	root.AddCommand(newStorageFailureCmd())
	return root
}
