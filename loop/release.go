package loop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

var ErrReleaseBusy = errors.New("loop has unfinished work")

type releaseRequest struct {
	ctx   context.Context
	reply chan error
}

func (l *Loop) Ready() <-chan struct{} { return l.runReady }
func (l *Loop) Done() <-chan struct{}  { return l.runDone }

// FinishOnce is called by the sole Cycle owner after a manual bootstrap cycle.
func (l *Loop) FinishOnce() error { return l.releaseIfQuiescent() }

// Release asks the Run actor to stop only after its workers and inputs settle.
func (l *Loop) Release(ctx context.Context) error {
	request := releaseRequest{ctx: ctx, reply: make(chan error, 1)}
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
	if err := l.flushState(); err != nil {
		return err
	}
	if l.mergeQueue != nil {
		l.mergeQueue.Close()
	}
	l.watcherWG.Wait()
	return nil
}
