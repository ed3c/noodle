package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/loop"
	loopruntime "github.com/poteto/noodle/runtime"
)

func TestHostFinishRetainsCustodyAfterReleaseFailure(t *testing.T) {
	for _, owner := range []string{"openshell", "resident", "released"} {
		t.Run(owner, func(t *testing.T) {
			dir := t.TempDir()
			receipt := hostReceipt{ContextID: "original-context", RequestID: "original-request", ProjectDir: t.TempDir(), HostInstanceID: "host", BinarySHA256: "binary", ContextState: "active"}
			record := admission.Resident{ID: "original-resident", Name: "gateway", State: "exited", Contexts: []string{receipt.ContextID}}
			if owner == "resident" {
				record.State = "running"
			}
			data, err := json.Marshal(map[string]admission.Resident{record.ID: record})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "residents.json"), data, 0600); err != nil {
				t.Fatal(err)
			}
			residents, err := admission.OpenResidents(dir)
			if err != nil {
				t.Fatal(err)
			}
			lockPath := filepath.Join(receipt.ProjectDir, "context.lock")
			lock, err := lockfile.TryLock(lockPath)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			rt := &fakeHostRuntime{terminalCause: loop.TerminalMigrationDetach}
			c := &hostContext{receipt: receipt, runtime: rt, done: make(chan error, 1), cancel: cancel, lock: lock}
			c.done <- nil
			budget := admission.NewBudget(1)
			budget.Register(receipt.ContextID)
			h := &noodleHost{dir: dir, instanceID: "host", binarySHA: "binary", contexts: map[string]*hostContext{receipt.ContextID: c}, receipts: []hostReceipt{receipt}, residents: residents, budget: budget}
			if owner == "openshell" {
				c.openShell = true
				h.openShell = &loopruntime.OpenShellGateway{}
				h.openShellProfile = &loopruntime.OpenShellProfile{Gateway: loopruntime.OpenShellGatewaySpec{Resident: admission.ResidentSpec{Name: record.Name}}}
			}

			h.finish(c, <-c.done)
			if c.done != nil || rt.shutdowns != 1 || ctx.Err() == nil {
				t.Fatalf("completed actor was not finalized exactly once: done=%v shutdowns=%d cancellation=%v", c.done, rt.shutdowns, ctx.Err())
			}
			failed := owner != "released"
			state := "detached"
			if failed {
				state = "failed"
			}
			if c.failed != failed || h.blocked != failed || c.receipt.ContextState != state || (h.contexts[receipt.ContextID] == c) != failed {
				t.Fatalf("release outcome lost custody: receipt=%+v failed=%t blocked=%t contexts=%d", c.receipt, c.failed, h.blocked, len(h.contexts))
			}
			if failed && c.receipt.TerminalCause != "" {
				t.Fatalf("failed release reported successful cause: %+v", c.receipt)
			}
			other, lockErr := lockfile.TryLock(lockPath)
			if other != nil {
				other.Close()
			}
			var held *lockfile.AlreadyLockedError
			if failed && !errors.As(lockErr, &held) || !failed && lockErr != nil {
				t.Fatalf("project lock does not match custody: %v", lockErr)
			}
			budget.Observe(admission.Sample{At: time.Now(), Unsupported: true}, nil)
			permit, err := budget.TryAcquire(receipt.ContextID)
			if permit != nil {
				permit.Release()
			}
			if failed && err != nil || !failed && !errors.Is(err, admission.ErrDeferred) {
				t.Fatalf("context budget registration does not match custody: %v", err)
			}
			data, err = os.ReadFile(filepath.Join(dir, "receipts.json"))
			if err != nil {
				t.Fatal(err)
			}
			var saved []hostReceipt
			if err := json.Unmarshal(data, &saved); err != nil || len(saved) != 1 || saved[0] != c.receipt {
				t.Fatalf("original outcome was not durably retained: %s %v", data, err)
			}
			lookup := hostRequest{Operation: "status", ContextID: receipt.ContextID, RequestID: receipt.RequestID, ProjectDir: receipt.ProjectDir, InstanceID: "host", BinarySHA256: "binary"}
			wantStatus := "ok"
			if failed {
				wantStatus = "unknown_effect"
			}
			for _, response := range []hostResponse{h.handle(context.Background(), lookup), offlineHostStatus(dir, lookup, errors.New("host unavailable"))} {
				if response.Status != wantStatus || response.ContextState != state {
					t.Fatalf("readback misclassified release: %+v", response)
				}
			}
			if rt.shutdowns != 1 || c.done != nil {
				t.Fatal("readback retried the completed actor")
			}
		})
	}
}
