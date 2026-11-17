package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/poteto/noodle/dispatcher"
	"github.com/poteto/noodle/internal/workspaceartifacts"
)

type SourceAuthSavedInputClosure struct {
	Sources map[string]SessionReadbackFileRef `json:"sources"`
	Status  string                            `json:"status"`
	Error   string                            `json:"error,omitempty"`
	dispatcher.PreparedRemoteHarnessObservation
	BaselineHead    string `json:"baseline_head,omitempty"`
	BaselineEntries int    `json:"baseline_entries"`
}

func inspectSourceAuthSavedInputs(ctx context.Context, profile OpenShellProfile, receipt openShellDispatchReceipt, dispatchRef SessionReadbackFileRef) (result SourceAuthSavedInputClosure) {
	result.Status = "refused"
	result.Sources = map[string]SessionReadbackFileRef{}
	sources := result.Sources
	inspect := func() error {
		if receipt.Phase != "credential_waiting" || receipt.Worker != nil || receipt.Delivery != nil || receipt.Progress != nil || receipt.ProgressSource != nil || receipt.CandidateHead != "" || receipt.ProgressHead != "" {
			return fmt.Errorf("original dispatch is not a prepared pre-Create wait")
		}
		dir := filepath.Dir(dispatchRef.Path)
		if filepath.Base(dir) != receipt.SessionID {
			return fmt.Errorf("prepared session directory differs from original session")
		}
		if receipt.Baseline == nil {
			return fmt.Errorf("original captured baseline missing: %w", os.ErrNotExist)
		}
		if receipt.Baseline.Directory != filepath.Join(dir, "repo") {
			return fmt.Errorf("prepared repository is outside original session")
		}
		if err := workspaceartifacts.VerifyCapturedBaseline(ctx, receipt.Baseline); err != nil {
			return err
		}
		raw := func(role, name string) ([]byte, error) {
			ref, err := ReadSessionReadbackFile(filepath.Join(dir, name), nil)
			if err != nil {
				return nil, err
			}
			data, err := os.ReadFile(ref.Path)
			if err != nil {
				return nil, err
			}
			sum := sha256.Sum256(data)
			if hex.EncodeToString(sum[:]) != ref.SHA256 {
				return nil, fmt.Errorf("prepared input changed during read: %s", name)
			}
			sources[role] = ref
			return data, nil
		}
		spawn, err := raw("spawn", "spawn.json")
		if err != nil {
			return err
		}
		var metadata struct {
			Provider     string                           `json:"provider"`
			Instructions *dispatcher.InstructionBundleRef `json:"instruction_bundle"`
		}
		if err = json.Unmarshal(spawn, &metadata); err != nil {
			return err
		}
		route, ok := profile.Routes[metadata.Provider]
		if !ok {
			return fmt.Errorf("saved harness provider is outside original profile")
		}
		instructions, err := preparedInstructionEvidence(ctx, dir, receipt.OrderID, metadata.Instructions, sources)
		if err != nil {
			return err
		}
		input, err := raw("prepared_input", "input.txt")
		if err != nil {
			return err
		}
		prompt, err := raw("prepared_prompt", "prompt.txt")
		if err != nil {
			return err
		}
		startup, err := raw("prepared_startup", "codex-startup.json")
		if err != nil {
			return err
		}
		observed, err := dispatcher.InspectPreparedRemoteHarness(dispatcher.PreparedRemoteHarnessInput{Spawn: spawn, Startup: startup, Input: input, Prompt: prompt, Instructions: instructions}, dispatcher.PreparedRemoteHarnessBinding{OrderID: receipt.OrderID, SessionID: receipt.SessionID, WorktreePath: receipt.Target, Binary: route.Binary, InstructionVerifier: profile.InstructionVerifier})
		if err != nil {
			return err
		}
		if _, err = preparedInstructionEvidence(ctx, dir, receipt.OrderID, metadata.Instructions, sources); err != nil {
			return err
		}
		if err = workspaceartifacts.VerifyCapturedBaseline(ctx, receipt.Baseline); err != nil {
			return err
		}
		if err = VerifySessionReadbackFiles(sources); err != nil {
			return err
		}
		result.PreparedRemoteHarnessObservation = observed
		result.BaselineHead = receipt.Baseline.Head
		result.BaselineEntries = len(receipt.Baseline.Entries)
		return nil
	}
	if err := inspect(); err != nil {
		result.Error = err.Error()
		if errors.Is(err, os.ErrNotExist) {
			result.Status = "missing"
		}
		return result
	}
	result.Status = "verified"
	return result
}

// New preparation evidence has its own refusal boundary. Existing auth-only
// source drift still refuses the original proposal; closure drift cannot add a
// new prerequisite to that proposal or erase its original pending obligations.
func verifySourceAuthPlanSources(plan *SourceAuthPlan) error {
	if saved := plan.SavedInputClosure; saved != nil && saved.Status == "verified" {
		if err := VerifySessionReadbackFiles(saved.Sources); err != nil {
			saved.Status = "refused"
			saved.Error = err.Error()
			if errors.Is(err, os.ErrNotExist) {
				saved.Status = "missing"
			}
		}
	}
	return VerifySessionReadbackFiles(plan.Sources)
}
