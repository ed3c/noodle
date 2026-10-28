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

func TestHostGatewayWarningStartsBoundResidentAndSingleWorker(t *testing.T) {
	root := t.TempDir()
	request := hostTestRequest(t, root)
	profile := hostDeferralProfile(t)
	data := []byte(fmt.Sprintf("mode='manual'\n[server]\nenabled=false\n[runtime]\nadmitted_orders=['selected-task']\n[runtime.openshell]\nprofile=%q\n", profile))
	if err := os.WriteFile(filepath.Join(root, config.DefaultConfigPath), data, 0600); err != nil {
		t.Fatal(err)
	}
	request.ConfigSHA256 = hostDigest(data)
	request.WarningOrderID = "selected-task"
	request.RequiredHostInstanceID = "instance"
	host := &noodleHost{dir: t.TempDir(), instanceID: "instance", binaryPath: "/native/noodle", binarySHA: "test-binary", budget: admission.NewBudget(2), completed: make(chan string, 4)}
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
			t.Error(err)
		}
		if host.openShell != nil {
			if err := host.openShell.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	now := time.Now()
	for offset := 3; offset >= 0; offset-- {
		host.budget.Observe(admission.Sample{At: now.Add(-time.Duration(offset) * admission.PollInterval), Pressure: admission.Warning, Swapouts: 100}, nil)
	}
	response := host.handle(context.Background(), request)
	if response.Status != "ok" || response.ContextState != "active" {
		t.Fatalf("authorized attach: %+v", response)
	}
	if observed := host.budget.Snapshot(); observed.Pressure != admission.Warning || observed.Residents != 1 || observed.Active != 0 {
		t.Fatalf("gateway readback: %+v", observed)
	}
	worker, err := host.budget.TryAcquireTask(response.ContextID, request.WarningOrderID)
	if err != nil {
		t.Fatal(err)
	}
	worker.Release()
	grantRequest := request
	grantRequest.Operation = "grant_warning"
	grantRequest.InstanceID = response.HostInstanceID
	grantRequest.ContextID = response.ContextID
	grantRequest.GatewayWarning = true
	if result := host.grantWarning(grantRequest).(map[string]any); result["status"] != "consumed" {
		t.Fatalf("duplicate permission: %+v", result)
	}
	grantRequest.WarningRevision = 1
	if result := host.grantWarning(grantRequest).(map[string]any); result["status"] != "available" {
		t.Fatalf("explicit next launch: %+v", result)
	}
	worker, err = host.budget.TryAcquireTask(response.ContextID, request.WarningOrderID)
	if err != nil {
		t.Fatal(err)
	}
	worker.Release()
	if repeated := host.handle(context.Background(), request); repeated.ContextID != response.ContextID || len(host.budget.GatewayWarningGrants()) != 1 {
		t.Fatalf("attach replay changed permission: %+v", repeated)
	}
}

func TestHostGatewayWarningRejectsUnboundOrNonOpenShellAttach(t *testing.T) {
	for _, mismatch := range []string{"host", "order", "mode", "runtime"} {
		t.Run(mismatch, func(t *testing.T) {
			root := t.TempDir()
			request := hostTestRequest(t, root)
			data := []byte("mode='manual'\n[server]\nenabled=false\n[runtime]\nadmitted_orders=['selected-task']\n[runtime.openshell]\nprofile='missing.json'\n")
			request.WarningOrderID, request.RequiredHostInstanceID = "selected-task", "instance"
			switch mismatch {
			case "host":
				request.RequiredHostInstanceID = ""
			case "order":
				request.WarningOrderID = "other-task"
			case "mode":
				request.Mode = "supervised"
			case "runtime":
				data = []byte("mode='manual'\n[server]\nenabled=false\n[runtime]\nadmitted_orders=['selected-task']\n")
			}
			if err := os.WriteFile(filepath.Join(root, config.DefaultConfigPath), data, 0600); err != nil {
				t.Fatal(err)
			}
			request.ConfigSHA256 = hostDigest(data)
			host := &noodleHost{instanceID: "instance", binarySHA: "test-binary", budget: admission.NewBudget(2)}
			if response := host.handle(context.Background(), request); response.Status != "refused" || host.openShell != nil || len(host.receipts) != 0 {
				t.Fatalf("invalid scope acquired owner: %+v", response)
			}
		})
	}
}

func TestHostGatewayWarningRequestScopeIsImmutable(t *testing.T) {
	request := hostTestRequest(t, t.TempDir())
	request.WarningOrderID = "selected-task"
	request.RequiredHostInstanceID = "host"
	request.RequestFile = filepath.Join(t.TempDir(), "request.json")
	binding, unlock, err := bindHostRequest(&request)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	if binding.WarningOrderID != request.WarningOrderID {
		t.Fatal("warning scope missing from native request")
	}
	lookup := hostRequest{Operation: "status", ProjectDir: request.ProjectDir, BinarySHA256: request.BinarySHA256, RequestFile: request.RequestFile}
	_, unlock, err = bindHostRequest(&lookup)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	if lookup.WarningOrderID != request.WarningOrderID {
		t.Fatal("readback lost warning scope")
	}
	request.WarningOrderID = "foreign"
	if _, unlock, err = bindHostRequest(&request); err == nil {
		unlock()
		t.Fatal("existing request accepted a different warning scope")
	}
}
