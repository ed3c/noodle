package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/loop"
)

func TestHostGatewayResidentLeavesNineWorkerSlotsUnderCriticalObservation(t *testing.T) {
	root := t.TempDir()
	request := hostTestRequest(t, root)
	profile := hostDeferralProfile(t)
	data := []byte(fmt.Sprintf("mode='manual'\n[server]\nenabled=false\n[runtime]\nadmitted_orders=['selected-task']\n[runtime.openshell]\nprofile=%q\n", profile))
	if err := os.WriteFile(filepath.Join(root, config.DefaultConfigPath), data, 0600); err != nil {
		t.Fatal(err)
	}
	request.ConfigSHA256 = hostDigest(data)
	request.RequiredHostInstanceID = "instance"
	host := &noodleHost{dir: t.TempDir(), instanceID: "instance", binaryPath: "/native/noodle", binarySHA: "test-binary", budget: admission.NewBudget(admission.DefaultWorkerLimit), completed: make(chan string, 4)}
	var err error
	host.residents, err = admission.OpenResidents(host.dir)
	if err != nil {
		t.Fatal(err)
	}
	host.residents.BindBudget(host.budget)
	host.newRuntime = func(string, string, config.Config, loop.Dependencies) hostRuntime {
		return &fakeHostRuntime{ready: make(chan struct{}), released: make(chan struct{})}
	}
	t.Cleanup(func() {
		for _, owner := range host.contexts {
			if owner.done != nil {
				owner.cancel()
				host.finish(owner, <-owner.done)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := host.residents.WaitClosed(ctx); err != nil {
			records, readErr := host.residents.Snapshot()
			t.Errorf("%v; original resident readback: %+v; readback error: %v", err, records, readErr)
		}
		if host.openShell != nil {
			if err := host.openShell.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	now := time.Now()
	for offset := 3; offset >= 0; offset-- {
		host.budget.Observe(admission.Sample{At: now.Add(-time.Duration(offset) * admission.PollInterval), Pressure: admission.Critical, SwapUsedBytes: 99_000_000_000}, nil)
	}
	response := host.handle(context.Background(), request)
	if response.Status != "ok" || response.ContextState != "active" {
		t.Fatalf("authorized attach: %+v", response)
	}
	if observed := host.budget.Snapshot(); observed.Pressure != admission.Critical || observed.Residents != 1 || observed.Active != 0 {
		t.Fatalf("gateway readback: %+v", observed)
	}
	var workers []*admission.Permit
	for range admission.DefaultWorkerLimit {
		worker, err := host.budget.TryAcquireTask(response.ContextID, "selected-task")
		if err != nil {
			t.Fatal(err)
		}
		workers = append(workers, worker)
	}
	if _, err := host.budget.TryAcquireTask(response.ContextID, "selected-task"); err == nil {
		t.Fatal("tenth worker exceeded this Host ceiling")
	}
	for _, worker := range workers {
		worker.Release()
	}
	if repeated := host.handle(context.Background(), request); repeated.ContextID != response.ContextID {
		t.Fatalf("attach replay changed original context: %+v", repeated)
	}
}
