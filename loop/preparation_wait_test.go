package loop

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/state"
	loopruntime "github.com/poteto/noodle/runtime"
)

type preparationWaitCloser struct{ closed chan struct{} }

func (c preparationWaitCloser) Close() error { close(c.closed); return nil }

func TestPreparationReadyRunsAfterStartupCycleAndClosesOnExit(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(map[bool]string{false: "ready", true: "readback-failed"}[failure], func(t *testing.T) {
			dir := t.TempDir()
			cfg := config.DefaultConfig()
			cfg.Mode = "manual"
			cfg.Skills.Paths = []string{dir}
			mise := &fakeMise{}
			l := New(dir, "noodle", cfg, Dependencies{Runtimes: map[string]loopruntime.Runtime{"process": newMockRuntime()},
				Worktree: &fakeWorktree{}, Adapter: &fakeAdapterRunner{}, Mise: mise, Monitor: fakeMonitor{}, Registry: testLoopRegistry(), ModeOverride: state.RunModeManual})
			closed := make(chan struct{})
			called := make(chan struct{})
			original := errors.New("original ready source unavailable")
			l.deps.PreparationReady = func() (io.Closer, error) {
				if !l.canonicalLoaded || !l.ordersLoaded || l.startupReady || mise.calls == 0 {
					t.Error("ready preceded complete first Cycle")
				}
				close(called)
				if failure {
					return nil, original
				}
				return preparationWaitCloser{closed}, nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- l.Run(ctx) }()
			if failure {
				select {
				case err := <-result:
					if !errors.Is(err, original) {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				select {
				case <-l.Ready():
					t.Fatal("failed proof advertised Ready")
				default:
				}
				return
			}
			select {
			case <-l.Ready():
			case err := <-result:
				t.Fatal(err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			select {
			case <-called:
			default:
				t.Fatal("Ready skipped original source callback")
			}
			select {
			case <-closed:
				t.Fatal("live ready actor released wait proof")
			default:
			}
			cancel()
			select {
			case err := <-result:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("actor did not exit")
			}
			select {
			case <-closed:
			default:
				t.Fatal("exited actor kept wait proof alive")
			}
		})
	}
}

func TestPreparationReadyDoesNotDiscountAttemptedOrUnknownContext(t *testing.T) {
	for _, condition := range []string{"attempted", "unknown", "registry-error"} {
		t.Run(condition, func(t *testing.T) {
			l := newSteerTestLoop(t, newMockRuntime())
			l.canonicalLoaded = true
			called := false
			l.deps.PreparationReady = func() (io.Closer, error) { called = true; return nil, nil }
			switch condition {
			case "attempted":
				l.canonical.Orders = map[string]state.OrderNode{"original": {Stages: []state.StageNode{{Attempts: []state.AttemptNode{{AttemptID: "original"}}}}}}
			case "unknown":
				l.resourceUnknown = errors.New("original effects unknown")
			case "registry-error":
				l.registryErr = errors.New("original startup incomplete")
			}
			closer, err := l.beginPreparationWait()
			if called || closer != nil {
				t.Fatal("unresolved/attempted context published idle proof")
			}
			if condition != "attempted" && err == nil {
				t.Fatal("unknown startup became Ready")
			}
		})
	}
}
