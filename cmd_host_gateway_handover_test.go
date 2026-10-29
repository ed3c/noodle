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

func TestGatewayHandoverSharedPolicyRequiresExplicitFields(t *testing.T) {
	for _, fault := range []string{"none", "shutdown-missing", "requested-missing", "wrong-quota", "wrong-custody", "binary-changed", "schema2-supported", "schema2-missing", "schema3-supported", "schema3-missing"} {
		t.Run(fault, func(t *testing.T) {
			root, err := os.MkdirTemp("/tmp", "ngh-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(root)
			binary := filepath.Join(root, "capabilities")
			version := "4"
			if fault == "wrong-custody" {
				version = "3"
			}
			release := ""
			if fault == "schema2-supported" {
				release = ",\"gateway_custody_release\":2"
			}
			if fault == "schema3-supported" {
				release = ",\"gateway_custody_release\":3"
			}
			script := []byte("#!/bin/sh\n[ \"$1\" = host ] && [ \"$2\" = capabilities ] || exit 2\nprintf '%s\\n' '{\"openshell_custody_version\":" + version + ",\"gateway_host_handover\":1" + release + "}'\n")
			if err = os.WriteFile(binary, script, 0700); err != nil {
				t.Fatal(err)
			}
			sha := hostDigest(script)
			if fault == "binary-changed" {
				sha = strings.Repeat("0", 64)
			}
			shutdown := map[string]any{"requested": false}
			maximum := 4
			if fault == "wrong-quota" {
				maximum = 5
			}
			row := map[string]any{"protocol": hostProtocol, "host_root": root, "host_instance_id": strings.Repeat("a", 32), "rolling_host_version": 1, "binary_path": binary, "binary_sha256": sha, "shutdown": shutdown, "resources": map[string]any{"worker_admission_policy": "swap-used-critical-guard-v2", "swap_used_limit_bytes": 15000000000, "shared_quota": map[string]any{"maximum": maximum}}}
			if fault == "shutdown-missing" {
				delete(row, "shutdown")
			}
			if fault == "requested-missing" {
				delete(shutdown, "requested")
			}
			listener, err := net.Listen("unix", filepath.Join(root, "host.sock"))
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			done := make(chan struct{})
			go func() {
				defer close(done)
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				var req hostRequest
				if json.NewDecoder(conn).Decode(&req) != nil {
					return
				}
				if req.Operation != "resources" {
					t.Error("readback sent control", req.Operation)
				}
				_ = json.NewEncoder(conn).Encode(row)
			}()
			schema := 1
			if strings.HasPrefix(fault, "schema2-") {
				schema = 2
			}
			if strings.HasPrefix(fault, "schema3-") {
				schema = 3
			}
			instance, err := handoverSharedReadback(context.Background(), root, schema)
			<-done
			if fault == "none" || fault == "schema2-supported" || fault == "schema3-supported" {
				if err != nil || instance != strings.Repeat("a", 32) {
					t.Fatal(instance, err)
				}
			} else if err == nil {
				t.Fatal("incompatible shared owner accepted", fault)
			}
		})
	}
}

func TestGatewayHandoverCannotApplyThroughSwitchFlags(t *testing.T) {
	for _, args := range [][]string{{"--handover-root", "/existing/shared"}, {"--handover-root", "/existing/shared", "--initial-maintenance"}, {"--handover-root", "/existing/shared", "--status"}} {
		cmd := newHostRollingCmd()
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Fatal("handover bypassed the original read-only plan", args)
		}
	}
}

func TestGatewayHandoverRejectsSameRootWithoutEffects(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "preserved")
	before := []byte("original bytes")
	if err = os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = planGatewayHandover(context.Background(), root, root, "/never-executed", "unknown", nil); err == nil {
		t.Fatal("same root accepted")
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("read-only refusal changed source", err)
	}
	files, err := os.ReadDir(root)
	if err != nil || len(files) != 1 {
		t.Fatal("refusal created runtime state", err)
	}
}

func TestBlockedGatewayHandoverCannotBecomeExecutable(t *testing.T) {
	p := hostMaintenancePlan{Schema: 1, Root: "/source", Handover: &hostGatewayHandoverPlan{SharedRoot: "/shared"}, Blockers: []string{"original work remains"}}
	p, err := sealGatewayHandoverPlan(p)
	if err != nil {
		t.Fatal(err)
	}
	if p.Executable || validateRestrictedPlan(p) == nil {
		t.Fatal("blocked plan accepted")
	}
	p.Blockers = nil
	p.Executable = true
	if validateRestrictedPlan(p) == nil {
		t.Fatal("modified plan accepted")
	}
}

func TestGatewayHandoverJournalReadsItsOwnContinuation(t *testing.T) {
	sha := strings.Repeat("a", 64)
	ref := loopruntime.SessionReadbackFileRef{Path: "/fixture/source", SHA256: sha}
	native := &procx.NativeIdentity{PID: 1, UID: os.Getuid(), StartSec: 1, Version: 1, Token: []byte("fixture")}
	p, err := sealGatewayHandoverPlan(hostMaintenancePlan{Schema: 1, Root: "/fixture/private", Target: "/fixture/target", TargetSHA: sha, Policy: admission.WorkerAdmissionPolicy,
		Native: native, Source: legacyHostResources{Instance: strings.Repeat("b", 32), PID: 1, Contexts: 1},
		Contexts: []hostMaintenanceContextPlan{{RequestFile: "/fixture/request"}},
		Handover: &hostGatewayHandoverPlan{SharedRoot: "/fixture/shared", SharedInstance: strings.Repeat("c", 32), SourceBinary: ref, Profile: ref, Custody: ref}})
	if err != nil || validateRestrictedPlan(p) != nil {
		t.Fatal("invalid fixture plan", err)
	}
	for _, phase := range []string{"handover-prepared", "handover-fencing", "handover-cleanup", "handover-releasing", "handover-stopping", "handover-stopped", "handover-completed", "holding", "handover-unknown"} {
		for _, taskPhase := range []string{"prepared", "cleanup-intent", "detached", "pausing"} {
			t.Run(phase+"/"+taskPhase, func(t *testing.T) {
				dir := t.TempDir()
				j := restrictedMaintenance{Schema: 1, Plan: p, Phase: phase, Tasks: []restrictedTask{{Plan: p.Contexts[0], Phase: taskPhase}}}
				if err := saveRestrictedMaintenance(dir, j); err != nil {
					t.Fatal(err)
				}
				before, err := os.ReadFile(filepath.Join(dir, "journal.json"))
				if err != nil {
					t.Fatal(err)
				}
				_, err = readRestrictedMaintenance(dir)
				allowed := phase != "holding" && phase != "handover-unknown" && taskPhase != "pausing"
				if (err == nil) != allowed {
					t.Fatal("wrong continuation boundary", err)
				}
				after, err := os.ReadFile(filepath.Join(dir, "journal.json"))
				if err != nil || !bytes.Equal(before, after) {
					t.Fatal("readback changed the original journal", err)
				}
			})
		}
	}
}
