package loop

import (
	"context"
	"errors"
	"testing"

	"github.com/poteto/noodle/config"
	loopruntime "github.com/poteto/noodle/runtime"
)

func TestOpenShellPreflightNeverFallsBackAndReturnsAdmission(t *testing.T) {
	for _, admitted := range []bool{false, true} {
		t.Run(map[bool]string{false: "without host", true: "host admission"}[admitted], func(t *testing.T) {
			remote, local := newMockRuntime(), newMockRuntime()
			remote.dispatchErr = errors.New("profile refused before native submission")
			l := New(t.TempDir(), "noodle", config.DefaultConfig(), Dependencies{Runtimes: map[string]loopruntime.Runtime{"process": local, "openshell": remote}, Worktree: &fakeWorktree{}, Adapter: &fakeAdapterRunner{}, Mise: &fakeMise{}, Monitor: fakeMonitor{}, Registry: testLoopRegistry()})
			if admitted {
				b := resourceBudget(l, 1)
				healthyResourceBudget(b)
			}
			err := l.withAdmission(context.Background(), "openshell", "task", func(ctx context.Context) error {
				_, fallback, err := l.dispatchSession(ctx, loopruntime.DispatchRequest{Name: "task", Runtime: "openshell"})
				if fallback.Class != "" {
					t.Fatal("OpenShell fallback reported")
				}
				return err
			})
			if err == nil || errors.Is(err, errDispatchUnknown) || l.resourceUnknown != nil || len(local.calls) != 0 || len(remote.calls) != 1 {
				t.Fatalf("preflight/fallback state: %v local=%d remote=%d", err, len(local.calls), len(remote.calls))
			}
			if admitted && l.deps.Admission.Snapshot().Active != 0 {
				t.Fatal("preflight retained worker admission")
			}
		})
	}
}
func TestOpenShellNeedsManagedHostBinding(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Runtime.OpenShell.Profile = "/explicit/profile.json"
	l := New(t.TempDir(), "noodle", cfg, Dependencies{Runtimes: map[string]loopruntime.Runtime{"process": newMockRuntime()}, Worktree: &fakeWorktree{}, Adapter: &fakeAdapterRunner{}, Mise: &fakeMise{}, Monitor: fakeMonitor{}, Registry: testLoopRegistry()})
	if l.registryErr == nil || l.deps.Runtimes["openshell"] != nil {
		t.Fatal("OpenShell registered without gateway owner")
	}
}
