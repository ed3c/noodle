package loop

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/poteto/noodle/event"
	"github.com/poteto/noodle/internal/ingest"
	"github.com/poteto/noodle/worktree"
)

func (l *Loop) controlMerge(orderID string) error {
	orderID = strings.TrimSpace(orderID)
	if orderID == "" {
		return fmt.Errorf("merge: order ID empty")
	}
	pending, ok := l.cooks.pendingReview[orderID]
	if !ok {
		return fmt.Errorf("no pending review for %q", orderID)
	}
	if err := l.ensureCanonicalOrderFromOrders(orderID); err != nil {
		return err
	}

	// Merge the worktree.
	cook := &cookHandle{
		cookIdentity: pending.cookIdentity,
		orderStatus:  OrderStatusActive,
		worktreeName: pending.worktreeName,
		worktreePath: pending.worktreePath,
		session:      &adoptedSession{id: pending.sessionID, status: "completed"},
	}
	canMerge, err := l.worktreeHasChanges(cook)
	if err != nil {
		return fmt.Errorf("merge check: %w", err)
	}
	if !canMerge {
		if err := l.runDoneBeforeTerminal(context.Background(), cook); err != nil {
			return err
		}
	}
	if err := l.emitEventChecked(ingest.EventStageReviewApproved, map[string]any{
		"order_id":    cook.orderID,
		"stage_index": cook.stageIndex,
		"reason":      "approved by user",
	}); err != nil {
		return err
	}
	if err := l.emitEventChecked(ingest.EventStageCompleted, l.mergeLifecyclePayload(cook, canMerge)); err != nil {
		return err
	}

	if !canMerge {
		if err := l.advanceAndPersist(context.Background(), cook); err != nil {
			return err
		}
	} else {
		if l.mergeQueue == nil {
			if err := l.mergeCookWorktree(context.Background(), cook); err != nil {
				return err
			}
			if err := l.runDoneBeforeTerminal(context.Background(), cook); err != nil {
				return err
			}
			if err := l.emitEventChecked(ingest.EventMergeCompleted, map[string]any{
				"order_id":    cook.orderID,
				"stage_index": cook.stageIndex,
			}); err != nil {
				return err
			}
			if err := l.advanceAndPersist(context.Background(), cook); err != nil {
				return err
			}
		} else {
			// Queued path: drainMergeResults emits merge_completed.
			l.mergeQueue.Enqueue(MergeRequest{Cook: cook})
			if err := l.drainMergeResults(context.Background()); err != nil {
				return err
			}
		}
	}
	return l.syncPendingReviewProjection()
}

func (l *Loop) controlReject(orderID string) error {
	orderID = strings.TrimSpace(orderID)
	if orderID == "" {
		return fmt.Errorf("reject: order ID empty")
	}
	pending, ok := l.cooks.pendingReview[orderID]
	if !ok {
		return fmt.Errorf("no pending review for %q", orderID)
	}
	if err := l.ensureCanonicalOrderFromOrders(orderID); err != nil {
		return err
	}
	if strings.TrimSpace(pending.worktreeName) != "" {
		_ = l.deps.Worktree.Cleanup(pending.worktreeName, worktree.CleanupOpts{Force: true})
	}
	if err := l.emitEventChecked(ingest.EventStageReviewRejected, map[string]any{
		"order_id":    orderID,
		"stage_index": pending.stageIndex,
		"reason":      "rejected by user",
	}); err != nil {
		return err
	}
	if err := l.mirrorLegacyOrderFromCanonical(orderID); err != nil {
		return err
	}
	if err := l.syncPendingReviewProjection(); err != nil {
		return err
	}
	mistake := newCookMistakeEnvelope(cookRejectReasonForTask(pending.stage.TaskKey), orderID, pending.stageIndex)
	cook := &cookHandle{
		cookIdentity: pending.cookIdentity,
		session:      &adoptedSession{id: pending.sessionID, status: "completed"},
	}
	l.recordStageFailure(cook, "rejected by user", OrderFailureClassOrderTerminal, &mistake)
	l.classifyCookMistake(
		"control.review_reject",
		OrderFailureClassOrderTerminal,
		orderID,
		pending.stageIndex,
		"rejected by user",
		mistake.CookReason,
	)
	l.forwardToScheduler(cook, "review_rejected", "rejected by user", &mistake)
	return nil
}

func (l *Loop) controlRequestChanges(orderID, feedback string) error {
	orderID = strings.TrimSpace(orderID)
	if orderID == "" {
		return fmt.Errorf("request-changes: order ID empty")
	}
	pending, ok := l.cooks.pendingReview[orderID]
	if !ok {
		return fmt.Errorf("no pending review for %q", orderID)
	}
	if err := l.ensureCanonicalOrderFromOrders(orderID); err != nil {
		return err
	}
	if l.atMaxConcurrency() {
		l.logger.Info("request-changes deferred: at max concurrency", "order", orderID)
		return nil
	}

	// Only an exact typed blocked terminal attempt gains restart recovery.
	recoverable := false
	review := l.canonical.PendingReviews[orderID]
	outcome, outcomeErr := l.readRequiredStageOutcome(&cookHandle{cookIdentity: pending.cookIdentity, session: &adoptedSession{id: pending.sessionID}})
	if outcomeErr == nil && outcome.Outcome == event.StageOutcomeBlocked {
		if l.canonical.Orders[orderID].Status.IsTerminal() {
			return fmt.Errorf("request-changes order already failed; use edit-item then requeue")
		}
		captured, err := l.captureRequestChanges(pending)
		if err != nil {
			return err
		}
		if err := l.validateRecovery(l.canonical.Orders[orderID], review, captured); err != nil {
			return err
		}
		recoverable = true
		raw, err := json.Marshal(captured)
		if err != nil {
			return err
		}
		node := l.canonical.Orders[orderID]
		if node.Stages[pending.stageIndex].Extra == nil {
			node.Stages[pending.stageIndex].Extra = map[string]json.RawMessage{}
		}
		node.Stages[pending.stageIndex].Extra[requestChangesKey] = raw
		l.canonical.Orders[orderID] = node
		if err := l.persistCanonicalCheckpoint(); err != nil {
			return err
		}
	}

	reason := "changes requested"
	trimmedFeedback := strings.TrimSpace(feedback)
	if trimmedFeedback != "" {
		reason += ": " + trimmedFeedback
	}
	mistake := newCookMistakeEnvelope(CookMistakeReasonRequestChanges, orderID, pending.stageIndex)
	if err := l.emitEventChecked(ingest.EventStageReviewChangesRequested, map[string]any{
		"order_id":    orderID,
		"stage_index": pending.stageIndex,
		"reason":      reason,
	}); err != nil {
		return err
	}
	if recoverable {
		l.canonical.PendingReviews[orderID] = review
		if err := l.persistCanonicalCheckpoint(); err != nil {
			return err
		}
		if err := l.projectRecoveredOrder(orderID); err != nil {
			return err
		}
	}
	if err := l.mirrorLegacyOrderFromCanonical(orderID); err != nil {
		return err
	}
	if err := l.syncPendingReviewProjection(); err != nil {
		return err
	}
	cook := &cookHandle{
		cookIdentity: pending.cookIdentity,
		worktreeName: pending.worktreeName,
		session:      &adoptedSession{id: pending.sessionID, status: "completed"},
	}
	l.recordStageFailure(cook, reason, OrderFailureClassStageTerminal, &mistake)
	l.classifyCookMistake(
		"control.request_changes",
		OrderFailureClassStageTerminal,
		orderID,
		pending.stageIndex,
		reason,
		mistake.CookReason,
	)
	l.forwardToScheduler(cook, "request_changes", reason, &mistake)
	return nil
}
