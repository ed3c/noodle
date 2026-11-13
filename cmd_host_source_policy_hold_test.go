package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/poteto/noodle/internal/procx"
	loopruntime "github.com/poteto/noodle/runtime"
)

func sourcePolicyTestJSON(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func sourcePolicyTestPlan(t *testing.T) (hostMaintenancePlan, []byte) {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "n-src-policy-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	id := strings.Repeat("a", 32)
	native := procx.NativeIdentity{PID: 42, UID: os.Getuid(), StartSec: 10, Version: 1, Token: []byte("fixture-only-not-control-evidence")}
	if actual, err := procx.ObserveNativeIdentity(os.Getpid()); err == nil {
		native = actual
	}
	op := hostOperatorPlan{Schema: 1, Effects: []string{}, NativeScope: &hostOperatorNativeScope{Schema: 1, Host: hostOperatorActor{Process: native, Binary: loopruntime.SessionReadbackFileRef{Path: filepath.Join(root, "noodle"), SHA256: strings.Repeat("b", 64)}}}}
	op.Binding = loopruntime.OpenShellCustodyBinding{StateRoot: filepath.Join(root, "state"), Gateway: "https://127.0.0.1:1", Owner: "fixture", Workspace: "fixture", ConfigSource: filepath.Join(root, "config.json"), ConfigFingerprint: "fixture"}
	op.Source = sourcePolicyTestJSON(t, map[string]any{"protocol": hostProtocol, "host_instance_id": id, "host_pid": native.PID, "contexts": 2, "rolling_host_version": 1, "host_root": root})
	pool := hostPool{Schema: 1, Active: id, Generations: map[string]hostGeneration{id: {ID: id, Directory: filepath.Join(root, "g", id), State: "active"}}, Owners: map[string]hostTaskOwner{}, Queue: []hostVersionRequest{{ID: "queued", State: "queued"}}}
	for i, state := range []string{"active", "failed"} {
		r := hostReceipt{Schema: 1, Protocol: hostProtocol, HostPID: native.PID, HostInstanceID: id, ContextID: strings.Repeat(string(rune('c'+i)), 32), RequestID: strings.Repeat(string(rune('e'+i)), 32), ProjectDir: filepath.Join(root, state), ContextState: state, BinarySHA256: op.NativeScope.Host.Binary.SHA256, BinaryPath: op.NativeScope.Host.Binary.Path, OwnerBinding: &hostOwnerBinding{Schema: 1, Sources: map[string]loopruntime.SessionReadbackFileRef{"original": {Path: "/fixture/original", SHA256: "fixture"}}}}
		op.Contexts = append(op.Contexts, r)
		current := r
		current.ContextState = "active" // Preserve the real history/current distinction.
		pool.Owners[r.RequestID] = hostTaskOwner{Original: r, Current: current, State: "unknown", RebuildError: "original failure", Previous: []hostReceipt{r}}
	}
	doc := map[string]any{}
	if err := json.Unmarshal(sourcePolicyTestJSON(t, pool), &doc); err != nil {
		t.Fatal(err)
	}
	doc["future_pool_member"] = map[string]any{"keep": true}
	for _, value := range doc["owners"].(map[string]any) {
		value.(map[string]any)["future_owner_member"] = []any{"keep", true}
	}
	raw := sourcePolicyTestJSON(t, doc)
	path := filepath.Join(root, "pool.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	op.Pool, err = loopruntime.ReadSessionReadbackFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]*loopruntime.SessionReadbackFileRef{"request": &op.Request, "config": &op.Config, "profile": &op.Profile, "history": &op.History, "custody": &op.Custody} {
		data := []byte(`{"fixture":"original"}`)
		if name == "history" {
			data = sourcePolicyTestJSON(t, op.Contexts)
		}
		if name == "custody" {
			data = sourcePolicyTestJSON(t, loopruntime.OpenShellCustodyState{Version: 4, ControlRoot: root, Binding: op.Binding, Blocked: "original-unknown", Records: []loopruntime.OpenShellCustodyRecord{{Name: "original-unknown"}}})
		}
		path := filepath.Join(root, name+".json")
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		*target, err = loopruntime.ReadSessionReadbackFile(path, nil)
		if err != nil {
			t.Fatal(err)
		}
	}
	path = filepath.Join(root, "snapshot.json")
	if err := os.WriteFile(path, sourcePolicyTestJSON(t, op), 0600); err != nil {
		t.Fatal(err)
	}
	p, err := prepareSourcePolicyHold(path)
	if err != nil {
		t.Fatal(err)
	}
	return p, raw
}

func TestSourcePolicyPrepareSelectsOnlyAdmissionHoldAndKeepsOriginalBinding(t *testing.T) {
	p, _ := sourcePolicyTestPlan(t)
	if err := validateRestrictedPlan(p); err != nil {
		t.Fatal("same nonnil OwnerBinding was compared by pointer", err)
	}
	if p.SourcePolicy.Operation != "admission-hold" || p.Contexts[1].Original.ContextState != "failed" || p.Handover != nil || len(p.Effects) != 0 {
		t.Fatal("preparation changed scope or original outcome")
	}
	cmd := newHostOperatorPlanCmd()
	var output bytes.Buffer
	cmd.SetOut(&output)
	cmd.SetArgs([]string{"--hold-plan-file", p.SourcePolicy.Snapshot.Path})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var actual hostMaintenancePlan
	if err := json.Unmarshal(output.Bytes(), &actual); err != nil || actual.PlanSHA != p.PlanSHA {
		t.Fatal("original CLI did not prepare the same bounded plan", err)
	}
	changed := p
	copyPolicy := *p.SourcePolicy
	copyPolicy.Operation = "retire"
	changed.SourcePolicy, changed.PlanSHA = &copyPolicy, ""
	changed.PlanSHA = hostDigest(sourcePolicyTestJSON(t, changed))
	if err := validateRestrictedPlan(changed); err == nil {
		t.Fatal("valid digest expanded admission hold into retirement")
	}
	j := restrictedMaintenance{Schema: 1, Plan: p, Phase: "source-policy-admission-held"}
	dir := t.TempDir()
	if err := saveRestrictedMaintenance(dir, j); err != nil {
		t.Fatal(err)
	}
	readback, err := readRestrictedMaintenance(dir)
	if err != nil || !reflect.DeepEqual(j, readback) {
		t.Fatal("original maintenance journal lost Source variant or original states", err)
	}
}

func TestSourcePolicyAdmissionHoldPreservesOpaqueStateAndNeverRewindsNativeOutcome(t *testing.T) {
	p, before := sourcePolicyTestPlan(t)
	dir := t.TempDir()
	if err := sourcePolicyHoldPool(p.Root, dir, before, p); err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(filepath.Join(p.Root, "pool.json"))
	if err != nil {
		t.Fatal(err)
	}
	var original, held map[string]any
	if err = json.Unmarshal(before, &original); err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(current, &held); err != nil {
		t.Fatal(err)
	}
	for _, owner := range original["owners"].(map[string]any) {
		owner.(map[string]any)["maintenance_hold"] = p.PlanSHA
	}
	if !reflect.DeepEqual(original, held) {
		t.Fatal("hold changed unknown fields, outcomes, generations or queue")
	}
	if err = sourcePolicyHoldPool(p.Root, dir, before, p); err != nil {
		t.Fatal("same hold could not read back", err)
	}
	owner := held["owners"].(map[string]any)[p.Contexts[0].Original.RequestID].(map[string]any)
	owner["state"] = "native-outcome-progressed"
	owner["current"].(map[string]any)["context_state"] = "failed"
	progressed := sourcePolicyTestJSON(t, held)
	path := filepath.Join(p.Root, "pool.json")
	if err = os.WriteFile(path, progressed, 0600); err != nil {
		t.Fatal(err)
	}
	if err = sourcePolicyHoldPool(p.Root, dir, before, p); err != nil {
		t.Fatal("same epoch positive readback refused native outcome progress", err)
	}
	if actual, err := os.ReadFile(path); err != nil || !bytes.Equal(actual, progressed) {
		t.Fatal("original outcome was replayed over native progress", err)
	}
}

func TestSourcePolicyAdmissionHoldKeepsProgressBeforeFirstWrite(t *testing.T) {
	p, before := sourcePolicyTestPlan(t)
	var doc map[string]any
	if err := json.Unmarshal(before, &doc); err != nil {
		t.Fatal(err)
	}
	owner := doc["owners"].(map[string]any)[p.Contexts[0].Original.RequestID].(map[string]any)
	owner["state"] = "native-progress-before-hold"
	current := sourcePolicyTestJSON(t, doc)
	if err := os.WriteFile(filepath.Join(p.Root, "pool.json"), current, 0600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := sourcePolicyHoldPool(p.Root, dir, before, p); err != nil {
		t.Fatal(err)
	}
	var after map[string]any
	raw, err := os.ReadFile(filepath.Join(p.Root, "pool.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(raw, &after); err != nil {
		t.Fatal(err)
	}
	actual := after["owners"].(map[string]any)[p.Contexts[0].Original.RequestID].(map[string]any)
	if actual["state"] != "native-progress-before-hold" || actual["maintenance_hold"] != p.PlanSHA {
		t.Fatal("first hold rewound native progress")
	}
	archived, err := os.ReadFile(filepath.Join(dir, "pool-before-"+hostDigest(current)+".json"))
	if err != nil || !bytes.Equal(archived, current) {
		t.Fatal("first CAS source was not preserved", err)
	}
}

func TestSourcePolicyHistoryPreservesFailureAndAcceptsOnlyOutcomeProgress(t *testing.T) {
	p, _ := sourcePolicyTestPlan(t)
	op, err := readSourcePolicySnapshot(p.SourcePolicy.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	before, err := os.ReadFile(op.History.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "original-history.json"), before, 0600); err != nil {
		t.Fatal(err)
	}
	var current []hostReceipt
	if err = json.Unmarshal(before, &current); err != nil {
		t.Fatal(err)
	}
	current[0].ContextState = "failed"
	progressed := sourcePolicyTestJSON(t, current)
	if err = os.WriteFile(op.History.Path, progressed, 0600); err != nil {
		t.Fatal(err)
	}
	ref, contexts, err := sourcePolicyCurrentHistory(op, dir)
	if err != nil || contexts != 2 || ref.SHA256 != hostDigest(progressed) {
		t.Fatal("native outcome progress was frozen to the old SHA", err)
	}
	if err = verifySourcePolicyHoldFiles(op); err != nil {
		t.Fatal("current history was treated as a Frozen configuration input", err)
	}
	if original, err := os.ReadFile(filepath.Join(dir, "original-history.json")); err != nil || !bytes.Equal(original, before) {
		t.Fatal("original failure history was rewritten", err)
	}
	current[0].ProjectDir = "/foreign/project"
	if err = os.WriteFile(op.History.Path, sourcePolicyTestJSON(t, current), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err = sourcePolicyCurrentHistory(op, dir); err == nil {
		t.Fatal("foreign context identity accepted as outcome progress")
	}
	if err = os.WriteFile(op.History.Path, append(progressed, []byte("\n{}")...), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err = sourcePolicyCurrentHistory(op, dir); err == nil {
		t.Fatal("trailing history data was silently ignored")
	}
}

func TestSourcePolicyCustodyAllowsFactsButRefusesControlIdentityDrift(t *testing.T) {
	p, _ := sourcePolicyTestPlan(t)
	op, err := readSourcePolicySnapshot(p.SourcePolicy.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	before, err := os.ReadFile(op.Custody.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "original-custody.json"), before, 0600); err != nil {
		t.Fatal(err)
	}
	var current loopruntime.OpenShellCustodyState
	if err = json.Unmarshal(before, &current); err != nil {
		t.Fatal(err)
	}
	current.Blocked = "native updated diagnostic"
	current.PendingRetained = []loopruntime.OpenShellRetainedSandbox{{Name: "native pending"}}
	if err = os.WriteFile(op.Custody.Path, sourcePolicyTestJSON(t, current), 0600); err != nil {
		t.Fatal(err)
	}
	if err = verifySourcePolicyCustody(op, dir); err != nil {
		t.Fatal("native ledger facts were frozen during admission-only hold", err)
	}
	current.ControlRoot = "/other/control"
	if err = os.WriteFile(op.Custody.Path, sourcePolicyTestJSON(t, current), 0600); err != nil {
		t.Fatal(err)
	}
	if err = verifySourcePolicyCustody(op, dir); err == nil {
		t.Fatal("foreign control identity accepted as native facts")
	}
	if original, err := os.ReadFile(filepath.Join(dir, "original-custody.json")); err != nil || !bytes.Equal(original, before) {
		t.Fatal("original custody unknown/failure was changed", err)
	}
}

func TestSourcePolicyAdmissionRunnerResumesNativeOutcomeWithoutLoopControl(t *testing.T) {
	if _, err := procx.ObserveNativeIdentity(os.Getpid()); err != nil {
		t.Skip("native kernel identity readback unavailable")
	}
	p, before := sourcePolicyTestPlan(t)
	op, err := readSourcePolicySnapshot(p.SourcePolicy.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(p.Root, "maintenance", p.PlanSHA)
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	j := restrictedMaintenance{Schema: 1, Plan: p, Phase: "source-policy-prepared"}
	if err = saveRestrictedMaintenance(dir, j); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"original-history.json": op.History.Path, "original-pool.json": op.Pool.Path, "original-custody.json": op.Custody.Path} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	bootstrap := hostBootstrap{Schema: 1, Phase: "active", Target: p.Target, TargetSHA: p.TargetSHA, Source: restrictedSource(p), ProcessIdentity: "original-fixture"}
	if err = saveHostBootstrap(p.Root, bootstrap); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(p.Root, "bootstrap.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "original-bootstrap.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	generation := filepath.Join(p.Root, "g", p.Source.Instance)
	if err = os.MkdirAll(generation, 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(generation, "host.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	backend, err := os.Lstat(filepath.Join(generation, "host.sock"))
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			var request hostRequest
			if err = json.NewDecoder(conn).Decode(&request); err != nil || request.Operation != "resources" {
				t.Error("hold attempted native control instead of resource readback", err, request.Operation)
				conn.Close()
				return
			}
			var resource map[string]any
			if err = json.Unmarshal(op.Source, &resource); err != nil {
				t.Error(err)
				conn.Close()
				return
			}
			resource["resources"] = map[string]int{"active_workers": 3}
			if err = json.NewEncoder(conn).Encode(resource); err != nil {
				t.Error(err)
			}
			conn.Close()
		}
	}()
	if err = os.Symlink(filepath.Join(generation, "host.sock"), filepath.Join(p.Root, "host.sock")); err != nil {
		t.Fatal(err)
	}
	for round := 0; round < 2; round++ {
		if round == 1 {
			var custody loopruntime.OpenShellCustodyState
			data, err := os.ReadFile(op.Custody.Path)
			if err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(data, &custody); err != nil {
				t.Fatal(err)
			}
			custody.Blocked = "native-updated-diagnostic"
			custody.Records = append(custody.Records, loopruntime.OpenShellCustodyRecord{Name: "native-progress"})
			if err = os.WriteFile(op.Custody.Path, sourcePolicyTestJSON(t, custody), 0600); err != nil {
				t.Fatal(err)
			}
			var history []hostReceipt
			data, err = os.ReadFile(op.History.Path)
			if err != nil || json.Unmarshal(data, &history) != nil {
				t.Fatal(err)
			}
			history[0].ContextState = "failed"
			if err = os.WriteFile(op.History.Path, sourcePolicyTestJSON(t, history), 0600); err != nil {
				t.Fatal(err)
			}
			var pool hostPool
			data, err = os.ReadFile(op.Pool.Path)
			if err != nil || json.Unmarshal(data, &pool) != nil {
				t.Fatal(err)
			}
			o := pool.Owners[p.Contexts[0].Original.RequestID]
			o.State, o.Current.ContextState = "native-failed", "failed"
			pool.Owners[o.Current.RequestID] = o
			if err = os.WriteFile(op.Pool.Path, sourcePolicyTestJSON(t, pool), 0600); err != nil {
				t.Fatal(err)
			}
		}
		var output bytes.Buffer
		if err = runSourcePolicyHold(context.Background(), p, dir, json.NewEncoder(&output)); err != nil {
			t.Fatal("isolated original maintenance resume failed", round, err)
		}
		var result restrictedMaintenance
		if err = json.Unmarshal(output.Bytes(), &result); err != nil || result.Phase != "source-policy-admission-held" {
			t.Fatal("bounded admission hold did not read back", err)
		}
		if len(result.Tasks) != 0 || result.Plan.Contexts[0].Original.ContextState != "active" || result.Plan.Contexts[1].Original.ContextState != "failed" {
			t.Fatal("original history was replaced by paused/completed states")
		}
	}
	if actual, err := os.Lstat(filepath.Join(generation, "host.sock")); err != nil || !os.SameFile(backend, actual) {
		t.Fatal("generation socket was moved or replaced", err)
	}
	for _, cp := range p.Contexts {
		if _, err := os.Stat(filepath.Join(cp.Original.ProjectDir, ".noodle", "control.ndjson")); !os.IsNotExist(err) {
			t.Fatal("admission hold sent Loop control", err)
		}
	}
	if actual, err := os.ReadFile(filepath.Join(dir, "original-pool.json")); err != nil || !bytes.Equal(actual, before) {
		t.Fatal("original pool image was lost during resume", err)
	}
}

func TestSourcePolicyAdmissionHoldRefusesCompetingEpochAndUnknownWrite(t *testing.T) {
	for _, fault := range []string{"competing-epoch", "identity", "unplanned-owner", "unknown-write"} {
		t.Run(fault, func(t *testing.T) {
			p, before := sourcePolicyTestPlan(t)
			dir := t.TempDir()
			var pool map[string]any
			if err := json.Unmarshal(before, &pool); err != nil {
				t.Fatal(err)
			}
			owners := pool["owners"].(map[string]any)
			owner := owners[p.Contexts[0].Original.RequestID].(map[string]any)
			switch fault {
			case "competing-epoch":
				owner["maintenance_hold"] = "other-epoch"
			case "identity":
				owner["current"].(map[string]any)["context_id"] = "different"
			case "unplanned-owner":
				owners["unplanned"] = owner
			case "unknown-write":
				if err := os.WriteFile(filepath.Join(dir, "pool-import.json"), []byte("{}"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			current := sourcePolicyTestJSON(t, pool)
			path := filepath.Join(p.Root, "pool.json")
			if err := os.WriteFile(path, current, 0600); err != nil {
				t.Fatal(err)
			}
			if err := sourcePolicyHoldPool(p.Root, dir, before, p); err == nil {
				t.Fatal("conflict or unknown outcome allowed another write")
			}
			if actual, err := os.ReadFile(path); err != nil || !bytes.Equal(actual, current) {
				t.Fatal("refusal changed original pool bytes", err)
			}
		})
	}
}

func TestSourcePolicyMarkerPreservesBeforeImageAndRefusesAnotherOwner(t *testing.T) {
	p, poolBefore := sourcePolicyTestPlan(t)
	dir := t.TempDir()
	before := []byte(`{"phase":"active","opaque":"preserve exactly"}`)
	path := filepath.Join(p.Root, "bootstrap.json")
	for _, target := range []string{path, filepath.Join(dir, "original-bootstrap.json")} {
		if err := os.WriteFile(target, before, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := sourcePolicyMarker(p.Root, dir, p); err != nil {
		t.Fatal(err)
	}
	if err := sourcePolicyMarker(p.Root, dir, p); err != nil {
		t.Fatal("same marker could not read back", err)
	}
	if err := sourcePolicyHoldPool(p.Root, dir, poolBefore, p); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(p.Root, "host.sock")
	if err := os.Symlink(filepath.Join(p.Root, "maintenance.sock"), alias); err != nil {
		t.Fatal(err)
	}
	if err := verifySourcePolicyAdmissionHold(p.Root, p); err != nil {
		t.Fatal("same epoch final admission readback refused", err)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(p.Root, "foreign.sock"), alias); err != nil {
		t.Fatal(err)
	}
	if err := verifySourcePolicyAdmissionHold(p.Root, p); err == nil {
		t.Fatal("public alias drift was reported admission-held")
	}
	foreign := []byte(`{"phase":"other-maintenance"}`)
	if err := os.WriteFile(path, foreign, 0600); err != nil {
		t.Fatal(err)
	}
	if err := sourcePolicyMarker(p.Root, dir, p); err == nil {
		t.Fatal("another owner's marker was overwritten")
	}
	if actual, err := os.ReadFile(path); err != nil || !bytes.Equal(actual, foreign) {
		t.Fatal("marker refusal had effects", err)
	}
	if actual, err := os.ReadFile(filepath.Join(dir, "original-bootstrap.json")); err != nil || !bytes.Equal(actual, before) {
		t.Fatal("original marker bytes were not preserved", err)
	}
}
