package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/procx"
	loopruntime "github.com/poteto/noodle/runtime"
)

func emptyRetirementControllerFixture(t *testing.T, fault string) (string, string, *admission.Budget, *noodleHost, emptyHostRetirementNative) {
	t.Helper()
	root, goal, template, binary := goalHostFixture(t)
	isolatedGoalTemplatePath(t, root)
	owner := filepath.Join(root, "owner")
	id := strings.Repeat("b", 32)
	host := filepath.Join(owner, "h", id[:16])
	if err := os.MkdirAll(filepath.Join(owner, "goals"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(host, 0700); err != nil {
		t.Fatal(err)
	}
	materialized, err := loopruntime.MaterializeGoalOpenShellProfile(template, host, "127.0.0.1:12345")
	if err != nil {
		t.Fatal(err)
	}
	goalRef, err := loopruntime.ReadSessionReadbackFile(goal, nil)
	if err != nil {
		t.Fatal(err)
	}
	binaryRef, err := hostReadbackBinaryRef(binary)
	if err != nil {
		t.Fatal(err)
	}
	selected := goalHostSelection{Schema: 2, Goal: goalRef, Root: host, Template: materialized.Template, Profile: materialized.Profile, Noodle: binaryRef, Phase: "ready"}
	b := admission.NewBudget(9)
	if err = b.BindMachineCapacity(filepath.Join(root, "machine"), host+"/resident"); err != nil {
		t.Fatal(err)
	}
	claim := admission.ResourceClaim{Source: "original-owner-empty-resident-fixture", MemoryBytes: 10, CPUs: 1, DiskBytes: 20, DiskPaths: []string{root}, Policy: admission.CapacityPolicy{MemoryBytes: 1000, CPUs: 8, DiskSafetyBytes: 10, RecoverySamples: 2}}
	source := admission.PreparationSource{Purpose: "host-resident", Bindings: map[string]string{
		"host_root": host, "native_root": owner, "goal_path": goalRef.Path, "goal_sha256": goalRef.SHA256,
		"binary_path": binaryRef.Path, "binary_sha256": binaryRef.SHA256, "template_path": materialized.Template.Path, "template_sha256": materialized.Template.SHA256, "template_policy": materialized.TemplatePolicySHA256}}
	ref, err := b.PrepareMachineResources(context.Background(), source, claim, hostShutdownTestSampler{})
	if err != nil {
		t.Fatal(err)
	}
	if err = b.StartPreparationEffects(ref); err != nil {
		t.Fatal(err)
	}
	selected.Preparation = ref
	if err = saveGoalHostSelection(filepath.Join(owner, "goals", hostDigest([]byte(goalRef.Path+"\x00"+goalRef.SHA256))+".json"), selected); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(host, "g", id)
	if err = os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	generation := hostGeneration{ID: id, Directory: directory, Binary: binary, SHA: binaryRef.SHA256, Preparation: &ref, State: "active", Process: procx.ProcessRecord{PID: os.Getpid(), SessionID: id, StartedAt: time.Now().UTC()}}
	if err = updateHostPool(host, func(pool *hostPool) error { pool.Active = id; pool.Generations[id] = generation; return nil }); err != nil {
		t.Fatal(err)
	}
	if err = b.BindSharedQuota(filepath.Join(host, "quota"), id); err != nil {
		t.Fatal(err)
	}
	residents, err := admission.OpenResidents(directory)
	if err != nil {
		t.Fatal(err)
	}
	h := &noodleHost{dir: directory, instanceID: id, binaryPath: binary, binarySHA: binaryRef.SHA256, budget: b, residents: residents, contexts: map[string]*hostContext{}, rolling: &hostRolling{root: host, carry: map[string]rollingCarry{}}}
	identity := procx.NativeIdentity{PID: os.Getpid(), UID: os.Getuid(), StartSec: 1, Version: 1, Token: []byte("explicit test-only native identity")}
	exited := false
	native := emptyHostRetirementNative{
		budget:  func(string) (*admission.Budget, error) { return b, nil },
		observe: func(int) (procx.NativeIdentity, error) { return identity, nil },
		absent:  func(procx.ProcessRecord) (bool, error) { return exited, nil },
		rpc: func(ctx context.Context, actual string, req hostRequest) (json.RawMessage, error) {
			if actual != directory || req.Operation != "resources" {
				return nil, errors.New("foreign native readback")
			}
			// A serial actor also needs this lock for socket repair. The Owner
			// must never hold it while waiting for a resource RPC to complete.
			lock, err := lockfile.TryLock(filepath.Join(host, "pool.lock"))
			if err != nil {
				return nil, errors.New("resources barrier waited on its own pool lock")
			}
			lock.Close()
			pool, err := readHostPool(host)
			if err != nil {
				return nil, err
			}
			if pool.Active == "" {
				if fault == "raced-owner" {
					if err := updateHostPool(host, func(p *hostPool) error {
						p.Owners[strings.Repeat("c", 32)] = hostTaskOwner{State: "admitting"}
						return nil
					}); err != nil {
						return nil, err
					}
				} else if fault != "missing-witness" && !exited {
					if !h.advanceRolling(ctx) {
						return nil, errors.New("original inactive empty Actor did not retire")
					}
					if err := h.saveResidentExitWitness(); err != nil {
						return nil, err
					}
					exited = true
					if fault == "lost-actor-ack" {
						return nil, errors.New("original Actor reply was lost after witness")
					}
				}
			}
			return json.Marshal(h.resources())
		}}
	return owner, goal, b, h, native
}

func TestEmptyHostRetirementOriginalActorWitnessReturnsSameRow(t *testing.T) {
	for _, fault := range []string{"none", "lost-actor-ack"} {
		t.Run(fault, func(t *testing.T) {
			owner, goal, budget, h, native := emptyRetirementControllerFixture(t, fault)
			result, err := retireEmptyGoalHost(context.Background(), owner, goal, true, native)
			if err != nil || result["status"] != "retired" {
				t.Fatal("original empty retirement did not converge", result, err)
			}
			if snapshot := budget.Snapshot().MachineCapacity; snapshot == nil || snapshot.Leases != 0 {
				t.Fatal("original resident row remained held", snapshot)
			}
			pool, err := readHostPool(h.controlRoot())
			if err != nil || pool.Active != "" || pool.Generations[h.instanceID].ResidentExit == nil {
				t.Fatal("original Actor witness or fence was lost", pool, err)
			}
			for range 2 {
				again, err := retireEmptyGoalHost(context.Background(), owner, goal, false, native)
				if err != nil || again["status"] != "retired" {
					t.Fatal("original retirement readback lost closure", again, err)
				}
			}
			if _, err = os.Stat(filepath.Join(h.controlRoot(), "openshell", "profile.json")); err != nil {
				t.Fatal("retirement deleted original profile", err)
			}
		})
	}
}

func TestEmptyHostRetirementPreservesRacedOwnerAndMissingWitness(t *testing.T) {
	for _, fault := range []string{"raced-owner", "missing-witness"} {
		t.Run(fault, func(t *testing.T) {
			owner, goal, budget, h, native := emptyRetirementControllerFixture(t, fault)
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			result, err := retireEmptyGoalHost(ctx, owner, goal, true, native)
			if err == nil || result["status"] == "retired" || budget.Snapshot().MachineCapacity.Leases != 1 {
				t.Fatal("unknown retirement returned original resident resources", result, err)
			}
			if fault == "raced-owner" {
				pool, err := readHostPool(h.controlRoot())
				if err != nil || len(pool.Owners) != 1 {
					t.Fatal("raced owner was removed or relabelled", err)
				}
			}
		})
	}
}

func TestEmptyHostRetirementSlowFenceNeverReturnsCapacity(t *testing.T) {
	owner, goal, budget, h, native := emptyRetirementControllerFixture(t, "none")
	native.verify = func(refs map[string]loopruntime.SessionReadbackFileRef) error {
		// Trigger the actual old Actor tick while the entire fence callback is
		// inside the pool transaction. Wait for its original lock timeout rather
		// than assuming that hashing/fsync will finish within one second.
		done := make(chan bool, 1)
		go func() { done <- h.advanceRolling(context.Background()) }()
		if <-done || h.rolling.failure == "" {
			return errors.New("old Actor lock-poison counterexample was not reached")
		}
		return loopruntime.VerifySessionReadbackFiles(refs)
	}
	result, err := retireEmptyGoalHost(context.Background(), owner, goal, true, native)
	if err == nil || result["status"] == "retired" || h.rolling.failure == "" || budget.Snapshot().MachineCapacity.Leases != 1 {
		t.Fatal("slow pool transaction became a false completed retirement", result, err)
	}
}
