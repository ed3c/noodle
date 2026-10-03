package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/poteto/noodle/cmdmeta"
	"github.com/poteto/noodle/loop"
	"github.com/spf13/cobra"
)

func newInterruptionCmd(app *App) *cobra.Command {
	command := &cobra.Command{Use: "interruption", Short: cmdmeta.Short("interruption")}
	run := func(cmd *cobra.Command, args []string) error {
		binary, err := os.Executable()
		if err != nil {
			return err
		}
		var receipt loop.InterruptionInspection
		if len(args) == 2 {
			receipt = loop.InspectInterruption(app.projectDir, binary, args[0], args[1])
		} else {
			receipt = loop.PrepareInterruption(app.projectDir, binary, args[0], args[1], args[2])
		}
		if err := json.NewEncoder(cmd.OutOrStdout()).Encode(receipt); err != nil {
			return err
		}
		if receipt.Status == "refused" {
			return fmt.Errorf("interruption refused: %s", receipt.Invalid)
		}
		return nil
	}
	command.AddCommand(&cobra.Command{Use: "inspect ORDER_ID SUBJECT", Short: cmdmeta.Short("interruption", "inspect"), Args: cobra.ExactArgs(2), RunE: run}, &cobra.Command{Use: "prepare ORDER_ID SUBJECT CUSTODY_SHA256", Short: cmdmeta.Short("interruption", "prepare"), Args: cobra.ExactArgs(3), RunE: run})
	return command
}
