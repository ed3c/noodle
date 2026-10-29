package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
	"github.com/poteto/noodle/loop"
)

func TestRestrictedRecoveryRequiresNativePauseThenCheckpoint(t *testing.T) {
	dir, custody := t.TempDir(), t.TempDir()
	intent := filepath.Join(custody, "pause.json")
	if held, err := legacyPauseCapture(context.Background(), dir, intent, true); err != nil || held {
		t.Fatal(held, err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "control.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	var cmd loop.ControlCommand
	if err = json.Unmarshal(raw, &cmd); err != nil {
		t.Fatal(err)
	}
	if cmd.Action != "pause" {
		t.Fatal("recovery attempted business control", cmd)
	}
	ack, _ := json.Marshal(loop.ControlAck{ID: cmd.ID, Action: "pause", Status: "ok", DispatchState: loop.StatePaused, At: time.Now()})
	if err = os.WriteFile(filepath.Join(dir, "control-ack.ndjson"), append(ack, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	status := []byte(`{"effective_mode":"supervised","loop_state":"paused"}`)
	if err = os.WriteFile(filepath.Join(dir, "status.json"), status, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "control.ndjson"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if held, err := legacyPauseCapture(context.Background(), dir, intent, true); err != nil || !held {
		t.Fatal(held, err)
	}
	if _, err = os.Stat(filepath.Join(dir, "state.snapshot.json")); !os.IsNotExist(err) {
		t.Fatal("pause invented a canonical checkpoint", err)
	}
	modePath := filepath.Join(custody, "mode.json")
	if round, err := legacyModeCheckpoint(context.Background(), dir, modePath); err != nil || round != nil {
		t.Fatal(round, err)
	}
	modeRaw, _ := os.ReadFile(filepath.Join(dir, "control.ndjson"))
	var mode loop.ControlCommand
	if err = json.Unmarshal(modeRaw, &mode); err != nil {
		t.Fatal(err)
	}
	modeACK, _ := json.Marshal(loop.ControlAck{ID: mode.ID, Action: "mode", Status: "ok", DispatchState: loop.StatePaused, At: time.Now()})
	if err = os.WriteFile(filepath.Join(dir, "control-ack.ndjson"), append(append(ack, '\n'), append(modeACK, '\n')...), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = legacyModeCheckpoint(context.Background(), dir, modePath); err == nil {
		t.Fatal("ACK substituted for native canonical checkpoint")
	}
	at := time.Now()
	snapshot := reducer.BuildSnapshot(state.State{Mode: state.RunModeManual, ModeEpoch: 1, ModeTransitions: []state.ModeTransitionRecord{{FromMode: state.RunModeSupervised, ToMode: state.RunModeManual, Epoch: 1, AppliedAt: at}}}, nil, at)
	if err = reducer.WriteSnapshotAtomic(filepath.Join(dir, "state.snapshot.json"), snapshot); err != nil {
		t.Fatal(err)
	}
	round, err := legacyModeCheckpoint(context.Background(), dir, modePath)
	if err != nil || round == nil || round.Mode != "supervised" || round.DispatchState != loop.StatePaused {
		t.Fatal(round, err)
	}
}

func TestRestrictedImportRetainsHistoryAndHoldsAllWork(t *testing.T) {
	root := t.TempDir()
	oldID, newSource := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	var profile hostExecutionProfile
	var inputs hostContextInputs
	if err := json.Unmarshal([]byte(`{"future":{"unknown":true}}`), &profile); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"opaque":[1,2,3]}`), &inputs); err != nil {
		t.Fatal(err)
	}
	old := hostPool{Schema: 1, Active: oldID, Generations: map[string]hostGeneration{oldID: {ID: oldID, Directory: filepath.Join(root, "g", oldID), State: "active"}}, Owners: map[string]hostTaskOwner{"historical": {State: "ready", Original: hostReceipt{RequestID: "historical"}, RebuildError: "existing gap", ExecutionProfile: &profile, ContextInputs: &inputs}}, Queue: []hostVersionRequest{{ID: "old-request", State: "queued", Binary: "/original", SHA: "unchanged"}}}
	r := hostReceipt{RequestID: "new-request", ContextID: "context", HostInstanceID: newSource, ProjectDir: "/original/project", Mode: "manual", ConfigSHA256: "config", BinarySHA256: "binary"}
	detached := r
	detached.ContextState = "detached"
	detached.TerminalCause = loop.TerminalMigrationDetach
	file := filepath.Join(root, "request.json")
	binding, _ := json.Marshal(hostRequestBinding{EnvironmentSHA256: "original-environment"})
	if err := os.WriteFile(file, binding, 0600); err != nil {
		t.Fatal(err)
	}
	j := restrictedMaintenance{Plan: hostMaintenancePlan{PlanSHA: "epoch"}, Tasks: []restrictedTask{{Plan: hostMaintenanceContextPlan{Original: r, RequestFile: file}, Detached: &detached, Round: &loop.HostContinuation{Mode: "supervised", DispatchState: loop.StatePaused}}}}
	p, err := restrictedImportedPool(old, hostGeneration{ID: newSource, Directory: filepath.Join(root, "g", newSource), State: "exited"}, j)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Generations) != 2 || len(p.Owners) != 2 || len(p.Queue) != 1 || p.Active != "" {
		t.Fatal("lost historical records", p)
	}
	if p.Owners["historical"].RebuildError != "existing gap" || p.Owners["historical"].MaintenanceHold != "epoch" || p.Owners[r.RequestID].Original != r || p.Owners[r.RequestID].MaintenanceHold != "epoch" {
		t.Fatal("historical work was altered or resumed", p)
	}
	profileBefore, _ := json.Marshal(old.Owners["historical"].ExecutionProfile)
	profileAfter, _ := json.Marshal(p.Owners["historical"].ExecutionProfile)
	inputsBefore, _ := json.Marshal(old.Owners["historical"].ContextInputs)
	inputsAfter, _ := json.Marshal(p.Owners["historical"].ContextInputs)
	if !bytes.Equal(profileBefore, profileAfter) || !bytes.Equal(inputsBefore, inputsAfter) {
		t.Fatal("opaque execution metadata changed")
	}
	if p.Queue[0].State != "maintenance-held" || p.Queue[0].MaintenanceState != "queued" || old.Queue[0].State != "queued" || old.Owners["historical"].MaintenanceHold != "" {
		t.Fatal("import mutated original history or replayed a version", p)
	}
	if err = replaceRestrictedPool(root, t.TempDir(), "unknown", p); err == nil {
		t.Fatal("missing original pool accepted")
	}
}

func TestRestrictedPoolImportUncertainIntentNeverRewrites(t *testing.T) {
	root, custody := t.TempDir(), t.TempDir()
	old := []byte(`{"schema":1,"active":"","generations":{},"owners":{},"queue":[]}`)
	path := filepath.Join(root, "pool.json")
	if err := os.WriteFile(path, old, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(custody, "pool-import.json"), []byte(`{"uncertain":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	next := hostPool{Schema: 1, Generations: map[string]hostGeneration{}, Owners: map[string]hostTaskOwner{}}
	if err := replaceRestrictedPool(root, custody, hostDigest(old), next); err == nil {
		t.Fatal("unknown import was repeated")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(after, old) {
		t.Fatal("unknown pool import rewrote source")
	}
}

func TestRestrictedGateNeverReplacesUnexpectedEntry(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "host.sock")
	if err := os.WriteFile(path, []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := publishRestrictedGate(root); err == nil {
		t.Fatal("overwrote unexpected public entry")
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != "foreign" {
		t.Fatal("entry changed")
	}
}

func TestRestrictedCaptureUnknownPhaseDoesNotCreateControlIntent(t *testing.T) {
	root := t.TempDir()
	r := hostReceipt{ProjectDir: root, RequestID: "original"}
	j := restrictedMaintenance{Tasks: []restrictedTask{{Plan: hostMaintenanceContextPlan{Original: r}, Phase: "pausing"}}}
	_, err := captureRestrictedTasksFromOwner(context.Background(), root, &j, hostGeneration{})
	if err == nil || !strings.Contains(err.Error(), "readback only") {
		t.Fatal("unknown phase allowed a fresh effect", err)
	}
	if _, err = os.Stat(filepath.Join(root, "original", "pause.json")); !os.IsNotExist(err) {
		t.Fatal("unknown pause intent was reconstructed", err)
	}
}

func TestRestrictedHeldOwnerRefusesExplicitAndAutomaticRebuild(t *testing.T) {
	root := t.TempDir()
	id := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	req := hostTestRequest(t, t.TempDir())
	r := hostReceipt{RequestID: req.RequestID, ProjectDir: req.ProjectDir, Mode: req.Mode, ConfigSHA256: req.ConfigSHA256, BinarySHA256: "old", HostInstanceID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", ContextID: "original", ContextState: "detached", TerminalCause: loop.TerminalMigrationDetach}
	if err := updateHostPool(root, func(p *hostPool) error {
		p.Active = id
		p.Generations[id] = hostGeneration{ID: id, Directory: filepath.Join(root, "g", id)}
		p.Owners[req.RequestID] = hostTaskOwner{Original: r, Current: r, State: "ready", MaintenanceHold: "epoch", RoundState: &loop.HostContinuation{Mode: "manual", DispatchState: loop.StatePaused}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h := &noodleHost{instanceID: id, rolling: &hostRolling{root: root}}
	req.AdmissionSHA = "old"
	req.RoundSource = r.HostInstanceID
	req.RoundState = &loop.HostContinuation{Mode: "manual", DispatchState: loop.StatePaused}
	for _, got := range []hostResponse{h.rebuildRollingContext(context.Background(), req), h.rollingRound(context.Background(), req)} {
		if got.Status != "busy" {
			t.Fatal("held original task accepted", got)
		}
	}
	p, err := readHostPool(root)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := json.Marshal(p)
	h.rebuildReleasedRounds(context.Background(), p)
	after, err := readHostPool(root)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(after)
	if !bytes.Equal(before, raw) {
		t.Fatal("automatic rebuild changed held work")
	}
}

func TestRestrictedCheckpointCannotDropOriginalAdmittedWork(t *testing.T) {
	dir := t.TempDir()
	cfg := []byte("[runtime.process]\nadmitted_orders=['original-order']\n")
	file := filepath.Join(t.TempDir(), "noodle.toml")
	if err := os.WriteFile(file, cfg, 0600); err != nil {
		t.Fatal(err)
	}
	cp := hostMaintenanceContextPlan{ConfigSource: file, Original: hostReceipt{ConfigSHA256: hostDigest(cfg)}}
	path := filepath.Join(dir, "state.snapshot.json")
	snapshot := reducer.BuildSnapshot(state.State{Orders: map[string]state.OrderNode{}}, nil, time.Now())
	if err := reducer.WriteSnapshotAtomic(path, snapshot); err != nil {
		t.Fatal(err)
	}
	if err := verifyRestrictedCheckpoint(cp, dir); err == nil {
		t.Fatal("empty export substituted for original admitted work")
	}
	snapshot.State.Orders["original-order"] = state.OrderNode{OrderID: "original-order"}
	if err := reducer.WriteSnapshotAtomic(path, snapshot); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	if err := verifyRestrictedCheckpoint(cp, dir); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("checkpoint validator changed original evidence")
	}
}
