package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/poteto/noodle/cmdmeta"
	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/loop"
	"github.com/spf13/cobra"
)

func newReviewCmd(app *App) *cobra.Command {
	command := &cobra.Command{Use: "review", Short: cmdmeta.Short("review"), Long: cmdmeta.StoppedReviewGuide + "\n\n" + cmdmeta.ReturnRecoveryGuide}
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
	command.AddCommand(&cobra.Command{Use: "prepare-local-merge-recovery CANDIDATE_PATH CANDIDATE_SHA BACKUP_DIR OUTPUT_PATH", Short: cmdmeta.Short("review", "prepare-local-merge-recovery"), Args: cobra.ExactArgs(4), RunE: func(cmd *cobra.Command, args []string) error {
		result, err := loop.PrepareLocalTaskMergeRecovery(args[0], args[1], args[2], args[3])
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
	}})
	command.AddCommand(&cobra.Command{Use: "prepare-local-completion-recovery ORDER_ID OUTPUT_PATH [PRIOR_PATH PRIOR_SHA PRIOR_CONTROL_ID]", Short: cmdmeta.Short("review", "prepare-local-completion-recovery"), Args: func(cmd *cobra.Command, args []string) error {
		if len(args) != 2 && len(args) != 5 {
			return fmt.Errorf("completion recovery requires two or five arguments")
		}
		return nil
	}, RunE: func(cmd *cobra.Command, args []string) error {
		result, err := loop.PrepareLocalTaskCompletionRecovery(app.projectDir, app.Config, args[0], args[1], args[2:]...)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
	}})
	command.AddCommand(newLandedLocalMergeCmd(app, false), newLandedLocalMergeCmd(app, true))
	for _, operation := range []string{"prepare", "inspect", "detach", "continue"} {
		command.AddCommand(newStoppedLocalMergeCmd(app, operation))
	}
	for _, operation := range []string{"prepare", "inspect", "continue"} {
		command.AddCommand(newReturnRecoveryCmd(app, operation))
	}
	return command
}

func newReturnRecoveryCmd(app *App, operation string) *cobra.Command {
	var expected string
	args, usage := 1, operation+"-return-recovery PLAN"
	if operation == "prepare" { args, usage = 4, "prepare-return-recovery HOST_REQUEST ORDER_ID EVENT_ID ORIGINAL_FRAME" }
	cmd := &cobra.Command{Use: usage, Short: cmdmeta.Short("review", operation+"-return-recovery"), Args: cobra.ExactArgs(args), RunE: func(cmd *cobra.Command, args []string) error {
		var r returnRecoveryResult
		var err error
		if operation == "prepare" {
			r, err = prepareReturnRecovery(app.projectDir, args[0], args[1], args[2], args[3])
		} else {
			r, err = operateReturnRecovery(app.projectDir, args[0], expected, operation, nil)
		}
		if err != nil { r.Error = err.Error() }
		return errors.Join(err, json.NewEncoder(cmd.OutOrStdout()).Encode(r))
	}}
	if operation == "continue" {
		cmd.Flags().StringVar(&expected, "expected-version", "", "Exact original plan SHA from inspection")
		_ = cmd.MarkFlagRequired("expected-version")
	}
	return cmd
}

type returnRecoveryResult struct {
	Schema           int                         `json:"schema"`
	Owner            string                      `json:"owner"`
	Status           string                      `json:"status"`
	Error            string                      `json:"error"`
	Plan             stoppedMergeRef             `json:"plan"`
	Subject          loop.ReturnRecoverySubject  `json:"subject"`
	Producer         config.RuntimeSource        `json:"original_producer"`
	ActualController stoppedMergeRef             `json:"actual_controller"`
	NoDelivery       string                      `json:"original_no_delivery"`
	Readback         *loop.ReturnCustodyReadback  `json:"custody_readback"`
	CustodyEvidence  stoppedMergeRef             `json:"custody_evidence"`
	Next             map[string]any              `json:"next"`
	Effects          []string                    `json:"effects"`
	MainWake         string                      `json:"main_native_wake"`
	Acceptance       string                      `json:"acceptance"`
}

func returnRecoveryBase() returnRecoveryResult {
	return returnRecoveryResult{Schema: 1, Owner: "Noodle original return owner", Status: "unknown", NoDelivery: "unknown", Next: map[string]any{"owner": "Noodle original return owner", "required": "original source-bound event/no-delivery readback"}, Effects: []string{}, MainWake: "NOT_OBSERVED", Acceptance: "NOT_RUN"}
}

func returnRecoveryFor(p returnRecoveryPlan, ref stoppedMergeRef) returnRecoveryResult {
	r := returnRecoveryBase()
	r.Plan, r.Subject, r.Producer, r.ActualController = ref, p.Event.Subject, p.Event.Subject.Packet.Producer, p.Controller
	r.NoDelivery = "dial_before_encode_no_delivery"
	r.Next = map[string]any{"owner": r.Owner, "operation": "inspect-return-recovery", "readback_argv": []string{p.Controller.Path, "--project-dir", p.Project, "review", "inspect-return-recovery", ref.Path}}
	return r
}


func returnRecoveryNext(r returnRecoveryResult, p returnRecoveryPlan, ref stoppedMergeRef) returnRecoveryResult {
	r.Next["operation"] = "continue-return-recovery"
	r.Next["argv"] = []string{p.Controller.Path, "--project-dir", p.Project, "review", "continue-return-recovery", ref.Path, "--expected-version", ref.SHA256}
	return r
}

