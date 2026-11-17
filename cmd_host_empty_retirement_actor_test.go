package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/procx"
)

func TestEmptyRetirementActorUsesLocalWitnessWithUnsupportedGlobal(t *testing.T) {
	root := hostCapacityRoot(t)
	id := strings.Repeat("b", 32)
	directory := filepath.Join(root, "g", id)
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	budget := admission.NewBudget(9)
	if err := budget.BindSharedQuota(filepath.Join(root, "quota"), id); err != nil {
		t.Fatal(err)
	}
	machine := filepath.Join(root, "machine")
	if err := budget.BindMachineCapacity(machine, root+"/generation/"+id); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(machine, "capacity.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	// An unknown field gives the same strict-decoder HOLD as the legacy reader
	// seeing the new retirement field. The pinned old-source decoder is tested
	// separately against the actual fenced journal bytes.
	fields["unsupported_global_retirement_fixture"] = json.RawMessage(`true`)
	raw, err = json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(machine, "capacity.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	residents, err := admission.OpenResidents(directory)
	if err != nil {
		t.Fatal(err)
	}
	ref := admission.PreparationRef{ID: strings.Repeat("c", 32), SHA256: strings.Repeat("d", 64)}
	g := hostGeneration{ID: id, Directory: directory, Binary: "/fixture/original-noodle", SHA: strings.Repeat("a", 64), State: "retiring", Preparation: &ref, Process: procx.ProcessRecord{PID: os.Getpid(), SessionID: id, StartedAt: time.Now()}}
	if err = updateHostPool(root, func(p *hostPool) error { p.Active = ""; p.Generations[id] = g; return nil }); err != nil {
		t.Fatal(err)
	}
	h := &noodleHost{dir: directory, instanceID: id, binaryPath: g.Binary, binarySHA: g.SHA, budget: budget, residents: residents, contexts: map[string]*hostContext{}, rolling: &hostRolling{root: root, carry: map[string]rollingCarry{}}}
	snapshot := budget.Snapshot()
	if snapshot.MachineCapacity == nil || !strings.Contains(snapshot.MachineCapacity.Reason, "unknown field") || snapshot.SharedQuota == nil || snapshot.SharedQuota.Error != "" || snapshot.SharedQuota.Allocations != 0 {
		t.Fatal("global decoder HOLD destroyed local quota readback", snapshot)
	}
	if !h.advanceRolling(context.Background()) {
		t.Fatal("original inactive empty Actor depends on global decoder success", h.rolling.failure)
	}
	if err = h.saveResidentExitWitness(); err != nil {
		t.Fatal(err)
	}
	pool, err := readHostPool(root)
	if err != nil || pool.Generations[id].ResidentExit == nil || pool.Generations[id].State != "exiting" {
		t.Fatal("original Actor did not save its local exit witness", pool, err)
	}
}
