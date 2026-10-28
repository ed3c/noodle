package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/poteto/noodle/internal/admission"
)

func TestHostResidentRecoveryRequiresExactReceipt(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	r, err := admission.OpenResidents(dir)
	if err != nil {
		t.Fatal(err)
	}
	receipt := hostReceipt{ProjectDir: dir, ContextID: "context", RequestID: "request", HostInstanceID: "host", ContextState: "active", ConfigSHA256: "config", Mode: "manual"}
	h := &noodleHost{dir: dir, instanceID: "host", binarySHA: "binary", budget: admission.NewBudget(1), residents: r, contexts: map[string]*hostContext{"context": {receipt: receipt}}}
	req := hostRequest{Operation: "resident_recover_bound", ProjectDir: dir, ContextID: "context", RequestID: "request", InstanceID: "host", BinarySHA256: "binary", ConfigSHA256: "wrong", Mode: "manual"}
	got := h.residentOperation(context.Background(), req).(map[string]any)
	if got["status"] != "refused" || got["error"] != "exact original recovery request receipt required" {
		t.Fatal(got)
	}
	req.ConfigSHA256 = "config"
	got = h.residentOperation(context.Background(), req).(map[string]any)
	if got["status"] != "refused" || got["error"] != "fixture evidence manifest missing" {
		t.Fatal(got)
	}
	rows, err := r.FixtureRecoveries().Snapshot()
	if err != nil || len(rows) != 0 {
		t.Fatal("refused binding registered recovery", rows, err)
	}
}
