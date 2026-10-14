package loop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/state"
	loopruntime "github.com/poteto/noodle/runtime"
)

func TestReleaseRunActorRejectsBusyAndJoinsIdle(t *testing.T) {
	dir := t.TempDir()
	cfg := config.DefaultConfig()
	cfg.Mode = "manual"
	cfg.Skills.Paths = []string{dir}
	l := New(dir, "noodle", cfg, Dependencies{
		Runtimes: map[string]loopruntime.Runtime{"process": newMockRuntime()},
		Worktree: &fakeWorktree{}, Adapter: &fakeAdapterRunner{}, Mise: &fakeMise{}, Monitor: fakeMonitor{},
		Registry: testLoopRegistry(), ModeOverride: state.RunModeManual,
	})
	l.watcherCount.Store(1)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- l.Run(ctx) }()
	select {
	case <-l.Ready():
	case err := <-result:
		t.Fatalf("startup failed: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := l.Release(ctx); !errors.Is(err, ErrReleaseBusy) {
		t.Fatalf("busy release: %v", err)
	}
	select {
	case <-l.Done():
		t.Fatal("busy release stopped the loop")
	default:
	}
	l.watcherCount.Store(0)
	if err := l.Release(ctx); err != nil {
		t.Fatalf("idle release: %v", err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case <-l.Done():
	default:
		t.Fatal("Run returned before background workers joined")
	}
	select {
	case <-l.mergeQueue.doneCh:
	default:
		t.Fatal("merge queue was not joined")
	}
}

func TestReleaseRefusesUnfinishedOwnerWork(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*Loop)
	}{
		{"active worker", func(l *Loop) { l.cooks.activeCooksByOrder = map[string]*cookHandle{"one": {}} }},
		{"adopted worker", func(l *Loop) { l.cooks.adoptedSessions = []string{"one"} }},
		{"merge", func(l *Loop) { l.mergeQueue = &MergeQueue{inFlight: 1} }},
		{"merge result", func(l *Loop) { l.mergeQueue = &MergeQueue{results: []MergeResult{{}}} }},
		{"completion", func(l *Loop) {
			l.completionBuf.completions = make(chan StageResult, 1)
			l.completionBuf.completions <- StageResult{}
		}},
		{"control", func(l *Loop) {
			if err := os.WriteFile(filepath.Join(l.runtimeDir, "control.ndjson"), []byte("{}\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
		{"orders input", func(l *Loop) {
			if err := os.WriteFile(filepath.Join(l.runtimeDir, "orders-next.json"), []byte("{}\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			l := &Loop{runtimeDir: t.TempDir()}
			tc.setup(l)
			if err := l.releaseIfQuiescent(); !errors.Is(err, ErrReleaseBusy) {
				t.Fatalf("unfinished work accepted: %v", err)
			}
		})
	}
}
