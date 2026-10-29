package loop

import (
	"strings"

	"github.com/poteto/noodle/worktree"
)

// recordStageFailure performs the state transition for a stage failure:
// builds failure metadata and emits StageFailed + OrderFailed loop events.
// It does NOT classify the order — callers handle classification after calling
// this (classifyOrderHard, classifyCookMistake, etc.).
func (l *Loop) recordStageFailure(cook *cookHandle, reason string, orderClass OrderFailureClass, mistake *AgentMistakeEnvelope) {
	failureMetadata := eventFailureMetadataForLoop(
		CycleFailureClassOrderHard,
		orderClass,
		mistake,
	)
	payload := StageFailedPayload{
		OrderID:    cook.orderID,
		StageIndex: cook.stageIndex,
		Reason:     reason,
		SessionID:  sessionIDPtr(cook),
		Failure:    &failureMetadata,
	}
	if mistake != nil {
		payload.AgentMistake = mistake
	}
	_ = l.events.Emit(LoopEventStageFailed, payload)
	_ = l.events.Emit(LoopEventOrderFailed, OrderFailedPayload{
		OrderID:      cook.orderID,
		Reason:       reason,
		AgentMistake: mistake,
		Failure:      &failureMetadata,
	})
}

// Bound task data outlives worker resources and requires an explicit deletion.
func (l *Loop) cleanupCookWorktree(cook *cookHandle) {
	if !l.retainTaskData(cook.orderID) && strings.TrimSpace(cook.worktreeName) != "" {
		_ = l.deps.Worktree.Cleanup(cook.worktreeName, worktree.CleanupOpts{Force: true})
	}
}
