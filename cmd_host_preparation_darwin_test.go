//go:build darwin && cgo

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/poteto/noodle/internal/admission"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/poteto/noodle/worktree"
)

func preparationTemplateResources(config string, state string) string {
	config = strings.Replace(config, "state_dir='/original/vm'", fmt.Sprintf("state_dir=%q\nmem_mib=4\nvcpus=2\noverlay_disk_mib=4", state), 1)
	return config + `[noodle_worker_resources]
schema=1
memory_peak_mib=6
disk_peak_mib=10
checkout_peak_mib=1
dependency_peak_mib=0
return_tar_peak_mib=1
restore_peak_mib=1
retention_peak_mib=3
memory_budget_mib=64
cpu_budget=8
disk_safety_mib=1
max_swap_used_mib=0
max_swapout_bytes_per_second=0
recovery_samples=2
[noodle_preparation_resources]
schema=1
memory_peak_mib=2
cpus=1
disk_peak_mib=3
[noodle_resident_resources]
schema=1
memory_peak_mib=3
cpus=1
disk_peak_mib=2
`
}

func TestHostPreparationOriginalSourceFlowsThroughControlAndWorkerNonce(t *testing.T) {
	ctx := context.Background()
	root, goal, template, binary := goalHostFixture(t)
	configPath := filepath.Join(filepath.Dir(template), "gateway.toml")
	raw, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(configPath, []byte(preparationTemplateResources(string(raw), filepath.Join(root, "vm"))), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(binary, 0555); err != nil {
		t.Fatal(err)
	}
	hostRoot := filepath.Join(root, "h", strings.Repeat("a", 16))
	if err = os.MkdirAll(hostRoot, 0700); err != nil {
		t.Fatal(err)
	}
	templateRef, err := loopruntime.ReadSessionReadbackFile(template, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = materializeGoalHostProfile(ctx, goalHostSelection{Root: hostRoot, Template: templateRef}, func() (net.Listener, error) { return net.Listen("tcp", "127.0.0.1:0") }); err != nil {
		t.Fatal(err)
	}
	binaryRef, err := hostReadbackBinaryRef(binary)
	if err != nil {
		t.Fatal(err)
	}
	repository := preparationRepository(t)
	task := filepath.Join(root, "task.json")
	raw, err = json.Marshal(map[string]string{"repository": repository, "base_ref": "main", "goal_source": goal})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(task, raw, 0600); err != nil {
		t.Fatal(err)
	}
	environment := []string{"HOME=" + hostCapacityRoot(t), "PATH=" + os.Getenv("PATH")}
	pool := filepath.Join(root, "machine")
	producer := admission.NewBudget(9)
	planned, err := worktree.PlanTaskControl(ctx, hostRoot, task)
	if err != nil {
		t.Fatal(err)
	}
	if err = producer.BindMachineCapacity(pool, filepath.Dir(planned.ControlRoot)); err != nil {
		t.Fatal(err)
	}
	var original hostTaskPreparationPlan
	control, err := worktree.PrepareTaskControlWithAdmission(ctx, hostRoot, task, func(ctx context.Context, control worktree.TaskControl) (admission.PreparationRef, error) {
		var err error
		original, err = taskPreparationPlan(ctx, hostRoot, control, environment, binaryRef)
		if err != nil {
			return admission.PreparationRef{}, err
		}
		ref, err := producer.PrepareMachineResources(ctx, original.Source, original.Claim, hostShutdownTestSampler{})
		if err != nil {
			return ref, err
		}
		if err = verifyTaskPreparationPlan(original); err != nil {
			return ref, err
		}
		return ref, producer.StartPreparationEffects(ref)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(original.Storage.Source.StorageRoot); !os.IsNotExist(err) {
		t.Fatal("planning materialized context storage", err)
	}
	worker := admission.NewBudget(9)
	if err = worker.BindMachineCapacity(pool, "original-host-generation"); err != nil {
		t.Fatal(err)
	}
	h := &noodleHost{dir: hostRoot, instanceID: "original-host-generation", binaryPath: binary, binarySHA: binaryRef.SHA256, budget: worker}
	configuration := []byte(fmt.Sprintf("[runtime]\nadmitted_orders=['original-order']\n[runtime.openshell]\nprofile=%q\n", filepath.Join(hostRoot, "openshell", "profile.json")))
	consumed, plan, err := h.taskPreparation(ctx, hostRequest{ProjectDir: control.ControlRoot, Environment: environment}, configuration)
	if err != nil {
		t.Fatal(err)
	}
	if consumed != control {
		t.Fatal("Host lost original control or preparation")
	}
	preparedRoots := append([]string{plan.Storage.Source.CommonDir}, plan.Archive.diskRoots()...)
	h.bindWorkerResourceSource(control.Preparation.ID, control.ControlRoot, plan.Storage.Source.StorageRoot, &plan.Profile, preparedRoots...)
	worker.Register(control.Preparation.ID)
	for index := 3; index >= 0; index-- {
		worker.Observe(admission.Sample{At: time.Now().Add(-time.Duration(index) * time.Second), Pressure: admission.Normal, TotalMemoryBytes: 16 << 30, CPUs: 8}, nil)
	}
	permit, err := worker.TryAcquireTask(control.Preparation.ID, "original-order", "openshell")
	if err != nil {
		t.Fatal(err)
	}
	binding := admission.MachineSessionBinding{ContextID: control.Preparation.ID, ProjectDir: control.ControlRoot, OrderID: "original-order", Runtime: "openshell", AttemptID: "original-attempt", SessionID: "original-session", WorktreePath: filepath.Join(control.ControlRoot, ".worktrees", "worker")}
	if err = permit.BindSession(binding); err != nil {
		t.Fatal(err)
	}
	permit.Release()
	leases, err := worker.MachineSessionLeases(binding.ContextID, binding.ProjectDir)
	if err != nil || len(leases) != 1 || leases[0].ID != control.Preparation.ID || leases[0].Binding == nil || *leases[0].Binding != binding {
		t.Fatal("original Source acquired another lifetime or lost its real session", err)
	}
	if _, err := os.Stat(original.Storage.Source.StorageRoot); !os.IsNotExist(err) {
		t.Fatal("source/nonce observation created context storage", err)
	}
}
