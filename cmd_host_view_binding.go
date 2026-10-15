package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"

	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/loopview"
	"github.com/poteto/noodle/internal/reducer"
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
		view, err := loopview.Load(dir)
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
			return fmt.Errorf("run %q already has a different native binding", runID)
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
		if r.RequestID == request.RequestID && r.ProjectDir == request.ProjectDir && r.ConfigSHA256 == request.ConfigSHA256 && r.BinarySHA256 == request.BinarySHA256 && r.Mode == request.Mode {
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
		if s.Kind != "native_stage" && s.Kind != "native_prompt" && s.Kind != "native_input" {
			stages = append(stages, s)
		}
	}
	run.Stages = stages
	for si, stage := range order.Stages {
		id := fmt.Sprintf("native_stage_%d", si)
		run.Stages = append(run.Stages, loopview.Stage{ID: id, Kind: "native_stage", Status: string(stage.Status), Prompt: stage.Prompt, InputRefs: []string{snapshotPath}, OutputRefs: []string{}, ConsumerStatus: "UNKNOWN"})
		for ai, attempt := range stage.Attempts {
			if attempt.SessionID == "" {
				continue
			}
			if !viewIdentity.MatchString(attempt.SessionID) {
				return fmt.Errorf("native session identity is invalid")
			}
			base := filepath.Join(request.ProjectDir, ".noodle", "sessions", attempt.SessionID)
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
		}
	}
	if review, ok := snapshot.State.PendingReviews[binding.OrderID]; ok {
		run.Breakpoints = append(run.Breakpoints, loopview.Breakpoint{Kind: "native_review", Status: "pending", StageID: fmt.Sprintf("native_stage_%d", review.StageIndex), Message: review.Reason})
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
