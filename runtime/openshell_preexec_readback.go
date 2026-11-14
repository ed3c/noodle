package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"

	"github.com/poteto/noodle/dispatcher"
	"github.com/poteto/noodle/internal/instructionbundle"
	"github.com/poteto/noodle/internal/workspaceartifacts"
)

// Absence of the durable StartOwned intent, not the worker's diagnostic zero
// value, proves that this original owner did not submit the model command.
func validateNotStartedRecord(record OpenShellCustodyRecord, expected OpenShellIdentity) error {
	if err := validateStoppedIdentity(record, expected); err != nil {
		return err
	}
	if record.Exec != nil || record.ProgressStop != nil || record.Worker.ExecResult != (OpenShellExecResult{}) {
		return fmt.Errorf("openshell original execution has an intent or conflicting result")
	}
	return nil
}

// readNotStartedSession observes a stopped pre-exec failure. It cannot grant a
// new attempt and does not reinterpret the original failure as saved progress.
func (r *OpenShellRuntime) readNotStartedSession(ctx context.Context, b CompletedSessionBinding) (SavedProgressReadback, error) {
	fail := func(err error) (SavedProgressReadback, error) { return SavedProgressReadback{}, err }
	if b.OrderID == "" || b.Provider == "" || b.Model == "" || b.StageIndex < 0 || b.RetryCount < 0 || !filepath.IsAbs(b.WorktreePath) {
		return fail(fmt.Errorf("pre-exec session binding incomplete"))
	}
	if !r.mu.TryLock() {
		return fail(fmt.Errorf("OpenShell runtime operation in progress"))
	}
	active := r.active[b.SessionID] != nil
	r.mu.Unlock()
	if active {
		return fail(fmt.Errorf("original session has not settled"))
	}
	if err := VerifyOpenShellProfile(r.config.Profile); err != nil {
		return fail(err)
	}
	profileSHA, err := OpenShellDispatchSHA256(r.config.Profile)
	if err != nil {
		return fail(err)
	}
	dir := filepath.Join(r.config.RuntimeDir, "sessions", b.SessionID)
	sources := map[string]SessionReadbackFileRef{}
	read := func(name string, value any) error {
		ref, err := ReadSessionReadbackFile(filepath.Join(dir, name), value)
		if err == nil {
			sources[name] = ref
		}
		return err
	}
	var receipt openShellDispatchReceipt
	if err = read("openshell-dispatch.json", &receipt); err != nil {
		return fail(err)
	}
	if receipt.Version != 1 || receipt.ContextID != r.config.ContextID || receipt.Maintenance != nil || receipt.ProfileSHA256 != profileSHA || receipt.OrderID != b.OrderID || receipt.SessionID != b.SessionID || receipt.Target != b.WorktreePath || receipt.AttemptID == "" || receipt.Phase != "settled" || receipt.Error == "" || receipt.Outcome.Status != StatusFailed || receipt.Outcome.HasDeliverable || receipt.Worker == nil || receipt.Baseline == nil || receipt.Baseline.Head == "" || receipt.ProgressHead != "" || receipt.CandidateHead != "" || receipt.Delivery != nil || receipt.Progress != nil || receipt.ProgressSource != nil {
		return fail(fmt.Errorf("original dispatch is not a bound stopped pre-exec failure"))
	}
	identity := receipt.Worker.Identity
	if identity.OrderID != b.OrderID || identity.AttemptID != receipt.AttemptID || identity.SessionID != b.SessionID || receipt.Worker.Boundary != OpenShellBoundaryStopped || receipt.Worker.StopPending || receipt.Worker.StopError != "" || receipt.Worker.ExecResult != (OpenShellExecResult{}) {
		return fail(fmt.Errorf("pre-exec dispatch worker binding differs"))
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
		return fail(err)
	}
	if spawn.SessionID != b.SessionID || spawn.Provider != b.Provider || spawn.Model != b.Model || spawn.Runtime != "openshell" || spawn.WorktreePath != b.WorktreePath || spawn.OrderID != b.OrderID || spawn.StageIndex != strconv.Itoa(b.StageIndex) || spawn.RetryCount != b.RetryCount {
		return fail(fmt.Errorf("original pre-exec spawn binding differs"))
	}
	var meta struct {
		SessionID   string          `json:"session_id"`
		Provider    json.RawMessage `json:"provider"`
		Model       json.RawMessage `json:"model"`
		Runtime     json.RawMessage `json:"runtime"`
		Status      string          `json:"status"`
		Alive       *bool           `json:"alive"`
		ExitPending json.RawMessage `json:"exit_pending"`
	}
	if err = read("meta.json", &meta); err != nil {
		return fail(err)
	}
	// Before plan.Start there may be no canonical events. The original monitor
	// then emits failed/alive=false and omits these optional identity fields.
	optionalIdentity := func(raw json.RawMessage, expected string) bool {
		if raw == nil {
			return true
		}
		var actual string
		return json.Unmarshal(raw, &actual) == nil && actual == expected
	}
	if meta.ExitPending != nil {
		var pending *bool
		if err = json.Unmarshal(meta.ExitPending, &pending); err != nil || pending == nil || *pending {
			return fail(fmt.Errorf("original pre-exec terminal exit is unresolved"))
		}
	}
	if meta.SessionID != b.SessionID || !optionalIdentity(meta.Provider, b.Provider) || !optionalIdentity(meta.Model, b.Model) || !optionalIdentity(meta.Runtime, "openshell") || (meta.Status != "exited" && meta.Status != "failed") || meta.Alive == nil || *meta.Alive {
		return fail(fmt.Errorf("original pre-exec terminal metadata differs"))
	}
	prepared, err := preparedInstructionReadback(ctx, dir, b.OrderID, spawn.InstructionBundle, sources)
	if err != nil {
		return fail(err)
	}
	verifyLocal := func() error {
		if err := workspaceartifacts.VerifyBaseline(ctx, b.WorktreePath, receipt.Baseline); err != nil {
			return err
		}
		head, err := sessionReadbackHead(ctx, b.WorktreePath)
		if err != nil {
			return err
		}
		if head != receipt.Baseline.Head {
			return fmt.Errorf("pre-exec worktree HEAD differs from original baseline")
		}
		return nil
	}
	if err = verifyLocal(); err != nil {
		return fail(err)
	}
	if r.config.Gateway == nil {
		return fail(fmt.Errorf("original OpenShell gateway unavailable"))
	}
	var custody OpenShellCustodyState
	ref, err := ReadSessionReadbackFile(filepath.Join(r.config.Gateway.spec.Binding.StateRoot, "owner.json"), &custody)
	if err != nil {
		return fail(err)
	}
	if !openShellCustodySupportsProfile(custody, r.config.Gateway.spec) || custody.Binding != r.config.Gateway.spec.Binding || custody.Blocked != "" {
		return fail(fmt.Errorf("original pre-exec custody source differs"))
	}
	var original OpenShellCustodyRecord
	count := 0
	for _, row := range custody.Records {
		if row.Worker.Identity == identity {
			original = row
			count++
		}
	}
	if count != 1 {
		return fail(fmt.Errorf("original pre-exec custody identity missing or ambiguous"))
	}
	if err = validateNotStartedRecord(original, identity); err != nil {
		return fail(err)
	}
	sources["native_custody"] = ref
	observed, err := r.config.Gateway.observeSettled(ctx, r.config.ContextID, identity, notStartedObservation)
	if err != nil {
		return fail(err)
	}
	if observed.Identity != identity || observed.Task != (OpenShellTask{b.OrderID, receipt.AttemptID, b.SessionID}) || observed.Phase != "Stopped" || !observed.CreateAcknowledged || !observed.StopAcknowledged || observed.MainProcessInstanceID != original.MainProcessInstanceID || observed.CommandSHA256 != "" || observed.ExecAttemptID != "" || observed.ExecRequestID != "" {
		return fail(fmt.Errorf("original pre-exec native observation differs"))
	}
	if err = VerifySessionReadbackFiles(sources); err != nil {
		return fail(err)
	}
	if err = verifyLocal(); err != nil {
		return fail(err)
	}
	// Verify transported files again after the native call; file refs alone cover
	// the manifest bytes, not the contents and modes named inside that manifest.
	if _, err = preparedInstructionReadback(ctx, dir, b.OrderID, spawn.InstructionBundle, sources); err != nil {
		return fail(err)
	}
	if err = VerifyOpenShellProfile(r.config.Profile); err != nil {
		return fail(err)
	}
	return SavedProgressReadback{Status: "not_started", OutcomeStatus: receipt.Outcome.Status, HarnessExitCode: receipt.Outcome.ExitCode, BaselineHead: receipt.Baseline.Head, OriginalError: receipt.Error, ProfileSHA256: profileSHA, NativeAttemptID: receipt.AttemptID, NativePhase: observed.Phase, StopAcknowledged: true, MainProcessInstanceID: observed.MainProcessInstanceID, PreparedManifest: prepared, Sources: sources}, nil
}

// These are prepared local inputs. No exec command or native consumption is
// inferred from them, so they never become CompletedInstructionEvidence.
func preparedInstructionReadback(ctx context.Context, dir, order string, ref *dispatcher.InstructionBundleRef, sources map[string]SessionReadbackFileRef) (*SessionReadbackFileRef, error) {
	mappingPath := filepath.Join(dir, "instruction-mapping.json")
	if ref == nil {
		if _, err := os.Lstat(mappingPath); !os.IsNotExist(err) {
			return nil, fmt.Errorf("prepared instruction mapping has no spawn binding")
		}
		return nil, nil
	}
	directory := filepath.Join(dir, "instructions-"+ref.SHA256)
	manifest, err := instructionbundle.Verify(ctx, directory, ref.SHA256, order)
	if err != nil {
		return nil, err
	}
	var mapping struct {
		OriginalManifest dispatcher.InstructionBundleRef `json:"original_manifest"`
		Prepared         *instructionbundle.Prepared     `json:"prepared"`
	}
	mappingRef, err := ReadSessionReadbackFile(mappingPath, &mapping)
	if err != nil {
		return nil, err
	}
	p := mapping.Prepared
	if mapping.OriginalManifest != *ref || p == nil || p.Directory != directory || p.GuestDirectory != "/sandbox/"+filepath.Base(directory) || p.ManifestSHA256 != ref.SHA256 || !reflect.DeepEqual(p.Manifest, manifest) {
		return nil, fmt.Errorf("original prepared instruction mapping differs")
	}
	manifestRef, err := ReadSessionReadbackFile(filepath.Join(directory, instructionbundle.ManifestName), nil)
	if err != nil {
		return nil, err
	}
	if manifestRef.SHA256 != ref.SHA256 {
		return nil, fmt.Errorf("prepared instruction manifest changed")
	}
	inputRef, err := ReadSessionReadbackFile(filepath.Join(dir, "input.txt"), nil)
	if err != nil {
		return nil, err
	}
	input, err := os.ReadFile(inputRef.Path)
	if err != nil {
		return nil, err
	}
	if !strings.Contains(string(input), p.PromptSection()) {
		return nil, fmt.Errorf("prepared input lacks original instruction mapping")
	}
	for name, current := range map[string]SessionReadbackFileRef{"instruction_mapping": mappingRef, "instruction_manifest": manifestRef, "instruction_input": inputRef} {
		if before, ok := sources[name]; ok && before != current {
			return nil, fmt.Errorf("prepared instruction source changed: %s", name)
		}
		sources[name] = current
	}
	return &manifestRef, nil
}
