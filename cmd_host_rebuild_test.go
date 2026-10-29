package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/execution"
	"github.com/poteto/noodle/internal/procx"
	"github.com/poteto/noodle/loop"
)

func TestHostContextInputsKeepOnlyRequiredSources(t *testing.T) {
	req := hostTestRequest(t, t.TempDir())
	data := []byte("[server]\nenabled=false\n[runtime.process]\nrebuild_environment=['TASK_REGION']\ncredential_environment=['TASK_TOKEN']\n")
	if err := os.WriteFile(filepath.Join(req.ProjectDir, config.DefaultConfigPath), data, 0600); err != nil {
		t.Fatal(err)
	}
	req.ConfigSHA256 = hostDigest(data)
	req.Environment = append(req.Environment, "TASK_REGION=local", "TASK_TOKEN=never-persist-this", "IRRELEVANT=before")
	inputs, err := captureHostContextInputs(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(inputs)
	if err != nil || strings.Contains(string(raw), "never-persist-this") || strings.Contains(string(raw), "CONTEXT_SECRET") || strings.Contains(string(raw), "IRRELEVANT") {
		t.Fatalf("context captured unnecessary or secret values: %s %v", raw, err)
	}
	env := []string{"HOME=/different/host", "PATH=" + os.Getenv("PATH"), "TASK_REGION=local", "TASK_TOKEN=refreshed", "IRRELEVANT=after"}
	rebuilt, err := inputs.rebuild(req.ProjectDir, env)
	if err != nil {
		t.Fatal(err)
	}
	e, _ := execution.New(req.ProjectDir, rebuilt)
	if home, _ := e.Lookup("HOME"); home != inputs.Home {
		t.Fatalf("replacement Host changed task home: %s", home)
	}
	if token, _ := e.Lookup("TASK_TOKEN"); token != "refreshed" {
		t.Fatal("credential reference did not read its current source")
	}
	for _, bad := range [][]string{{"TASK_REGION=local"}, {"TASK_REGION=changed", "TASK_TOKEN=current"}} {
		if _, err := inputs.rebuild(req.ProjectDir, bad); err == nil {
			t.Fatal("missing credential or changed required value accepted")
		}
	}
}

func TestRollingExitedProducerRetainsUnresolvedTaskAndQuota(t *testing.T) {
	root := t.TempDir()
	id := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	child := exec.Command("go", "version")
	if err := child.Run(); err != nil {
		t.Fatal(err)
	}
	owner := hostTaskOwner{State: "ready", RebuildError: "missing required source", Current: hostReceipt{HostInstanceID: id, RequestID: "original"}}
	if err := updateHostPool(root, func(p *hostPool) error {
		p.Generations[id] = hostGeneration{ID: id, Directory: filepath.Join(root, "g", id), State: "retiring", Process: procx.ProcessRecord{PID: child.Process.Pid, SessionID: id, StartedAt: time.Now()}}
		p.Owners["original"] = owner
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	budget := admission.NewBudget(4)
	if err := budget.BindSharedQuota(filepath.Join(root, "quota"), id); err != nil {
		t.Fatal(err)
	}
	budget.Register(id)
	observeKnownHostFixtureSamples(budget)
	permit, err := budget.TryAcquire(id)
	if err != nil {
		t.Fatal(err)
	}
	defer permit.Release()
	quota := filepath.Join(root, "quota", "quota.json")
	before, err := os.ReadFile(quota)
	if err != nil {
		t.Fatal(err)
	}
	h := &noodleHost{budget: budget, rolling: &hostRolling{root: root}}
	p, err := readHostPool(root)
	if err != nil {
		t.Fatal(err)
	}
	h.observeRetiredGenerations(&p)
	p, err = readHostPool(root)
	if err != nil {
		t.Fatal(err)
	}
	if p.Generations[id].State != "retiring" || p.Owners["original"].State != "ready" || p.Owners["original"].RebuildError != owner.RebuildError {
		t.Fatalf("exit readback lost unresolved task: %+v", p)
	}
	data, err := os.ReadFile(quota)
	if err != nil || string(data) != string(before) || budget.Snapshot().Allocations != 1 {
		t.Fatal("exit observation changed quota", err)
	}
	permit.Release()
	h.observeRetiredGenerations(&p)
	p, err = readHostPool(root)
	if err != nil {
		t.Fatal(err)
	}
	if p.Generations[id].State != "exited" || p.Owners["original"].RebuildError != owner.RebuildError {
		t.Fatal("physical exit lost task recovery state")
	}
}

func TestHostRebuildReleasedRoundPreservesBindingAndPause(t *testing.T) {
	root := t.TempDir()
	oldID, newID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	req := hostTestRequest(t, t.TempDir())
	if err := exec.Command("git", "init", req.ProjectDir).Run(); err != nil {
		t.Fatal(err)
	}
	inputs, err := captureHostContextInputs(req)
	if err != nil {
		t.Fatal(err)
	}
	original := hostReceipt{Schema: 1, Protocol: hostProtocol, RequestID: req.RequestID, ProjectDir: req.ProjectDir, ConfigSHA256: req.ConfigSHA256, Mode: req.Mode, BinarySHA256: "old", BinaryPath: "/old/noodle", HostInstanceID: oldID, ContextID: "old-context", ContextState: "active", HostDir: root}
	detached := original
	detached.ContextState, detached.TerminalCause = "detached", loop.TerminalMigrationDetach
	continuation := &loop.HostContinuation{Mode: "manual", DispatchState: loop.StatePaused}
	oldEnv := hostDigest([]byte(strings.Join(req.Environment, "\x00")))
	if err := updateHostPool(root, func(p *hostPool) error {
		p.Active = newID
		p.Generations[newID] = hostGeneration{ID: newID, Directory: filepath.Join(root, "g", newID), Binary: "/new/noodle", SHA: "new", State: "active"}
		p.Owners[req.RequestID] = hostTaskOwner{Original: original, Current: detached, State: "ready", EnvironmentSHA: oldEnv, ContextInputs: &inputs, RoundState: continuation}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(req.ProjectDir, ".noodle"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(req.ProjectDir, ".noodle/state.snapshot.json"), []byte("{\"state\":{\"orders\":{}}}"), 0600); err != nil {
		t.Fatal(err)
	}
	var runtimes []*fakeHostRuntime
	h := &noodleHost{dir: filepath.Join(root, "g", newID), instanceID: newID, binaryPath: "/new/noodle", binarySHA: "new", rolling: &hostRolling{root: root, carry: map[string]rollingCarry{}}}
	if err = os.MkdirAll(h.dir, 0700); err != nil {
		t.Fatal(err)
	}
	h.newRuntime = func(_ string, _ string, _ config.Config, deps loop.Dependencies) hostRuntime {
		if deps.HostContinuation == nil || *deps.HostContinuation != *continuation {
			t.Fatal("rebuild lost pause/manual continuation")
		}
		rt := &fakeHostRuntime{ready: make(chan struct{}), released: make(chan struct{})}
		runtimes = append(runtimes, rt)
		return rt
	}
	req.AdmissionSHA, req.RoundSource, req.Operation = "old", oldID, "round_rebuild"
	req.Environment = append(req.Environment, "UNRELATED_REPLACEMENT_VALUE=changed")
	// An active or unknown predecessor cannot be reconstructed.
	if err = updateHostPool(root, func(p *hostPool) error {
		o := p.Owners[req.RequestID]
		o.State = "owned"
		p.Owners[req.RequestID] = o
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := h.rebuildRollingContext(context.Background(), req); got.Status != "unknown_effect" || len(runtimes) != 0 {
		t.Fatalf("active predecessor replayed: %+v", got)
	}
	if err = updateHostPool(root, func(p *hostPool) error {
		o := p.Owners[req.RequestID]
		o.State = "ready"
		p.Owners[req.RequestID] = o
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, data := range []string{"{}", `{"state":{"orders":{"original":{"stages":[{"runtime":"codex-app-server","attempts":[{"session_id":"missing-native-history"}]}]}}}}`} {
		if err = os.WriteFile(filepath.Join(req.ProjectDir, ".noodle/state.snapshot.json"), []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		refused := h.rebuildRollingContext(context.Background(), req)
		if refused.Status == "ok" || len(runtimes) != 0 {
			t.Fatalf("missing original snapshot or native custody was replaced: %+v", refused)
		}
	}
	if err = os.WriteFile(filepath.Join(req.ProjectDir, ".noodle/state.snapshot.json"), []byte(`{"state":{"orders":{}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	history := detached
	history.ContextID, history.BinarySHA256, history.HostInstanceID = "history-a", "new", newID
	h.receipts = append(h.receipts, history)
	if err = updateHostPool(root, func(p *hostPool) error {
		o := p.Owners[req.RequestID]
		o.Previous = []hostReceipt{history}
		p.Owners[req.RequestID] = o
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err = updateHostPool(root, func(p *hostPool) error {
		o := p.Owners[req.RequestID]
		o.State = "admitting"
		x := hostError("unknown_effect", fmt.Errorf("lost admission reply"))
		o.LastAdmission = &x
		p.Owners[req.RequestID] = o
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := h.rebuildRollingContext(context.Background(), req); got.Status != "unknown_effect" || len(runtimes) != 0 {
		t.Fatalf("unknown admission replayed: %+v", got)
	}
	if err = updateHostPool(root, func(p *hostPool) error {
		o := p.Owners[req.RequestID]
		x := hostError("refused", fmt.Errorf("known pre-context source refusal"))
		o.LastAdmission = &x
		p.Owners[req.RequestID] = o
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got := h.rebuildRollingContext(context.Background(), req)
	if got.Status != "ok" || got.ContextID == "old-context" || got.HostInstanceID != newID || len(runtimes) != 1 {
		t.Fatalf("rebuild: %+v", got)
	}
	lookup := req
	lookup.Operation, lookup.ContextID, lookup.InstanceID, lookup.BinarySHA256 = "status", got.ContextID, newID, "new"
	for _, contextID := range []string{got.ContextID, history.ContextID} {
		lookup.ContextID = contextID
		if result := h.handle(context.Background(), lookup); result.Status != "ok" || result.ContextID != contextID {
			t.Fatalf("bound context readback: %+v", result)
		}
	}
	lookup.ContextID = got.ContextID
	for _, change := range []func(*hostRequest){
		func(r *hostRequest) { r.RequestID = "foreign-task" },
		func(r *hostRequest) { r.ConfigSHA256 = "foreign-config" },
		func(r *hostRequest) { r.ConfigPath = "/foreign/config.toml" },
		func(r *hostRequest) { r.ContextID = "foreign-context" },
		func(r *hostRequest) { r.InstanceID = oldID },
	} {
		foreign := lookup
		change(&foreign)
		if result := h.handle(context.Background(), foreign); result.Status == "ok" {
			t.Fatalf("foreign binding accepted: %+v", result)
		}
	}
	defer func() {
		if r := h.handle(context.Background(), hostRequest{Operation: "detach", ProjectDir: req.ProjectDir, ContextID: got.ContextID, InstanceID: newID, BinarySHA256: "new"}); r.Status != "ok" {
			t.Errorf("cleanup: %+v", r)
		}
	}()
	repeated := h.rebuildRollingContext(context.Background(), req)
	if repeated.Status != "ok" || repeated.ContextID != got.ContextID || len(runtimes) != 1 {
		t.Fatalf("duplicate rebuild: %+v", repeated)
	}
	p, err := readHostPool(root)
	if err != nil {
		t.Fatal(err)
	}
	o := p.Owners[req.RequestID]
	if o.EnvironmentSHA != oldEnv || o.Original != original || len(o.Previous) != 2 || o.Previous[1] != detached || o.Previous[0] != history || o.RuntimeEnvironmentSHA == oldEnv {
		t.Fatalf("original binding or successor history changed: %+v", o)
	}
}

func TestHostContextInputsRejectChangedToolAndMissingNativeCredential(t *testing.T) {
	root := t.TempDir()
	tool := filepath.Join(root, "codex")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	req := hostTestRequest(t, t.TempDir())
	quoted, _ := json.Marshal(root)
	data := []byte("[server]\nenabled=false\n[agents.codex]\npath=" + string(quoted) + "\n[runtime]\ndefault='codex-app-server'\n")
	if err := os.WriteFile(filepath.Join(req.ProjectDir, config.DefaultConfigPath), data, 0600); err != nil {
		t.Fatal(err)
	}
	req.ConfigSHA256 = hostDigest(data)
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, "auth.json"), []byte("{\"token\":\"never-copy-auth\"}"), 0600); err != nil {
		t.Fatal(err)
	}
	req.Environment = append(req.Environment, "CODEX_HOME="+home)
	inputs, err := captureHostContextInputs(req)
	if err != nil {
		t.Fatal(err)
	}
	if inputs.Tools[tool] != tool || inputs.NativeHome != home || !inputs.NativeAuth {
		t.Fatalf("native sources: %+v", inputs)
	}
	if _, err = inputs.rebuild(req.ProjectDir, req.Environment); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(home, "auth.json")); err != nil {
		t.Fatal(err)
	}
	if _, err = inputs.rebuild(req.ProjectDir, req.Environment); err == nil {
		t.Fatal("missing original credential source accepted")
	}
	inputs.NativeAuth = false
	if err = os.Remove(tool); err != nil {
		t.Fatal(err)
	}
	if _, err = inputs.rebuild(req.ProjectDir, req.Environment); err == nil {
		t.Fatal("missing tool accepted")
	}
}

func TestRollingRebuildAfterProducerExitBlocksOnlyMissingTask(t *testing.T) {
	poolRoot := t.TempDir()
	oldID, newID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	child := exec.Command("go", "version")
	if err := child.Run(); err != nil {
		t.Fatal(err)
	}
	h := &noodleHost{dir: filepath.Join(poolRoot, "g", newID), instanceID: newID, binaryPath: "/new/noodle", binarySHA: "new", rolling: &hostRolling{root: poolRoot, carry: map[string]rollingCarry{}}}
	if err := os.MkdirAll(h.dir, 0700); err != nil {
		t.Fatal(err)
	}
	var runtimes []*fakeHostRuntime
	h.newRuntime = func(_ string, _ string, _ config.Config, _ loop.Dependencies) hostRuntime {
		r := &fakeHostRuntime{ready: make(chan struct{}), released: make(chan struct{})}
		runtimes = append(runtimes, r)
		return r
	}
	good := hostTestRequest(t, t.TempDir())
	bad := hostTestRequest(t, t.TempDir())
	for _, req := range []hostRequest{good, bad} {
		if err := exec.Command("git", "init", req.ProjectDir).Run(); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(req.ProjectDir, ".noodle"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(req.ProjectDir, ".noodle/state.snapshot.json"), []byte(`{"state":{"orders":{}}}`), 0600); err != nil {
			t.Fatal(err)
		}
		inputs, err := captureHostContextInputs(req)
		if err != nil {
			t.Fatal(err)
		}
		if req.RequestID == bad.RequestID {
			inputs.CredentialEnvironment = []string{"NOODLE_TEST_MISSING_CONTEXT_CREDENTIAL"}
		}
		r := hostReceipt{RequestID: req.RequestID, ProjectDir: req.ProjectDir, ConfigSHA256: req.ConfigSHA256, Mode: req.Mode, BinarySHA256: "old", BinaryPath: "/old/noodle", HostInstanceID: oldID, ContextID: req.RequestID, ContextState: "detached", TerminalCause: loop.TerminalMigrationDetach, HostDir: poolRoot}
		if err := updateHostPool(poolRoot, func(p *hostPool) error {
			p.Active = newID
			p.Generations[oldID] = hostGeneration{ID: oldID, Directory: filepath.Join(poolRoot, "g", oldID), State: "retiring", Process: procx.ProcessRecord{PID: child.Process.Pid, SessionID: oldID, StartedAt: time.Now()}}
			p.Generations[newID] = hostGeneration{ID: newID, Directory: h.dir, Binary: h.binaryPath, SHA: h.binarySHA, State: "active"}
			p.Owners[req.RequestID] = hostTaskOwner{Original: r, Current: r, State: "ready", ContextInputs: &inputs, RoundState: &loop.HostContinuation{Mode: "manual", DispatchState: loop.StatePaused}}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	p, err := readHostPool(poolRoot)
	if err != nil {
		t.Fatal(err)
	}
	h.rebuildReleasedRounds(context.Background(), p)
	p, err = readHostPool(poolRoot)
	if err != nil {
		t.Fatal(err)
	}
	if p.Owners[good.RequestID].State != "owned" || p.Owners[bad.RequestID].State != "ready" || p.Owners[bad.RequestID].RebuildError == "" || len(runtimes) != 1 || h.rolling.failure != "" {
		t.Fatalf("one task's missing source blocked another: %+v", p.Owners)
	}
	h.rebuildReleasedRounds(context.Background(), p)
	if len(runtimes) != 1 {
		t.Fatal("unchanged readback created a second runtime")
	}
	current := p.Owners[good.RequestID].Current
	if got := h.handle(context.Background(), hostRequest{Operation: "detach", ProjectDir: good.ProjectDir, ContextID: current.ContextID, InstanceID: newID, BinarySHA256: h.binarySHA}); got.Status != "ok" {
		t.Fatalf("cleanup: %+v", got)
	}
}

func TestSelectedProfileRequiresOriginalThreadAndWorktreeHistory(t *testing.T) {
	home := t.TempDir()
	thread := "11111111-1111-4111-8111-111111111111"
	logical := "/original/task/worktree"
	path := filepath.Join(home, "sessions", "2026", "10", "27", "rollout-"+thread+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	line := []byte(`{"type":"session_meta","payload":{"id":"` + thread + `","cwd":"` + logical + `"}}` + "\n")
	if err := os.WriteFile(path, line, 0600); err != nil {
		t.Fatal(err)
	}
	source, err := findHostThreadProfileSource(context.Background(), home, thread, logical)
	if err != nil || source.Thread != thread || source.Path != path {
		t.Fatal(source, err)
	}
	for _, cwd := range []string{"/foreign/worktree", ""} {
		if _, err = findHostThreadProfileSource(context.Background(), home, thread, cwd); err == nil {
			t.Fatal("foreign worktree history accepted")
		}
	}
	other := filepath.Join(home, "archived_sessions", "copy-"+thread+".jsonl")
	if err = os.MkdirAll(filepath.Dir(other), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(other, line, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = findHostThreadProfileSource(context.Background(), home, thread, logical); err == nil {
		t.Fatal("ambiguous profile history accepted")
	}
}

func TestHostReviewControllerCanReleaseOnSameGenerationWithoutDrainingHost(t *testing.T) {
	root, project := t.TempDir(), t.TempDir()
	oldID, id := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	req := hostTestRequest(t, project)
	if err := exec.Command("git", "init", project).Run(); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(project, ".noodle"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(project, ".noodle/state.snapshot.json"), []byte(`{"state":{"orders":{},"pending_reviews":{"original":{}}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	r := hostReceipt{Schema: 1, Protocol: hostProtocol, RequestID: req.RequestID, ProjectDir: project, ConfigSHA256: req.ConfigSHA256, Mode: req.Mode, BinarySHA256: "old", BinaryPath: "/old/noodle", HostInstanceID: oldID, ContextID: "old-context", ContextState: "detached", TerminalCause: loop.TerminalMigrationDetach, HostDir: root}
	if err := updateHostPool(root, func(p *hostPool) error {
		p.Active = id
		p.Generations[id] = hostGeneration{ID: id, Directory: filepath.Join(root, "g", id), Binary: "/new/noodle", SHA: "new", State: "active"}
		p.Owners[req.RequestID] = hostTaskOwner{Original: r, Current: r, State: "ready", EnvironmentSHA: "immutable-original", RebuildError: "original profile missing", RoundState: &loop.HostContinuation{Mode: "supervised", DispatchState: loop.StatePaused}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var rt *rollingBusyRuntime
	h := &noodleHost{dir: filepath.Join(root, "g", id), instanceID: id, binaryPath: "/new/noodle", binarySHA: "new", rolling: &hostRolling{root: root, carry: map[string]rollingCarry{}}}
	if err := os.MkdirAll(h.dir, 0700); err != nil {
		t.Fatal(err)
	}
	h.newRuntime = func(_ string, _ string, _ config.Config, deps loop.Dependencies) hostRuntime {
		if deps.DispatchBlocked == "" || deps.HostContinuation.Mode != "manual" || deps.HostContinuation.DispatchState != loop.StatePaused {
			t.Fatal("controller can dispatch or lost pause")
		}
		rt = &rollingBusyRuntime{fakeHostRuntime{ready: make(chan struct{}), released: make(chan struct{})}}
		return rt
	}
	req.AdmissionSHA, req.RoundSource, req.Operation = "old", oldID, "round_rebuild"
	got := h.rebuildRollingContext(context.Background(), req)
	if got.Status != "ok" || rt == nil {
		t.Fatalf("controller restore: %+v", got)
	}
	p, _ := readHostPool(root)
	owner := p.Owners[req.RequestID]
	if !owner.ControllerOnly || owner.ContextInputs != nil || owner.EnvironmentSHA != "immutable-original" {
		t.Fatalf("controller fabricated worker inputs: %+v", owner)
	}
	// An ordinary same-generation release is forbidden. Explicit profile
	// adoption may release only the fenced controller, without draining peers.
	if got := h.prepareRollingRebuild(context.Background(), req); got.Status != "refused" {
		t.Fatalf("ordinary release: %+v", got)
	}
	req.AdoptCurrentProfile = true
	got = h.prepareRollingRebuild(context.Background(), req)
	if got.Status != "ok" || h.rolling.retiring || len(h.rolling.carry) != 0 || len(h.contexts) != 0 {
		t.Fatalf("profile release drained shared host: %+v", got)
	}
	p, _ = readHostPool(root)
	if p.Owners[req.RequestID].State != "ready" || p.Owners[req.RequestID].Current.TerminalCause != loop.TerminalMigrationDetach {
		t.Fatal("same-generation boundary not persisted")
	}
}

func TestHostRebuildKnownRefusalNeverReplaysNewOrFailedReceipt(t *testing.T) {
	for _, contextState := range []string{"active", "failed"} {
		t.Run(contextState, func(t *testing.T) {
			root, id := t.TempDir(), "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
			req := hostTestRequest(t, t.TempDir())
			req.AdmissionSHA = "old"
			original := hostReceipt{Schema: 1, Protocol: hostProtocol, RequestID: req.RequestID, ProjectDir: req.ProjectDir, ConfigSHA256: req.ConfigSHA256, Mode: req.Mode, BinarySHA256: "old", HostInstanceID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ContextID: "original", ContextState: "detached", TerminalCause: loop.TerminalMigrationDetach}
			req.RoundSource = original.HostInstanceID
			rejected := hostError("refused", fmt.Errorf("known source refusal"))
			if err := updateHostPool(root, func(p *hostPool) error {
				p.Active = id
				p.Generations[id] = hostGeneration{ID: id, Directory: filepath.Join(root, "g", id), SHA: "new"}
				p.Owners[req.RequestID] = hostTaskOwner{Original: original, Current: original, State: "admitting", LastAdmission: &rejected}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			receipt := original
			receipt.ContextID = "new-c"
			receipt.HostInstanceID = id
			receipt.BinarySHA256 = "new"
			receipt.ContextState = contextState
			receipt.TerminalCause = ""
			h := &noodleHost{instanceID: id, binarySHA: "new", rolling: &hostRolling{root: root}, receipts: []hostReceipt{receipt}}
			h.newRuntime = func(string, string, config.Config, loop.Dependencies) hostRuntime {
				t.Fatal("new receipt replayed")
				return nil
			}
			got := h.rebuildRollingContext(context.Background(), req)
			if contextState == "active" && (got.Status != "ok" || got.ContextID != "new-c") {
				t.Fatalf("original new acceptance not read back: %+v", got)
			}
			if contextState == "failed" && got.Status != "unknown_effect" {
				t.Fatalf("failed acceptance reset: %+v", got)
			}
			p, _ := readHostPool(root)
			if p.Owners[req.RequestID].State == "ready" {
				t.Fatal("new/failed receipt erased")
			}
		})
	}
}
