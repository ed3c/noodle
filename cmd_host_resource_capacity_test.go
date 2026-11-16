package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/poteto/noodle/internal/admission"
	loopruntime "github.com/poteto/noodle/runtime"
)

func hostCapacityRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil { t.Fatal(err) }
	return root
}

func TestHostWorkerCapacityBindsSelectedProfileAndQueuesMissingPeaks(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" { t.Skip("native identity unsupported") }
	profilePath := hostDeferralProfile(t)
	profilePath, err := filepath.EvalSymlinks(profilePath)
	if err != nil { t.Fatal(err) }
	directory := filepath.Dir(profilePath)
	configuration := fmt.Sprintf(`[openshell.drivers.vm]
mem_mib=1
vcpus=1
overlay_disk_mib=1
state_dir=%q
[noodle_worker_resources]
schema=1
memory_peak_mib=1
disk_peak_mib=4
checkout_peak_mib=1
dependency_peak_mib=0
return_tar_peak_mib=1
restore_peak_mib=1
memory_budget_mib=1
cpu_budget=1
disk_safety_mib=1
max_swap_used_mib=0
max_swapout_bytes_per_second=0
recovery_samples=2
`, filepath.Join(directory, "vm"))
	if err := os.WriteFile(filepath.Join(directory, "address"), []byte(configuration), 0600); err != nil { t.Fatal(err) }
	p, err := loopruntime.LoadOpenShellProfile(profilePath)
	if err != nil { t.Fatal(err) }
	h := &noodleHost{dir: hostCapacityRoot(t), instanceID: "fixture-generation", budget: admission.NewBudget(9)}
	if err = h.budget.BindMachineCapacity(filepath.Join(hostCapacityRoot(t), "machine"), h.dir+"/"+h.instanceID); err != nil { t.Fatal(err) }
	h.bindWorkerResourceSource("context", directory, directory, &p)
	h.budget.Register("context")
	for i := 3; i >= 0; i-- { h.budget.Observe(admission.Sample{At: time.Now().Add(-time.Duration(i)*time.Second), Pressure: admission.Normal, TotalMemoryBytes: 4<<20, CPUs: 4}, nil) }
	permit, err := h.budget.TryAcquireTask("context", "task", "openshell")
	if err != nil { t.Fatal("Host producer did not reach Budget", err) }
	permit.Release()
	if _, err = h.budget.TryAcquireTask("context", "unsupported", "process"); !errors.Is(err, admission.ErrDeferred) || !strings.Contains(err.Error(), "runtime_unsupported") { t.Fatal("unsupported runtime took trusted OpenShell demand", err) }
	// Replacing the Host's current profile cannot replace this context's source.
	h.openShellProfile = &loopruntime.OpenShellProfile{}
	permit, err = h.budget.TryAcquireTask("context", "original-profile", "openshell")
	if err != nil { t.Fatal("old context was hot rebound", err) }
	permit.Release()
	if err = os.WriteFile(filepath.Join(directory, "address"), []byte(strings.Replace(configuration, "return_tar_peak_mib=1\n", "", 1)), 0600); err != nil { t.Fatal(err) }
	if _, err = h.budget.TryAcquireTask("context", "drift", "openshell"); !errors.Is(err, admission.ErrDeferred) || !strings.Contains(err.Error(), "source_changed") { t.Fatal("changed original config was admitted", err) }
	updated, err := loopruntime.LoadOpenShellProfile(profilePath)
	if err != nil { t.Fatal(err) }
	h.bindWorkerResourceSource("new-context", directory, directory, &updated)
	h.budget.Register("new-context")
	if _, err = h.budget.TryAcquireTask("new-context", "missing-peak", "openshell"); !errors.Is(err, admission.ErrDeferred) || !strings.Contains(err.Error(), "return_tar_peak_mib") { t.Fatal("new profile guessed missing archive peak", err) }
	if s := h.budget.Snapshot(); s.AdmissionPolicy != admission.WorkerAdmissionPolicy || s.Limit != 9 || s.MachineCapacity.IO != "unknown_no_reliable_observer" { t.Fatalf("old worker policy changed meaning or I/O became PASS: %+v", s) }
}

func TestHostMachineCapabilitiesSeparateSourceFromLaterOwners(t *testing.T) {
	c := hostCapabilities("darwin")
	if c["machine_worker_admission"] != 1 || c["machine_admission_policy"] != admission.MachineAdmissionPolicy || c["worker_admission_policy"] != admission.WorkerAdmissionPolicy { t.Fatal("worker/machine capability versions merged") }
	for _, missing := range []string{"machine_pre_control_admission", "machine_gc_release", "machine_safe_pause"} {
		if c[missing] != 0 { t.Fatalf("unconnected owner %s reported ready", missing) }
	}
}

func TestMachineCapacityDirectoryIgnoresPerHostEnvironment(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" { t.Skip("native identity unsupported") }
	first, err := admission.DefaultMachineCapacityDirectory()
	if err != nil { t.Fatal(err) }
	t.Setenv("HOME", hostCapacityRoot(t))
	t.Setenv("XDG_CACHE_HOME", hostCapacityRoot(t))
	second, err := admission.DefaultMachineCapacityDirectory()
	if err != nil || first != second { t.Fatal("Host environment split physical machine capacity", err) }
}

func TestHostMachineGenerationGuardKeepsLegacyReadbackAndOriginalNext(t *testing.T) {
	if runtime.GOOS == "windows" { t.Skip("fixture shell unsupported") }
	root := hostCapacityRoot(t)
	binary := filepath.Join(root, "old-reader")
	script := []byte("#!/bin/sh\nprintf '%s\\n' '{\"machine_worker_admission\":0}'\n")
	if err := os.WriteFile(binary, script, 0700); err != nil { t.Fatal(err) }
	current, old := strings.Repeat("a", 32), strings.Repeat("b", 32)
	pool := hostPool{Schema: 1, Active: current, Owners: map[string]hostTaskOwner{}, Generations: map[string]hostGeneration{
		current: {ID: current, Directory: filepath.Join(root, "g", current), SHA: "current-reader", State: "active"},
		old: {ID: old, Directory: filepath.Join(root, "g", old), Binary: binary, SHA: hostDigest(script), State: "retiring"},
	}}
	raw, err := json.Marshal(pool)
	if err != nil { t.Fatal(err) }
	path := filepath.Join(root, "pool.json")
	if err = os.WriteFile(path, raw, 0600); err != nil { t.Fatal(err) }
	h := &noodleHost{instanceID: current, binarySHA: "current-reader", rolling: &hostRolling{root: root}}
	guard := h.machineGenerationGuard()
	if err = guard(); err == nil { t.Fatal("old generation was assumed to participate in machine policy") }
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, after) { t.Fatal("capability guard changed original generation custody", err) }
	if err = verifyMachineRollingTarget(context.Background(), binary, hostDigest(script)); err == nil { t.Fatal("old queued binary regained admission without machine capability") }
	generation := pool.Generations[old]
	generation.State = "exited"
	pool.Generations[old] = generation
	raw, err = json.Marshal(pool)
	if err != nil { t.Fatal(err) }
	if err = os.WriteFile(path, raw, 0600); err != nil { t.Fatal(err) }
	if err = guard(); err != nil { t.Fatal("original exited readback did not unblock generation gate", err) }
	after, err = os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, after) { t.Fatal("generation readback rewrote old owner", err) }
}
