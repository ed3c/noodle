package loop

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
	loopruntime "github.com/poteto/noodle/runtime"
)

const failedTaskClosureKey = "failed_task_closure"

type taskExecutionClosure struct {
	Order       state.OrderNode                               `json:"order"`
	Reason      string                                        `json:"reason"`
	Diagnostics map[string]loopruntime.SessionReadbackFileRef `json:"diagnostics"`
}

type executionSessionReclaimer interface {
	ReclaimCleanupSession(context.Context, loopruntime.CompletedSessionBinding, string, loopruntime.SessionStatus, string, loopruntime.SessionReadbackFileRef) (loopruntime.CleanupSessionReadback, error)
}
type settledWorktreeRemover interface{ RemoveSettledExecutionCopy(string, string) error }
type settledWorktreeObserver interface{ ObserveSettledExecutionAbsent(string, string) error }

func (l *Loop) executionClosurePath(orderID string) string {
	return filepath.Join(l.runtimeDir, "execution-closures", recoveryDigest([]byte(orderID))+".json")
}

func (l *Loop) retainExecutionClosure(order state.OrderNode) (loopruntime.SessionReadbackFileRef, error) {
	path := l.executionClosurePath(order.OrderID)
	if order.Status != state.OrderCompleted {
		return l.failedExecutionClosure(order)
	}
	var existing taskExecutionClosure
	ref, err := loopruntime.ReadSessionReadbackFile(path, &existing)
	if err != nil {
		return ref, err
	}
	if existing.Reason != "original task owner accepted completed delivery" || !sameClosureJSON(existing.Order, order) || len(existing.Diagnostics) != 1 {
		return ref, fmt.Errorf("original accepted task closure differs")
	}
	acceptance, err := loopruntime.ReadTaskDeliveryAcceptance(existing.Diagnostics["owner_acceptance"])
	if err != nil {
		return ref, err
	}
	if err := l.validateCompletedAcceptance(order, acceptance); err != nil {
		return ref, err
	}
	return ref, nil
}

func (l *Loop) failedExecutionClosure(order state.OrderNode) (loopruntime.SessionReadbackFileRef, error) {
	if (order.Status != state.OrderFailed && order.Status != state.OrderCancelled) || len(order.Stages) == 0 {
		return loopruntime.SessionReadbackFileRef{}, fmt.Errorf("task is not explicitly closed")
	}
	var ref loopruntime.SessionReadbackFileRef
	if json.Unmarshal(order.Stages[len(order.Stages)-1].Extra[failedTaskClosureKey], &ref) != nil {
		return ref, fmt.Errorf("failed task closure is missing")
	}
	var closure taskExecutionClosure
	actual, err := loopruntime.ReadSessionReadbackFile(l.executionClosurePath(order.OrderID), &closure)
	if err != nil {
		return ref, err
	}
	if ref != actual || closure.Order.OrderID != order.OrderID || closure.Reason == "" || len(closure.Diagnostics) == 0 {
		return ref, fmt.Errorf("failed task closure evidence differs")
	}
	// Strip only this closure's own backlink; the terminal attempt and all other
	// source state must remain exactly what the original owner explicitly closed.
	current := order
	current.Stages = append([]state.StageNode(nil), order.Stages...)
	for i := range current.Stages {
		extra := map[string]json.RawMessage{}
		for k, v := range current.Stages[i].Extra {
			if k != failedTaskClosureKey {
				extra[k] = v
			}
		}
		current.Stages[i].Extra = extra
		if len(extra) == 0 {
			current.Stages[i].Extra = nil
		}
	}
	if !sameClosureJSON(current, closure.Order) {
		return ref, fmt.Errorf("failed task state changed after explicit closure")
	}
	return ref, loopruntime.VerifySessionReadbackFiles(closure.Diagnostics)
}

// Invoked only after completion/closure, session readback and quiescence checks.
// Never changes the task's result, retries a worker or touches retained history.
func (l *Loop) reclaimExecutionCopies() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for id, order := range l.canonical.Orders {
		if !reducer.ReclaimableTaskData(l.effectLedger.All(), id) {
			continue
		}
		authority, err := l.retainExecutionClosure(order)
		if err != nil {
			return err
		}
		receiptPath := filepath.Join(l.runtimeDir, "execution-closures", recoveryDigest([]byte(id))+".reclaimed.json")
		if _, err := os.Lstat(receiptPath); err == nil {
			existing, _, err := loopruntime.ReadExecutionReclamationReceipt(receiptPath, id, authority)
			if err != nil {
				return err
			}
			observer, ok := l.deps.Worktree.(settledWorktreeObserver)
			if !ok {
				return fmt.Errorf("original worktree absence readback capability unavailable")
			}
			for name, head := range existing.Worktrees {
				if err := observer.ObserveSettledExecutionAbsent(name, head); err != nil {
					return err
				}
			}
			continue
		} else if !os.IsNotExist(err) {
			return err
		}
		receipt := loopruntime.ExecutionReclamationReceipt{Schema: 1, OrderID: id, Authority: authority, Sessions: []loopruntime.ExecutionReclaimedSession{}, Worktrees: map[string]string{}}
		heads := map[string]string{}
		for index, stage := range order.Stages {
			for retry, attempt := range stage.Attempts {
				if stage.Runtime != "openshell" {
					return fmt.Errorf("execution reclamation unavailable for runtime %q", stage.Runtime)
				}
				reclaimer, ok := l.deps.Runtimes[stage.Runtime].(executionSessionReclaimer)
				if !ok {
					return fmt.Errorf("original runtime reclamation capability unavailable")
				}
				var outcome loopruntime.SessionStatus
				switch attempt.Status {
				case state.AttemptCompleted:
					outcome = loopruntime.StatusCompleted
				case state.AttemptFailed:
					outcome = loopruntime.StatusFailed
				case state.AttemptCancelled:
					outcome = loopruntime.StatusCancelled
				default:
					return fmt.Errorf("execution attempt remains unresolved")
				}
				project, err := canonicalDirectory(l.projectDir)
				if err != nil {
					return err
				}
				if !cleanupSessionName(attempt.WorktreeName) {
					return fmt.Errorf("execution worktree identity missing")
				}
				binding := loopruntime.CompletedSessionBinding{OrderID: id, SessionID: attempt.SessionID, WorktreePath: filepath.Join(project, ".worktrees", attempt.WorktreeName), Provider: stage.Provider, Model: stage.Model, StageIndex: index, RetryCount: retry}
				observed, err := reclaimer.ReclaimCleanupSession(ctx, binding, project, outcome, "original task owner closed execution; retained authority binds outcome", authority)
				if err != nil {
					return fmt.Errorf("reclaim session %s: %w", attempt.SessionID, err)
				}
				custodyPath := filepath.Join(l.runtimeDir, "execution-closures", recoveryDigest([]byte(id)), recoveryDigest([]byte(attempt.SessionID))+"-custody.json")
				observed, err = loopruntime.RetainReclamationCustody(observed, custodyPath)
				if err != nil {
					return err
				}
				receipt.Sessions = append(receipt.Sessions, loopruntime.ExecutionReclaimedSession{OrderID: id, SessionID: attempt.SessionID, Readback: observed})
				heads[attempt.WorktreeName] = observed.PublishedHead
			}
		}
		for name, head := range heads {
			remover, ok := l.deps.Worktree.(settledWorktreeRemover)
			if !ok {
				return fmt.Errorf("original worktree reclamation capability unavailable")
			}
			if err := remover.RemoveSettledExecutionCopy(name, head); err != nil {
				return err
			}
			receipt.Worktrees[name] = head
		}
		raw, err := json.Marshal(receipt)
		if err != nil {
			return err
		}
		if err := filex.WriteFileAtomicDurable(receiptPath, raw); err != nil {
			return err
		}
	}
	return nil
}

func sameClosureJSON(a, b any) bool {
	left, err := json.Marshal(a)
	if err != nil {
		return false
	}
	right, err := json.Marshal(b)
	return err == nil && bytes.Equal(left, right)
}
