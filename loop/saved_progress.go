package loop

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/poteto/noodle/dispatcher"
	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/orderx"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
	"github.com/poteto/noodle/internal/statever"
	loopruntime "github.com/poteto/noodle/runtime"
)

const savedProgressAttemptsKey = "noodle_saved_progress_attempts"

type SavedProgressInspection struct {
	SchemaVersion         int                               `json:"schema_version"`
	Status                string                            `json:"status"`
	Version               string                            `json:"version"`
	OrderID               string                            `json:"order_id"`
	StageIndex            int                               `json:"stage_index"`
	AttemptID             string                            `json:"attempt_id"`
	SessionID             string                            `json:"session_id"`
	AttemptStatus         state.AttemptStatus               `json:"attempt_status"`
	Runtime               string                            `json:"runtime"`
	Provider              string                            `json:"provider"`
	Model                 string                            `json:"model"`
	WorktreePath          string                            `json:"worktree_path"`
	NativeResumeSupported bool                              `json:"native_resume_supported"`
	Execution             loopruntime.SavedProgressReadback `json:"execution"`
	Effects               []string                          `json:"effects"`
}

type SavedProgressQueued struct {
	Status    string   `json:"status"`
	CommandID string   `json:"command_id"`
	Version   string   `json:"version"`
	Effects   []string `json:"effects"`
}

type savedProgressReader interface {
	ReadSavedProgress(context.Context, loopruntime.CompletedSessionBinding) (loopruntime.SavedProgressReadback, error)
}

type savedProgressAttempt struct {
	CommandID     string `json:"command_id"`
	Version       string `json:"version"`
	SessionID     string `json:"session_id"`
	ProgressHead  string `json:"progress_head"`
	NextAttemptID string `json:"next_attempt_id"`
}

// InspectSavedProgress reads the canonical checkpoint and existing runtime owner.
// It neither claims native-session resume nor authorizes dispatch.
func (l *Loop) InspectSavedProgress(ctx context.Context, orderID, sessionID string) (SavedProgressInspection, error) {
	result, _, err := l.inspectSavedProgress(ctx, orderID, sessionID)
	return result, err
}

func (l *Loop) inspectSavedProgress(ctx context.Context, orderID, sessionID string) (result SavedProgressInspection, snapshot reducer.DurableSnapshot, err error) {
	if orderID == "" || sessionID == "" {
		return result, snapshot, fmt.Errorf("exact stopped order/session identity missing")
	}
	ref, err := loopruntime.ReadSessionReadbackFile(l.canonicalSnapshotPath(), &snapshot)
	if err != nil {
		return result, snapshot, err
	}
	order, ok := snapshot.State.Orders[orderID]
	if snapshot.State.SchemaVersion != statever.Current || !ok || order.OrderID != orderID || order.Status != state.OrderActive || len(order.Stages) != 1 {
		return result, snapshot, fmt.Errorf("original active local task is unavailable")
	}
	stage := order.Stages[0]
	if stage.StageIndex != 0 || stage.Status != state.StageReview || stage.Runtime != "openshell" || !l.isAdmittedLocalTask(order, stage) || len(stage.Attempts) == 0 {
		return result, snapshot, fmt.Errorf("stopped OpenShell task review is unavailable")
	}
	attempt := stage.Attempts[len(stage.Attempts)-1]
	review, ok := snapshot.State.PendingReviews[orderID]
	if !ok || review.SessionID != sessionID || review.StageIndex != 0 || attempt.SessionID != sessionID || attempt.AttemptID == "" || (attempt.Status != state.AttemptCancelled && attempt.Status != state.AttemptFailed) || attempt.WorktreeName == "" || filepath.Base(attempt.WorktreeName) != attempt.WorktreeName || attempt.WorktreeName == "." || attempt.WorktreeName == ".." || review.WorktreeName != attempt.WorktreeName || attempt.WorktreeName != cookBaseName(orderID, 0, stage.TaskKey) {
		return result, snapshot, fmt.Errorf("latest stopped attempt/review binding differs")
	}
	matches := 0
	for _, o := range snapshot.State.Orders {
		for _, s := range o.Stages {
			for _, a := range s.Attempts {
				if a.SessionID == sessionID {
					matches++
				}
			}
		}
	}
	if matches != 1 {
		return result, snapshot, fmt.Errorf("stopped session identity is ambiguous")
	}

	var grants []savedProgressAttempt
	if raw := stage.Extra[savedProgressAttemptsKey]; len(raw) > 0 {
		if err = json.Unmarshal(raw, &grants); err != nil {
			return result, snapshot, err
		}
	}
	for _, grant := range grants {
		if grant.SessionID == sessionID {
			return result, snapshot, fmt.Errorf("new attempt was already offered; original dispatch readback required")
		}
	}
	if err = validateSavedProgressLedger(snapshot, orderID); err != nil {
		return result, snapshot, err
	}
	target, err := canonicalDirectory(filepath.Join(l.projectDir, ".worktrees", attempt.WorktreeName))
	if err != nil {
		return result, snapshot, err
	}
	reviewTarget, err := canonicalDirectory(review.WorktreePath)
	if err != nil || reviewTarget != target {
		return result, snapshot, fmt.Errorf("saved progress worktree differs from original review")
	}
	reader, ok := l.deps.Runtimes[stage.Runtime].(savedProgressReader)
	if !ok {
		return result, snapshot, fmt.Errorf("original runtime has no saved-progress readback")
	}
	execution, err := reader.ReadSavedProgress(ctx, loopruntime.CompletedSessionBinding{OrderID: orderID, SessionID: sessionID, WorktreePath: target, Provider: stage.Provider, Model: stage.Model, StageIndex: 0, RetryCount: len(stage.Attempts) - 1})
	if err != nil {
		return result, snapshot, err
	}
	if execution.Status != "saved" || string(execution.OutcomeStatus) != string(attempt.Status) || !execution.StopAcknowledged || execution.ProgressHead == "" || execution.ProfileSHA256 == "" || execution.CommandSHA256 == "" || execution.Sources == nil {
		return result, snapshot, fmt.Errorf("original stopped progress evidence is incomplete")
	}
	if err = l.validateSavedProgressInputs(snapshot, order, stage, sessionID, &execution); err != nil {
		return result, snapshot, err
	}
	result = SavedProgressInspection{SchemaVersion: 1, Status: "retryable", OrderID: orderID, StageIndex: 0, AttemptID: attempt.AttemptID, SessionID: sessionID, AttemptStatus: attempt.Status, Runtime: stage.Runtime, Provider: stage.Provider, Model: stage.Model, WorktreePath: target, Execution: execution, Effects: []string{}}
	// Ignore only the checkpoint clock. Definition, attempts, review, effects and
	// every runtime source remain part of the consent version.
	semantic := struct {
		Revision string
		Order    state.OrderNode
		Review   state.PendingReviewNode
		Ledger   []reducer.EffectLedgerRecord
		Result   SavedProgressInspection
	}{snapshot.OrderRevision, order, review, snapshot.EffectLedger, result}
	raw, err := json.Marshal(semantic)
	if err != nil {
		return result, snapshot, err
	}
	result.Version = fmt.Sprintf("%x", sha256.Sum256(raw))
	result.Execution.Sources["canonical_snapshot"] = ref
	if err = loopruntime.VerifySessionReadbackFiles(result.Execution.Sources); err != nil {
		return SavedProgressInspection{}, snapshot, err
	}
	return result, snapshot, nil
}

// Rebind the next task to the exact original transported instructions and
// canonical prompt. Resume context may follow the original prompt, never
// replace its task definition or reasoning selection.
func (l *Loop) validateSavedProgressInputs(snapshot reducer.DurableSnapshot, order state.OrderNode, stage state.StageNode, sessionID string, execution *loopruntime.SavedProgressReadback) error {
	view := &Loop{config: l.config, projectDir: l.projectDir, runtimeDir: l.runtimeDir, deps: l.deps, canonical: snapshot.State}
	selected := Stage{TaskKey: stage.TaskKey, Prompt: stage.Prompt, Skill: stage.Skill, Provider: stage.Provider, Model: stage.Model, Runtime: stage.Runtime, ExtraPrompt: stage.ExtraPrompt}
	cand := dispatchCandidate{OrderID: order.OrderID, StageIndex: 0, Stage: selected}
	bundle, effort, err := view.remoteTaskInputs(cand)
	if err != nil {
		return err
	}
	role, skill, err := view.taskRole(cand)
	if err != nil {
		return err
	}
	var spawn struct {
		InstructionBundle *dispatcher.InstructionBundleRef `json:"instruction_bundle"`
		ReasoningLevel    string                           `json:"reasoning_level"`
		TaskRole          string                           `json:"task_role"`
		Skill             string                           `json:"skill"`
	}
	dir := filepath.Join(l.runtimeDir, "sessions", sessionID)
	spawnRef, err := loopruntime.ReadSessionReadbackFile(filepath.Join(dir, "spawn.json"), &spawn)
	if err != nil {
		return err
	}
	if !recoveryJSONEqual(bundle, spawn.InstructionBundle) || effort != spawn.ReasoningLevel || role != spawn.TaskRole || skill != spawn.Skill || execution.Instructions == nil || execution.Instructions.Manifest.SHA256 != bundle.SHA256 {
		return fmt.Errorf("saved progress task instructions or reasoning selection changed")
	}
	expectedSpawn, ok := execution.Sources["spawn.json"]
	if !ok || expectedSpawn != spawnRef {
		return fmt.Errorf("saved progress spawn source differs from original runtime")
	}
	selected.Skill = skill
	prefix := buildCookPrompt(order.OrderID, selected, order.Plan, order.Title, order.Rationale, "")
	promptRef, err := loopruntime.ReadSessionReadbackFile(filepath.Join(dir, "prompt.txt"), nil)
	if err != nil {
		return err
	}
	prompt, err := os.ReadFile(promptRef.Path)
	if err != nil {
		return err
	}
	if string(prompt) != prefix && !strings.HasPrefix(string(prompt), prefix+"\n\n") {
		return fmt.Errorf("saved progress original task prompt changed")
	}
	execution.Sources["original_prompt"] = promptRef
	return nil
}

func validateSavedProgressLedger(snapshot reducer.DurableSnapshot, orderID string) error {
	admitted := false
	for _, record := range snapshot.EffectLedger {
		if record.Status == reducer.EffectLedgerRunning || record.Status == reducer.EffectLedgerFailed {
			return fmt.Errorf("unresolved effect still requires owner readback: %s", record.EffectID)
		}
		if record.EffectID != initialAdmissionEffectID(orderID) {
			continue
		}
		var p struct {
			OrderID  string `json:"order_id"`
			Revision string `json:"initial_revision"`
		}
		if json.Unmarshal(record.Effect.Payload, &p) != nil || p.OrderID != orderID || !orderx.ValidOrderRevision(p.Revision) || record.Effect.EffectID != record.EffectID || record.Effect.Type != reducer.EffectInitialAdmission || record.Status != reducer.EffectLedgerDone || record.Result == nil || record.Result.EffectID != record.EffectID || record.Result.Status != reducer.EffectResultCompleted {
			return fmt.Errorf("original done task admission differs")
		}
		admitted = true
	}
	if !admitted {
		return fmt.Errorf("original task admission is missing")
	}
	return validateInterruptionLedger(snapshot, orderID)
}

// QueueSavedProgressAttempt uses the existing control queue and its original
// writer lock. A queued acknowledgement is not a worker-start acknowledgement.
func (l *Loop) QueueSavedProgressAttempt(ctx context.Context, orderID, sessionID, version string) (SavedProgressQueued, error) {
	inspected, err := l.InspectSavedProgress(ctx, orderID, sessionID)
	if err != nil {
		return SavedProgressQueued{}, err
	}
	if inspected.Version != version {
		return SavedProgressQueued{}, fmt.Errorf("saved progress consent version changed")
	}
	command := ControlCommand{ID: "saved-progress-" + version, Action: "retry-saved-progress", OrderID: orderID, Name: sessionID, Value: version, At: time.Now().UTC()}
	path, _, lockPath := l.controlPaths()
	lock, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return SavedProgressQueued{}, err
	}
	defer lock.Close()
	if err = acquireFileLock(lock.Fd()); err != nil {
		return SavedProgressQueued{}, err
	}
	defer releaseFileLock(lock.Fd())
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return SavedProgressQueued{}, err
	}
	err = json.NewEncoder(f).Encode(command)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil {
		err = filex.SyncDir(l.runtimeDir)
	}
	if err != nil {
		return SavedProgressQueued{Status: "unknown", CommandID: command.ID, Version: version, Effects: []string{"control_append_attempted"}}, err
	}
	return SavedProgressQueued{Status: "queued", CommandID: command.ID, Version: version, Effects: []string{"original_control_queued"}}, nil
}

func (l *Loop) controlRetrySavedProgress(cmd ControlCommand) error {
	if cmd.ID != "saved-progress-"+cmd.Value {
		return fmt.Errorf("saved progress command identity differs")
	}
	ctx := context.Background()
	inspectCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	inspected, snapshot, err := l.inspectSavedProgress(inspectCtx, cmd.OrderID, cmd.Name)
	cancel()
	if err != nil {
		return err
	}
	if cmd.Value != inspected.Version {
		return fmt.Errorf("saved progress consent version changed")
	}
	if !recoveryJSONEqual(l.canonical.Orders[cmd.OrderID], snapshot.State.Orders[cmd.OrderID]) || !recoveryJSONEqual(l.canonical.PendingReviews[cmd.OrderID], snapshot.State.PendingReviews[cmd.OrderID]) || !recoveryJSONEqual(l.effectLedger.All(), snapshot.EffectLedger) {
		return fmt.Errorf("original Loop state changed before new attempt")
	}
	if l.atMaxConcurrency() {
		return admission.ErrDeferred
	}
	if len(l.cooks.activeCooksByOrder) != 0 || len(l.cooks.adoptedTargets) != 0 {
		return fmt.Errorf("original Loop still owns an active worker")
	}
	if _, ok := l.registry.ByKey(snapshot.State.Orders[cmd.OrderID].Stages[0].Skill); !ok {
		return fmt.Errorf("original local-task skill is unavailable")
	}
	if err = l.requireAdmittedOrder(cmd.OrderID); err != nil {
		return err
	}
	return l.withAdmission(ctx, "openshell", cmd.OrderID, func(ctx context.Context) error {
		node := l.canonical.Orders[cmd.OrderID]
		s := node.Stages[0]
		next := len(s.Attempts)
		var grants []savedProgressAttempt
		if raw := s.Extra[savedProgressAttemptsKey]; len(raw) > 0 {
			if err := json.Unmarshal(raw, &grants); err != nil {
				return err
			}
		}
		grants = append(grants, savedProgressAttempt{CommandID: cmd.ID, Version: cmd.Value, SessionID: cmd.Name, ProgressHead: inspected.Execution.ProgressHead, NextAttemptID: dispatchAttemptID(cmd.OrderID, 0, next)})
		raw, err := json.Marshal(grants)
		if err != nil {
			return err
		}
		if s.Extra == nil {
			s.Extra = map[string]json.RawMessage{}
		}
		s.Extra[savedProgressAttemptsKey] = raw
		node.Stages[0] = s
		l.canonical.Orders[cmd.OrderID] = node
		// The offer is durable before dispatch. Any later uncertainty cannot grant
		// a second launch from the predecessor, even if no ACK reaches the caller.
		if err = l.persistCanonicalCheckpoint(); err != nil {
			return err
		}
		checkpoint, err := os.OpenFile(l.canonicalSnapshotPath(), os.O_RDWR, 0)
		if err != nil {
			return err
		}
		err = checkpoint.Sync()
		closeErr := checkpoint.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if err = filex.SyncDir(l.runtimeDir); err != nil {
			return err
		}
		if err = l.projectRecoveredOrder(cmd.OrderID); err != nil {
			return err
		}
		stage := Stage{TaskKey: s.TaskKey, Prompt: s.Prompt, Skill: s.Skill, Provider: s.Provider, Model: s.Model, Runtime: s.Runtime, Group: s.Group, Status: StageStatusActive, Extra: cloneLegacyExtra(s.Extra), ExtraPrompt: s.ExtraPrompt}
		order := Order{ID: node.OrderID, Title: node.Title, Plan: node.Plan, Rationale: node.Rationale, Status: OrderStatusActive, Stages: []Stage{stage}}
		prompt := fmt.Sprintf("This is an explicitly authorized new attempt after stopped session %s. Saved progress is commit %s in this worktree. Inspect the preserved files before repeating actions. This is not continuation of the original harness conversation.", cmd.Name, inspected.Execution.ProgressHead)
		err = l.spawnCookAdmitted(ctx, dispatchCandidate{OrderID: cmd.OrderID, StageIndex: 0, Stage: stage}, order, spawnOptions{attempt: next, resume: prompt})
		if err != nil {
			return err
		}
		active := l.cooks.activeCooksByOrder[cmd.OrderID]
		if active == nil || active.attempt != next || active.session == nil {
			return fmt.Errorf("new attempt was refused before worker registration; inspect original dispatch history")
		}
		return l.syncPendingReviewProjection()
	})
}
