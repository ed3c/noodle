package loop

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/mise"
	loopruntime "github.com/poteto/noodle/runtime"
)

func TestOrderAdmissionCycleSkipsForeignAndSchedulerBeforeCapacity(t *testing.T) {
	for _, selected := range []string{"process", "openshell"} {
		for _, defaultRuntime := range []string{"process", "openshell"} {
			for _, warning := range []bool{false, true} {
				name := selected + "/default-" + defaultRuntime + "/" + map[bool]string{false: "normal", true: "warning-grant"}[warning]
				t.Run(name, func(t *testing.T) { checkOrderAdmissionCycle(t, selected, defaultRuntime, warning) })
			}
		}
	}
}

func checkOrderAdmissionCycle(t *testing.T, selected, defaultRuntime string, warning bool) {
	root := t.TempDir()
	dir := filepath.Join(root, ".noodle")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	orders := OrdersFile{Orders: []Order{testOrder("foreign", "execute", "execute", "codex", "model"), testOrder("allowed", "execute", "execute", "codex", "model"), scheduleOrder(config.DefaultConfig(), "")}}
	orders.Orders[0].Stages[0].Runtime = "openshell"
	orders.Orders[1].Stages[0].Runtime = selected
	rt := newMockRuntime()
	wt := &fakeWorktree{}
	cfg := config.DefaultConfig()
	cfg.Concurrency.MaxConcurrency = 1
	cfg.Runtime.AdmittedOrders = []string{"allowed"}
	cfg.Runtime.Default = defaultRuntime
	l := New(root, "noodle", cfg, Dependencies{Runtimes: map[string]loopruntime.Runtime{"process": rt, "openshell": rt}, Worktree: wt, Adapter: &fakeAdapterRunner{}, Mise: &fakeMise{}, Monitor: fakeMonitor{}, Registry: testLoopRegistry(), OrdersFile: writeTestOrders(t, dir, orders), ModeOverride: "manual"})
	if warning {
		b := resourceBudget(l, 1)
		now := time.Now()
		for i := 3; i >= 0; i-- {
			b.Observe(admission.Sample{At: now.Add(-time.Duration(i) * admission.PollInterval), Pressure: admission.Warning}, nil)
		}
		if _, err := b.GrantWarning(l.projectDir, "allowed"); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := l.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	if len(rt.calls) != 0 {
		t.Fatal("manual hold dispatched")
	}
	if err := l.controlMode("supervised"); err != nil {
		t.Fatal(err)
	}
	if err := l.Cycle(ctx); err != nil {
		t.Fatal(err)
	}
	if len(rt.calls) != 1 || rt.calls[0].EnvVars["NOODLE_ORDER_ID"] != "allowed" || rt.calls[0].Runtime != selected {
		t.Fatalf("dispatches: %#v", rt.calls)
	}
	if len(wt.created) != 1 {
		t.Fatalf("worktrees: %v", wt.created)
	}
	empty := OrdersFile{}
	if _, err := l.ensureScheduleIfNeeded(mise.Brief{}, &empty, true); err != nil {
		t.Fatal(err)
	}
	if len(empty.Orders) != 0 {
		t.Fatal("restricted context injected scheduler")
	}
	rt.sessions[0].complete("completed")
	cancel()
	l.watcherWG.Wait()
	l.mergeQueue.Close()
}

func TestOrderAdmissionDirectPathsRefuseBeforePreparation(t *testing.T) {
	for _, runtimeName := range []string{"process", "openshell", "sprites", "cursor"} {
		t.Run(runtimeName, func(t *testing.T) { checkOrderAdmissionDirectPaths(t, runtimeName) })
	}
}

func checkOrderAdmissionDirectPaths(t *testing.T, runtimeName string) {
	cfg := config.DefaultConfig()
	cfg.Runtime.AdmittedOrders = []string{"allowed"}
	cfg.Runtime.Default = runtimeName
	wt := &fakeWorktree{}
	rt := newMockRuntime()
	l := New(t.TempDir(), "noodle", cfg, Dependencies{Runtimes: map[string]loopruntime.Runtime{"process": rt, runtimeName: rt}, Worktree: wt, Registry: testLoopRegistry()})
	o := testOrder("foreign", "execute", "execute", "codex", "model")
	o.Stages[0].Runtime = runtimeName
	for name, fn := range map[string]func() error{
		"cook": func() error {
			return l.spawnCook(context.Background(), dispatchCandidate{OrderID: o.ID, Stage: o.Stages[0]}, o, spawnOptions{})
		},
		"schedule":  func() error { return l.spawnSchedule(context.Background(), scheduleOrder(cfg, ""), 0, "") },
		"bootstrap": func() error { return l.spawnBootstrapIfNeeded(context.Background(), scheduleOrder(cfg, "")) },
		"dispatch-foreign": func() error {
			_, _, e := l.dispatchSession(context.Background(), loopruntime.DispatchRequest{Runtime: runtimeName, EnvVars: map[string]string{"NOODLE_ORDER_ID": "foreign"}})
			return e
		},
		"dispatch-missing-order": func() error {
			_, _, e := l.dispatchSession(context.Background(), loopruntime.DispatchRequest{Runtime: runtimeName})
			return e
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := fn(); err == nil {
				t.Fatal("foreign dispatch accepted")
			}
		})
	}
	if len(wt.created) != 0 || len(rt.calls) != 0 {
		t.Fatal("refusal prepared worktree or dispatched")
	}
}

func TestOrderAdmissionFallbackCannotBypassScope(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Runtime.AdmittedOrders = []string{"allowed"}
	remote := newMockRuntime()
	remote.dispatchErr = errors.New("offline")
	local := newMockRuntime()
	l := New(t.TempDir(), "noodle", cfg, Dependencies{Runtimes: map[string]loopruntime.Runtime{"remote": remote, "process": local}, Registry: testLoopRegistry()})
	_, _, err := l.dispatchSession(context.Background(), loopruntime.DispatchRequest{Runtime: "remote", EnvVars: map[string]string{"NOODLE_ORDER_ID": "foreign"}})
	if err == nil || len(remote.calls) != 0 || len(local.calls) != 0 {
		t.Fatal("fallback bypassed admission")
	}
	_, fallback, err := l.dispatchSession(context.Background(), loopruntime.DispatchRequest{Runtime: "remote", EnvVars: map[string]string{"NOODLE_ORDER_ID": "allowed"}})
	if err != nil || len(remote.calls) != 1 || len(local.calls) != 1 || fallback.SelectedRuntime != "process" {
		t.Fatalf("admitted fallback changed: %+v %v", fallback, err)
	}
	local.calls = nil
	cfg.Runtime.AdmittedOrders = nil
	l.config = cfg
	if _, _, err = l.dispatchSession(context.Background(), loopruntime.DispatchRequest{Runtime: "process"}); err != nil {
		t.Fatal(err)
	}
	if len(local.calls) != 1 {
		t.Fatal("empty allowlist changed legacy behavior")
	}
}

func TestOrderAdmissionEmptyScopeAllowsEveryRuntime(t *testing.T) {
	for _, runtimeName := range []string{"process", "openshell", "sprites", "cursor"} {
		t.Run(runtimeName, func(t *testing.T) {
			rt := newMockRuntime()
			l := New(t.TempDir(), "noodle", config.DefaultConfig(), Dependencies{Runtimes: map[string]loopruntime.Runtime{runtimeName: rt}, Worktree: &fakeWorktree{}, Registry: testLoopRegistry()})
			if _, _, err := l.dispatchSession(t.Context(), loopruntime.DispatchRequest{Runtime: runtimeName}); err != nil || len(rt.calls) != 1 {
				t.Fatalf("unrestricted runtime changed: calls=%d err=%v", len(rt.calls), err)
			}
		})
	}
}
