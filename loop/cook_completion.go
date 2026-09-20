package loop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/poteto/noodle/adapter"
	"github.com/poteto/noodle/event"
	"github.com/poteto/noodle/internal/ingest"
	"github.com/poteto/noodle/internal/mode"
	"github.com/poteto/noodle/internal/state"
	"github.com/poteto/noodle/internal/stringx"
)

func (l *Loop) drainCompletions(ctx context.Context) error {
	drainedAny := false
	for {
		select {
		case result := <-l.completionBuf.completions:
			drainedAny = true
			if err := l.applyStageResult(ctx, result); err != nil {
				return err
			}
		default:
			goto drainedChannel
		}
	}

drainedChannel:
	if !drainedAny && l.watcherCount.Load() > 0 {
		waitCtx := ctx
		if waitCtx == nil {
			waitCtx = context.Background()
		}
		select {
		case result := <-l.completionBuf.completions:
			if err := l.applyStageResult(waitCtx, result); err != nil {
				return err
			}
			for {
				select {
				case late := <-l.completionBuf.completions:
					if err := l.applyStageResult(waitCtx, late); err != nil {
						return err
					}
				default:
					goto lateDrainDone
				}
			}
		case <-time.After(2 * time.Millisecond):
		case <-waitCtx.Done():
		}
	}

lateDrainDone:
	overflow := l.takeCompletionOverflow()
	for _, result := range overflow {
		if err := l.applyStageResult(ctx, result); err != nil {
			return err
		}
	}
	return l.collectAdoptedCompletions(ctx)
}

func (l *Loop) applyStageResult(ctx context.Context, result StageResult) error {
	if result.IsBootstrap {
		l.handleBootstrapResult(result)
		return nil
	}
	cook, exists := l.cooks.activeCooksByOrder[result.OrderID]
	if !exists {
		return nil
	}
	if cook.generation != result.Generation {
		return nil
	}
	l.trackCookCompleted(cook, result)
	delete(l.cooks.activeCooksByOrder, cook.orderID)
	if err := l.handleCompletion(ctx, cook, result.Status, string(result.Status)); err != nil {
		var adapterErr *completionAdapterError
		if errors.As(err, &adapterErr) {
			return err
		}
		if conflictErr := l.handleMergeConflict(cook, err); conflictErr != nil {
			return conflictErr
		}
	}
	return nil
}

func (l *Loop) handleBootstrapResult(result StageResult) {
	if l.bootstrapInFlight == nil {
		return
	}
	if l.bootstrapInFlight.generation != result.Generation {
		return
	}
	cook := l.bootstrapInFlight
	l.trackCookCompleted(cook, result)
	l.bootstrapInFlight = nil

	if result.Status == StageResultCompleted {
		l.rebuildRegistry()
		_ = l.events.Emit(LoopEventBootstrapCompleted, nil)
		l.logger.Info("bootstrap completed")
		return
	}

	l.bootstrapAttempts++
	if l.bootstrapAttempts >= 3 {
		l.bootstrapExhausted = true
	}
	l.logger.Warn("bootstrap failed", "attempt", l.bootstrapAttempts, "status", string(result.Status))
}

func (l *Loop) handleCompletion(ctx context.Context, cook *cookHandle, resultStatus StageResultStatus, rawStatus string) error {
	status := stringx.Normalize(rawStatus)
	if status == "" {
		status = stringx.Normalize(cook.session.Outcome().Status.String())
	}
	var typedMessage *string
	if l.requiresTypedOutcome(cook) && !isScheduleStage(cook.stage) {
		outcome, err := l.readRequiredStageOutcome(cook)
		if err != nil {
			return l.parkPendingReview(cook, "typed stage outcome refused: "+err.Error())
		}
		message := outcome.Message
		switch outcome.Outcome {
		case event.StageOutcomeBlocked:
			l.forwardToScheduler(cook, "stage_message_blocked", outcome.Message, nil)
			return l.parkPendingReview(cook, "blocked by typed stage outcome: "+outcome.Message)
		case event.StageOutcomeFailed:
			return l.failStage(ctx, cook, "cook reported failed outcome: "+outcome.Message)
		case event.StageOutcomeCompleted:
			resultStatus = StageResultCompleted
			status = string(StageResultCompleted)
			typedMessage = &message
		}
	}

	if resultStatus == StageResultCompleted {
		if isScheduleStage(cook.stage) {
			return l.handleScheduleCompletion(cook)
		}
		msg := typedMessage
		if msg == nil {
			blocked, legacyMessage := l.processStageMessage(cook)
			if blocked {
				return nil
			}
			msg = legacyMessage
		}
		canMerge, err := l.worktreeHasChanges(cook)
		if err != nil {
			return l.failStage(ctx, cook, fmt.Sprintf("merge check: %v", err))
		}
		gate := mode.ModeGate{}
		canAutoMerge := gate.CanAutoMerge(l.canonical.Mode)
		if canMerge && !canAutoMerge {
			reason := gate.BlockedReason(l.canonical.Mode, mode.ActionAutoMerge)
			if reason == "" {
				reason = "merge requires review"
			}
			return l.parkPendingReview(cook, reason)
		}
		mergeable := canMerge && canAutoMerge
		if !mergeable {
			if err := l.runDoneBeforeTerminal(ctx, cook); err != nil {
				if parkErr := l.parkPendingReview(cook, "backlog.done refused completion: "+err.Error()); parkErr != nil {
					return parkErr
				}
				return err
			}
		}
		if err := l.emitEventChecked(ingest.EventStageCompleted, l.mergeLifecyclePayload(cook, mergeable)); err != nil {
			return err
		}
		if mergeable {
			return l.completeWithMerge(ctx, cook, msg)
		}
		return l.completeWithoutMerge(ctx, cook, msg)
	}

	return l.handleStageFailure(ctx, cook, resultStatus, status)
}

// handleScheduleCompletion handles a successful schedule stage completion.
func (l *Loop) handleScheduleCompletion(cook *cookHandle) error {
	l.logger.Info("schedule completed", "session", cook.session.ID())
	_ = l.events.Emit(LoopEventScheduleCompleted, ScheduleCompletedPayload{
		SessionID: cook.session.ID(),
	})
	return l.removeOrder(cook.orderID)
}

// processStageMessage reads stage_message events from the session and handles
// blocking messages. Returns (blocked, message) where blocked is true if the
// stage message prevents advancement, and message is the non-blocking message
// pointer (nil if no message).
func (l *Loop) processStageMessage(cook *cookHandle) (bool, *string) {
	stageMsg := l.readStageMessage(cook.session.ID())
	if stageMsg == nil {
		return false, nil
	}
	if stageMsg.IsBlocking() {
		l.logger.Info("stage message blocks advance",
			"order", cook.orderID, "session", cook.session.ID())
		l.forwardToScheduler(cook, "stage_message_blocked", stageMsg.Message, nil)
		_ = l.parkPendingReview(cook, "blocked by stage message: "+stageMsg.Message)
		return true, nil
	}
	l.forwardToScheduler(cook, "stage_message", stageMsg.Message, nil)
	return false, &stageMsg.Message
}

// completeWithMerge handles a successful mergeable stage by persisting merge
// metadata and either directly merging or enqueueing to the merge queue.
func (l *Loop) completeWithMerge(ctx context.Context, cook *cookHandle, msg *string) error {
	l.logger.Info("cook completing",
		"order", cook.orderID, "session", cook.session.ID())
	if l.mergeQueue != nil {
		l.mergeQueue.Enqueue(MergeRequest{Cook: cook})
		l.logger.Info("cook queued for merge",
			"order", cook.orderID,
			"session", cook.session.ID(),
			"stage", cook.stageIndex)
		return nil
	}
	if err := l.mergeCookWorktree(ctx, cook); err != nil {
		return err
	}
	if err := l.runDoneBeforeTerminal(ctx, cook); err != nil {
		return err
	}
	if err := l.emitEventChecked(ingest.EventMergeCompleted, map[string]any{
		"order_id":    cook.orderID,
		"stage_index": cook.stageIndex,
	}); err != nil {
		return err
	}
	return l.advanceAndPersist(ctx, cook, msg)
}

// completeWithoutMerge handles a successful non-mergeable stage (e.g. review)
// by advancing directly without merge.
func (l *Loop) completeWithoutMerge(ctx context.Context, cook *cookHandle, msg *string) error {
	return l.advanceAndPersist(ctx, cook, msg)
}

// handleStageFailure handles a cook that exited with a non-success status.
func (l *Loop) handleStageFailure(ctx context.Context, cook *cookHandle, resultStatus StageResultStatus, status string) error {
	// Schedule cooks may write orders-next.json before exiting non-cleanly.
	if isScheduleStage(cook.stage) {
		if _, statErr := os.Stat(l.deps.OrdersNextFile); statErr == nil {
			l.logger.Info("schedule wrote orders-next before failing, treating as complete",
				"session", cook.session.ID())
			return l.removeOrder(cook.orderID)
		}
		if l.schedulePromoted {
			l.logger.Info("schedule already promoted, removing schedule order",
				"session", cook.session.ID())
			return l.removeOrder(cook.orderID)
		}
	}

	// Check for stage_yield — agent declared its deliverable complete before exit.
	if yield := l.readStageYield(cook.session.ID()); yield != nil {
		l.logger.Info("stage_yield overrides non-clean exit",
			"order", cook.orderID, "session", cook.session.ID(), "exit_status", status)
		l.forwardToScheduler(cook, "stage_yield", yield.Message, nil)
		return l.handleCompletion(ctx, cook, StageResultCompleted, "completed")
	}

	reason := "cook exited with status " + status
	if resultStatus == StageResultCancelled {
		reason = "cook cancelled with status " + status
	}
	return l.failStage(nil, cook, reason)
}

func (l *Loop) failStage(_ context.Context, cook *cookHandle, reason string) error {
	if err := l.ensureCanonicalOrderFromOrders(cook.orderID); err != nil {
		return err
	}
	if err := l.emitEventChecked(ingest.EventStageFailed, map[string]any{
		"order_id":    cook.orderID,
		"stage_index": cook.stageIndex,
		"error":       reason,
		"attempt_id":  dispatchAttemptID(cook.orderID, cook.stageIndex, cook.attempt),
		"session_id":  sessionIDPtr(cook),
	}); err != nil {
		return err
	}
	if err := l.mirrorLegacyOrderFromCanonical(cook.orderID); err != nil {
		return err
	}
	if err := l.syncPendingReviewProjection(); err != nil {
		return err
	}
	l.recordStageFailure(cook, reason, OrderFailureClassStageTerminal, nil)
	l.classifyOrderHard(
		"cycle.stage_terminal",
		OrderFailureClassStageTerminal,
		cook.orderID,
		cook.stageIndex,
		reason,
		nil,
	)

	l.forwardToScheduler(cook, "stage_failed", reason, nil)
	l.cleanupCookWorktree(cook)
	return nil
}

// advanceAndPersist advances the order stage and persists the result.
// The optional message is included in the stage.completed event payload.
func (l *Loop) advanceAndPersist(ctx context.Context, cook *cookHandle, message ...*string) error {
	if err := l.ensureCanonicalOrderFromOrders(cook.orderID); err != nil {
		return err
	}
	orderStatus := state.OrderLifecycleStatus("")
	if order, ok := l.canonical.Orders[cook.orderID]; ok {
		orderStatus = order.Status
	}
	if err := l.mirrorLegacyOrderFromCanonical(cook.orderID); err != nil {
		return err
	}
	if err := l.syncPendingReviewProjection(); err != nil {
		return err
	}
	var msg *string
	if len(message) > 0 {
		msg = message[0]
	}
	_ = l.events.Emit(LoopEventStageCompleted, StageCompletedPayload{
		OrderID:    cook.orderID,
		StageIndex: cook.stageIndex,
		TaskKey:    cook.stage.TaskKey,
		SessionID:  sessionIDPtr(cook),
		Message:    msg,
	})

	if orderStatus == state.OrderCompleted {
		_ = l.events.Emit(LoopEventOrderCompleted, OrderCompletedPayload{
			OrderID: cook.orderID,
		})
		if strings.EqualFold(strings.TrimSpace(cook.orderID), scheduleBootstrapOrderID) {
			if l.ensureSkillFresh(scheduleOrderID) {
				if err := l.mutateOrdersState(func(orders *OrdersFile) (bool, error) {
					if hasScheduleOrder(*orders) || hasScheduleBootstrapOrder(*orders) {
						return false, nil
					}
					prependOrder(orders, scheduleOrder(l.config, ""))
					return true, nil
				}); err != nil {
					return err
				}
				orders, err := l.currentOrders()
				if err != nil {
					return err
				}
				for _, order := range orders.Orders {
					if order.ID != scheduleOrderID {
						continue
					}
					l.syncCanonicalOrderFromLegacy(order)
					if err := l.persistCanonicalCheckpoint(); err != nil {
						return err
					}
					break
				}
				l.logger.Info("schedule bootstrap completed, injected schedule order")
			} else {
				l.logger.Warn("schedule bootstrap completed but schedule skill is still missing")
			}
		}
	}
	return nil
}

// runDoneBeforeTerminal invokes backlog.done only when completing this stage
// would make the order terminal. A refusal must be observed before the
// canonical event that commits stage/order completion.
func (l *Loop) runDoneBeforeTerminal(ctx context.Context, cook *cookHandle) error {
	order, ok := l.canonical.Orders[cook.orderID]
	if !ok || order.Status.IsTerminal() || cook.stageIndex < 0 || cook.stageIndex >= len(order.Stages) {
		return nil
	}
	for i, stage := range order.Stages {
		if i != cook.stageIndex && !stage.Status.IsTerminal() {
			return nil
		}
	}
	if _, err := l.deps.Adapter.Run(ctx, "backlog", "done", adapter.RunOptions{Args: []string{cook.orderID}}); err != nil && !isMissingAdapter(err) {
		return &completionAdapterError{err: err}
	}
	return nil
}

type completionAdapterError struct{ err error }

func (e *completionAdapterError) Error() string { return e.err.Error() }
func (e *completionAdapterError) Unwrap() error { return e.err }

// forwardToScheduler sends a message to the scheduler session about an event.
// Best-effort — if the scheduler is not alive, the message is dropped and the
// order stays in the orders file for later recovery.
func (l *Loop) forwardToScheduler(cook *cookHandle, eventType string, details string, mistake *AgentMistakeEnvelope) {
	var schedulerCook *cookHandle
	for _, c := range l.cooks.activeCooksByOrder {
		if isScheduleStage(c.stage) {
			schedulerCook = c
			break
		}
	}
	if schedulerCook == nil {
		l.logger.Info("scheduler not active, event stays in orders for later recovery",
			"order", cook.orderID, "event", eventType)
		return
	}
	controller := schedulerCook.session.Controller()
	if !controller.Steerable() {
		l.logger.Info("scheduler not steerable, event stays in orders for later recovery",
			"order", cook.orderID, "event", eventType)
		return
	}
	classification := ""
	if mistake != nil {
		classification = fmt.Sprintf(" owner=%s scope=%s", mistake.Owner, mistake.Scope)
		if reason := agentMistakeReason(mistake); reason != "" {
			classification += " reason=" + reason
		}
	}
	msg := fmt.Sprintf("[%s]%s order=%s stage=%d task=%s: %s",
		eventType, classification, cook.orderID, cook.stageIndex, cook.stage.TaskKey, details)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := controller.SendMessage(ctx, msg); err != nil {
			l.logger.Warn("failed to forward to scheduler", "order", cook.orderID, "err", err)
		}
	}()
}

func (l *Loop) collectAdoptedCompletions(ctx context.Context) error {
	for targetID, sessionID := range l.cooks.adoptedTargets {
		status, ok, err := l.readSessionStatus(sessionID)
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		switch status {
		case "running", "stuck", "spawning":
			continue
		}
		cook, processable, err := l.buildAdoptedCook(targetID, sessionID, status)
		if err != nil {
			return err
		}
		if !processable {
			l.logger.Info("adopted session dropped", "order", targetID, "session", sessionID)
			l.dropAdoptedTarget(targetID, sessionID)
			continue
		}
		l.logger.Info("adopted session completed", "order", targetID, "session", sessionID, "status", status)
		resultStatus, _ := stageResultFromSessionMetaStatus(status)
		if err := l.handleCompletion(ctx, cook, resultStatus, status); err != nil {
			if conflictErr := l.handleMergeConflict(cook, err); conflictErr != nil {
				return conflictErr
			}
		}
		l.dropAdoptedTarget(targetID, sessionID)
	}
	return nil
}

// removeOrder removes an order from orders.json by ID.
func (l *Loop) removeOrder(id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("remove order ID missing")
	}
	removed := false
	if err := l.mutateOrdersState(func(orders *OrdersFile) (bool, error) {
		filtered := make([]Order, 0, len(orders.Orders))
		for _, order := range orders.Orders {
			if order.ID == id {
				removed = true
				continue
			}
			filtered = append(filtered, order)
		}
		if !removed {
			return false, nil
		}
		orders.Orders = filtered
		return true, nil
	}); err != nil {
		return err
	}
	if removed {
		delete(l.canonical.Orders, id)
		if err := l.persistCanonicalCheckpoint(); err != nil {
			return err
		}
		l.logger.Info("order removed", "order", id)
	}
	return nil
}
