package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/procx"
	loopruntime "github.com/poteto/noodle/runtime"
)

func TestSourceServiceTargetRequiresDedicatedRoleBeforeEffects(t *testing.T) {
	for _, version := range []string{"{}", `{"rolling_host_version":1}`, `{"source_service_restore":1}`} {
		path := filepath.Join(t.TempDir(), "candidate")
		if err := os.WriteFile(path, []byte("#!/bin/sh\nprintf '%s' '"+version+"'\n"), 0700); err != nil {
			t.Fatal(err)
		}
		ref, err := hostReadbackBinaryRef(path)
		if err != nil {
			t.Fatal(err)
		}
		err = verifySourceServiceTarget(context.Background(), ref)
		if (version == `{"source_service_restore":1}`) != (err == nil) {
			t.Fatal("Source target capability boundary failed", version, err)
		}
		ref.SHA256 = "another-binary"
		if err := verifySourceServiceTarget(context.Background(), ref); err == nil {
			t.Fatal("changed Source target accepted")
		}
	}
}

func TestSourceServiceNeverInheritsHistoricalFixtureRecovery(t *testing.T) {
	root := t.TempDir()
	generation := filepath.Join(root, "g", "source-service")
	if err := os.MkdirAll(generation, 0700); err != nil {
		t.Fatal(err)
	}
	residents, err := admission.OpenResidents(generation)
	if err != nil {
		t.Fatal(err)
	}
	h := &noodleHost{dir: generation, residents: residents, rolling: &hostRolling{root: root}, sourceService: &loopruntime.SessionReadbackFileRef{Path: "original-retired-phase"}}
	h.reconcileFixtureLifecycle(t.Context())
	if _, err := os.Stat(filepath.Join(root, "fixture-recovery-claims")); !os.IsNotExist(err) {
		t.Fatal("Source role entered historical fixture ownership", err)
	}
}

func TestSourceServiceRefusesNativeExecutableDriftAfterRetirement(t *testing.T) {
	for _, changed := range []string{"gateway", "driver"} {
		t.Run(changed, func(t *testing.T) {
			scope := &hostOperatorNativeScope{}
			for name, actor := range map[string]*hostOperatorActor{"gateway": &scope.Gateway, "driver": &scope.Driver} {
				path := filepath.Join(t.TempDir(), name)
				if err := os.WriteFile(path, []byte("original executable bytes"), 0700); err != nil {
					t.Fatal(err)
				}
				ref, err := hostReadbackBinaryRef(path)
				if err != nil {
					t.Fatal(err)
				}
				actor.Binary = ref
			}
			if err := verifySourceServiceBinaries(scope); err != nil {
				t.Fatal(err)
			}
			actor := scope.Gateway
			if changed == "driver" {
				actor = scope.Driver
			}
			if err := os.WriteFile(actor.Binary.Path, []byte("same path, changed executable"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := verifySourceServiceBinaries(scope); err == nil {
				t.Fatal("unbound native executable accepted")
			}
		})
	}
}

func TestSourceServicePreflightKeepsOtherQuotaWritersOutsideRetirementScope(t *testing.T) {
	p, _ := sourcePolicyTestPlan(t)
	pool, err := readHostPool(p.Root)
	if err != nil {
		t.Fatal(err)
	}
	g := pool.Generations[p.Source.Instance]
	g.Process.PID = p.Source.PID
	pool.Generations[g.ID] = g
	path := filepath.Join(p.Root, "pool.json")
	if err := os.WriteFile(path, sourcePolicyTestJSON(t, pool), 0600); err != nil {
		t.Fatal(err)
	}
	if err := sourceServiceQuotaWritersRetired(p, true); err != nil {
		t.Fatal("selected Source could not remain live during preflight", err)
	}
	id := strings.Repeat("c", 32)
	other := g
	other.Process.PID = os.Getpid()
	other.ID = id
	other.Directory = filepath.Join(p.Root, "g", id)
	pool.Generations[id] = other
	if err := os.WriteFile(path, sourcePolicyTestJSON(t, pool), 0600); err != nil {
		t.Fatal(err)
	}
	if err := sourceServiceQuotaWritersRetired(p, true); err == nil {
		t.Fatal("another live or unknown original writer passed preflight")
	}
}

func sourcePublicationFixture(t *testing.T) (restrictedMaintenance, hostPool) {
	t.Helper()
	p, _ := sourcePolicyTestPlan(t)
	dir := filepath.Join(p.Root, "maintenance", p.PlanSHA)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	ref, err := saveSourceServiceObservation(dir, "source-retired-phase.json", map[string]any{"fixture": "original-phase"})
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("b", 32)
	g := hostGeneration{ID: id, Directory: filepath.Join(p.Root, "g", id), Binary: p.Target, SHA: p.TargetSHA, Process: procx.ProcessRecord{PID: 12345}, SourceService: &ref, State: "starting"}
	if err := os.MkdirAll(g.Directory, 0700); err != nil {
		t.Fatal(err)
	}
	pool, err := readHostPool(p.Root)
	if err != nil {
		t.Fatal(err)
	}
	pool.Active = ""
	pool.Generations[id] = g
	for id, owner := range pool.Owners {
		owner.MaintenanceHold = p.PlanSHA
		pool.Owners[id] = owner
	}
	if err := os.WriteFile(filepath.Join(p.Root, "pool.json"), sourcePolicyTestJSON(t, pool), 0600); err != nil {
		t.Fatal(err)
	}
	if err := saveHostBootstrap(p.Root, hostBootstrap{Schema: 1, Phase: "source-policy-" + p.PlanSHA, Source: pool.Generations[p.Source.Instance], Target: p.Target, TargetSHA: p.TargetSHA, ProcessIdentity: "fixture"}); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(g.Directory, "host.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			func() {
				defer conn.Close()
				var req hostRequest
				if json.NewDecoder(conn).Decode(&req) != nil {
					return
				}
				_ = json.NewEncoder(conn).Encode(map[string]any{"status": "healthy", "host_instance_id": g.ID, "host_pid": g.Process.PID, "binary_sha256": g.SHA})
			}()
		}
	}()
	return restrictedMaintenance{Plan: p, Phase: "source-service-launch-intent", Generation: &g}, pool
}

func TestSourceServicePublicationRejectsPoolProofDriftWithoutEffects(t *testing.T) {
	j, pool := sourcePublicationFixture(t)
	g := pool.Generations[j.Generation.ID]
	changed := *g.SourceService
	changed.SHA256 = "another-proof"
	g.SourceService = &changed
	pool.Generations[g.ID] = g
	path := filepath.Join(j.Plan.Root, "pool.json")
	if err := os.WriteFile(path, sourcePolicyTestJSON(t, pool), 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(j.Plan.Root, "bootstrap.json")
	beforeMarker, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if err := publishSourceServiceGeneration(t.Context(), j); err == nil {
		t.Fatal("different Source proof published")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("proof refusal changed pool", err)
	}
	after, err = os.ReadFile(marker)
	if err != nil || !bytes.Equal(beforeMarker, after) {
		t.Fatal("proof refusal changed public marker", err)
	}
}

func TestSourceServiceUnknownLaunchReadsOnlyExactOriginalGeneration(t *testing.T) {
	j, pool := sourcePublicationFixture(t)
	g, err := readOriginalSourceServiceLaunch(j.Plan)
	if err != nil || g.ID != j.Generation.ID || g.Process.PID != j.Generation.Process.PID {
		t.Fatal("original launch was not read back", g.ID, err)
	}
	id := strings.Repeat("c", 32)
	duplicate := g
	duplicate.ID = id
	duplicate.Directory = filepath.Join(j.Plan.Root, "g", id)
	pool.Generations[id] = duplicate
	path := filepath.Join(j.Plan.Root, "pool.json")
	if err := os.WriteFile(path, sourcePolicyTestJSON(t, pool), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readOriginalSourceServiceLaunch(j.Plan); err == nil {
		t.Fatal("ambiguous original Source launch accepted")
	}
	delete(pool.Generations, id)
	g.SourceService = &loopruntime.SessionReadbackFileRef{Path: "/other", SHA256: "other"}
	pool.Generations[g.ID] = g
	if err := os.WriteFile(path, sourcePolicyTestJSON(t, pool), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readOriginalSourceServiceLaunch(j.Plan); err == nil {
		t.Fatal("unbound Source launch was adopted")
	}
}

func TestSourceServicePoolRetirementPreservesEveryOpaqueBoundary(t *testing.T) {
	p, _ := sourcePolicyTestPlan(t)
	raw, err := os.ReadFile(filepath.Join(p.Root, "pool.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	var generations map[string]map[string]json.RawMessage
	if err := json.Unmarshal(doc["generations"], &generations); err != nil {
		t.Fatal(err)
	}
	generation := generations[p.Source.Instance]
	generation["future_generation"] = json.RawMessage(`{"keep":true}`)
	generation["process"] = sourcePolicyTestJSON(t, map[string]any{"pid": p.Source.PID})
	doc["generations"] = sourcePolicyTestJSON(t, generations)
	var queue []map[string]json.RawMessage
	if err := json.Unmarshal(doc["queue"], &queue); err != nil {
		t.Fatal(err)
	}
	queue[0]["future_queue"] = json.RawMessage(`{"keep":true}`)
	doc["queue"] = sourcePolicyTestJSON(t, queue)
	if err := os.WriteFile(filepath.Join(p.Root, "pool.json"), sourcePolicyTestJSON(t, doc), 0600); err != nil {
		t.Fatal(err)
	}
	if err := updateHostPool(p.Root, func(pool *hostPool) error {
		for id, owner := range pool.Owners {
			owner.MaintenanceHold = p.PlanSHA
			pool.Owners[id] = owner
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := retireSourceServicePool(p); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(filepath.Join(p.Root, "pool.json"))
	if err != nil {
		t.Fatal(err)
	}
	var actual map[string]json.RawMessage
	if err := json.Unmarshal(saved, &actual); err != nil {
		t.Fatal(err)
	}
	compact := func(raw json.RawMessage) string {
		var b bytes.Buffer
		if err := json.Compact(&b, raw); err != nil {
			t.Fatal(err)
		}
		return b.String()
	}
	if compact(actual["future_pool_member"]) != `{"keep":true}` {
		t.Fatal("opaque pool lost")
	}
	var owners map[string]map[string]json.RawMessage
	if err := json.Unmarshal(actual["owners"], &owners); err != nil {
		t.Fatal(err)
	}
	for _, owner := range owners {
		if compact(owner["future_owner_member"]) != `["keep",true]` || string(owner["state"]) != `"unknown"` {
			t.Fatal("held unknown owner changed")
		}
	}
	if err := json.Unmarshal(actual["generations"], &generations); err != nil {
		t.Fatal(err)
	}
	if compact(generations[p.Source.Instance]["future_generation"]) != `{"keep":true}` || string(generations[p.Source.Instance]["state"]) != `"exited"` {
		t.Fatal("generation provenance lost")
	}
	if err := json.Unmarshal(actual["queue"], &queue); err != nil {
		t.Fatal(err)
	}
	if len(queue) != 1 || compact(queue[0]["future_queue"]) != `{"keep":true}` || string(queue[0]["state"]) != `"queued"` {
		t.Fatal("old queue changed")
	}
}
