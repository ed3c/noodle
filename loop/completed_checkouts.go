package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/poteto/noodle/worktree"
	"github.com/poteto/noodle/worktree/archive"
)

// Run's original quiescent cleanup actor is the only caller. Policy comes from
// the bound Host constructor, not a task-selected authority document.
func (l *Loop) cleanupOwnedCheckouts(apply bool) error {
	if l.deps.CheckoutArchivePolicy == nil {
		return nil // Legacy context release does not claim physical reclamation.
	}
	app, ok := l.deps.Worktree.(*worktree.App)
	if !ok {
		return fmt.Errorf("original completed checkout owner is unavailable")
	}
	ids := make([]string, 0, len(l.canonical.Orders))
	for id := range l.canonical.Orders {
		if id != scheduleOrderID {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		order := l.canonical.Orders[id]
		if len(order.Stages) != 1 || !l.isAdmittedLocalTask(order, order.Stages[0]) {
			return fmt.Errorf("completed checkout Main disposition is unavailable: %s", id)
		}
		stage := order.Stages[0]
		// The original VM retirement owner is a separate required effect. This
		// process-only slice must not report an OpenShell lifecycle reclaimed.
		if stage.Runtime != "" && stage.Runtime != "process" {
			return fmt.Errorf("completed checkout requires original %s retirement owner: %s", stage.Runtime, id)
		}
		if len(stage.Attempts) == 0 {
			return fmt.Errorf("completed checkout original attempt is unavailable")
		}
		attempt := stage.Attempts[len(stage.Attempts)-1]
		if !cleanupSessionName(attempt.WorktreeName) {
			return fmt.Errorf("completed checkout original worktree name is unavailable")
		}
		for _, prior := range stage.Attempts {
			if prior.WorktreeName != attempt.WorktreeName {
				return fmt.Errorf("historical checkout requires its original disposition/evidence; preserve %s", prior.WorktreeName)
			}
		}
		expected, candidate, err := l.completedCheckoutCandidate(order, stage, attempt)
		if err != nil {
			return err
		}
		tree, err := recoveryGitWithExecution(l.deps.Execution, l.projectDir, "rev-parse", expected.Head+"^{tree}")
		if err != nil || tree != expected.Tree {
			return fmt.Errorf("original completed candidate tree differs: %v", err)
		}
		operation := l.completedCheckoutOperation(id, attempt.AttemptID, candidate)
		if err := app.CompletedCheckout(context.Background(), attempt.WorktreeName, operation, expected, *l.deps.CheckoutArchivePolicy, apply); err != nil {
			return fmt.Errorf("completed checkout %s remains held: %w", id, err)
		}
	}
	return nil
}

func (l *Loop) completedCheckoutOperation(orderID, attemptID string, candidate localTaskFileRef) string {
	return "completed-checkout/" + recoveryDigest([]byte(l.deps.AdmissionContext+"\x00"+orderID+"\x00"+attemptID+"\x00"+candidate.SHA256))
}

// Capacity settlement consumes this proof in the same original actor. Readiness
// and legacy release are insufficient: every checkout must have a known effect.
type completedCheckoutReclamation struct {
	OrderID       string
	StageIndex    int
	AttemptID     string
	SessionID     string
	RetryCount    int
	WorktreeName  string
	Runtime       string
	AttemptStatus state.AttemptStatus
	Candidate     localTaskFileRef
	Head          string
	Tree          string
	Branch        string
	ControlID     string
	ControlAction string
	MainACK       loopruntime.SessionReadbackFileRef
	Archive       archive.CheckoutReclamationEvidence
}

func (l *Loop) verifyCompletedCheckoutReclamation() ([]completedCheckoutReclamation, error) {
	var reclaimed []completedCheckoutReclamation
	if l.deps.CheckoutArchivePolicy == nil {
		return nil, fmt.Errorf("legacy context has no qualified checkout reclamation policy")
	}
	if err := l.checkQuiescent(); err != nil {
		return nil, err
	}
	if err := l.checkCompletedTasks(); err != nil {
		return nil, err
	}
	app, ok := l.deps.Worktree.(*worktree.App)
	if !ok {
		return nil, fmt.Errorf("original completed checkout owner is unavailable")
	}
	if l.deps.Execution == nil {
		return nil, fmt.Errorf("original checkout execution source is unavailable")
	}
	if _, err := worktree.PrepareStorageExcludes(context.Background(), l.deps.Execution); err != nil {
		return nil, err
	}
	for id, order := range l.canonical.Orders {
		if id == scheduleOrderID {
			continue
		}
		if len(order.Stages) != 1 || !l.isAdmittedLocalTask(order, order.Stages[0]) || len(order.Stages[0].Attempts) == 0 {
			return nil, fmt.Errorf("original completed task disposition is unavailable: %s", id)
		}
		stage := order.Stages[0]
		attempt := stage.Attempts[len(stage.Attempts)-1]
		if !cleanupSessionName(attempt.WorktreeName) {
			return nil, fmt.Errorf("original completed checkout name is unavailable")
		}
		expected, candidate, err := l.completedCheckoutCandidate(order, stage, attempt)
		if err != nil {
			return nil, err
		}
		tree, err := recoveryGitWithExecution(l.deps.Execution, l.projectDir, "rev-parse", expected.Head+"^{tree}")
		if err != nil || tree != expected.Tree {
			return nil, fmt.Errorf("original completed candidate tree differs: %v", err)
		}
		operation := l.completedCheckoutOperation(id, attempt.AttemptID, candidate)
		packet, err := archive.ReadCheckoutPacket(*l.deps.CheckoutArchivePolicy, operation)
		if err != nil {
			return nil, fmt.Errorf("original checkout removal evidence unavailable: %w", err)
		}
		logical := worktree.WorktreePath(app.Root, attempt.WorktreeName)
		base, err := filepath.EvalSymlinks(filepath.Dir(logical))
		if err != nil || packet.Source.Path != filepath.Join(base, filepath.Base(logical)) || packet.Source.Head != expected.Head || packet.Source.Branch != expected.Branch {
			return nil, fmt.Errorf("original removed checkout identity differs: %v", err)
		}
		if _, err := os.Lstat(logical); !os.IsNotExist(err) {
			return nil, fmt.Errorf("original logical checkout is not absent: %v", err)
		}
		archived, err := archive.ReadCheckoutReclamation(packet)
		if err != nil {
			return nil, fmt.Errorf("original checkout reclamation closure is unresolved: %w", err)
		}
		_, ackPath, _ := l.controlPaths()
		ack, err := loopruntime.ReadSessionReadbackFile(ackPath, nil)
		if err != nil {
			return nil, err
		}
		controlID, controlAction := "", "merge-local-task"
		if len(stage.Extra[localTaskRetentionKey]) != 0 {
			retained, _, err := retainedLocalTask(order)
			if err != nil {
				return nil, err
			}
			controlID, controlAction = retained.ControlID, "retain-local-task"
		} else {
			var binding localTaskMergeBinding
			if err := decodeStoppedReview(stage.Extra[localTaskMergeKey], &binding); err != nil {
				return nil, err
			}
			controlID = binding.ControlID
		}
		for retry, original := range stage.Attempts {
			if original.WorktreeName != attempt.WorktreeName {
				return nil, fmt.Errorf("historical checkout reclamation evidence unavailable: %s", original.WorktreeName)
			}
			reclaimed = append(reclaimed, completedCheckoutReclamation{OrderID: id, StageIndex: stage.StageIndex, AttemptID: original.AttemptID, SessionID: original.SessionID, RetryCount: retry, WorktreeName: original.WorktreeName, Runtime: stage.Runtime, AttemptStatus: original.Status, Candidate: candidate, Head: expected.Head, Tree: expected.Tree, Branch: expected.Branch, ControlID: controlID, ControlAction: controlAction, MainACK: ack, Archive: archived})
		}
	}
	return reclaimed, nil
}

func (l *Loop) completedCheckoutCandidate(order state.OrderNode, stage state.StageNode, attempt state.AttemptNode) (worktree.CheckoutCandidate, localTaskFileRef, error) {
	var expected worktree.CheckoutCandidate
	if len(stage.Extra[localTaskRetentionKey]) != 0 {
		retained, _, err := retainedLocalTask(order)
		if err != nil {
			return expected, localTaskFileRef{}, err
		}
		if err = l.checkRetainedControlACK(retained.ControlID); err != nil {
			return expected, retained.Candidate, err
		}
		return worktree.CheckoutCandidate{Head: retained.Head, Tree: retained.Tree, Branch: retained.Branch}, retained.Candidate, nil
	}
	var binding localTaskMergeBinding
	if err := decodeStoppedReview(stage.Extra[localTaskMergeKey], &binding); err != nil {
		return expected, binding.Candidate, err
	}
	if binding.ControlID == "" || !strings.HasPrefix(binding.Branch, "refs/heads/") {
		return expected, binding.Candidate, fmt.Errorf("original Main integration control or branch evidence is missing; preserve legacy checkout")
	}
	if err := l.checkDispositionControlACK(binding.ControlID, "merge-local-task"); err != nil {
		return expected, binding.Candidate, err
	}
	merges := 0
	for _, record := range l.effectLedger.All() {
		if record.Effect.Type != reducer.EffectMerge {
			continue
		}
		var payload struct {
			OrderID    string `json:"order_id"`
			Worktree   string `json:"worktree_name"`
			StageIndex int    `json:"stage_index"`
			Mode       string `json:"merge_mode"`
		}
		if json.Unmarshal(record.Effect.Payload, &payload) != nil || payload.OrderID != order.OrderID {
			continue
		}
		if record.Status != reducer.EffectLedgerDone || record.Result == nil || record.Result.Status != reducer.EffectResultCompleted || record.Result.EffectID != record.EffectID || payload.Worktree != attempt.WorktreeName || payload.StageIndex != stage.StageIndex || payload.Mode != "local" {
			return expected, binding.Candidate, fmt.Errorf("original Main integration effect is unresolved")
		}
		merges++
	}
	if merges != 1 {
		return expected, binding.Candidate, fmt.Errorf("original Main integration effect is not unique")
	}
	if _, err := recoveryGitWithExecution(l.deps.Execution, l.projectDir, "merge-base", "--is-ancestor", binding.Head, "HEAD"); err != nil {
		return expected, binding.Candidate, fmt.Errorf("original Main integration is not retained: %w", err)
	}
	return worktree.CheckoutCandidate{Head: binding.Head, Tree: binding.Tree, Branch: binding.Branch}, binding.Candidate, nil
}
