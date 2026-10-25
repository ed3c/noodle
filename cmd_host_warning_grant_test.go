package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/poteto/noodle/internal/admission"
)

func warningHost(t *testing.T) (*noodleHost, hostRequest) {
	t.Helper()
	root := hostConfigDir(t)
	data := []byte("[runtime.process]\nadmitted_orders = ['selected-task']\n")
	if err := os.WriteFile(filepath.Join(root, ".noodle.toml"), data, 0600); err != nil {
		t.Fatal(err)
	}
	req := hostRequest{Operation: "grant_warning", ProjectDir: root, ContextID: "context", InstanceID: "host", RequestID: "request", ConfigSHA256: hostDigest(data), BinarySHA256: "binary", Mode: "manual", WarningOrderID: "selected-task"}
	receipt := hostReceipt{ProjectDir: root, ContextID: req.ContextID, HostInstanceID: req.InstanceID, RequestID: req.RequestID, ConfigSHA256: req.ConfigSHA256, BinarySHA256: req.BinarySHA256, Mode: req.Mode, ContextState: "active"}
	b := admission.NewBudget(4)
	b.Register(req.ContextID)
	now := time.Now()
	for i := 3; i >= 0; i-- {
		b.Observe(admission.Sample{At: now.Add(-time.Duration(i) * admission.PollInterval), Pressure: admission.Warning, Swapouts: 100}, nil)
	}
	h := &noodleHost{instanceID: "host", binarySHA: "binary", budget: b, contexts: map[string]*hostContext{req.ContextID: {receipt: receipt}}}
	return h, req
}

func TestHostWarningGrantBindsTaskAndOriginalRequest(t *testing.T) {
	h, req := warningHost(t)
	first := h.grantWarning(req).(map[string]any)
	if first["status"] != "available" {
		t.Fatal(first)
	}
	permit, err := h.budget.TryAcquireTask(req.ContextID, req.WarningOrderID)
	if err != nil {
		t.Fatal(err)
	}
	permit.Release()
	second := h.grantWarning(req).(map[string]any)
	if second["status"] != "consumed" {
		t.Fatal(second)
	}
	if h.resources().(map[string]any)["warning_grants"] == nil {
		t.Fatal("grant missing from readback")
	}
}

func TestHostWarningGrantRefusesForeignOrChangedSource(t *testing.T) {
	for _, field := range []string{"instance", "binary", "context", "request", "project", "config", "config-path", "mode", "cleanup", "order", "blocked", "failed", "drift"} {
		t.Run(field, func(t *testing.T) {
			h, req := warningHost(t)
			switch field {
			case "instance":
				req.InstanceID = "foreign"
			case "binary":
				req.BinarySHA256 = "foreign"
			case "context":
				req.ContextID = "foreign"
			case "request":
				req.RequestID = "foreign"
			case "project":
				req.ProjectDir = t.TempDir()
			case "config":
				req.ConfigSHA256 = "foreign"
			case "config-path":
				req.ConfigPath = "/foreign"
			case "mode":
				req.Mode = "auto"
			case "cleanup":
				req.CleanupCompleted = true
			case "order":
				req.WarningOrderID = "other-task"
			case "blocked":
				h.blocked = true
			case "failed":
				h.contexts[req.ContextID].failed = true
			case "drift":
				if err := os.WriteFile(filepath.Join(req.ProjectDir, ".noodle.toml"), []byte("[runtime.process]\nadmitted_orders = ['selected-task', 'other-task']\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if got := h.grantWarning(req).(map[string]any); got["status"] != "refused" {
				t.Fatal(got)
			}
			if len(h.budget.WarningGrants()) != 0 {
				t.Fatal("refused request created permission")
			}
		})
	}
}
