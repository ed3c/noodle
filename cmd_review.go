package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/poteto/noodle/cmdmeta"
	"github.com/poteto/noodle/loop"
	"github.com/spf13/cobra"
)

func newReviewCmd(app *App) *cobra.Command {
	command := &cobra.Command{Use: "review", Short: cmdmeta.Short("review"), Long: cmdmeta.StoppedReviewGuide}
	run := func(cmd *cobra.Command, args []string) error {
		binary, err := os.Executable()
		if err != nil {
			return err
		}
		var r loop.StoppedReviewInspection
		if len(args) == 2 {
			r = loop.InspectStoppedReview(app.projectDir, binary, args[0], args[1])
		} else {
			r = loop.RejectStoppedReview(app.projectDir, binary, args[0], args[1], args[2])
		}
		if err := json.NewEncoder(cmd.OutOrStdout()).Encode(r); err != nil {
			return err
		}
		if r.Status == "refused" {
			return fmt.Errorf("stopped review refused: %s", r.Invalid)
		}
		return nil
	}
	command.AddCommand(&cobra.Command{Use: "inspect ORDER_ID SUBJECT", Short: cmdmeta.Short("review", "inspect"), Args: cobra.ExactArgs(2), RunE: run}, &cobra.Command{Use: "reject ORDER_ID SUBJECT CUSTODY_SHA256", Short: cmdmeta.Short("review", "reject"), Args: cobra.ExactArgs(3), RunE: run})
	return command
}
