package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"

	"github.com/poteto/noodle/internal/state"
	loopruntime "github.com/poteto/noodle/runtime"
)

const nativeDegradationKey = "native_context_degradations"

type nativeDegradation struct {
	Refusal          loopruntime.CodexResumeRefusal `json:"refusal"`
	FailedAttempt    int                            `json:"failed_attempt"`
	SuccessorAttempt int                            `json:"successor_attempt"`
	PromptSHA256     string                         `json:"prompt_sha256"`
	WorktreeSHA256   string                         `json:"worktree_sha256"`
	Offered          bool                           `json:"offered"`
}

func nativeDegradations(stage state.StageNode) (map[string]nativeDegradation, error) {
	records := map[string]nativeDegradation{}
	if raw := stage.Extra[nativeDegradationKey]; len(raw) > 0 {
		if err := json.Unmarshal(raw, &records); err != nil {
			return nil, err
		}
	}
	return records, nil
}

func (l *Loop) saveNativeDegradations(orderID string, index int, records map[string]nativeDegradation) error {
	raw, err := json.Marshal(records)
	if err != nil {
		return err
	}
	order := l.canonical.Orders[orderID]
	stage := &order.Stages[index]
	if stage.Extra == nil {
		stage.Extra = map[string]json.RawMessage{}
	}
	stage.Extra[nativeDegradationKey] = raw
	l.canonical.Orders[orderID] = order
	if err := l.persistCanonicalCheckpoint(); err != nil {
		return err
	}
	return l.projectRecoveredOrder(orderID)
}

func (l *Loop) recoverMissingNativeContext(ctx context.Context, cook *cookHandle) (bool, error) {
	if !l.retainTaskData(cook.orderID) || cook.stage.Provider != "codex" || cook.attempt == 0 {
		return false, nil
	}
	if outcome := cook.session.Outcome(); outcome.Status != loopruntime.StatusFailed || outcome.ExitCode != 1 {
		return false, nil
	}
	l.runMonitorPass(ctx)
	refusal, err := loopruntime.ReadCodexResumeRefusal(l.runtimeDir, cook.session.ID())
	if err != nil {
		return true, l.parkPendingReview(cook, "native context recovery readback unknown: "+err.Error())
	}
	if refusal == nil {
		return false, nil
	}
	if refusal.OrderID != cook.orderID || refusal.StageIndex != strconv.Itoa(cook.stageIndex) || refusal.TaskKey != cook.stage.TaskKey || refusal.WorktreePath != cook.worktreePath {
		return true, l.parkPendingReview(cook, "native context recovery original task differs")
	}
	order := l.canonical.Orders[cook.orderID]
	stage := order.Stages[cook.stageIndex]
	if len(stage.Attempts) != cook.attempt+1 || stage.Attempts[cook.attempt].SessionID != cook.session.ID() || !l.nativeRefusalPredecessor(stage, cook.stageIndex, *refusal, cook.attempt) {
		return true, l.parkPendingReview(cook, "native context recovery original attempt differs")
	}
	records, err := nativeDegradations(stage)
	if err != nil {
		return true, err
	}
	if prior, exists := records[cook.session.ID()]; exists {
		if !recoveryJSONEqual(prior.Refusal, *refusal) {
			return true, fmt.Errorf("native context recovery evidence changed")
		}
	} else {
		digest, err := l.localTaskWorktreeDigest(stage, cook.worktreePath)
		if err != nil {
			return true, err
		}
		records[cook.session.ID()] = nativeDegradation{Refusal: *refusal, FailedAttempt: cook.attempt, SuccessorAttempt: cook.attempt + 1, PromptSHA256: recoveryDigest([]byte(stage.Prompt)), WorktreeSHA256: digest}
		if err := l.saveNativeDegradations(cook.orderID, cook.stageIndex, records); err != nil {
			return true, err
		}
	}
	return true, l.continueNativeDegradation(ctx, cook, *refusal)
}

func (l *Loop) continueNativeDegradation(ctx context.Context, cook *cookHandle, refusal loopruntime.CodexResumeRefusal) error {
	current := l.canonical.Orders[cook.orderID]
	if current.Status == state.OrderActive && current.Stages[cook.stageIndex].Status == state.StagePending {
		return nil
	}
	if current.Stages[cook.stageIndex].Status != state.StageFailed {
		if err := l.failStage(ctx, cook, refusal.Reason); err != nil {
			return err
		}
	}
	return l.controlRequeue(cook.orderID)
}

// Complete only a saved pre-turn refusal decision, without lifting the hold or
// replaying a successor that was already offered to the original dispatcher.
func (l *Loop) reconcileNativeDegradations(ctx context.Context) error {
	for orderID, order := range l.canonical.Orders {
		if !l.retainTaskData(orderID) {
			continue
		}
		for index, stage := range order.Stages {
			records, err := nativeDegradations(stage)
			if err != nil {
				return err
			}
			for _, record := range records {
				if record.Offered || record.SuccessorAttempt != len(stage.Attempts) {
					continue
				}
				if _, live := l.cooks.adoptedTargets[orderID]; live {
					return fmt.Errorf("native degradation still has an adopted worker")
				}
				if record.FailedAttempt != len(stage.Attempts)-1 || record.FailedAttempt < 1 || stage.Attempts[record.FailedAttempt].SessionID != record.Refusal.SessionID || !l.nativeRefusalPredecessor(stage, index, record.Refusal, record.FailedAttempt) || record.Refusal.OrderID != orderID || record.Refusal.StageIndex != strconv.Itoa(index) || record.Refusal.TaskKey != stage.TaskKey || record.PromptSHA256 != recoveryDigest([]byte(stage.Prompt)) {
					return fmt.Errorf("native degradation original continuation differs")
				}
				fresh, err := loopruntime.ReadCodexResumeRefusal(l.runtimeDir, record.Refusal.SessionID)
				if err != nil {
					return err
				}
				if fresh == nil || !recoveryJSONEqual(*fresh, record.Refusal) {
					return fmt.Errorf("native degradation original refusal changed")
				}
				name := cookBaseName(orderID, index, stage.TaskKey)
				path := l.worktreePath(name)
				digest, err := l.localTaskWorktreeDigest(stage, path)
				if err != nil {
					return err
				}
				if path != record.Refusal.WorktreePath || digest != record.WorktreeSHA256 {
					return fmt.Errorf("native degradation original worktree changed")
				}
				if err := l.projectRecoveredOrder(orderID); err != nil {
					return err
				}
				cook := &cookHandle{cookIdentity: cookIdentity{orderID: orderID, stageIndex: index, stage: Stage{TaskKey: stage.TaskKey, Skill: stage.Skill, Provider: stage.Provider, Runtime: stage.Runtime}}, session: &adoptedSession{id: record.Refusal.SessionID, status: "failed"}, attempt: record.FailedAttempt, worktreeName: name, worktreePath: path}
				if err := l.continueNativeDegradation(ctx, cook, record.Refusal); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (l *Loop) degradedTaskDispatch(cand dispatchCandidate, attempt int, req loopruntime.DispatchRequest) (loopruntime.DispatchRequest, error) {
	stage := l.canonical.Orders[cand.OrderID].Stages[cand.StageIndex]
	records, err := nativeDegradations(stage)
	if err != nil {
		return req, err
	}
	var selected *nativeDegradation
	key := ""
	for id, record := range records {
		if record.SuccessorAttempt != attempt {
			continue
		}
		if selected != nil {
			return req, fmt.Errorf("native degraded continuation ambiguous")
		}
		copy := record
		selected = &copy
		key = id
	}
	if selected == nil {
		return req, nil
	}
	if selected.Offered {
		return req, fmt.Errorf("%w: native degraded successor already offered; inspect original attempt", errDispatchUnknown)
	}
	if req.ResumeSessionID != selected.Refusal.SessionID || req.WorktreePath != selected.Refusal.WorktreePath || req.TaskKey != selected.Refusal.TaskKey || recoveryDigest([]byte(stage.Prompt)) != selected.PromptSHA256 {
		return req, fmt.Errorf("native degraded continuation source differs")
	}
	fresh, err := loopruntime.ReadCodexResumeRefusal(l.runtimeDir, selected.Refusal.SessionID)
	if err != nil {
		return req, err
	}
	if fresh == nil || !recoveryJSONEqual(*fresh, selected.Refusal) {
		return req, fmt.Errorf("native degraded continuation evidence changed")
	}
	digest, err := l.localTaskWorktreeDigest(stage, req.WorktreePath)
	if err != nil {
		return req, err
	}
	if digest != selected.WorktreeSHA256 {
		return req, fmt.Errorf("native degraded continuation worktree changed")
	}
	selected.Offered = true
	records[key] = *selected
	if err := l.saveNativeDegradations(cand.OrderID, cand.StageIndex, records); err != nil {
		return req, err
	}
	req.ResumeSessionID = ""
	req.DegradedFromSessionID = selected.Refusal.ResumeSessionID
	req.DegradationReason = selected.Refusal.Reason
	req.Prompt += fmt.Sprintf("\n\nNative context recovery: DEGRADED. This is the single new conversation for the saved continuation after native resume refused before any turn. The current task requirements and original worktree remain in force. Original conversation memory was not restored. Retained original attempt evidence: %s. Resume refusal evidence: %s. Read relevant prior decisions and unknown effects before continuing; do not repeat effects from missing evidence.",
		filepath.Join(l.runtimeDir, "sessions", selected.Refusal.ResumeSessionID),
		filepath.Join(l.runtimeDir, "sessions", selected.Refusal.SessionID))
	return req, nil
}

func (l *Loop) nativeRefusalPredecessor(stage state.StageNode, index int, refusal loopruntime.CodexResumeRefusal, attempt int) bool {
	cand := dispatchCandidate{OrderID: refusal.OrderID, StageIndex: index, Stage: Stage{
		TaskKey: stage.TaskKey, Skill: stage.Skill, Provider: stage.Provider, Runtime: stage.Runtime, Prompt: stage.Prompt}}
	prior, err := l.resumeTaskPredecessor(cand, attempt)
	return err == nil && prior != "" && prior == refusal.ResumeSessionID
}
