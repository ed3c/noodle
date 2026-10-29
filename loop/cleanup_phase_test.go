package loop

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/state"
	loopruntime "github.com/poteto/noodle/runtime"
)

func TestExecutionReclamationLocalPhaseHasNoFixedDeadline(t *testing.T) {
	l, _, request := failedClosureFixture(t)
	if err := l.closeFailedTask(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	reader := l.deps.Runtimes["openshell"].(*cleanupReadbackRuntime)
	reached := false
	localErr := errors.New("local durability scan paused before any native effect")
	l.deps.Runtimes["openshell"] = &reclaimingRuntime{cleanupReadbackRuntime: reader, reclaim: func(ctx context.Context, _ loopruntime.CompletedSessionBinding, _ string, _ loopruntime.SessionStatus, _ string, _ loopruntime.SessionReadbackFileRef) (loopruntime.CleanupSessionReadback, error) {
		reached = true
		if deadline, ok := ctx.Deadline(); ok {
			t.Errorf("local durability inherited fixed total deadline: %s", deadline)
		}
		return loopruntime.CleanupSessionReadback{}, localErr
	}}
	if err := l.cleanupCompleted(t.Context(), true); !errors.Is(err, localErr) || !reached {
		t.Fatalf("local phase not observed: %v", err)
	}
}

type phasedCleanupRuntime struct {
	*reclaimingRuntime
	localRead func(context.Context, loopruntime.CompletedSessionBinding, string, loopruntime.SessionStatus) (loopruntime.CleanupSessionReadback, error)
}

func (r *phasedCleanupRuntime) ReadCleanupSession(ctx context.Context, b loopruntime.CompletedSessionBinding, project string, status loopruntime.SessionStatus) (loopruntime.CleanupSessionReadback, error) {
	return r.localRead(ctx, b, project, status)
}

func TestCleanupRunCancellationInterruptsLocalScanWithoutRelease(t *testing.T) {
	for _, tc := range []struct {
		phase         string
		callerTimeout bool
	}{{"readback", false}, {"readback", true}, {"reclamation", false}, {"reclamation", true}} {
		t.Run(fmt.Sprintf("%s/caller-timeout-%v", tc.phase, tc.callerTimeout), func(t *testing.T) {
			l, request := completedAcceptanceFixture(t)
			if err := l.acceptCompletedTask(t.Context(), request); err != nil {
				t.Fatal(err)
			}
			if err := l.flushState(); err != nil {
				t.Fatal(err)
			}
			worktreePath := l.worktreePath(l.canonical.Orders[request.OrderID].Stages[0].Attempts[0].WorktreeName)
			l.config.Mode = "manual"
			l.config.Skills.Paths = []string{t.TempDir()}
			l.deps.ModeOverride = state.RunModeManual
			logger, handler := newTestLogger()
			l.logger = logger
			reader := l.deps.Runtimes["openshell"].(*cleanupReadbackRuntime)
			entered := make(chan context.Context, 1)
			exited := make(chan struct{})
			pauseLocal := func(ctx context.Context) error {
				entered <- ctx
				<-ctx.Done()
				close(exited)
				return ctx.Err()
			}
			var nativeCalls atomic.Int32
			l.deps.Runtimes["openshell"] = &phasedCleanupRuntime{
				reclaimingRuntime: &reclaimingRuntime{cleanupReadbackRuntime: reader, reclaim: func(ctx context.Context, _ loopruntime.CompletedSessionBinding, _ string, _ loopruntime.SessionStatus, _ string, _ loopruntime.SessionReadbackFileRef) (loopruntime.CleanupSessionReadback, error) {
					if tc.phase == "reclamation" {
						if err := pauseLocal(ctx); err != nil {
							return loopruntime.CleanupSessionReadback{}, err
						}
					}
					nativeCalls.Add(1)
					return loopruntime.CleanupSessionReadback{}, errors.New("native retirement reached after cancelled local scan")
				}},
				localRead: func(ctx context.Context, binding loopruntime.CompletedSessionBinding, project string, status loopruntime.SessionStatus) (loopruntime.CleanupSessionReadback, error) {
					if tc.phase == "readback" {
						return loopruntime.CleanupSessionReadback{}, pauseLocal(ctx)
					}
					return reader.read(binding, project, status)
				},
			}
			var worktreeCalls atomic.Int32
			l.deps.Worktree = &reclaimingWorktree{fakeWorktree: &fakeWorktree{}, remove: func(string, string) error {
				worktreeCalls.Add(1)
				return nil
			}}
			runCtx, cancelRun := context.WithCancel(t.Context())
			defer cancelRun()
			result := make(chan error, 1)
			go func() { result <- l.Run(runCtx) }()
			wait := time.NewTimer(5 * time.Second)
			defer wait.Stop()
			select {
			case <-l.Ready():
			case err := <-result:
				t.Fatalf("startup failed: %v", err)
			case <-wait.C:
				t.Fatal("actor startup did not finish")
			}
			callerCtx := t.Context()
			if tc.callerTimeout {
				var cancel context.CancelFunc
				callerCtx, cancel = context.WithTimeout(callerCtx, time.Second)
				defer cancel()
			}
			response := make(chan error, 1)
			go func() { response <- l.Cleanup(callerCtx, true) }()
			var scanCtx context.Context
			select {
			case scanCtx = <-entered:
			case err := <-response:
				t.Fatalf("local scan not entered: %v", err)
			case <-wait.C:
				t.Fatal("local scan not entered")
			}
			if tc.callerTimeout {
				select {
				case err := <-response:
					if !errors.Is(err, context.DeadlineExceeded) {
						t.Fatalf("caller did not lose response: %v", err)
					}
				case <-wait.C:
					t.Fatal("caller deadline was not observed")
				}
				if scanCtx.Err() != nil {
					t.Fatalf("caller timeout cancelled original owner: %v", scanCtx.Err())
				}
			}
			// The original owner remains active while local work is in flight,
			// including after the RPC caller has lost its response.
			lock, err := lockfile.TryLock(filepath.Join(l.runtimeDir, "control.lock"))
			if err == nil {
				lock.Close()
				t.Fatal("cleanup released control custody during local scan")
			}
			var busy *lockfile.AlreadyLockedError
			if !errors.As(err, &busy) {
				t.Fatalf("control custody was not observed: %v", err)
			}
			select {
			case <-l.Done():
				t.Fatal("cleanup detached original actor during local scan")
			case <-l.mergeQueue.doneCh:
				t.Fatal("cleanup released merge owner during local scan")
			default:
			}
			cancelRun()
			select {
			case <-exited:
			case <-wait.C:
				t.Fatal("Run cancellation did not interrupt local scan")
			}
			select {
			case err := <-result:
				if err != nil {
					t.Fatal(err)
				}
			case <-wait.C:
				t.Fatal("cancelled actor did not finish")
			}
			if !tc.callerTimeout {
				if err := <-response; !errors.Is(err, context.Canceled) {
					t.Fatalf("cancelled cleanup response: %v", err)
				}
			}
			if nativeCalls.Load() != 0 || worktreeCalls.Load() != 0 || l.TerminalCause() != "" || l.cleanupUnknown == nil {
				t.Fatalf("cancelled scan advanced cleanup: native=%d worktrees=%d cause=%q unknown=%v", nativeCalls.Load(), worktreeCalls.Load(), l.TerminalCause(), l.cleanupUnknown)
			}
			receiptPath := filepath.Join(l.runtimeDir, "execution-closures", recoveryDigest([]byte(request.OrderID))+".reclaimed.json")
			if _, err := os.Lstat(receiptPath); !os.IsNotExist(err) {
				t.Fatalf("cancelled scan persisted reclamation receipt: %v", err)
			}
			if _, err := os.Stat(worktreePath); err != nil {
				t.Fatalf("original worktree lost: %v", err)
			}
			phase := "session_readback"
			if tc.phase == "reclamation" {
				phase = "runtime_reclamation"
			}
			logged := false
			for _, entry := range handler.snapshot() {
				if entry.Attrs["phase"] == "release" {
					t.Fatal("cancelled scan entered release")
				}
				if entry.Message == "cleanup phase failed" && entry.Attrs["phase"] == phase {
					logged = entry.Attrs["order"] == request.OrderID && entry.Attrs["session"] == request.SessionID && entry.Attrs["elapsed"] != nil && entry.Attrs["error"] != nil
				}
			}
			if !logged {
				t.Fatalf("cancelled %s lacks bound phase diagnostics", phase)
			}
		})
	}
}

func TestCleanupCancelledReadbackDoesNotClearStickyUnknown(t *testing.T) {
	l := completedCleanupFixture(t)
	unknown := errors.New("original cleanup response unknown")
	l.cleanupUnknown = unknown
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := l.cleanupCompleted(ctx, false); !errors.Is(err, context.Canceled) || l.cleanupUnknown != unknown {
		t.Fatalf("cancelled readback rearmed cleanup: err=%v unknown=%v", err, l.cleanupUnknown)
	}
}
