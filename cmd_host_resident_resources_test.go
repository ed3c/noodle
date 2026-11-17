//go:build darwin || linux

package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/procx"
	loopruntime "github.com/poteto/noodle/runtime"
)

func TestHostResidentReturnRequiresOriginalWitnessAndActualGroupAbsence(t *testing.T) {
	root := hostCapacityRoot(t)
	binary, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatal(err)
	}
	binary, err = filepath.EvalSymlinks(binary)
	if err != nil {
		t.Fatal(err)
	}
	binaryRef, err := hostReadbackBinaryRef(binary)
	if err != nil {
		t.Fatal(err)
	}
	b := admission.NewBudget(9)
	if err = b.BindMachineCapacity(filepath.Join(root, "machine"), root+"/resident"); err != nil {
		t.Fatal(err)
	}
	claim := admission.ResourceClaim{Source: "isolated-original-operator-resident-budget", MemoryBytes: 10, CPUs: 1, DiskBytes: 20, DiskPaths: []string{root}, Policy: admission.CapacityPolicy{MemoryBytes: 1000, CPUs: 8, DiskSafetyBytes: 10, RecoverySamples: 2}}
	source := admission.PreparationSource{Purpose: "host-resident", Bindings: map[string]string{"host_root": root, "binary_path": binary, "binary_sha256": binaryRef.SHA256}}
	ref, err := b.PrepareMachineResources(context.Background(), source, claim, hostShutdownTestSampler{})
	if err != nil {
		t.Fatal(err)
	}
	if err = b.StartPreparationEffects(ref); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "30")
	detachHostProcess(cmd)
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		if !waited {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	id := strings.Repeat("a", 32)
	directory := filepath.Join(root, "g", id)
	if err = os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	g := hostGeneration{ID: id, Directory: directory, Binary: binary, SHA: binaryRef.SHA256, State: "retiring", Preparation: &ref, Process: procx.ProcessRecord{PID: cmd.Process.Pid, SessionID: id, StartedAt: time.Now().UTC()}}
	witness := residentExitWitness{Schema: 1, Root: root, Generation: id, PID: cmd.Process.Pid, Preparation: ref, Residents: []admission.Resident{}}
	raw, err := json.Marshal(witness)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "resource-resident-exit.json")
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	exit, err := loopruntime.ReadSessionReadbackFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	g.ResidentExit = &exit
	if err = returnHostResidentResourcesWithBudget(root, g, b); err == nil {
		t.Fatal("running native group returned resident capacity")
	}
	if err = cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	waited = true
	foreign := g
	foreign.SHA = strings.Repeat("b", 64)
	if err = returnHostResidentResourcesWithBudget(root, foreign, b); err == nil {
		t.Fatal("foreign binary returned original resident capacity")
	}
	protected := g
	protected.SourceService = &loopruntime.SessionReadbackFileRef{Path: filepath.Join(root, "original-login-source"), SHA256: strings.Repeat("a", 64)}
	if err = returnHostResidentResourcesWithBudget(root, protected, b); err == nil {
		t.Fatal("protected login Source returned resident capacity")
	}
	if err = os.WriteFile(path, append(raw, ' '), 0600); err != nil {
		t.Fatal(err)
	}
	if err = returnHostResidentResourcesWithBudget(root, g, b); err == nil {
		t.Fatal("changed native witness returned capacity")
	}
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err = returnHostResidentResourcesWithBudget(root, g, b); err != nil {
		t.Fatal(err)
	}
	if err = returnHostResidentResourcesWithBudget(root, g, b); err != nil {
		t.Fatal("same return did not read its original ACK", err)
	}
	if snapshot := b.Snapshot().MachineCapacity; snapshot == nil || snapshot.Leases != 0 {
		t.Fatal("actual resident return retained future claims", snapshot)
	}
}
