package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/poteto/noodle/internal/admission"
)

func TestHostBudgetValidatesAndForwardsCapacity(t *testing.T) {
	for _, raw := range []string{"", "-1", "1.5", " 15", "9007199254740992"} {
		t.Run("invalid-"+raw, func(t *testing.T) {
			t.Setenv("NOODLE_HOST_SWAP_USED_LIMIT_BYTES", raw)
			if _, err := newHostBudget(); err == nil {
				t.Fatal("invalid capacity accepted")
			}
		})
	}
	t.Setenv("NOODLE_HOST_SWAP_USED_LIMIT_BYTES", "15000000000")
	b, err := newHostBudget()
	if err != nil {
		t.Fatal(err)
	}
	if b.Snapshot().SwapUsedLimit != admission.DefaultSwapUsedLimit {
		t.Fatal(b.Snapshot())
	}
	if !slices.Contains(hostBaselineEnvironment(t.TempDir()), "NOODLE_HOST_SWAP_USED_LIMIT_BYTES=15000000000") {
		t.Fatal("capacity lost at launcher boundary")
	}
}

func TestRestrictedHeldPublicRequestsNeverCreateRuntime(t *testing.T) {
	root := t.TempDir()
	oldID, newID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	original := hostReceipt{RequestID: "original", ContextID: "context", HostInstanceID: oldID, ProjectDir: "/original/project", ConfigSHA256: "config", Mode: "manual", BinarySHA256: "original-binary", ContextState: "detached"}
	oldDir := filepath.Join(root, "g", oldID)
	if err := os.MkdirAll(oldDir, 0700); err != nil {
		t.Fatal(err)
	}
	receipts, _ := json.Marshal([]hostReceipt{original})
	if err := os.WriteFile(filepath.Join(oldDir, "receipts.json"), receipts, 0600); err != nil {
		t.Fatal(err)
	}
	if err := updateHostPool(root, func(p *hostPool) error {
		p.Active = newID
		p.Generations[oldID] = hostGeneration{ID: oldID, Directory: oldDir, SHA: "original-binary", State: "exited"}
		p.Generations[newID] = hostGeneration{ID: newID, Directory: filepath.Join(root, "g", newID), SHA: "new-binary", State: "active"}
		p.Owners[original.RequestID] = hostTaskOwner{Original: original, Current: original, MaintenanceHold: "confirmed-plan"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(root, "pool.json"))
	if err != nil {
		t.Fatal(err)
	}
	// There is deliberately no runtime factory: a resumed attach would fail.
	h := &noodleHost{instanceID: newID, binarySHA: "new-binary", rolling: &hostRolling{root: root, generation: newID}}
	req := hostRequest{RequestID: original.RequestID, ProjectDir: original.ProjectDir, ConfigSHA256: original.ConfigSHA256, Mode: original.Mode, BinarySHA256: "new-binary"}
	for _, op := range []string{"attach", "once", "detach"} {
		req.Operation = op
		if got := h.handle(context.Background(), req); got.Status != "busy" {
			t.Fatalf("%s resumed held work: %+v", op, got)
		}
	}
	req.Operation = "status"
	if got := h.handle(context.Background(), req); got.Status != "ok" || got.hostReceipt != original {
		t.Fatalf("original readback changed: %+v", got)
	}
	byContext := req
	byContext.RequestID = ""
	byContext.ContextID, byContext.InstanceID = original.ContextID, oldID
	byContext.Operation = "detach"
	if got := h.handle(context.Background(), byContext); got.Status != "busy" {
		t.Fatal("context lookup bypassed maintenance hold", got)
	}
	req.ConfigSHA256 = "different"
	if got := h.handle(context.Background(), req); got.Status != "refused" {
		t.Fatal("changed binding admitted", got)
	}
	after, _ := os.ReadFile(filepath.Join(root, "pool.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("held readback mutated history")
	}
}

func TestHostExplicitConfigSourceCannotFallBack(t *testing.T) {
	root := t.TempDir()
	req := hostTestRequest(t, root)
	req.ConfigPath = filepath.Join(t.TempDir(), "missing.toml")
	if _, err := readHostRequestConfig(req); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("missing explicit source fell back", err)
	}
	req.ConfigPath = "relative.toml"
	if _, err := readHostRequestConfig(req); err == nil {
		t.Fatal("relative source accepted")
	}
}

// Compatibility must be established before restricted maintenance can retire
// the original owner. The fake CLI has no Host effects.
func TestRollingTargetRetainsRestrictedMaintenanceAndAdmissionPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, policy string
		maintenance  int
		accepted     bool
	}{
		{"compatible", admission.WorkerAdmissionPolicy, 1, true},
		{"foreign-policy", "foreign-policy", 1, false},
		{"missing-maintenance", admission.WorkerAdmissionPolicy, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			caps, err := json.Marshal(map[string]any{"protocol": hostProtocol, "rolling_host_version": 1, "shared_worker_quota": 1, "codex_native_runtime": 1, "task_warning_grant": 1, "restricted_host_maintenance": tc.maintenance, "worker_admission_policy": tc.policy})
			if err != nil {
				t.Fatal(err)
			}
			binary := filepath.Join(t.TempDir(), "target")
			data := []byte(fmt.Sprintf("#!/bin/sh\nif [ \"$2\" = capabilities ]; then\n printf '%%s\\n' '%s'\nelse\n printf '%%s\\n' '{\"local_task_directory_scope\":1,\"local_task_roles\":1}'\nfi\n", caps))
			if err = os.WriteFile(binary, data, 0700); err != nil {
				t.Fatal(err)
			}
			err = verifyRollingTarget(context.Background(), binary, hostDigest(data))
			if tc.accepted && err != nil {
				t.Fatal(err)
			}
			if !tc.accepted && (err == nil || !strings.Contains(err.Error(), "compatible rolling Host")) {
				t.Fatalf("incompatible target did not refuse before owner control: %v", err)
			}
		})
	}
}
