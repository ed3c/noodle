package loop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
	loopruntime "github.com/poteto/noodle/runtime"
)

func completedCleanupFixture(t *testing.T) *Loop {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	return &Loop{runtimeDir: dir, canonicalLoaded: true, effectLedger: reducer.NewEffectLedger(),
		canonical: state.State{SchemaVersion: 1, Mode: state.RunModeManual, LastEventID: "1", Orders: map[string]state.OrderNode{
			"done":     {OrderID: "done", Status: state.OrderCompleted, Stages: []state.StageNode{{StageIndex: 0, TaskKey: "execute", Skill: "execute", Status: state.StageCompleted}}},
			"schedule": {OrderID: "schedule", Status: state.OrderActive, Stages: []state.StageNode{{StageIndex: 0, TaskKey: "schedule", Skill: "schedule", Status: state.StagePending}}},
		}}}
}

func TestCleanupRetainedCompletionAndRefusals(t *testing.T) {
	for _, tc := range []struct {
		name    string
		change  func(*Loop)
		allowed bool
	}{
		{"completed-canonical-not-projection", func(*Loop) {}, true},
		{"fresh-context", func(l *Loop) { delete(l.canonical.Orders, "done") }, false},
		{"pending-admission", func(l *Loop) {
			o := l.canonical.Orders["done"]
			o.Status = state.OrderPending
			o.Stages[0].Status = state.StagePending
			l.canonical.Orders["done"] = o
		}, false},
		{"pending-review", func(l *Loop) { l.canonical.PendingReviews = map[string]state.PendingReviewNode{"done": {}} }, false},
		{"failed-needs-recovery", func(l *Loop) {
			o := l.canonical.Orders["done"]
			o.Status = state.OrderFailed
			l.canonical.Orders["done"] = o
		}, false},
		{"outer-action", func(l *Loop) { l.canonical.ActionNeeded = []string{"review original delivery"} }, false},
		{"unknown-dispatch", func(l *Loop) { l.resourceUnknown = errors.New("dispatch unknown") }, false},
		{"pending-merge", func(l *Loop) {
			l.effectLedger.Record(reducer.Effect{EffectID: "merge", Type: reducer.EffectMerge, Payload: []byte(`{"order_id":"done"}`)})
		}, false},
		{"control-input", func(l *Loop) {
			if err := os.WriteFile(filepath.Join(l.runtimeDir, "control.ndjson"), []byte("{}\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := completedCleanupFixture(t)
			tc.change(l)
			err := l.cleanupCompleted(false)
			if (err == nil) != tc.allowed {
				t.Fatalf("cleanup allowed=%v err=%v", tc.allowed, err)
			}
		})
	}
}

func TestCleanupRefusesConcurrentControlProducer(t *testing.T) {
	l := completedCleanupFixture(t)
	lock, err := lockfile.TryLock(filepath.Join(l.runtimeDir, "control.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err = l.cleanupCompleted(false); !errors.Is(err, ErrReleaseBusy) {
		t.Fatalf("control producer ignored: %v", err)
	}
}

func TestCleanupRequestInspectDoesNotEndActor(t *testing.T) {
	l := &Loop{releaseRequests: make(chan releaseRequest), runDone: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	called := make(chan error, 1)
	go func() { called <- l.Cleanup(ctx, false) }()
	req := <-l.releaseRequests
	if !req.cleanup || req.apply {
		t.Fatalf("inspect request %+v", req)
	}
	req.reply <- nil
	if err := <-called; err != nil {
		t.Fatal(err)
	}
	go func() { called <- l.Release(ctx) }()
	req = <-l.releaseRequests
	if req.cleanup || !req.apply {
		t.Fatalf("migration release changed %+v", req)
	}
	req.reply <- nil
	if err := <-called; err != nil {
		t.Fatal(err)
	}
}

func TestCleanupAutomaticPolicyRetainsManualHoldAndPending(t *testing.T) {
	l := completedCleanupFixture(t)
	if err := l.cleanupAutomatic(); !errors.Is(err, ErrReleaseBusy) {
		t.Fatalf("legacy auto cleanup: %v", err)
	}
	l.deps.CleanupCompleted = true
	if err := l.cleanupAutomatic(); !errors.Is(err, ErrReleaseBusy) {
		t.Fatalf("manual hold lost: %v", err)
	}
	l.canonical.Mode = state.RunModeSupervised
	for _, held := range []State{StatePaused, StateDraining} {
		l.state = held
		if err := l.cleanupAutomatic(); !errors.Is(err, ErrReleaseBusy) {
			t.Fatalf("dispatch hold %s lost: %v", held, err)
		}
	}
	l.state = StateRunning
	o := l.canonical.Orders["done"]
	o.Status = state.OrderPending
	o.Stages[0].Status = state.StagePending
	l.canonical.Orders["done"] = o
	if err := l.cleanupAutomatic(); !errors.Is(err, ErrReleaseBusy) {
		t.Fatalf("pending task lost: %v", err)
	}
}

func TestCleanupUnknownRequiresSuccessfulReadbackBeforeApply(t *testing.T) {
	l := completedCleanupFixture(t)
	l.deps.CleanupCompleted = true
	l.canonical.Mode = state.RunModeSupervised
	if err := os.Remove(filepath.Join(l.runtimeDir, "sessions")); err != nil {
		t.Fatal(err)
	}
	unknown := l.cleanupAutomatic()
	if unknown == nil || errors.Is(unknown, ErrReleaseBusy) {
		t.Fatalf("missing readback treated as busy: %v", unknown)
	}
	if err := os.Mkdir(filepath.Join(l.runtimeDir, "sessions"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := l.cleanupAutomatic(); err != unknown {
		t.Fatalf("unknown automatically retried: %v", err)
	}
	if err := l.cleanupCompleted(false); err != nil {
		t.Fatal(err)
	}
	if err := l.cleanupAutomatic(); err != nil {
		t.Fatalf("readback did not permit completion: %v", err)
	}
}

func TestCleanupLocalTaskKeepsUnacceptedCompletionForRecovery(t *testing.T) {
	l, cook, _ := localTaskCompletedFixture(t)
	before := recoverySessionBytes(t, l, cook.session.ID())
	if err := l.checkCompletedTasks(); !errors.Is(err, ErrReleaseBusy) {
		t.Fatalf("unaccepted dirty completion released: %v", err)
	}
	if !recoveryJSONEqual(before, recoverySessionBytes(t, l, cook.session.ID())) {
		t.Fatal("cleanup rewrote original completion evidence")
	}
}

func TestCleanupLocalTaskRecognizesOriginalAcceptedMerge(t *testing.T) {
	l, cook, command := localTaskReviewFixture(t)
	localTaskRealWorktree(t, l)
	command.Action = "merge-local-task"
	if err := l.dispatchControlCommand(command); err != nil {
		t.Fatal(err)
	}
	order := l.canonical.Orders[cook.orderID]
	if order.Status != state.OrderCompleted {
		t.Fatal("native merge did not complete")
	}
	if err := l.checkCompletedLocalTask(order, order.Stages[0]); err != nil {
		t.Fatalf("accepted native merge rejected: %v", err)
	}
	if err := l.checkCompletedTasks(); err != nil {
		t.Fatalf("accepted native merge left unresolved cleanup custody: %v", err)
	}
	order.Stages[0].Extra[localTaskMergeKey] = []byte(`{"head":"invalid"}`)
	if err := l.checkCompletedLocalTask(order, order.Stages[0]); err == nil || errors.Is(err, ErrReleaseBusy) {
		t.Fatalf("invalid merge evidence is not unknown: %v", err)
	}
}

func TestCleanupAutomaticRunEventReleasesCompletedAfterManualHold(t *testing.T) {
	dir := t.TempDir()
	runtimeDir := filepath.Join(dir, ".noodle")
	if err := os.MkdirAll(runtimeDir, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := config.DefaultConfig()
	cfg.Mode = "manual"
	cfg.Skills.Paths = []string{t.TempDir()}
	cfg.Runtime.Process.AdmittedOrders = []string{"done"}
	rt := newMockRuntime()
	l := New(dir, "noodle", cfg, Dependencies{
		Runtimes: map[string]loopruntime.Runtime{"process": rt},
		Worktree: &fakeWorktree{}, Adapter: &fakeAdapterRunner{}, Mise: &fakeMise{}, Monitor: fakeMonitor{},
		Registry: testLoopRegistry(), ModeOverride: state.RunModeManual, CleanupCompleted: true,
	})
	seed := completedCleanupFixture(t)
	if err := reducer.WriteSnapshotAtomic(filepath.Join(runtimeDir, "state.snapshot.json"), reducer.BuildSnapshot(seed.canonical, seed.effectLedger, time.Now())); err != nil {
		t.Fatal(err)
	}
	seed.runtimeDir = runtimeDir
	if err := seed.flushState(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- l.Run(ctx) }()
	select {
	case <-l.Ready():
	case err := <-result:
		t.Fatalf("startup: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := l.CleanupAutomatic(ctx); !errors.Is(err, ErrReleaseBusy) {
		t.Fatalf("manual hold lost: %v", err)
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, "control.ndjson"), []byte("{\"id\":\"release-hold\",\"action\":\"mode\",\"value\":\"supervised\"}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		<-result
		t.Fatalf("completed context did not release through the existing control event: %v; canonical=%+v", l.cleanupAutomatic(), l.canonical)
	}
	if len(rt.calls) != 0 {
		t.Fatal("cleanup dispatched new work")
	}
	if l.TerminalCause() != TerminalAutoCompleted {
		t.Fatalf("automatic completion lost cause: %q", l.TerminalCause())
	}
	snapshot, err := reducer.ReadSnapshot(filepath.Join(runtimeDir, "state.snapshot.json"))
	if err != nil || snapshot.State.Orders["done"].Status != state.OrderCompleted {
		t.Fatalf("completion custody changed: %v", err)
	}
}

func TestCleanupRunRecordsCauseOnlyAfterSuccessfulApply(t *testing.T) {
	for _, outcome := range []string{"apply", "inspect-cancel", "unknown-cancel"} {
		t.Run(outcome, func(t *testing.T) {
			dir := t.TempDir()
			runtimeDir := filepath.Join(dir, ".noodle")
			if err := os.MkdirAll(runtimeDir, 0700); err != nil {
				t.Fatal(err)
			}
			cfg := config.DefaultConfig()
			cfg.Mode = "manual"
			cfg.Skills.Paths = []string{t.TempDir()}
			l := New(dir, "noodle", cfg, Dependencies{
				Runtimes: map[string]loopruntime.Runtime{"process": newMockRuntime()},
				Worktree: &fakeWorktree{}, Adapter: &fakeAdapterRunner{}, Mise: &fakeMise{}, Monitor: fakeMonitor{},
				Registry: testLoopRegistry(), ModeOverride: state.RunModeManual,
			})
			seed := completedCleanupFixture(t)
			if err := reducer.WriteSnapshotAtomic(filepath.Join(runtimeDir, "state.snapshot.json"), reducer.BuildSnapshot(seed.canonical, seed.effectLedger, time.Now())); err != nil {
				t.Fatal(err)
			}
			seed.runtimeDir = runtimeDir
			if err := seed.flushState(); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- l.Run(ctx) }()
			select {
			case <-l.Ready():
			case err := <-result:
				t.Fatalf("startup: %v", err)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if outcome == "unknown-cancel" {
				if err := os.Remove(filepath.Join(runtimeDir, "sessions")); err != nil {
					t.Fatal(err)
				}
			}
			err := l.Cleanup(ctx, outcome != "inspect-cancel")
			if outcome == "unknown-cancel" {
				if err == nil || errors.Is(err, ErrReleaseBusy) {
					t.Fatalf("missing source did not remain unknown: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			want := TerminalExplicitCleanup
			if outcome != "apply" {
				select {
				case <-l.Done():
					t.Fatal("non-apply result ended the actor")
				default:
				}
				want = ""
				cancel()
			}
			select {
			case err := <-result:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("actor did not finish")
			}
			if l.TerminalCause() != want {
				t.Fatalf("cause=%q want=%q", l.TerminalCause(), want)
			}
		})
	}
}
