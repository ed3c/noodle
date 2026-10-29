package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
	loopruntime "github.com/poteto/noodle/runtime"
)

type failedTaskClosureRequest struct {
	OrderID, SessionID, Reason string
	Diagnostics                loopruntime.SessionReadbackFileRef
}

// CloseFailedTask is an explicit original-flow decision. It is serialized by
// Run, persists diagnostic custody, and never marks the failed task successful.
func (l *Loop) CloseFailedTask(ctx context.Context, orderID, sessionID, reason string, diagnostics loopruntime.SessionReadbackFileRef) error {
	request := releaseRequest{ctx: ctx, reply: make(chan error, 1), failureClosure: &failedTaskClosureRequest{OrderID: orderID, SessionID: sessionID, Reason: reason, Diagnostics: diagnostics}}
	select {
	case l.releaseRequests <- request:
	case <-l.runDone:
		return fmt.Errorf("original Loop is no longer running")
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-request.reply:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *Loop) closeFailedTask(request failedTaskClosureRequest) error {
	if strings.TrimSpace(request.Reason) == "" || len(request.Reason) > 4096 || !cleanupSessionName(request.SessionID) {
		return fmt.Errorf("failed task closure reason or session missing")
	}
	if err := l.checkQuiescent(); err != nil {
		return err
	}
	if !l.canonicalLoaded || l.effectLedger == nil || !reducer.ReclaimableTaskData(l.effectLedger.All(), request.OrderID) {
		return fmt.Errorf("original admission does not authorize task closure reclamation")
	}
	order, ok := l.canonical.Orders[request.OrderID]
	if !ok || len(order.Stages) != 1 || !l.isAdmittedLocalTask(order, order.Stages[0]) {
		return fmt.Errorf("original local task admission missing")
	}
	stage := order.Stages[0]
	if stage.Merge != nil || len(stage.Attempts) == 0 {
		return fmt.Errorf("failed task has unresolved merge or no attempt")
	}
	latest := stage.Attempts[len(stage.Attempts)-1]
	if latest.SessionID != request.SessionID || (latest.Status != state.AttemptFailed && latest.Status != state.AttemptCancelled) || (stage.Status != state.StageReview && stage.Status != state.StageFailed && stage.Status != state.StageCancelled) {
		return fmt.Errorf("latest original task attempt is not a settled failure")
	}
	if _, exists := stage.Extra[failedTaskClosureKey]; exists {
		return fmt.Errorf("original failure already closed; re-observe its receipt")
	}
	diagnostics := map[string]loopruntime.SessionReadbackFileRef{"original_flow_diagnostics": request.Diagnostics}
	if err := loopruntime.VerifySessionReadbackFiles(diagnostics); err != nil {
		return err
	}
	// Fresh original transport/publication/stop evidence is mandatory. A failed
	// status, timeout or incomplete download is insufficient diagnostic custody.
	if err := l.observeCleanupSessions(); err != nil {
		return err
	}
	order.Status = state.OrderFailed
	order.Stages[0].Status = state.StageFailed
	closure := taskExecutionClosure{Order: order, Reason: request.Reason, Diagnostics: diagnostics}
	raw, err := json.Marshal(closure)
	if err != nil {
		return err
	}
	path := l.executionClosurePath(order.OrderID)
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		return fmt.Errorf("failure closure path already exists or is unresolved")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if err := filex.WriteFileAtomicDurable(path, raw); err != nil {
		return err
	}
	ref, err := loopruntime.ReadSessionReadbackFile(path, nil)
	if err != nil {
		return err
	}
	binding, err := json.Marshal(ref)
	if err != nil {
		return err
	}
	if order.Stages[0].Extra == nil {
		order.Stages[0].Extra = map[string]json.RawMessage{}
	}
	order.Stages[0].Extra[failedTaskClosureKey] = binding
	l.canonical.Orders[order.OrderID] = order
	delete(l.canonical.PendingReviews, order.OrderID)
	delete(l.cooks.pendingReview, order.OrderID)
	if err := l.mirrorLegacyOrderFromCanonical(order.OrderID); err != nil {
		return err
	}
	return l.persistCanonicalCheckpoint()
}
