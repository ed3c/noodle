package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"strconv"

	"github.com/poteto/noodle/dispatcher"
	"github.com/poteto/noodle/internal/workspaceartifacts"
)

type SessionReadbackFileRef struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}
type CompletedSessionBinding struct {
	OrderID, SessionID, WorktreePath, Provider, Model string
	StageIndex, RetryCount                            int
}
type CompletedSessionReadback struct {
	ProfileSHA256         string                            `json:"profile_sha256"`
	CommandSHA256         string                            `json:"command_sha256"`
	Instructions          *CompletedInstructionEvidence     `json:"instructions"`
	NativeAttemptID       string                            `json:"native_attempt_id"`
	CandidateHead         string                            `json:"candidate_head"`
	NativeExecExitCode    int                               `json:"native_exec_exit_code"`
	NativePhase           string                            `json:"native_phase"`
	StopAcknowledged      bool                              `json:"stop_acknowledged"`
	MainProcessInstanceID string                            `json:"main_process_instance_id"`
	Sources               map[string]SessionReadbackFileRef `json:"sources"`
}

// ReadSessionReadbackFile binds bytes to a regular file without returning their
// contents in a public report. Callers retain refs and recheck after observation.
func ReadSessionReadbackFile(path string, value any) (SessionReadbackFileRef, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > 64<<20 {
		return SessionReadbackFileRef{}, fmt.Errorf("session evidence is not a bounded regular file: %s", path)
	}
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	data, err := os.ReadFile(canonical)
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	sum := sha256.Sum256(data)
	ref := SessionReadbackFileRef{Path: canonical, SHA256: hex.EncodeToString(sum[:])}
	if value != nil {
		if err = json.Unmarshal(data, value); err != nil {
			return SessionReadbackFileRef{}, fmt.Errorf("decode session evidence %s: %w", path, err)
		}
	}
	return ref, nil
}
func VerifySessionReadbackFiles(refs map[string]SessionReadbackFileRef) error {
	for name, ref := range refs {
		current, err := ReadSessionReadbackFile(ref.Path, nil)
		if err != nil {
			return err
		}
		if current != ref {
			return fmt.Errorf("session readback source changed: %s", name)
		}
	}
	return nil
}
func sessionReadbackHead(ctx context.Context, target string) (string, error) {
	return workspaceartifacts.ReadCleanHead(ctx, target)
}

// ReadCompletedSession borrows the existing runtime and original gateway. It
// never dispatches, acquires, recovers, stops, initializes or replays a session.
func (r *OpenShellRuntime) ReadCompletedSession(ctx context.Context, b CompletedSessionBinding) (CompletedSessionReadback, error) {
	return r.readCompletedSession(ctx, b, func(ctx context.Context, identity OpenShellIdentity) (OpenShellCompletedObservation, error) {
		return r.config.Gateway.ObserveCompleted(ctx, r.config.ContextID, identity)
	})
}
func (r *OpenShellRuntime) readCompletedSession(ctx context.Context, b CompletedSessionBinding, observe func(context.Context, OpenShellIdentity) (OpenShellCompletedObservation, error)) (CompletedSessionReadback, error) {
	result, err := r.readSettledSession(ctx, b, observe, false, nil)
	return result.completed, err
}

type settledSessionEvidence struct {
	completed       CompletedSessionReadback
	progressHead    string
	harnessExitCode int
	status          SessionStatus
}

func (r *OpenShellRuntime) readSettledSession(ctx context.Context, b CompletedSessionBinding, observe func(context.Context, OpenShellIdentity) (OpenShellCompletedObservation, error), progress bool, retained func(context.Context, openShellDispatchReceipt, string) error) (result settledSessionEvidence, err error) {
	if b.SessionID == "" || filepath.Base(b.SessionID) != b.SessionID || b.SessionID == "." || b.SessionID == ".." || b.OrderID == "" || b.Provider == "" || b.Model == "" || b.StageIndex < 0 || b.RetryCount < 0 || !filepath.IsAbs(b.WorktreePath) {
		return result, fmt.Errorf("session readback binding incomplete")
	}
	if !r.mu.TryLock() {
		return result, fmt.Errorf("OpenShell runtime operation in progress")
	}
	active := r.active[b.SessionID] != nil
	r.mu.Unlock()
	if active {
		return result, fmt.Errorf("original session has not settled")
	}
	if err = VerifyOpenShellProfile(r.config.Profile); err != nil {
		return result, err
	}
	profileSHA, err := OpenShellDispatchSHA256(r.config.Profile)
	if err != nil {
		return result, err
	}
	dir := filepath.Join(r.config.RuntimeDir, "sessions", b.SessionID)
	sources := map[string]SessionReadbackFileRef{}
	read := func(name string, value any) error {
		ref, e := ReadSessionReadbackFile(filepath.Join(dir, name), value)
		if e == nil {
			sources[name] = ref
		}
		return e
	}
	var receipt openShellDispatchReceipt
	if err = read("openshell-dispatch.json", &receipt); err != nil {
		return result, err
	}
	if receipt.ContextID != r.config.ContextID {
		if _, err := maintenanceCompletion(receipt); err != nil {
			return result, err
		}
	}
	if receipt.Maintenance != nil {
		if _, err := maintenanceCompletion(receipt); err != nil {
			return result, err
		}
		var marker SessionReadbackFileRef
		if err = read("maintenance-completion.json", &marker); err != nil {
			return result, err
		}
		if marker != *receipt.Maintenance {
			return result, fmt.Errorf("maintenance completion marker differs")
		}
	}
	if receipt.ProfileSHA256 != profileSHA || receipt.Version != 1 || receipt.OrderID != b.OrderID || receipt.SessionID != b.SessionID || receipt.Target != b.WorktreePath || receipt.AttemptID == "" || receipt.Phase != "settled" || receipt.Error != "" || receipt.Worker == nil || receipt.Baseline == nil {
		return result, fmt.Errorf("original OpenShell dispatch is not a bound successful settled result")
	}
	publishedHead := receipt.CandidateHead
	if progress {
		if (receipt.Outcome.Status != StatusCancelled && receipt.Outcome.Status != StatusFailed) || receipt.ProgressHead == "" || receipt.CandidateHead != "" {
			return result, fmt.Errorf("original dispatch is not saved interrupted progress")
		}
		publishedHead = receipt.ProgressHead
	} else if receipt.Outcome.Status != StatusCompleted || receipt.Outcome.ExitCode != 0 || receipt.CandidateHead == "" || receipt.ProgressHead != "" {
		return result, fmt.Errorf("original dispatch is not a completed candidate")
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
	if err = read("spawn.json", &spawn); err != nil {
		return result, err
	}
	if spawn.SessionID != b.SessionID || spawn.Provider != b.Provider || spawn.Model != b.Model || spawn.Runtime != "openshell" || spawn.WorktreePath != b.WorktreePath || spawn.OrderID != b.OrderID || spawn.StageIndex != strconv.Itoa(b.StageIndex) || spawn.RetryCount != b.RetryCount {
		return result, fmt.Errorf("original OpenShell spawn binding differs")
	}
	type terminalMetadata struct {
		SessionID   string `json:"session_id"`
		Status      string `json:"status"`
		Alive       *bool  `json:"alive"`
		Provider    string `json:"provider"`
		Model       string `json:"model"`
		Runtime     string `json:"runtime"`
		ExitPending bool   `json:"exit_pending"`
	}
	var meta terminalMetadata
	if err = read("meta.json", &meta); err != nil {
		return result, err
	}
	if meta.SessionID != b.SessionID || meta.Status != "exited" || meta.Alive == nil || *meta.Alive || meta.ExitPending || meta.Provider != b.Provider || meta.Model != b.Model || meta.Runtime != "openshell" {
		return result, fmt.Errorf("original OpenShell terminal metadata differs")
	}
	// Cleanup observes stable terminal identity, not the monitor's derived clock
	// and counters. Normal session readback retains its full snapshot contract.
	if retained != nil {
		delete(sources, "meta.json")
	}
	var delivery workspaceartifacts.Result
	if err = read("delivery/result.json", &delivery); err != nil {
		return result, err
	}
	if receipt.Delivery == nil || !reflect.DeepEqual(receipt.Delivery, &delivery) || !delivery.Applied || delivery.State != "published" || delivery.Error != "" || delivery.PatchPath != filepath.Join(dir, "delivery", "changes.patch") || delivery.ReturnedDirectory != filepath.Join(dir, "returned") {
		return result, fmt.Errorf("OpenShell artifact delivery is unresolved or differs")
	}
	if err = read("delivery/changes.patch", nil); err != nil {
		return result, err
	}
	var publication struct {
		Phase        string `json:"phase"`
		Parent       string `json:"parent"`
		ObservedHead string `json:"observed_head"`
		Error        string `json:"error"`
	}
	if err = read("delivery/publish.json", &publication); err != nil {
		return result, err
	}
	if publication.Phase != "published" || publication.Parent != receipt.Baseline.Head || publication.ObservedHead != publishedHead || publication.Error != "" {
		return result, fmt.Errorf("OpenShell candidate publication differs")
	}
	verifyLocal := func() error {
		if retained != nil {
			return retained(ctx, receipt, publishedHead)
		}
		head, err := sessionReadbackHead(ctx, b.WorktreePath)
		if err != nil {
			return err
		}
		if head != publishedHead {
			return fmt.Errorf("OpenShell candidate HEAD drifted")
		}
		return nil
	}
	if err = verifyLocal(); err != nil {
		return result, err
	}
	identity := receipt.Worker.Identity
	if identity.OrderID != b.OrderID || identity.SessionID != b.SessionID || identity.AttemptID != receipt.AttemptID || receipt.Worker.Boundary != OpenShellBoundaryStopped {
		return result, fmt.Errorf("OpenShell native attempt differs from dispatch")
	}
	if r.config.Gateway == nil {
		return result, fmt.Errorf("original OpenShell gateway unavailable")
	}
	var custody OpenShellCustodyState
	custodyRef, err := ReadSessionReadbackFile(filepath.Join(r.config.Gateway.spec.Binding.StateRoot, "owner.json"), &custody)
	if err != nil {
		return result, err
	}
	if custody.Version != openShellCustodyVersion || custody.Binding != r.config.Gateway.spec.Binding || custody.Blocked != "" {
		return result, fmt.Errorf("original OpenShell custody source differs")
	}
	count := 0
	var original OpenShellCustodyRecord
	for _, record := range custody.Records {
		if record.Worker.Identity == identity {
			original = record
			count++
		}
	}
	if count != 1 {
		return result, fmt.Errorf("original OpenShell completion source missing or ambiguous")
	}
	if receipt.Maintenance != nil {
		refs, err := maintainedSources(receipt, original)
		if err != nil {
			return result, err
		}
		maps.Copy(sources, refs)
	}
	validate := validateCompletedRecord
	if progress {
		validate = validateProgressRecord
	}
	if err = validate(original, identity); err != nil {
		return result, err
	}
	sources["native_custody"] = custodyRef
	observed, err := observe(ctx, identity)
	if err != nil {
		return result, err
	}
	if observed.Identity != identity || observed.Task != (OpenShellTask{b.OrderID, receipt.AttemptID, b.SessionID}) || observed.ExecExitCode != 0 || observed.Phase != "Stopped" || !observed.StopAcknowledged || !observed.CreateAcknowledged || observed.MainProcessInstanceID == "" || observed.MainProcessInstanceID != original.MainProcessInstanceID || observed.ExecAttemptID != original.Exec.AttemptID || observed.ExecRequestID != original.Exec.Command.RequestID {
		return result, fmt.Errorf("OpenShell native completion differs")
	}
	commandSHA256, err := OpenShellCommandSHA256(original.Exec.Command)
	if err != nil {
		return result, err
	}
	if observed.CommandSHA256 != commandSHA256 {
		return result, fmt.Errorf("OpenShell native command differs from original owner")
	}
	controlID, controlSHA := "", ""
	if original.ProgressStop != nil {
		controlID = original.ProgressStop.Command.RequestID
		controlSHA, err = OpenShellCommandSHA256(original.ProgressStop.Command)
		if err != nil {
			return result, err
		}
	}
	if observed.ProgressStopRequestID != controlID || observed.ProgressStopCommandSHA256 != controlSHA {
		return result, fmt.Errorf("OpenShell original progress control differs")
	}
	progressRef, err := readSavedProgress(dir, receipt, original.Exec.Command, controlID)
	if err != nil || (!progress && receipt.Progress.ExitCode != 0) || (err == nil && receipt.Progress.ExitCode != receipt.Outcome.ExitCode) {
		return result, errors.Join(err, fmt.Errorf("OpenShell saved harness progress is not successful"))
	}
	sources["progress_receipt"] = progressRef
	instructions, instructionSources, err := r.instructionReadback(ctx, dir, b, spawn.InstructionBundle, original.Exec.Command)
	if err != nil {
		return result, err
	}
	maps.Copy(sources, instructionSources)
	if retained != nil {
		var afterMeta terminalMetadata
		if _, err = ReadSessionReadbackFile(filepath.Join(dir, "meta.json"), &afterMeta); err != nil {
			return result, err
		}
		if !reflect.DeepEqual(meta, afterMeta) {
			return result, fmt.Errorf("OpenShell cleanup terminal metadata identity changed")
		}
	}
	if err = VerifySessionReadbackFiles(sources); err != nil {
		return result, err
	}
	if err = verifyLocal(); err != nil {
		return result, err
	}
	if instructions != nil {
		afterInstructions, afterSources, e := r.instructionReadback(ctx, dir, b, spawn.InstructionBundle, original.Exec.Command)
		if e != nil {
			return result, e
		}
		if !reflect.DeepEqual(instructions, afterInstructions) || !maps.Equal(instructionSources, afterSources) {
			return result, fmt.Errorf("OpenShell instruction evidence changed during readback")
		}
	}
	if err = VerifyOpenShellProfile(r.config.Profile); err != nil {
		return result, err
	}
	completed := CompletedSessionReadback{ProfileSHA256: profileSHA, CommandSHA256: commandSHA256, Instructions: instructions, NativeAttemptID: receipt.AttemptID, CandidateHead: receipt.CandidateHead, NativeExecExitCode: observed.ExecExitCode, NativePhase: observed.Phase, StopAcknowledged: observed.StopAcknowledged, MainProcessInstanceID: observed.MainProcessInstanceID, Sources: sources}
	return settledSessionEvidence{completed: completed, progressHead: receipt.ProgressHead, harnessExitCode: receipt.Progress.ExitCode, status: receipt.Outcome.Status}, nil
}
