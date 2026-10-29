package loop

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/guestprogress"
	"github.com/poteto/noodle/internal/instructionbundle"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
	"github.com/poteto/noodle/internal/statusfile"
	"github.com/poteto/noodle/internal/workspaceartifacts"
	loopruntime "github.com/poteto/noodle/runtime"
)

// ArtifactCompactionOwner is a local eligibility observation, not native custody
// or a reclamation receipt. The caller must hold control.lock during deletion and
// separately obtain eligible readback from the original live binary/Host.
type ArtifactCompactionOwner struct {
	Binding         loopruntime.CompletedSessionBinding           `json:"binding"`
	Baseline        *workspaceartifacts.Baseline                  `json:"baseline"`
	Result          *workspaceartifacts.Result                    `json:"result"`
	Head            string                                        `json:"head"`
	CanonicalSHA256 string                                        `json:"canonical_sha256"`
	MetadataSHA256  string                                        `json:"metadata_sha256"`
	Sources         map[string]loopruntime.SessionReadbackFileRef `json:"sources"`
}

// ReadArtifactCompactionOwner never starts a Loop, Runtime or Gateway, writes a
// checkpoint, or upgrades an admission. Only accepted completed publications are
// supported; diagnostic failures do not have this five-copy publication intent.
func ReadArtifactCompactionOwner(project string, cfg config.Config, orderID, sessionID, contextID, pauseID string) (out ArtifactCompactionOwner, err error) {
	canonical, err := filepath.EvalSymlinks(project)
	if err != nil || canonical != project || !filepath.IsAbs(project) || !cleanupSessionName(sessionID) || orderID == "" || contextID == "" || pauseID == "" {
		return out, fmt.Errorf("artifact continuation original identity is incomplete or noncanonical")
	}
	dir := filepath.Join(project, ".noodle")
	if err := artifactCompactionPaused(dir, pauseID); err != nil {
		return out, err
	}
	var snapshot reducer.DurableSnapshot
	snapshotRef, err := loopruntime.ReadSessionReadbackFile(filepath.Join(dir, "state.snapshot.json"), &snapshot)
	if err != nil {
		return out, err
	}
	if snapshot.GeneratedAt.IsZero() || snapshot.OrderRevision == "" || snapshot.EffectLedger == nil {
		return out, fmt.Errorf("artifact continuation canonical checkpoint is incomplete")
	}
	if err = validateAdmissionSnapshot(snapshot); err != nil {
		return out, err
	}
	if len(snapshot.State.PendingReviews) != 0 || len(snapshot.State.ActionNeeded) != 0 || !reducer.ReclaimableTaskData(snapshot.EffectLedger, orderID) {
		return out, fmt.Errorf("artifact continuation original admission or owner work is unresolved")
	}
	order, ok := snapshot.State.Orders[orderID]
	if !ok || order.Status != state.OrderCompleted {
		return out, fmt.Errorf("artifact continuation original accepted order is unavailable")
	}
	// This passive value calls only the existing local acceptance validators.
	reader := &Loop{projectDir: project, runtimeDir: dir, config: cfg, canonical: snapshot.State}
	closure, err := reader.retainExecutionClosure(order)
	if err != nil {
		return out, err
	}
	var original taskExecutionClosure
	if _, err = loopruntime.ReadSessionReadbackFile(closure.Path, &original); err != nil {
		return out, err
	}
	acceptance, err := loopruntime.ReadTaskDeliveryAcceptance(original.Diagnostics["owner_acceptance"])
	if err != nil {
		return out, err
	}
	out.Sources = map[string]loopruntime.SessionReadbackFileRef{"task_closure": closure, "owner_acceptance": original.Diagnostics["owner_acceptance"], "canonical_snapshot": snapshotRef}
	for i, condition := range acceptance.Conditions {
		for j, ref := range condition.Evidence {
			out.Sources[fmt.Sprintf("condition_%d_%d", i, j)] = ref
		}
	}
	stage := order.Stages[0]
	found := 0
	for retry, attempt := range stage.Attempts {
		if attempt.SessionID != sessionID {
			continue
		}
		if attempt.Status != state.AttemptCompleted || stage.Runtime != "openshell" || !cleanupSessionName(attempt.WorktreeName) {
			return out, fmt.Errorf("artifact continuation attempt is not a settled publication")
		}
		out.Binding = loopruntime.CompletedSessionBinding{OrderID: orderID, SessionID: sessionID, WorktreePath: filepath.Join(project, ".worktrees", attempt.WorktreeName), Provider: stage.Provider, Model: stage.Model, StageIndex: stage.StageIndex, RetryCount: retry}
		found++
	}
	if found != 1 {
		return out, fmt.Errorf("artifact continuation original session is missing or duplicated")
	}
	session := filepath.Join(dir, "sessions", sessionID)
	read := func(name string, value any) error {
		ref, e := loopruntime.ReadSessionReadbackFile(filepath.Join(session, name), value)
		if e == nil {
			out.Sources[name] = ref
		}
		return e
	}
	var dispatch struct {
		Version                                                                                                    int
		ContextID, OrderID, SessionID, AttemptID, Target, Phase, Error, CandidateHead, ProgressHead, ProfileSHA256 string
		Baseline                                                                                                   *workspaceartifacts.Baseline
		Delivery                                                                                                   *workspaceartifacts.Result
		Worker                                                                                                     *loopruntime.OpenShellWorkerRecord
		Outcome                                                                                                    loopruntime.SessionOutcome
		Progress                                                                                                   *guestprogress.Receipt
		ProgressSource                                                                                             *loopruntime.SessionReadbackFileRef
	}
	if err = read("openshell-dispatch.json", &dispatch); err != nil {
		return out, err
	}
	b := out.Binding
	if dispatch.Version != 1 || dispatch.ContextID != contextID || dispatch.OrderID != orderID || dispatch.SessionID != sessionID || dispatch.AttemptID == "" || dispatch.Target != b.WorktreePath || dispatch.Phase != "settled" || dispatch.Error != "" || dispatch.CandidateHead == "" || dispatch.ProgressHead != "" || dispatch.ProfileSHA256 == "" || dispatch.Outcome.Status != loopruntime.StatusCompleted || dispatch.Outcome.ExitCode != 0 || dispatch.Baseline == nil || dispatch.Delivery == nil || dispatch.Worker == nil {
		return out, fmt.Errorf("artifact continuation original dispatch differs")
	}
	w := dispatch.Worker
	if w.Identity.OrderID != orderID || w.Identity.SessionID != sessionID || w.Identity.AttemptID != dispatch.AttemptID || w.Boundary != loopruntime.OpenShellBoundaryStopped || w.StopPending || w.StopError != "" {
		return out, fmt.Errorf("artifact continuation original worker boundary is unresolved")
	}
	var spawn struct {
		OrderID           string                     `json:"order_id"`
		SessionID         string                     `json:"session_id"`
		Provider          string                     `json:"provider"`
		Model             string                     `json:"model"`
		Runtime           string                     `json:"runtime"`
		WorktreePath      string                     `json:"worktree_path"`
		StageIndex        string                     `json:"stage_index"`
		RetryCount        int                        `json:"retry_count"`
		InstructionBundle *instructionbundle.FileRef `json:"instruction_bundle"`
	}
	if err = read("spawn.json", &spawn); err != nil {
		return out, err
	}
	if spawn.OrderID != orderID || spawn.SessionID != sessionID || spawn.Provider != b.Provider || spawn.Model != b.Model || spawn.Runtime != "openshell" || spawn.WorktreePath != b.WorktreePath || spawn.StageIndex != strconv.Itoa(b.StageIndex) || spawn.RetryCount != b.RetryCount {
		return out, fmt.Errorf("artifact continuation original spawn differs")
	}
	if err := artifactCompactionOriginalInputs(session, orderID, spawn.InstructionBundle, dispatch.Progress, dispatch.ProgressSource, out.Sources); err != nil {
		return out, err
	}
	if cfg.Runtime.OpenShell.Profile != "" {
		profile, err := loopruntime.LoadOpenShellProfile(cfg.Runtime.OpenShell.Profile)
		if err != nil {
			return out, err
		}
		sha, err := loopruntime.OpenShellDispatchSHA256(profile)
		if err != nil || sha != dispatch.ProfileSHA256 {
			return out, fmt.Errorf("artifact continuation original profile binding differs")
		}
		ref, err := loopruntime.ReadSessionReadbackFile(profile.SourcePath, nil)
		if err != nil {
			return out, err
		}
		out.Sources["profile"] = ref
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
	if err = read("meta.json", &meta); err != nil {
		return out, err
	}
	if meta.SessionID != sessionID || meta.Status != "exited" || meta.Alive == nil || *meta.Alive || meta.ExitPending || meta.Provider != b.Provider || meta.Model != b.Model || meta.Runtime != "openshell" {
		return out, fmt.Errorf("artifact continuation original metadata is not terminal")
	}
	out.Baseline, out.Result, out.Head = dispatch.Baseline, dispatch.Delivery, dispatch.CandidateHead
	if sessionID == acceptance.SessionID && out.Head != acceptance.CandidateHead {
		return out, fmt.Errorf("artifact continuation accepted publication head differs")
	}
	if out.Baseline.Directory != filepath.Join(session, "repo") || out.Result.ReturnedDirectory != filepath.Join(session, "returned") || out.Result.PatchPath != filepath.Join(session, "delivery", "changes.patch") || !out.Result.Applied || out.Result.State != "published" || out.Result.Error != "" {
		return out, fmt.Errorf("artifact continuation publication paths differ")
	}
	// Paused Cycle returns before its flush. publishState and an unchanged
	// stampStatus do not rewrite the checkpoint; retain strict original bytes.
	out.CanonicalSHA256 = snapshotRef.SHA256
	out.MetadataSHA256 = out.Sources["meta.json"].SHA256
	return out, loopruntime.VerifySessionReadbackFiles(out.Sources)
}

func artifactCompactionPaused(dir, pauseID string) error {
	var status statusfile.Status
	if _, err := loopruntime.ReadSessionReadbackFile(filepath.Join(dir, "status.json"), &status); err != nil {
		return err
	}
	if status.LoopState != string(StatePaused) || len(status.Active) != 0 {
		return fmt.Errorf("artifact continuation original actor is not paused and inactive")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "control-ack.ndjson"))
	if err != nil {
		return err
	}
	count := 0
	var latest ControlAck
	for _, line := range bytes.Split(raw, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if err := json.Unmarshal(line, &latest); err != nil {
			return err
		}
		if latest.ID == pauseID {
			count++
		}
	}
	if count != 1 || latest.ID != pauseID || latest.Action != "pause" || latest.Status != "ok" || latest.DispatchState != StatePaused {
		return fmt.Errorf("artifact continuation unique latest original pause ACK is unavailable")
	}
	for _, name := range []string{"control.ndjson", "orders-next.json"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Size() != 0 {
			return fmt.Errorf("artifact continuation original input remains pending: %s", name)
		}
	}
	return nil
}
