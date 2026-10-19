package loop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var ErrReleaseBusy = errors.New("loop has unfinished work")

type TerminalCause string

const (
	TerminalAutoCompleted   TerminalCause = "auto-completed"
	TerminalExplicitCleanup TerminalCause = "explicit-cleanup"
	TerminalMigrationDetach TerminalCause = "migration-detach"
)

// TerminalCause is read only after Run returns. Empty means no recorded cause.
func (l *Loop) TerminalCause() TerminalCause { return l.terminalCause }

type releaseRequest struct {
	ctx       context.Context
	reply     chan error
	cleanup   bool
	automatic bool
	apply     bool
}

func (l *Loop) Ready() <-chan struct{} { return l.runReady }
func (l *Loop) Done() <-chan struct{}  { return l.runDone }

// FinishOnce is called by the sole Cycle owner after a manual bootstrap cycle.
func (l *Loop) FinishOnce() error { return l.releaseIfQuiescent() }

// Release asks the Run actor to stop only after its workers and inputs settle.
func (l *Loop) Release(ctx context.Context) error {
	return l.requestRelease(ctx, false, true, false)
}

// Cleanup requires completed task evidence; Release also permits held migration.
func (l *Loop) Cleanup(ctx context.Context, apply bool) error {
	return l.requestRelease(ctx, true, apply, false)
}

func (l *Loop) CleanupAutomatic(ctx context.Context) error {
	return l.requestRelease(ctx, true, true, true)
}

func (l *Loop) requestRelease(ctx context.Context, cleanup, apply, automatic bool) error {
	request := releaseRequest{ctx: ctx, reply: make(chan error, 1), cleanup: cleanup, apply: apply, automatic: automatic}
	select {
	case l.releaseRequests <- request:
	case <-l.runDone:
		return errors.New("loop is no longer running")
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

func (l *Loop) releaseIfQuiescent() error {
	if err := l.checkQuiescent(); err != nil {
		return err
	}
	return l.finishRelease()
}

func (l *Loop) checkQuiescent() error {
	if l.resourceUnknown != nil {
		return l.resourceUnknown
	}
	if len(l.cooks.activeCooksByOrder) != 0 || len(l.cooks.adoptedSessions) != 0 ||
		len(l.cooks.adoptedTargets) != 0 || l.bootstrapInFlight != nil || l.watcherCount.Load() != 0 {
		return fmt.Errorf("%w: workers are active", ErrReleaseBusy)
	}
	if l.mergeQueue != nil {
		l.mergeQueue.mu.Lock()
		busy := l.mergeQueue.pending != 0 || l.mergeQueue.inFlight != 0 || len(l.mergeQueue.results) != 0
		l.mergeQueue.mu.Unlock()
		if busy {
			return fmt.Errorf("%w: merges are pending", ErrReleaseBusy)
		}
	}
	l.completionBuf.completionOverflowMu.Lock()
	busy := len(l.completionBuf.completions) != 0 || len(l.completionBuf.completionOverflow) != 0
	l.completionBuf.completionOverflowMu.Unlock()
	if busy {
		return fmt.Errorf("%w: completions are pending", ErrReleaseBusy)
	}
	for _, name := range []string{"control.ndjson", "orders-next.json"} {
		info, err := os.Stat(filepath.Join(l.runtimeDir, name))
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("inspect pending input %s: %w", name, err)
		}
		if err == nil && info.Size() != 0 {
			return fmt.Errorf("%w: %s is pending", ErrReleaseBusy, name)
		}
	}
	return nil
}

func (l *Loop) finishRelease() error {
	if err := l.flushState(); err != nil {
		return err
	}
	if l.mergeQueue != nil {
		l.mergeQueue.Close()
	}
	l.watcherWG.Wait()
	return nil
}
