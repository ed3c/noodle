package main

import (
	"context"
	"encoding/json"
	"github.com/poteto/noodle/internal/procx"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/poteto/noodle/internal/admission"
	loopruntime "github.com/poteto/noodle/runtime"
)

func TestSessionObserverRequiresOriginalZeroWorkerFrame(t *testing.T) {
	profile := hostDeferralProfile(t)
	spec, err := loopruntime.LoadOpenShellGatewaySpec(profile)
	if err != nil {
		t.Fatal(err)
	}
	guard, err := loopruntime.OpenShellGatewayGuardID(spec)
	if err != nil {
		t.Fatal(err)
	}
	owner := &hostOwnerBinding{HostRoot: "/original", GenerationDir: "/original/g/generation", ExecutionBinary: loopruntime.SessionReadbackFileRef{Path: "/original/noodle", SHA256: "original-sha"}, Current: hostReceipt{Schema: 1, Protocol: hostProtocol, HostPID: 123, HostInstanceID: "generation", ContextID: "context", RequestID: "request", ProjectDir: "/original/project", ConfigSHA256: "config", ContextState: "active", HostDir: "/original", BinaryPath: "/original/noodle", BinarySHA256: "original-sha"}}
	resident := admission.Resident{ID: spec.Resident.Name, Name: spec.Resident.Name, Sharing: "shared", State: "running", PID: 456, ProcessGroup: 456, Started: "original start", Fingerprint: "original fingerprint", GuardID: guard, Contexts: []string{"context"}}
	for _, fault := range []string{"", "pressure-only", "resident-history", "host", "root", "binary", "context", "status", "active", "missing-active", "unknown-quota", "missing-quota", "unresolved", "resident-error", "missing-persist-result", "guard", "lease", "fingerprint", "resident-unknown", "shutting-down"} {
		t.Run(fault, func(t *testing.T) {
			status := hostResponse{hostReceipt: owner.Current, Status: "ok"}
			budget := map[string]any{"active_workers": 0, "unresolved_residents": 0, "shared_quota": map[string]any{"allocations": 1, "residents": 1, "error": ""}}
			row := resident
			residents := map[string]any{"records": []admission.Resident{row}, "error": nil}
			resource := map[string]any{"protocol": hostProtocol, "host_root": owner.HostRoot, "host_dir": owner.GenerationDir, "host_pid": 123, "host_instance_id": "generation", "binary_path": owner.ExecutionBinary.Path, "binary_sha256": owner.ExecutionBinary.SHA256, "shutdown": map[string]any{"requested": false}, "resources": budget, "residents": residents}
			switch fault {
			case "pressure-only":
				budget["maximum_workers"] = 4
				budget["pressure"] = "critical"
				budget["swap_used_bytes"] = 99e9
				resource["admission"] = map[string]any{"can_accept": false}
			case "resident-history":
				row.Error = "original historical release refusal"
				row.Errors = []string{row.Error}
			case "host":
				resource["host_pid"] = 124
			case "root":
				resource["host_root"] = "foreign"
			case "binary":
				resource["binary_sha256"] = "foreign"
			case "context":
				status.ContextID = "foreign"
			case "status":
				status.Status = "unknown_effect"
			case "active":
				budget["active_workers"] = 1
			case "missing-active":
				delete(budget, "active_workers")
			case "unknown-quota":
				budget["shared_quota"] = map[string]any{"allocations": 1, "residents": 1, "error": "original launch unknown"}
			case "missing-quota":
				delete(budget, "shared_quota")
			case "unresolved":
				budget["unresolved_residents"] = 1
			case "resident-error":
				residents["error"] = "original persistence unknown"
			case "missing-persist-result":
				delete(residents, "error")
			case "guard":
				row.GuardID = "foreign"
			case "lease":
				row.Contexts = nil
			case "fingerprint":
				row.Fingerprint = ""
			case "resident-unknown":
				row.State = "needs_owner_readback"
			case "shutting-down":
				resource["shutdown"] = map[string]any{"requested": true}
			}
			residents["records"] = []admission.Resident{row}
			raw, err := json.Marshal(resource)
			if err != nil {
				t.Fatal(err)
			}
			var observed admission.Resident
			err = validateSessionObserverResources(owner, status, raw, spec, &observed)
			if fault == "" || fault == "pressure-only" || fault == "resident-history" {
				if err != nil {
					t.Fatal(err)
				}
				if observed.PID != resident.PID {
					t.Fatal("resident lost")
				}
			} else if err == nil {
				t.Fatal("invalid original activity frame accepted")
			}
		})
	}
}

func TestSessionObserverOriginalCLIUsesOnlyOriginalReadonlyEntrypoints(t *testing.T) {
	root := t.TempDir()
	binary := filepath.Join(root, "original-noodle")
	script := `#!/bin/sh
[ "$NOODLE_OBSERVER_FAIL" = 1 ] && exit 7
[ "$1" = --project-dir ] && [ "$2" = "$NOODLE_OBSERVER_PROJECT" ] && [ "$3" = host ] || exit 21
[ "$NOODLE_HOST_DIR" = "$NOODLE_OBSERVER_GENERATION" ] || exit 22
case "$4" in
status) [ "$5" = --request-file ] && [ "$6" = "$NOODLE_OBSERVER_REQUEST" ] && [ "$#" = 6 ] || exit 23;;
resources) [ "$#" = 4 ] || exit 24;;
*) exit 25;;
esac
printf '{"original":"%s"}' "$4"
`
	if err := os.WriteFile(binary, []byte(script), 0500); err != nil {
		t.Fatal(err)
	}
	owner := &hostOwnerBinding{ExecutionBinary: loopruntime.SessionReadbackFileRef{Path: binary}, GenerationDir: filepath.Join(root, "g"), Current: hostReceipt{ProjectDir: root}}
	request := filepath.Join(root, "original-request.json")
	t.Setenv("NOODLE_HOST_DIR", "unrelated-host")
	t.Setenv("NOODLE_OBSERVER_GENERATION", owner.GenerationDir)
	t.Setenv("NOODLE_OBSERVER_PROJECT", root)
	t.Setenv("NOODLE_OBSERVER_REQUEST", request)
	for _, operation := range []string{"status", "resources"} {
		var got map[string]string
		if err := readOriginalSessionHostCLI(context.Background(), owner, request, operation, &got); err != nil {
			t.Fatal(err)
		}
		if got["original"] != operation {
			t.Fatal("original readonly command lost")
		}
	}
	if err := readOriginalSessionHostCLI(context.Background(), owner, request, "attach", new(any)); err == nil {
		t.Fatal("effecting operation accepted by fixture")
	}
	// A failed original CLI remains a refusal; no fallback attach is selected.
	t.Setenv("NOODLE_OBSERVER_FAIL", "1")
	if err := readOriginalSessionHostCLI(context.Background(), owner, request, "status", new(any)); err == nil {
		t.Fatal("original CLI failure swallowed")
	}
}

func TestSessionObserverFramesRejectNativeGenerationAndLeaseDrift(t *testing.T) {
	original := procx.NativeIdentity{PID: 123, UID: 501, ParentPID: 1, Group: 123, StartSec: 10, Version: 1, Token: []byte("original")}
	gateway := original
	gateway.PID = 456
	gateway.ParentPID = 123
	gateway.Group = 456
	before := hostSessionObserverFrame{Host: hostOperatorActor{Process: original, Binary: loopruntime.SessionReadbackFileRef{Path: "host", SHA256: "host-sha"}}, Gateway: hostOperatorActor{Process: gateway, Binary: loopruntime.SessionReadbackFileRef{Path: "gateway", SHA256: "gateway-sha"}}, Resident: admission.Resident{PID: 456, Started: "original", Fingerprint: "original", GuardID: "original", Contexts: []string{"context"}}}
	for _, fault := range []string{"", "host-version", "host-parent", "host-binary", "gateway-version", "gateway-parent", "gateway-group", "resident-start", "resident-guard", "resident-lease"} {
		t.Run(fault, func(t *testing.T) {
			after := before
			switch fault {
			case "host-version":
				after.Host.Process.Version++
			case "host-parent":
				after.Host.Process.ParentPID++
			case "host-binary":
				after.Host.Binary.SHA256 = "foreign"
			case "gateway-version":
				after.Gateway.Process.Version++
			case "gateway-parent":
				after.Gateway.Process.ParentPID++
			case "gateway-group":
				after.Gateway.Process.Group++
			case "resident-start":
				after.Resident.Started = "foreign"
			case "resident-guard":
				after.Resident.GuardID = "foreign"
			case "resident-lease":
				after.Resident.Contexts = []string{"foreign"}
			}
			err := verifySessionObserverFrames(before, after)
			if fault == "" && err != nil {
				t.Fatal(err)
			}
			if fault != "" && (err == nil || !strings.Contains(err.Error(), "changed")) {
				t.Fatal("accepted original actor drift", err)
			}
		})
	}
}

func TestSessionObserverLoadsOriginalFileProfileWithoutConfusingDigests(t *testing.T) {
	path := hostDeferralProfile(t)
	ref, err := loopruntime.ReadSessionReadbackFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := loadSessionObserverProfile(path, ref)
	if err != nil {
		t.Fatal(err)
	}
	if profile.SourceSHA256 == ref.SHA256 {
		t.Fatal("fixture failed to distinguish aggregate profile and raw JSON digests")
	}
	changed := ref
	changed.SHA256 = "foreign"
	if _, err := loadSessionObserverProfile(path, changed); err == nil {
		t.Fatal("foreign raw profile digest accepted")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, append(raw, ' '), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := loadSessionObserverProfile(path, ref); err == nil {
		t.Fatal("profile raw source drift accepted")
	}
}
