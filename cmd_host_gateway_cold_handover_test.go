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

func TestColdResourceScopePreservesAccountingAndRefusesOtherWork(t *testing.T) {
	p := hostMaintenancePlan{Root: "/original", Source: legacyHostResources{Instance: strings.Repeat("a", 32), PID: 42}, Contexts: []hostMaintenanceContextPlan{{Original: hostReceipt{ContextID: "failed"}}, {Original: hostReceipt{ContextID: "completed"}}}, Handover: &hostGatewayHandoverPlan{Binding: loopruntime.OpenShellCustodyBinding{Owner: "gateway"}}}
	for _, fault := range []string{"none", "other-worker", "other-resident", "foreign-lease", "missing-fault", "unknown-resource"} {
		t.Run(fault, func(t *testing.T) {
			row := map[string]any{"host_instance_id": p.Source.Instance, "host_pid": 42, "contexts": 2, "resources": map[string]int{"active_workers": 1, "active_residents": 1, "active_allocations": 2, "unresolved_residents": 0}, "residents": map[string]any{"error": "rename /original/residents-tmp /original/residents.json: no space left on device", "records": []admission.Resident{{ID: "gateway", Name: "gateway", PID: 50, ProcessGroup: 50, Started: "exact birth", State: "running", Contexts: []string{"failed"}}}}}
			resources := row["resources"].(map[string]int)
			resident := row["residents"].(map[string]any)
			switch fault {
			case "other-worker":
				resources["active_workers"] = 2
			case "other-resident":
				resources["active_residents"] = 2
			case "unknown-resource":
				resources["unresolved_residents"] = 1
			case "foreign-lease":
				resident["records"].([]admission.Resident)[0].Contexts = []string{"outside"}
			case "missing-fault":
				resident["error"] = nil
			}
			raw, _ := json.Marshal(row)
			_, err := coldResourceScope(p, raw)
			if (fault == "none") != (err == nil) {
				t.Fatalf("%s: %v", fault, err)
			}
			if resources["active_allocations"] != 2 {
				t.Fatal("old accounts were rewritten")
			}
		})
	}
}

func TestColdRetirementIntentDoesNotRepeatControl(t *testing.T) {
	if !procx.NativeFixtureControlAvailable() {
		t.Skip("generation-bound native control unavailable")
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	identity, err := procx.ObserveNativeIdentity(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	actor := loopruntime.OpenShellRetiredActor{Process: identity, IntentAt: time.Now().UTC()}
	writes := 0
	if err := retireColdActor(context.Background(), &actor, func() error { writes++; return nil }); err == nil || writes != 0 || !actor.ExitedAt.IsZero() {
		t.Fatal("unknown intent repeated or accepted", err, writes)
	}
	current, err := procx.ObserveNativeIdentity(cmd.Process.Pid)
	if err != nil || !identity.SameGeneration(current) || current.Zombie {
		t.Fatal("readback signalled the live fixture", err)
	}
}

func TestColdRetirementPersistsIntentBeforeNativeControl(t *testing.T) {
	if !procx.NativeFixtureControlAvailable() {
		t.Skip("generation-bound native control unavailable")
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	identity, err := procx.ObserveNativeIdentity(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	actor := loopruntime.OpenShellRetiredActor{Process: identity}
	writes := 0
	err = retireColdActor(context.Background(), &actor, func() error {
		writes++
		if writes == 1 && (actor.IntentAt.IsZero() || !actor.ExitedAt.IsZero()) {
			t.Fatal("control preceded intent")
		}
		return nil
	})
	if err != nil || writes != 2 || actor.ExitedAt.IsZero() {
		t.Fatal("native retirement lacks actual exit", err, writes)
	}
	writes = 0
	if err := retireColdActor(context.Background(), &actor, func() error { writes++; return nil }); err != nil || writes != 0 {
		t.Fatal("completed retirement repeated", err, writes)
	}
}

func TestColdFailureRequiresOriginalHandoverPlanEntry(t *testing.T) {
	cmd := newHostRollingCmd()
	cmd.SetArgs([]string{"--failure-recovery-file", "/original/recovery.json"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("cold recovery bypassed original plan")
	}
}

func TestColdJournalRejectsCompletedOwnerCleanupPhases(t *testing.T) {
	ref := loopruntime.SessionReadbackFileRef{Path: "/fixture/source", SHA256: strings.Repeat("a", 64)}
	native := procx.NativeIdentity{PID: 42, UID: os.Getuid(), StartSec: 1, Version: 1, Token: []byte("fixture")}
	gateway := native
	gateway.PID = 43
	p, err := sealGatewayHandoverPlan(hostMaintenancePlan{Schema: 1, Root: "/fixture/private", Target: "/fixture/target", TargetSHA: ref.SHA256, Policy: admission.WorkerAdmissionPolicy, Native: &native, Source: legacyHostResources{Instance: strings.Repeat("b", 32), PID: 42, Contexts: 1}, Contexts: []hostMaintenanceContextPlan{{RequestFile: "/fixture/request"}}, Handover: &hostGatewayHandoverPlan{SharedRoot: "/fixture/shared", SharedInstance: strings.Repeat("c", 32), SourceBinary: ref, Profile: ref, Custody: ref, Cold: &hostColdHandoverPlan{Recovery: ref, StopPlan: ref, Stop: ref, Profile: ref, Inventory: ref, Actors: []procx.NativeIdentity{native, gateway}}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, phase := range []string{"handover-prepared", "handover-fencing", "handover-cleanup", "handover-cold-retiring", "handover-cold-releasing", "handover-completed", "handover-releasing", "handover-stopping"} {
		dir := t.TempDir()
		j := restrictedMaintenance{Schema: 1, Plan: p, Phase: phase, Tasks: []restrictedTask{{Plan: p.Contexts[0], Phase: "prepared"}}}
		if err := saveRestrictedMaintenance(dir, j); err != nil {
			t.Fatal(err)
		}
		_, err := readRestrictedMaintenance(dir)
		want := phase != "handover-releasing" && phase != "handover-stopping"
		if want != (err == nil) {
			t.Fatal(phase, err)
		}
	}
}

func TestColdAbsentSourceUsesOriginalIdentityAndImmutableHistory(t *testing.T) {
	if !procx.NativeFixtureControlAvailable() {
		t.Skip("generation-bound source observation unavailable")
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	identity, err := procx.ObserveNativeIdentity(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	write := func(name string, value any) loopruntime.SessionReadbackFileRef {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		ref, err := loopruntime.ReadSessionReadbackFile(path, nil)
		if err != nil {
			t.Fatal(err)
		}
		return ref
	}
	history := write("receipts.json", []string{"original-failed-task"})
	var resources legacyHostResources
	resources.PID, resources.Instance = identity.PID, strings.Repeat("b", 32)
	resource := write("resources.json", resources)
	plan, err := sealGatewayHandoverPlan(hostMaintenancePlan{Schema: 1, Root: root, Native: &identity, Source: resources, Files: map[string]string{"receipts.json": history.SHA256}, Blockers: []string{"original resource failure"}})
	if err != nil {
		t.Fatal(err)
	}
	planRef := write("plan.json", plan)
	recovery := write("recovery.json", map[string]any{"Schema": 1, "HostResources": resource, "HostHistory": history})
	if _, err = readColdAbsentSource(root, recovery.Path, planRef.Path); err == nil {
		t.Fatal("live original owner treated as absent")
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if _, err = readColdAbsentSource(root, recovery.Path, planRef.Path); err != nil {
		t.Fatal("native absence rejected", err)
	}
	write("receipts.json", []string{"different-task"})
	if _, err = readColdAbsentSource(root, recovery.Path, planRef.Path); err == nil {
		t.Fatal("changed history accepted")
	}
}

func TestColdRetirementReadsNaturalActorExitWithoutSignal(t *testing.T) {
	if !procx.NativeFixtureControlAvailable() {
		t.Skip("generation-bound source observation unavailable")
	}
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	identity, err := procx.ObserveNativeIdentity(cmd.Process.Pid)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatal(err)
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	actor := loopruntime.OpenShellRetiredActor{Process: identity}
	writes := 0
	if err := retireColdActor(context.Background(), &actor, func() error { writes++; return nil }); err != nil || writes != 1 || actor.ExitedAt.IsZero() {
		t.Fatal("natural exit required an unnecessary control", err, writes)
	}
}
