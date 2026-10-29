package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
	loopruntime "github.com/poteto/noodle/runtime"
)

type completedTaskAcceptanceRequest struct {
	OrderID, SessionID string
	Acceptance         loopruntime.SessionReadbackFileRef
}

func (l *Loop) AcceptCompletedTask(ctx context.Context, orderID, sessionID string, acceptance loopruntime.SessionReadbackFileRef) error {
	request := releaseRequest{ctx: ctx, reply: make(chan error, 1), deliveryAcceptance: &completedTaskAcceptanceRequest{orderID, sessionID, acceptance}}
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

func (l *Loop) validateCompletedAcceptance(order state.OrderNode, acceptance loopruntime.TaskDeliveryAcceptance) error {
	if order.Status != state.OrderCompleted || len(order.Stages) != 1 || acceptance.OrderID != order.OrderID {
		return fmt.Errorf("original accepted task is not completed")
	}
	stage := order.Stages[0]
	if stage.Status != state.StageCompleted || stage.Merge != nil || len(stage.Attempts) == 0 {
		return fmt.Errorf("completed task custody is unresolved")
	}
	latest := stage.Attempts[len(stage.Attempts)-1]
	if latest.Status != state.AttemptCompleted || latest.SessionID != acceptance.SessionID {
		return fmt.Errorf("outer acceptance session differs")
	}
	if err := l.checkCompletedLocalTask(order, stage); err != nil {
		return err
	}
	var binding localTaskMergeBinding
	if err := json.Unmarshal(stage.Extra[localTaskMergeKey], &binding); err != nil {
		return err
	}
	if binding.Head != acceptance.CandidateHead {
		return fmt.Errorf("outer acceptance integrated head differs")
	}
	return nil
}

func (l *Loop) acceptCompletedTask(ctx context.Context, request completedTaskAcceptanceRequest) error {
	if err := l.checkQuiescent(); err != nil {
		return err
	}
	if !l.canonicalLoaded || l.effectLedger == nil || !reducer.ReclaimableTaskData(l.effectLedger.All(), request.OrderID) {
		return fmt.Errorf("original admission does not authorize task acceptance reclamation")
	}
	acceptance, err := loopruntime.ReadTaskDeliveryAcceptance(request.Acceptance)
	if err != nil {
		return err
	}
	order, ok := l.canonical.Orders[request.OrderID]
	if !ok || acceptance.SessionID != request.SessionID {
		return fmt.Errorf("original acceptance task binding differs")
	}
	if err := l.validateCompletedAcceptance(order, acceptance); err != nil {
		return err
	}
	if err := l.observeCleanupSessions(ctx); err != nil {
		return err
	}
	path := l.executionClosurePath(order.OrderID)
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		return fmt.Errorf("original task already accepted or closure path unresolved; read its receipt")
	}
	closure := taskExecutionClosure{Order: order, Reason: "original task owner accepted completed delivery", Diagnostics: map[string]loopruntime.SessionReadbackFileRef{"owner_acceptance": request.Acceptance}}
	raw, err := json.Marshal(closure)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return filex.WriteFileAtomicDurable(path, raw)
}
