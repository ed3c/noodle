package runtime

import (
	"context"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"github.com/poteto/noodle/dispatcher"
	"github.com/poteto/noodle/internal/instructionbundle"
	"github.com/poteto/noodle/internal/workspaceartifacts"
	"google.golang.org/protobuf/proto"
)

type DiagnosticFailureReadback struct {
	ExecutionBasis string                         `json:"execution_basis"`
	Outcome        SessionOutcome                 `json:"outcome"`
	Observation    OpenShellCompletedObservation  `json:"observation"`
	Instructions   *DiagnosticInstructionEvidence `json:"instructions"`
}

type DiagnosticInstructionEvidence struct {
	Basis     string                 `json:"basis"`
	Directory string                 `json:"directory"`
	Manifest  SessionReadbackFileRef `json:"manifest"`
}

func (r *OpenShellRuntime) readDiagnosticFailure(ctx context.Context, b CompletedSessionBinding, repository string) (CleanupSessionReadback, error) {
	var result CleanupSessionReadback
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if b.SessionID == "" || b.SessionID == "." || b.SessionID == ".." || filepath.Base(b.SessionID) != b.SessionID || b.OrderID == "" || b.Provider == "" || b.Model == "" || b.StageIndex < 0 || b.RetryCount < 0 || !filepath.IsAbs(b.WorktreePath) {
		return result, fmt.Errorf("diagnostic session binding incomplete")
	}
	if !r.mu.TryLock() {
		return result, fmt.Errorf("OpenShell runtime operation in progress")
	}
	defer r.mu.Unlock()
	if len(r.active) != 0 {
		return result, fmt.Errorf("original diagnostic runtime is still active")
	}
	if err := VerifyOpenShellProfile(r.config.Profile); err != nil {
		return result, err
	}
	profileSHA, err := OpenShellDispatchSHA256(r.config.Profile)
	if err != nil {
		return result, err
	}
	dir := filepath.Join(r.config.RuntimeDir, "sessions", b.SessionID)
	sources, err := diagnosticSessionSources(dir)
	if err != nil {
		return result, err
	}
	var receipt openShellDispatchReceipt
	if _, err = ReadSessionReadbackFile(filepath.Join(dir, "openshell-dispatch.json"), &receipt); err != nil {
		return result, err
	}
	if receipt.Version != 1 || receipt.ContextID != r.config.ContextID || receipt.ProfileSHA256 != profileSHA || receipt.OrderID != b.OrderID || receipt.SessionID != b.SessionID || receipt.Target != b.WorktreePath || receipt.AttemptID == "" || receipt.Phase != "settled" || strings.TrimSpace(receipt.Error) == "" || receipt.Outcome.Status != StatusFailed || receipt.Outcome.ExitCode != -1 || receipt.Outcome.Reason != receipt.Error || receipt.Worker == nil || receipt.Baseline == nil || receipt.CandidateHead != "" || receipt.ProgressHead != "" || receipt.Progress != nil || receipt.ProgressSource != nil || receipt.Delivery != nil || receipt.Maintenance != nil {
		return result, fmt.Errorf("original dispatch is not a diagnosed settled pre-model failure")
	}
	if receipt.Baseline.Directory != filepath.Join(dir, "repo") {
		return result, fmt.Errorf("diagnostic original baseline path differs")
	}
	var spawn struct {
		InstructionBundle *dispatcher.InstructionBundleRef `json:"instruction_bundle"`
		SessionID         string                           `json:"session_id"`
		Provider          string                           `json:"provider"`
		Model             string                           `json:"model"`
		Runtime           string                           `json:"runtime"`
		WorktreePath      string                           `json:"worktree_path"`
		OrderID           string                           `json:"order_id"`
		StageIndex        string                           `json:"stage_index"`
		RetryCount        int                              `json:"retry_count"`
	}
	if _, err = ReadSessionReadbackFile(filepath.Join(dir, "spawn.json"), &spawn); err != nil {
		return result, err
	}
	if spawn.SessionID != b.SessionID || spawn.Provider != b.Provider || spawn.Model != b.Model || spawn.Runtime != "openshell" || spawn.WorktreePath != b.WorktreePath || spawn.OrderID != b.OrderID || spawn.StageIndex != strconv.Itoa(b.StageIndex) || spawn.RetryCount != b.RetryCount {
		return result, fmt.Errorf("diagnostic original spawn binding differs")
	}
	var meta struct {
		SessionID   string `json:"session_id"`
		Status      string `json:"status"`
		Alive       *bool  `json:"alive"`
		Provider    string `json:"provider"`
		Model       string `json:"model"`
		Runtime     string `json:"runtime"`
		ExitPending bool   `json:"exit_pending"`
	}
	if _, err = ReadSessionReadbackFile(filepath.Join(dir, "meta.json"), &meta); err != nil {
		return result, err
	}
	// Monitor reports stopped attempts without completion events as failed.
	// Both terminal statuses describe liveness; original custody below still
	// proves the failed/-1 outcome and that no model Exec was submitted.
	terminal := meta.Status == "failed" || meta.Status == "exited"
	if meta.SessionID != b.SessionID || !terminal || meta.Alive == nil || *meta.Alive || meta.ExitPending || meta.Provider != b.Provider || meta.Model != b.Model || meta.Runtime != "openshell" {
		return result, fmt.Errorf("diagnostic original terminal metadata differs")
	}
	instructions, err := diagnosticInstructions(ctx, dir, b, spawn.InstructionBundle)
	if err != nil {
		return result, err
	}
	identity := receipt.Worker.Identity
	if identity.OrderID != b.OrderID || identity.AttemptID != receipt.AttemptID || identity.SessionID != b.SessionID || receipt.Worker.Boundary != OpenShellBoundaryStopped || receipt.Worker.ExecResult != (OpenShellExecResult{}) || receipt.Worker.StopPending || receipt.Worker.StopError != "" {
		return result, fmt.Errorf("diagnostic original native worker differs")
	}
	if r.config.Gateway == nil {
		return result, fmt.Errorf("diagnostic original gateway unavailable")
	}
	var custody OpenShellCustodyState
	custodyRef, err := ReadSessionReadbackFile(filepath.Join(r.config.Gateway.spec.Binding.StateRoot, "owner.json"), &custody)
	if err != nil {
		return result, err
	}
	if custody.Version != openShellCustodyVersion || custody.Binding != r.config.Gateway.spec.Binding || custody.Blocked != "" {
		return result, fmt.Errorf("diagnostic original custody source differs")
	}
	var original OpenShellCustodyRecord
	count := 0
	for _, record := range custody.Records {
		if record.Worker.Identity == identity {
			original = record
			count++
		}
	}
	if count != 1 {
		return result, fmt.Errorf("diagnostic original custody identity missing or ambiguous")
	}
	if err = validateNotStartedRecord(original, identity); err != nil {
		return result, err
	}
	var intent pb.CreateSandboxRequest
	route, ok := r.config.Profile.Routes[b.Provider]
	if err = proto.Unmarshal(original.CreateIntent, &intent); err != nil || !ok || !proto.Equal(intent.GetSpec(), route.Spec) || intent.GetName() != original.Name || intent.GetRequestId() != original.CreateRequestID || intent.GetWorkspaceScope().GetWorkspace() != custody.Binding.Workspace {
		return result, fmt.Errorf("diagnostic original create intent differs from dispatch profile")
	}
	// The file cannot invent the absence of submission. Compare the complete
	// record with the living original owner before asking it for a fresh read.
	if r.config.Gateway.custody == nil {
		return result, fmt.Errorf("diagnostic original custody owner unavailable")
	}
	matches := 0
	for _, record := range r.config.Gateway.custody.Snapshot().Records {
		if reflect.DeepEqual(record, original) {
			matches++
		}
	}
	if matches != 1 {
		return result, fmt.Errorf("diagnostic custody file differs from original owner")
	}
	baselineSources := diagnosticBaselineSources(sources)
	if err = workspaceartifacts.VerifyDiagnosticBaseline(ctx, repository, receipt.Baseline, b.WorktreePath, baselineSources); err != nil {
		return result, err
	}
	observed, err := r.config.Gateway.observeDiagnosticFailure(ctx, r.config.ContextID, identity)
	if err != nil {
		return result, err
	}
	if !reflect.DeepEqual(observed, notStartedObservation(original)) {
		return result, fmt.Errorf("diagnostic native not-started observation differs")
	}
	afterSources, err := diagnosticSessionSources(dir)
	if err != nil {
		return result, err
	}
	if !maps.Equal(sources, afterSources) {
		return result, fmt.Errorf("diagnostic session evidence changed during observation")
	}
	afterInstructions, err := diagnosticInstructions(ctx, dir, b, spawn.InstructionBundle)
	if err != nil {
		return result, err
	}
	if !reflect.DeepEqual(instructions, afterInstructions) {
		return result, fmt.Errorf("diagnostic prepared instruction evidence changed")
	}
	sources["native_custody"] = custodyRef
	if err = VerifySessionReadbackFiles(sources); err != nil {
		return result, err
	}
	if err = workspaceartifacts.VerifyDiagnosticBaseline(ctx, repository, receipt.Baseline, b.WorktreePath, baselineSources); err != nil {
		return result, err
	}
	if err = VerifyOpenShellProfile(r.config.Profile); err != nil {
		return result, err
	}
	result = CleanupSessionReadback{OutcomeStatus: StatusFailed, PublishedHead: receipt.Baseline.Head, Execution: CompletedSessionReadback{ProfileSHA256: profileSHA, NativeAttemptID: receipt.AttemptID, NativeExecExitCode: -1, NativePhase: "Stopped", StopAcknowledged: true, MainProcessInstanceID: observed.MainProcessInstanceID, Sources: sources}, Diagnostic: &DiagnosticFailureReadback{ExecutionBasis: openShellNotStarted, Outcome: receipt.Outcome, Observation: observed, Instructions: instructions}}
	if v := original.Retirement; v != nil {
		if !v.Acknowledged || !v.Completed {
			return CleanupSessionReadback{}, fmt.Errorf("diagnostic native retirement remains unresolved")
		}
		result.Retirement = &CleanupRetirementReadback{ObservationBasis: "historical_not_started_stopped_with_fresh_native_absence", Closure: v.Closure, DeleteRequestID: v.RequestID, DeleteAcknowledged: true, AbsenceObserved: true}
	}
	return result, nil
}

// Freeze every original local diagnostic, including logs and prepared inputs.
// Only repo is a deletable copy; any output/delivery evidence refuses this path.
func diagnosticSessionSources(dir string) (map[string]SessionReadbackFileRef, error) {
	sources := map[string]SessionReadbackFileRef{}
	canonical, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	if canonical != dir {
		return nil, fmt.Errorf("diagnostic session path is redirected")
	}
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "repo" {
			return filepath.SkipDir
		}
		if rel == "delivery" || rel == "returned" || strings.HasPrefix(rel, "progress-") {
			return fmt.Errorf("diagnostic session contains delivery or progress evidence")
		}
		if rel == workspaceartifacts.DiagnosticBaselineCompactionName || rel == "execution-closure.json" {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		ref, err := ReadSessionReadbackFile(path, nil)
		if err != nil {
			return err
		}
		if ref.Path != path {
			return fmt.Errorf("diagnostic evidence path redirected")
		}
		sources["diagnostic_session/"+filepath.ToSlash(rel)] = ref
		return nil
	})
	return sources, err
}

func diagnosticBaselineSources(sources map[string]SessionReadbackFileRef) map[string]string {
	result := map[string]string{}
	for name, ref := range sources {
		if name != "native_custody" {
			result[ref.Path] = ref.SHA256
		}
	}
	return result
}

func diagnosticInstructions(ctx context.Context, dir string, b CompletedSessionBinding, ref *dispatcher.InstructionBundleRef) (*DiagnosticInstructionEvidence, error) {
	mappingPath := filepath.Join(dir, "instruction-mapping.json")
	if ref == nil {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "instructions-") || e.Name() == "instruction-mapping.json" {
				return nil, fmt.Errorf("diagnostic instructions lack original spawn binding")
			}
		}
		return nil, nil
	}
	directory := filepath.Join(dir, "instructions-"+ref.SHA256)
	manifest, err := instructionbundle.Verify(ctx, directory, ref.SHA256, b.OrderID)
	if err != nil {
		return nil, err
	}
	var mapping struct {
		OriginalManifest dispatcher.InstructionBundleRef `json:"original_manifest"`
		Prepared         *instructionbundle.Prepared     `json:"prepared"`
	}
	if _, err = ReadSessionReadbackFile(mappingPath, &mapping); err != nil {
		return nil, err
	}
	p := mapping.Prepared
	if mapping.OriginalManifest != *ref || p == nil || p.Directory != directory || p.GuestDirectory != "/sandbox/"+filepath.Base(directory) || p.ManifestSHA256 != ref.SHA256 || !reflect.DeepEqual(p.Manifest, manifest) {
		return nil, fmt.Errorf("diagnostic locally prepared instruction mapping differs")
	}
	input, err := os.ReadFile(filepath.Join(dir, "input.txt"))
	if err != nil {
		return nil, err
	}
	if !strings.Contains(string(input), p.PromptSection()) {
		return nil, fmt.Errorf("diagnostic input lacks prepared instruction mapping")
	}
	manifestRef, err := ReadSessionReadbackFile(filepath.Join(directory, instructionbundle.ManifestName), nil)
	if err != nil {
		return nil, err
	}
	if manifestRef.SHA256 != ref.SHA256 {
		return nil, fmt.Errorf("diagnostic prepared manifest differs")
	}
	return &DiagnosticInstructionEvidence{Basis: "locally_prepared_upload_unproven", Directory: directory, Manifest: manifestRef}, nil
}
