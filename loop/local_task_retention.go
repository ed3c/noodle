package loop

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/poteto/noodle/internal/ingest"
	"github.com/poteto/noodle/internal/state"
)

const localTaskRetentionKey = "local_task_retention"

type localTaskRetention struct {
	Schema      int              `json:"schema"`
	Disposition string           `json:"disposition"`
	Candidate   localTaskFileRef `json:"candidate"`
	Head        string           `json:"head"`
	Tree        string           `json:"tree"`
	Branch      string           `json:"branch"`
	ControlID   string           `json:"control_id"`
}

func (l *Loop) controlRetainLocalTask(cmd ControlCommand) error {
	return l.controlReviewedLocalTask(cmd, true)
}

func (l *Loop) saveRetainedLocalTask(cmd ControlCommand, ref localTaskFileRef, head, tree string, order state.OrderNode, stage state.StageNode, path string) error {
	branch, err := recoveryGitWithExecution(l.deps.Execution, path, "symbolic-ref", "HEAD")
	if err != nil || !strings.HasPrefix(branch, "refs/heads/") || cmd.ID == "" {
		return fmt.Errorf("local task retained branch or original control unavailable: %v", err)
	}
	binding := localTaskRetention{Schema: 1, Disposition: "retain_branch", Candidate: ref, Head: head, Tree: tree, Branch: branch, ControlID: cmd.ID}
	raw, err := json.Marshal(binding)
	if err != nil {
		return err
	}
	if stage.Extra == nil {
		stage.Extra = map[string]json.RawMessage{}
	}
	stage.Extra[localTaskRetentionKey] = raw
	order.Stages[0] = stage
	l.canonical.Orders[order.OrderID] = order
	if err = l.persistCanonicalCheckpoint(); err != nil {
		return err
	}
	_, err = l.continueRetainedLocalTask(cmd)
	return err
}

func retainedLocalTask(order state.OrderNode) (localTaskRetention, localTaskCandidate, error) {
	var retained localTaskRetention
	if len(order.Stages) != 1 {
		return retained, localTaskCandidate{}, fmt.Errorf("retained local task stage missing")
	}
	stage := order.Stages[0]
	if err := decodeStoppedReview(stage.Extra[localTaskRetentionKey], &retained); err != nil {
		return retained, localTaskCandidate{}, err
	}
	if retained.Schema != 1 || retained.Disposition != "retain_branch" || retained.ControlID == "" || !strings.HasPrefix(retained.Branch, "refs/heads/") || !reconciliationSHA.MatchString(retained.Head) || !reconciliationSHA.MatchString(retained.Tree) || len(stage.Extra[localTaskMergeKey]) != 0 {
		return retained, localTaskCandidate{}, fmt.Errorf("local task retained disposition differs")
	}
	candidate, err := readLocalTaskCandidate(retained.Candidate)
	if err != nil {
		return retained, candidate, err
	}
	if len(stage.Attempts) == 0 {
		return retained, candidate, fmt.Errorf("retained local task attempt missing")
	}
	attempt := stage.Attempts[len(stage.Attempts)-1]
	if candidate.OrderID != order.OrderID || candidate.SessionID != attempt.SessionID || candidate.AttemptID != attempt.AttemptID {
		return retained, candidate, fmt.Errorf("retained local task original identity differs")
	}
	return retained, candidate, nil
}

// A saved original disposition permits readback, never another merge or commit.
func (l *Loop) continueRetainedLocalTask(cmd ControlCommand) (bool, error) {
	order, exists := l.canonical.Orders[cmd.OrderID]
	if !exists || len(order.Stages) != 1 || len(order.Stages[0].Extra[localTaskRetentionKey]) == 0 {
		return false, nil
	}
	retained, candidate, err := retainedLocalTask(order)
	if err != nil {
		return true, err
	}
	stage := order.Stages[0]
	if !l.isAdmittedLocalTask(order, stage) || retained.Candidate != (localTaskFileRef{Path: cmd.Value, SHA256: cmd.Target}) || retained.ControlID != cmd.ID {
		return true, fmt.Errorf("original retained control or candidate changed")
	}
	if err = l.requireSettledMergeEffects(order.OrderID, 0); err != nil {
		return true, err
	}
	path, err := l.taskWorktreePhysical(candidate.Worktree)
	if err != nil {
		return true, err
	}
	for _, check := range []struct {
		args []string
		want string
	}{{[]string{"rev-parse", "HEAD"}, retained.Head}, {[]string{"rev-parse", "HEAD^{tree}"}, retained.Tree}, {[]string{"symbolic-ref", "HEAD"}, retained.Branch}, {[]string{"status", "--porcelain"}, ""}} {
		actual, e := recoveryGitWithExecution(l.deps.Execution, path, check.args...)
		if e != nil || actual != check.want {
			return true, fmt.Errorf("retained candidate Git identity changed: %v", e)
		}
	}
	if err = l.validateLocalTaskFiles(candidate, path); err != nil {
		return true, err
	}
	if order.Status == state.OrderCompleted && stage.Status == state.StageCompleted {
		return true, l.syncPendingReviewProjection()
	}
	if order.Status != state.OrderActive || stage.Status != state.StageReview || stage.Merge != nil {
		return true, fmt.Errorf("retained local task continuation differs")
	}
	attempt := stage.Attempts[len(stage.Attempts)-1]
	pending := &pendingReviewCook{cookIdentity: cookIdentity{orderID: order.OrderID, stageIndex: 0, stage: Stage{TaskKey: stage.TaskKey, Prompt: stage.Prompt, Skill: stage.Skill, Provider: stage.Provider, Model: stage.Model, Runtime: stage.Runtime}}, sessionID: candidate.SessionID, worktreeName: attempt.WorktreeName, worktreePath: candidate.Worktree}
	bound, err := l.captureRequestChanges(pending)
	if err != nil {
		return true, err
	}
	if err = l.validateLocalTaskSession(order, stage, bound); err != nil {
		return true, err
	}
	cook := &cookHandle{cookIdentity: pending.cookIdentity, worktreeName: pending.worktreeName, worktreePath: candidate.Worktree, session: &adoptedSession{id: candidate.SessionID, status: "completed"}, retainWorktree: true}
	if err = l.runDoneBeforeTerminal(context.Background(), cook); err != nil {
		return true, err
	}
	if review, ok := l.canonical.PendingReviews[order.OrderID]; ok {
		if review.SessionID != candidate.SessionID {
			return true, fmt.Errorf("retained local task pending review changed")
		}
		if err = l.emitEventChecked(ingest.EventStageReviewApproved, map[string]any{"order_id": order.OrderID, "stage_index": 0, "reason": "reviewed branch retained"}); err != nil {
			return true, err
		}
	}
	if err = l.emitEventChecked(ingest.EventStageCompleted, l.mergeLifecyclePayload(cook, false)); err != nil {
		return true, err
	}
	return true, l.advanceAndPersist(context.Background(), cook)
}

func (l *Loop) checkRetainedControlACK(id string) error {
	return l.checkDispositionControlACK(id, "retain-local-task")
}

func (l *Loop) checkDispositionControlACK(id, action string) error {
	if id == "" {
		return fmt.Errorf("original disposition control is missing")
	}
	_, path, _ := l.controlPaths()
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("retained disposition ACK unavailable: %w", err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var ack ControlAck
		if err := json.Unmarshal(scanner.Bytes(), &ack); err != nil {
			return fmt.Errorf("retained disposition ACK history is invalid: %w", err)
		}
		if ack.ID == id {
			if ack.Action != action || ack.Status != "ok" {
				return fmt.Errorf("original retained disposition ACK did not succeed")
			}
			return nil
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	return fmt.Errorf("original retained disposition ACK is missing")
}
