package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/poteto/noodle/cmdmeta"
	"github.com/poteto/noodle/loop"
	"github.com/spf13/cobra"
)

func newAdmissionCmd(app *App) *cobra.Command {
	command := &cobra.Command{Use: "admission", Short: cmdmeta.Short("admission"), Long: cmdmeta.AdmissionRecoveryGuide}
	run := func(cmd *cobra.Command, args []string) error {
		binary, err := os.Executable()
		if err != nil {
			return err
		}
		var result loop.AdmissionInspection
		if len(args) == 0 {
			result = loop.InspectAdmission(app.projectDir, binary)
		} else {
			result = loop.RetireAdmission(app.projectDir, binary, args[0], args[1])
		}
		if err := json.NewEncoder(cmd.OutOrStdout()).Encode(result); err != nil {
			return err
		}
		if result.Status == "refused" {
			return fmt.Errorf("admission refused: %s", result.Invalid)
		}
		return nil
	}
	command.AddCommand(&cobra.Command{Use: "inspect", Short: cmdmeta.Short("admission", "inspect"), Args: cobra.NoArgs, RunE: run}, &cobra.Command{Use: "retire PROPOSAL_SHA256 CURRENT_ORDER_REVISION", Short: cmdmeta.Short("admission", "retire"), Args: cobra.ExactArgs(2), RunE: run})
	return command
}
