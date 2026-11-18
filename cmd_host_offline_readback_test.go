//go:build darwin || linux

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/procx"
	"github.com/poteto/noodle/worktree"
)

func TestOfflineOriginalRequestReadbackPreservesCustody(t *testing.T) {
	root := hostCapacityRoot(t)
	id := strings.Repeat("a", 32)
	controlDir := filepath.Join(root, "controls", "original")
	dir, project, machineDir := filepath.Join(root, "g", id), filepath.Join(controlDir, "checkout"), filepath.Join(root, "machine")
	storageRoot := filepath.Join(root, "original-storage")
	storage := filepath.Join(storageRoot, ".noodle")
	workers := filepath.Join(storageRoot, ".worktrees")
	for _, path := range []string{dir, project, storage, workers} {
		if err := os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := exec.Command("git", "-C", project, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("initialize isolated control repository: %v %s", err, out)
	}
	plan, err := planHostStorage(context.Background(), hostRequest{ProjectDir: project, Environment: os.Environ()}, storageRoot, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(plan.OwnerDir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{plan.OwnerDir, storageRoot} {
		writeOwnerJSON(t, filepath.Join(path, "source.json"), plan.Source)
	}
	for _, target := range []string{storage, workers} {
		if err = os.Symlink(target, filepath.Join(project, filepath.Base(target))); err != nil {
			t.Fatal(err)
		}
	}
	planRaw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	binary, taskPath, request, cfg := filepath.Join(root, "original-noodle"), filepath.Join(root, "goal.json"), filepath.Join(root, "request.json"), filepath.Join(root, "task.toml")
	config := []byte(fmt.Sprintf("mode='manual'\n[runtime]\nadmitted_orders=['order']\n[runtime.openshell]\nprofile=%q\n", filepath.Join(root, "openshell", "profile.json")))
	for path, data := range map[string][]byte{binary: []byte("original binary"), taskPath: []byte("{}"), cfg: config} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	binaryRef, err := hostReadbackBinaryRef(binary)
	if err != nil {
		t.Fatal(err)
	}
	claim := admission.ResourceClaim{Source: "original profile", MemoryBytes: 10, CPUs: 1, DiskBytes: 20, DiskPaths: []string{root},
		Policy: admission.CapacityPolicy{MemoryBytes: 1000, CPUs: 8, DiskSafetyBytes: 10, RecoverySamples: 2}}
	prepare := func(owner, purpose string, bindings map[string]string) admission.PreparationRef {
		t.Helper()
		budget := admission.NewBudget(9)
		if err := budget.BindMachineCapacity(machineDir, owner); err != nil {
			t.Fatal(err)
		}
		ref, err := budget.PrepareMachineResources(context.Background(), admission.PreparationSource{Purpose: purpose, Bindings: bindings}, claim, hostShutdownTestSampler{})
		if err != nil {
			t.Fatal(err)
		}
		if err = budget.StartPreparationEffects(ref); err != nil {
			t.Fatal(err)
		}
		return ref
	}
	residentRef := prepare(root+"/resident", "host-resident", map[string]string{"host_root": root, "binary_path": binary, "binary_sha256": binaryRef.SHA256})
	taskSHA := hostDigest([]byte("{}"))
	taskRef := prepare(controlDir, "task-preparation", map[string]string{"host_root": root, "control_root": project,
		"task_path": taskPath, "task_sha256": taskSHA, "execution_binary": binary + "/" + binaryRef.SHA256, "storage": hostDigest(planRaw)})
	execution := admission.NewBudget(9)
	if err = execution.BindMachineCapacity(machineDir, root+"/generation/"+id); err != nil {
		t.Fatal(err)
	}
	if err = execution.BindPreparation(taskRef, taskRef.ID, "order", "openshell"); err != nil {
		t.Fatal(err)
	}
	hostPID := 99999999
	receipt := hostReceipt{Schema: 1, Protocol: hostProtocol, HostPID: hostPID, HostInstanceID: id, ContextID: taskRef.ID,
		ProjectDir: project, ContextState: "active", Mode: "manual", ConfigPath: cfg, ConfigSHA256: hostDigest(config),
		BinaryPath: binary, BinarySHA256: binaryRef.SHA256, RequestID: "accepted-request", HostDir: root}
	binding := hostRequestBinding{Schema: 1, RequestID: receipt.RequestID, Operation: "attach", ProjectDir: project,
		ConfigPath: cfg, ConfigSHA256: receipt.ConfigSHA256, Mode: receipt.Mode, BinarySHA256: binaryRef.SHA256,
		EnvironmentSHA256: "original-environment", Status: "accepted", HostDir: root}
	writeOwnerJSON(t, request, binding)
	receipts := filepath.Join(dir, "receipts.json")
	writeOwnerJSON(t, receipts, []hostReceipt{receipt})
	if err = updateHostPool(root, func(p *hostPool) error {
		p.Active = id
		p.Generations[id] = hostGeneration{ID: id, Directory: dir, Binary: binary, SHA: binaryRef.SHA256, State: "active",
			Process: procx.ProcessRecord{PID: hostPID, SessionID: id, StartedAt: time.Now()}, Preparation: &residentRef}
		p.Owners[binding.RequestID] = hostTaskOwner{State: "owned", Original: receipt, Current: receipt, EnvironmentSHA: binding.EnvironmentSHA256}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	control := filepath.Join(controlDir, "control.json")
	writeOwnerJSON(t, control, worktree.TaskControl{Schema: 2, Status: "prepared", ControlRoot: project,
		Task: worktree.ControlSource{Path: taskPath, SHA256: taskSHA}, Preparation: taskRef})
	stage := map[string]any{"status": "pending"}
	schedule := map[string]any{"task_key": "schedule", "skill": "schedule", "status": "pending"}
	snapshot := filepath.Join(storage, "state.snapshot.json")
	native := map[string]any{
		"state": map[string]any{"schema_version": 1, "orders": map[string]any{
			"order":    map[string]any{"order_id": "order", "stages": []any{stage}},
			"schedule": map[string]any{"order_id": "schedule", "stages": []any{schedule}},
		}},
		"effect_ledger": []any{map[string]any{"effect_id": "e", "status": "done",
			"effect": map[string]any{"effect_id": "e", "type": "initial_admission", "payload": map[string]string{"order_id": "order"}},
			"result": map[string]string{"effect_id": "e", "status": "completed"}}},
	}
	writeOwnerJSON(t, snapshot, native)
	writeOwnerJSON(t, filepath.Join(storage, "pending-review.json"), map[string]any{"items": []any{}})
	writeOwnerJSON(t, filepath.Join(storage, "tickets.json"), []any{})
	files := []string{request, control, receipts, filepath.Join(root, "pool.json"), filepath.Join(machineDir, "capacity.json"), snapshot}
	before := make(map[string]string)
	for _, path := range files {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		before[path] = string(raw)
	}

	got := readOfflineHostRequest(project, request, machineDir)
	if got.Status != "unknown_effect" || got.RequestID != receipt.RequestID || got.HostInstanceID != id ||
		got.ContextID != taskRef.ID || got.OrderID != "order" || got.StageReadback != "pending_no_worker_observed" ||
		got.OriginalProcessGroupAbsent == nil || !*got.OriginalProcessGroupAbsent ||
		got.ResidentPreparation == nil || got.TaskPreparation == nil ||
		!got.ResidentPreparation.EffectsPossible || !got.TaskPreparation.EffectsPossible ||
		got.ResidentPreparation.Closed || got.TaskPreparation.Closed || got.TaskPreparation.Promoted || got.TaskPreparation.SessionBound ||
		got.Next != nil || got.AuthorizesDispatch || len(got.Effects) != 0 {
		t.Fatalf("unexpected original readback: %+v", got)
	}
	wantMissing := []string{"original_native_round_handoff", "original_resident_exit", "original_resident_settlement", "original_task_preparation_settlement"}
	if !slices.Equal(got.Missing, wantMissing) {
		t.Fatalf("missing transitions: got %v, want %v", got.Missing, wantMissing)
	}
	for path, expected := range before {
		raw, err := os.ReadFile(path)
		if err != nil || string(raw) != expected {
			t.Fatalf("offline readback changed %s: %v", path, err)
		}
	}

	schedule["attempts"] = []any{map[string]string{"status": "failed"}}
	writeOwnerJSON(t, snapshot, native)
	got = readOfflineHostRequest(project, request, machineDir)
	if got.Status != "unknown_effect" || got.StageReadback != "" || !slices.Contains(got.Missing, "original_worker_history") {
		t.Fatalf("known schedule attempt became unstarted evidence: %+v", got)
	}
	delete(schedule, "attempts")
	writeOwnerJSON(t, snapshot, native)
	workerAlias := filepath.Join(project, ".worktrees")
	if err = os.Remove(workerAlias); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(root, workerAlias); err != nil {
		t.Fatal(err)
	}
	got = readOfflineHostRequest(project, request, machineDir)
	if got.Status != "unknown_effect" || got.StageReadback != "" || got.Error != "original task storage alias differs: .worktrees" {
		t.Fatalf("foreign worker storage alias selected original readback: %+v", got)
	}
	if got = readOfflineHostRequest(root, request, machineDir); got.Status != "input_required" || got.Next != nil {
		t.Fatalf("foreign project selected original readback: %+v", got)
	}
	binding.Status = "pending"
	writeOwnerJSON(t, request, binding)
	if got = readOfflineHostRequest(project, request, machineDir); got.Status != "input_required" || got.Next != nil {
		t.Fatalf("unconfirmed admission selected original readback: %+v", got)
	}
}
