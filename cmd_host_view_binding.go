package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/loopview"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
	"github.com/poteto/noodle/loop"
	"github.com/spf13/cobra"
)

type hostViewBinding struct {
	RunID   string             `json:"run_id"`
	OrderID string             `json:"order_id"`
	Request hostRequestBinding `json:"request"`
}

var viewIdentity = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

func viewBindingsPath(dir string) string { return filepath.Join(dir, "loop-view-bindings.json") }
func loadViewBindings(dir string) (map[string]hostViewBinding, error) {
	data, err := os.ReadFile(viewBindingsPath(dir))
	if os.IsNotExist(err) {
		return map[string]hostViewBinding{}, nil
	}
	if err != nil {
		return nil, err
	}
	var bindings map[string]hostViewBinding
	err = json.Unmarshal(data, &bindings)
	if bindings == nil {
		bindings = map[string]hostViewBinding{}
	}
	return bindings, err
}
func newHostViewBindCmd() *cobra.Command {
	var runID, orderID, requestFile string
	cmd := &cobra.Command{Use: "bind --run-id RUN --request-file FILE --order-id ORDER", Short: "Bind an imported display run to an original Host request without starting Host", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if !viewIdentity.MatchString(runID) || orderID == "" || requestFile == "" {
			return fmt.Errorf("run-id, order-id and request-file are required")
		}
		dir, err := hostDirectory()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(requestFile)
		if err != nil {
			return err
		}
		var request hostRequestBinding
		if err = json.Unmarshal(data, &request); err != nil {
			return err
		}
		if request.Schema != 1 || request.RequestID == "" || request.ConfigSHA256 == "" || request.BinarySHA256 == "" || request.HostDir != dir || request.Operation != "attach" {
			return fmt.Errorf("invalid original Host attach request")
		}
		lock, err := lockfile.TryLock(filepath.Join(dir, "loop-views.lock"))
		if err != nil {
			return err
		}
		defer lock.Close()
		view, err := loopview.LoadSummary(dir)
		if err != nil {
			return err
		}
		found := false
		for _, run := range view.Runs {
			if run.ID == runID {
				if run.Repository != request.ProjectDir {
					return fmt.Errorf("run repository differs from original Host request")
				}
				found = true
			}
		}
		if !found {
			return fmt.Errorf("run %q is not imported", runID)
		}
		bindings, err := loadViewBindings(dir)
		if err != nil {
			return err
		}
		// Mutable delivery status is not part of this display association.
		request.Status = ""
		binding := hostViewBinding{RunID: runID, OrderID: orderID, Request: request}
		if old, ok := bindings[runID]; ok && old != binding {
			if err := retainHostViewSuccessor(dir, old, binding); err != nil {
				return err
			}
		}
		bindings[runID] = binding
		data, err = json.Marshal(bindings)
		if err != nil {
			return err
		}
		if err = filex.WriteFileAtomicDurable(viewBindingsPath(dir), data); err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"schema_version": 1, "run_id": runID, "order_id": orderID, "request_id": request.RequestID, "display_only": true})
	}}
	cmd.Flags().StringVar(&runID, "run-id", "", "Imported run identity")
	cmd.Flags().StringVar(&requestFile, "request-file", "", "Original native Host attach request file")
	cmd.Flags().StringVar(&orderID, "order-id", "", "Original admitted Noodle order identity")
	return cmd
}

type hostViewObservation struct {
	Resources []loopview.Resource
	Receipts  []hostReceipt
}

func enrichHostView(dir string, view *loopview.View, observation hostViewObservation) error {
	view.Resources = observation.Resources
	bindings, err := loadViewBindings(dir)
	if err != nil {
		return err
	}
	for i := range view.Runs {
		run := &view.Runs[i]
		binding, ok := bindings[run.ID]
		if !ok {
			continue
		}
		observeErr := enrichNativeRun(run, binding, observation.Receipts)
		if observeErr != nil {
			run.Breakpoints = append(run.Breakpoints, loopview.Breakpoint{Kind: "native_readback", Status: "unknown", Message: observeErr.Error()})
		}
	}
	return nil
}
func enrichNativeRun(run *loopview.Run, binding hostViewBinding, receipts []hostReceipt) error {
	request := binding.Request
	if run.Repository != request.ProjectDir {
		return fmt.Errorf("native display repository binding changed")
	}
	matched := false
	for _, r := range receipts {
		if r.RequestID == request.RequestID && r.ProjectDir == request.ProjectDir && r.ConfigSHA256 == request.ConfigSHA256 && r.ConfigPath == request.ConfigPath && r.BinarySHA256 == request.BinarySHA256 && r.Mode == request.Mode {
			run.Resources = append(run.Resources, loopview.Resource{Label: "Native context", Value: r.ContextID + " · " + r.ContextState})
			if r.ContextState == "active" {
				matched = true
			}
			break
		}
	}
	if !matched {
		return fmt.Errorf("original native context is not attached; retained source projection only")
	}
	snapshotPath := filepath.Join(request.ProjectDir, ".noodle", "state.snapshot.json")
	snapshot, err := reducer.ReadSnapshot(snapshotPath)
	if err != nil {
		return fmt.Errorf("native checkpoint unavailable: %w", err)
	}
	order, ok := snapshot.State.Orders[binding.OrderID]
	if !ok {
		return fmt.Errorf("native order %q is not observed", binding.OrderID)
	}
	run.Resources = append(run.Resources, loopview.Resource{Label: "Native order status", Value: string(order.Status)}, loopview.Resource{Label: "Native mode", Value: string(snapshot.State.Mode)})
	// Native rows are rebuilt from the original checkpoint on every read.
	stages := make([]loopview.Stage, 0, len(run.Stages))
	for _, s := range run.Stages {
		if s.Kind != "native_stage" && s.Kind != "native_prompt" && s.Kind != "native_input" && s.Kind != "native_extra_prompt" && s.Kind != "native_attempt_feedback" && s.Kind != "native_review_feedback" {
			stages = append(stages, s)
		}
	}
	run.Stages = stages
	notes := make([]loopview.Note, 0, len(run.Notes))
	for _, note := range run.Notes {
		if !strings.HasPrefix(note.ID, "native_control_ack_") && !(strings.HasPrefix(note.ID, "native_stage_") && strings.HasSuffix(note.ID, "_extra_prompt_scope")) {
			notes = append(notes, note)
		}
	}
	run.Notes = notes
	breakpoints := make([]loopview.Breakpoint, 0, len(run.Breakpoints))
	for _, point := range run.Breakpoints {
		switch point.Kind {
		case "native_attempt", "native_review", "native_control", "native_control_readback", "native_readback":
		default:
			breakpoints = append(breakpoints, point)
		}
	}
	run.Breakpoints = breakpoints
	for si, stage := range order.Stages {
		id := fmt.Sprintf("native_stage_%d", si)
		run.Stages = append(run.Stages, loopview.Stage{ID: id, Kind: "native_stage", Status: string(stage.Status), Prompt: stage.Prompt, InputRefs: []string{snapshotPath}, OutputRefs: []string{}, ConsumerStatus: "UNKNOWN"})
		if stage.ExtraPrompt != "" {
			run.Stages = append(run.Stages, loopview.Stage{ID: id + "_extra_prompt", Kind: "native_extra_prompt", Status: "current_checkpoint", Prompt: stage.ExtraPrompt, InputRefs: []string{snapshotPath}, OutputRefs: []string{}, ConsumerStatus: "UNKNOWN"})
			run.Notes = append(run.Notes, loopview.Note{ID: id + "_extra_prompt_scope", Title: "Extra prompt 觀察範圍", Body: "這是目前 canonical extra_prompt；來源未保存各次 attempt 的版本。此列不證明歷史順序、語義改變或 consumer 已遵循。", SourceRefs: []string{snapshotPath}})
		}
		for ai, attempt := range stage.Attempts {
			if err := appendNativeAttemptInputs(run, request.ProjectDir, si, ai, attempt); err != nil {
				run.Breakpoints = append(run.Breakpoints, loopview.Breakpoint{Kind: "native_readback", Status: "unknown", StageID: id, Message: err.Error()})
			}
			// Keep each recorded outcome after its inputs and before the next attempt.
			// A later success does not erase a failed attempt or its review feedback.
			if attempt.Error != "" || attempt.Status == state.AttemptFailed {
				feedbackID := fmt.Sprintf("native_attempt_%d_%d_feedback", si, ai)
				run.Stages = append(run.Stages, loopview.Stage{ID: feedbackID, Kind: "native_attempt_feedback", Status: string(attempt.Status), Prompt: attempt.Error, InputRefs: []string{snapshotPath}, OutputRefs: []string{}, ConsumerStatus: "UNKNOWN"})
				message := attempt.Error
				if message == "" {
					message = "Native attempt failed; 原因未記錄（UNKNOWN）。"
				}
				run.Breakpoints = append(run.Breakpoints, loopview.Breakpoint{Kind: "native_attempt", Status: string(attempt.Status), StageID: feedbackID, Message: message})
			}
		}
		if review, ok := snapshot.State.PendingReviews[binding.OrderID]; ok && review.StageIndex == si {
			id := fmt.Sprintf("native_review_%d_feedback", si)
			run.Stages = append(run.Stages, loopview.Stage{ID: id, Kind: "native_review_feedback", Status: "pending", Prompt: review.Reason, InputRefs: []string{snapshotPath}, OutputRefs: []string{}, ConsumerStatus: "UNKNOWN"})
			run.Breakpoints = append(run.Breakpoints, loopview.Breakpoint{Kind: "native_review", Status: "pending", StageID: id, Message: review.Reason})
		}
	}
	if review, ok := snapshot.State.PendingReviews[binding.OrderID]; ok && (review.StageIndex < 0 || review.StageIndex >= len(order.Stages)) {
		run.Breakpoints = append(run.Breakpoints, loopview.Breakpoint{Kind: "native_review", Status: "unknown", Message: "Native pending review references an unobserved stage: " + review.Reason})
	}
	return appendNativeControlAcks(run, filepath.Join(request.ProjectDir, ".noodle", "control-ack.ndjson"))
}

func appendNativeAttemptInputs(run *loopview.Run, root string, si, ai int, attempt state.AttemptNode) error {
	if attempt.SessionID == "" {
		return nil
	}
	if !viewIdentity.MatchString(attempt.SessionID) {
		return fmt.Errorf("native session identity is invalid")
	}
	base := filepath.Join(root, ".noodle", "sessions", attempt.SessionID)
	for _, name := range []string{"prompt", "input"} {
		path := filepath.Join(base, name+".txt")
		data, err := readNativeViewFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		run.Stages = append(run.Stages, loopview.Stage{ID: fmt.Sprintf("native_attempt_%d_%d_%s", si, ai, name), Kind: "native_" + name, Status: string(attempt.Status), Prompt: string(data), InputRefs: []string{path}, OutputRefs: []string{filepath.Join(base, "raw.ndjson")}, ConsumerStatus: "input_recorded"})
	}
	return nil
}

func appendNativeControlAcks(run *loopview.Run, path string) error {
	data, err := readNativeViewFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for i, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var ack loop.ControlAck
		if err := json.Unmarshal(line, &ack); err != nil || strings.TrimSpace(ack.Action) == "" || strings.TrimSpace(ack.Status) == "" {
			run.Breakpoints = append(run.Breakpoints, loopview.Breakpoint{Kind: "native_control_readback", Status: "unknown", Message: fmt.Sprintf("native control ACK line %d is incomplete or invalid", i+1)})
			continue
		}
		// ACKs have no order identity. Keep them context-scoped, never infer an
		// order binding from a command ID or treat an ok ACK as effect completion.
		run.Notes = append(run.Notes, loopview.Note{ID: fmt.Sprintf("native_control_ack_%d", i), Title: "Native control ACK · " + ack.Action + " · " + ack.Status, Body: "同 control root 的 ACK；order 綁定 UNKNOWN。ACK 不證明 consumer 遵循或操作效果完成。\n" + string(line), SourceRefs: []string{path}})
		if ack.Status != "ok" {
			run.Breakpoints = append(run.Breakpoints, loopview.Breakpoint{Kind: "native_control", Status: ack.Status, Message: fmt.Sprintf("Context ACK %s · %s；order 綁定 UNKNOWN。%s", ack.ID, ack.Action, ack.Message)})
		}
	}
	return nil
}
func readNativeViewFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, loopview.MaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > loopview.MaxBytes {
		return nil, fmt.Errorf("native prompt exceeds display size bound")
	}
	return data, nil
}
