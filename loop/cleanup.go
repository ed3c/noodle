package loop

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
)

func (l *Loop) cleanupAutomatic(ctx context.Context) error {
	if !l.deps.CleanupCompleted || l.effectiveMode() == state.RunModeManual || l.state == StatePaused || l.state == StateDraining {
		return fmt.Errorf("%w: automatic cleanup disabled or dispatch hold retained", ErrReleaseBusy)
	}
	return l.cleanupCompleted(ctx, true)
}

// Called only by Run: new Cycle work cannot race the completion decision.
func (l *Loop) cleanupCompleted(ctx context.Context, apply bool) (err error) {
	if apply && l.cleanupUnknown != nil {
		return l.cleanupUnknown
	}
	defer func() {
		if apply && err != nil && !errors.Is(err, ErrReleaseBusy) {
			l.cleanupUnknown = err
		}
	}()
	lock, err := lockfile.TryLock(filepath.Join(l.runtimeDir, "control.lock"))
	if err != nil {
		var busy *lockfile.AlreadyLockedError
		if errors.As(err, &busy) {
			return fmt.Errorf("%w: control producer is active: %v", ErrReleaseBusy, err)
		}
		return fmt.Errorf("control custody is unresolved: %w", err)
	}
	defer lock.Close()
	if err := l.cleanupPhase(ctx, "quiescence", "", "", l.checkQuiescent); err != nil {
		return err
	}
	if err := l.cleanupPhase(ctx, "completion_readback", "", "", func() error { return l.checkCompletedTasks(ctx) }); err != nil {
		return err
	}
	if !apply {
		l.cleanupUnknown = nil
		return nil
	}
	if err := l.reclaimExecutionCopies(ctx); err != nil {
		return err
	}
	return l.cleanupPhase(ctx, "release", "", "", l.finishRelease)
}

func (l *Loop) checkCompletedTasks(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !l.canonicalLoaded || l.effectLedger == nil {
		return fmt.Errorf("native completion evidence is unavailable")
	}
	if err := l.canonical.Validate(); err != nil {
		return fmt.Errorf("native completion evidence is invalid: %w", err)
	}
	if len(l.canonical.PendingReviews) != 0 || len(l.canonical.ActionNeeded) != 0 {
		return fmt.Errorf("%w: pending owner work remains", ErrReleaseBusy)
	}
	completed := 0
	for id, order := range l.canonical.Orders {
		if err := ctx.Err(); err != nil {
			return err
		}
		if id == scheduleOrderID {
			for _, stage := range order.Stages {
				if stage.Status != state.StagePending || len(stage.Attempts) != 0 {
					return fmt.Errorf("%w: schedule is not idle", ErrReleaseBusy)
				}
			}
			continue
		}
		closedFailure := order.Status == state.OrderFailed || order.Status == state.OrderCancelled
		if closedFailure {
			if _, err := l.failedExecutionClosure(order); err != nil {
				return fmt.Errorf("%w: order %s requires explicit failure closure: %v", ErrReleaseBusy, id, err)
			}
		}
		if (!closedFailure && order.Status != state.OrderCompleted) || len(order.Stages) == 0 {
			return fmt.Errorf("%w: order %s is not completed", ErrReleaseBusy, id)
		}
		for _, stage := range order.Stages {
			if ((!closedFailure && stage.Status != state.StageCompleted && stage.Status != state.StageSkipped) || (closedFailure && stage.Status != state.StageCompleted && stage.Status != state.StageSkipped && stage.Status != state.StageFailed && stage.Status != state.StageCancelled)) || stage.Merge != nil {
				return fmt.Errorf("%w: order %s has unfinished stage custody", ErrReleaseBusy, id)
			}
			if !closedFailure && (stage.TaskKey == "local-task" || stage.Skill == "local-task") {
				if err := l.checkCompletedLocalTask(order, stage); err != nil {
					return err
				}
			}
		}
		if !closedFailure && reducer.ReclaimableTaskData(l.effectLedger.All(), id) {
			if _, err := l.retainExecutionClosure(order); err != nil {
				return fmt.Errorf("%w: order %s awaits original outer acceptance: %v", ErrReleaseBusy, id, err)
			}
		}
		completed++
	}
	if completed == 0 {
		return fmt.Errorf("%w: no retained completed task evidence", ErrReleaseBusy)
	}
	// Reuse the admission owner's distinction between declarative effects and
	// unresolved execution. Completion text alone cannot relax an unknown merge.
	snapshot := reducer.BuildSnapshot(l.canonical, l.effectLedger, time.Time{})
	if err := validateAdmissionSnapshot(snapshot); err != nil {
		return fmt.Errorf("completion evidence is unresolved: %w", err)
	}
	if err := l.observeCleanupSessions(ctx); err != nil {
		return fmt.Errorf("original session readback is unresolved: %w", err)
	}
	return nil
}

// Local work keeps the Run actor's cancellation and custody. A caller losing
// its response does not cancel this phase or hand the owner to another actor.
func (l *Loop) cleanupPhase(ctx context.Context, phase, order, session string, run func() error) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	started := time.Now()
	if l.logger != nil {
		l.logger.Info("cleanup phase started", "phase", phase, "order", order, "session", session)
		defer func() {
			if err != nil {
				l.logger.Error("cleanup phase failed", "phase", phase, "order", order, "session", session, "elapsed", time.Since(started), "error", err)
			} else {
				l.logger.Info("cleanup phase completed", "phase", phase, "order", order, "session", session, "elapsed", time.Since(started))
			}
		}()
	}
	if err = run(); err == nil {
		err = ctx.Err()
	}
	return err
}

func (l *Loop) checkCompletedLocalTask(order state.OrderNode, stage state.StageNode) error {
	if !l.isAdmittedLocalTask(order, stage) {
		return fmt.Errorf("local task completion admission is unresolved: %s", order.OrderID)
	}
	raw := stage.Extra[localTaskMergeKey]
	if len(raw) == 0 {
		return fmt.Errorf("%w: local task %s still requires candidate acceptance or completion recovery", ErrReleaseBusy, order.OrderID)
	}
	var binding localTaskMergeBinding
	if json.Unmarshal(raw, &binding) != nil || !reconciliationSHA.MatchString(binding.Head) || !reconciliationSHA.MatchString(binding.Tree) {
		return fmt.Errorf("local task completed merge binding is invalid: %s", order.OrderID)
	}
	candidate, err := readLocalTaskCandidate(binding.Candidate)
	if err != nil {
		return fmt.Errorf("local task completed candidate is unresolved: %w", err)
	}
	if len(stage.Attempts) == 0 || candidate.OrderID != order.OrderID || candidate.SessionID != stage.Attempts[len(stage.Attempts)-1].SessionID || candidate.AttemptID != stage.Attempts[len(stage.Attempts)-1].AttemptID {
		return fmt.Errorf("local task completed candidate identity differs: %s", order.OrderID)
	}
	return nil
}
