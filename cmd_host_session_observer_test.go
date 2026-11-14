package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/procx"
	"github.com/poteto/noodle/loop"
	loopruntime "github.com/poteto/noodle/runtime"
)

func TestSessionObserverRequiresOriginalResourceFrameWithOtherWorkers(t *testing.T) {
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
	for _, fault := range []string{"", "other-worker", "other-generation-worker", "pressure-only", "resident-history", "host", "root", "binary", "context", "status", "active", "negative-active", "missing-active", "unknown-quota", "missing-quota", "quota-maximum", "quota-overflow", "unresolved", "resident-error", "missing-persist-result", "guard", "lease", "fingerprint", "resident-unknown", "shutting-down"} {
		t.Run(fault, func(t *testing.T) {
			status := hostResponse{hostReceipt: owner.Current, Status: "ok"}
			quota := map[string]any{"maximum": 9, "allocations": 1, "residents": 1, "owned_allocations": 1, "error": ""}
			budget := map[string]any{"active_workers": 0, "unresolved_residents": 0, "shared_quota": quota}
			row := resident
			residents := map[string]any{"records": []admission.Resident{row}, "error": nil}
			resource := map[string]any{"protocol": hostProtocol, "host_root": owner.HostRoot, "host_dir": owner.GenerationDir, "host_pid": 123, "host_instance_id": "generation", "binary_path": owner.ExecutionBinary.Path, "binary_sha256": owner.ExecutionBinary.SHA256, "shutdown": map[string]any{"requested": false}, "resources": budget, "residents": residents}
			switch fault {
			case "other-worker":
				budget["active_workers"] = 1
				quota["allocations"] = 2
				quota["owned_allocations"] = 2
			case "other-generation-worker":
				quota["allocations"] = 2
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
			case "negative-active":
				budget["active_workers"] = -1
			case "missing-active":
				delete(budget, "active_workers")
			case "unknown-quota":
				budget["shared_quota"] = map[string]any{"allocations": 1, "residents": 1, "error": "original launch unknown"}
			case "missing-quota":
				delete(budget, "shared_quota")
			case "quota-maximum":
				quota["maximum"] = 4
			case "quota-overflow":
				quota["allocations"] = 11
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
			if fault == "" || fault == "other-worker" || fault == "other-generation-worker" || fault == "pressure-only" || fault == "resident-history" {
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

func TestSessionObserverQuotaPreservesNineAcrossGenerationsAndOwnLease(t *testing.T) {
	spec, err := loopruntime.LoadOpenShellGatewaySpec(hostDeferralProfile(t))
	if err != nil {
		t.Fatal(err)
	}
	owner := &hostOwnerBinding{Current: hostReceipt{HostInstanceID: "generation", ContextID: "context"}}
	for _, fault := range []string{"", "nine-workers", "schema", "policy", "maximum", "ten-workers", "unknown-count", "foreign-context", "foreign-owner", "released-only", "missing-key", "worker-key", "duplicate-resident"} {
		t.Run(fault, func(t *testing.T) {
			quota := sessionObserverQuota{Schema: 3, Maximum: 9, Policy: admission.WorkerAdmissionPolicy, Leases: map[string]sessionObserverQuotaLease{
				"resident": {Owner: "generation", Context: "context", Resident: true, Key: spec.Resident.Name},
				"other-worker": {Owner: "older-generation", Context: "another-context"},
			}}
			resident := quota.Leases["resident"]
			frame := hostSessionObserverFrame{Resident: admission.Resident{Contexts: []string{"context"}, Released: []string{"released"}}}
			switch fault {
			case "nine-workers", "ten-workers":
				for i := 1; i < 9; i++ {
					quota.Leases[fmt.Sprint("worker-", i)] = sessionObserverQuotaLease{Owner: "older-generation", Context: "another-context"}
				}
				if fault == "ten-workers" {
					quota.Leases["tenth"] = sessionObserverQuotaLease{Owner: "other-generation", Context: "other"}
				}
			case "schema":
				quota.Schema = 2
			case "policy":
				quota.Policy = "old-policy"
			case "maximum":
				quota.Maximum = 4
			case "foreign-context":
				resident.Context = "foreign"
			case "foreign-owner":
				resident.Owner = "foreign"
			case "released-only":
				resident.Context = "released"
			case "missing-key":
				resident.Key = ""
			case "worker-key":
				worker := quota.Leases["other-worker"]
				worker.Key = spec.Resident.Name
				quota.Leases["other-worker"] = worker
			case "duplicate-resident":
				quota.Leases["duplicate"] = resident
			}
			quota.Leases["resident"] = resident
			count := len(quota.Leases)
			if fault == "unknown-count" {
				count++
			}
			frame.Resources, err = json.Marshal(map[string]any{"resources": map[string]any{"shared_quota": admission.SharedQuotaSnapshot{Maximum: 9, Allocations: count, Residents: 1, Owned: 1}}})
			if err != nil {
				t.Fatal(err)
			}
			err = validateSessionObserverQuota(owner, quota, frame, spec)
			if fault == "" || fault == "nine-workers" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("invalid quota or own context lease accepted")
			}
		})
	}
}

// Native qualification and kernel reads are isolated here. Runtime tests drive
// the real Get-only producer; this control exercises the complete Host consumer.
func TestSessionObserverResponseConsumesSelectedProofWithOtherWorker(t *testing.T) {
	for _, fault := range []string{"", "missing-proof", "completion-refused", "actor-generation", "quota-source", "profile-source"} {
		t.Run(fault, func(t *testing.T) {
			project, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			profilePath := hostDeferralProfile(t)
			profile, err := loopruntime.LoadOpenShellProfile(profilePath)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.MkdirAll(profile.Gateway.Binding.StateRoot, 0700); err != nil {
				t.Fatal(err)
			}
			write := func(path string, raw []byte) loopruntime.SessionReadbackFileRef {
				t.Helper()
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
				ref, err := loopruntime.ReadSessionReadbackFile(path, nil)
				if err != nil {
					t.Fatal(err)
				}
				return ref
			}
			binary := write(filepath.Join(root, "observer"), []byte("immutable observer"))
			executor := write(filepath.Join(root, "executor"), []byte("original executor"))
			configPath := filepath.Join(project, ".noodle.toml")
			configRef := write(configPath, []byte(fmt.Sprintf("mode='supervised'\n[runtime.openshell]\nprofile=%q\n", profilePath)))
			owner := &hostOwnerBinding{HostRoot: root, GenerationDir: filepath.Join(root, "g", "generation"), ExecutionBinary: executor, Sources: map[string]loopruntime.SessionReadbackFileRef{}, Current: hostReceipt{Schema: 1, Protocol: hostProtocol, HostPID: 123, HostInstanceID: "generation", ContextID: "context", RequestID: "request", ProjectDir: project, ConfigPath: configPath, ConfigSHA256: configRef.SHA256, ContextState: "active", Mode: "supervised", HostDir: root, BinaryPath: executor.Path, BinarySHA256: executor.SHA256}}
			profile.Gateway.ControlRoot = root
			spec := profile.Gateway
			spec.Binding.StateRoot, err = filepath.EvalSymlinks(spec.Binding.StateRoot)
			if err != nil {
				t.Fatal(err)
			}
			guard, err := loopruntime.OpenShellGatewayGuardID(spec)
			if err != nil {
				t.Fatal(err)
			}
			resident := admission.Resident{ID: profile.Gateway.Resident.Name, Name: profile.Gateway.Resident.Name, Sharing: "shared", State: "running", PID: 456, ProcessGroup: 456, Started: "original start", Fingerprint: "original fingerprint", GuardID: guard, Contexts: []string{"context", "other-context"}}
			quota := sessionObserverQuota{Schema: 3, Maximum: 9, Policy: admission.WorkerAdmissionPolicy, Leases: map[string]sessionObserverQuotaLease{
				"resident": {Owner: "generation", Context: "context", Resident: true, Key: resident.Name},
				"resident-other": {Owner: "generation", Context: "other-context", Resident: true, Key: resident.Name},
				"worker": {Owner: "generation", Context: "other-context"},
			}}
			quotaRaw, err := json.Marshal(quota)
			if err != nil {
				t.Fatal(err)
			}
			quotaRef := write(filepath.Join(root, "quota", "quota.json"), quotaRaw)
			resources, err := json.Marshal(map[string]any{"protocol": hostProtocol, "host_root": root, "host_dir": owner.GenerationDir, "host_pid": 123, "host_instance_id": "generation", "binary_path": executor.Path, "binary_sha256": executor.SHA256, "shutdown": map[string]any{"requested": false}, "residents": map[string]any{"records": []admission.Resident{resident}, "error": nil}, "resources": map[string]any{"active_workers": 1, "unresolved_residents": 0, "shared_quota": admission.SharedQuotaSnapshot{Maximum: 9, Allocations: 3, Residents: 2, Owned: 3}}})
			if err != nil {
				t.Fatal(err)
			}
			identity := procx.NativeIdentity{PID: 123, UID: 501, ParentPID: 1, Group: 123, StartSec: 10, Version: 1, Token: []byte("host")}
			gateway := identity
			gateway.PID, gateway.ParentPID, gateway.Group, gateway.Token = 456, 123, 456, []byte("gateway")
			frames, completions := 0, 0
			reads := sessionObserverReads{}
			reads.frame = func(_ context.Context, got *hostOwnerBinding, request string, spec loopruntime.OpenShellGatewaySpec) (hostSessionObserverFrame, error) {
				if got != owner || request != "original-request" {
					t.Fatal("Host frame input changed")
				}
				frame := hostSessionObserverFrame{Status: hostResponse{hostReceipt: owner.Current, Status: "ok"}, Resources: resources, Host: hostOperatorActor{Process: identity, Binary: executor}, Gateway: hostOperatorActor{Process: gateway, Binary: binary}}
				if err := validateSessionObserverResources(owner, frame.Status, frame.Resources, spec, &frame.Resident); err != nil {
					return frame, err
				}
				frames++
				if frames == 2 && fault == "actor-generation" {
					frame.Host.Process.Version++
				}
				return frame, nil
			}
			var produced loopruntime.CompletedSessionReadback
			reads.session = func(_ context.Context, gotProject, runtimeDir, contextID string, p loopruntime.OpenShellProfile, orderID, sessionID string) (loop.SessionReadback, error) {
				if gotProject != project || runtimeDir != filepath.Join(project, ".noodle") || contextID != "context" || p.SourcePath != profilePath || orderID != "order" || sessionID != "session" {
					t.Fatal("original session inputs changed")
				}
				profileSHA, err := loopruntime.OpenShellDispatchSHA256(p)
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(p.Gateway.Binding.StateRoot, "owner.json")
				before := write(path, []byte("{}"))
				after := write(path, []byte("{} "))
				proof := &loopruntime.SessionCustodyProjection{Schema: 1, Scope: loopruntime.SelectedSessionStopped, Subject: loopruntime.SessionCustodySubject{ContextID: contextID, OrderID: orderID, SessionID: sessionID, NativeAttemptID: "native-attempt"}, ProfileSHA256: profileSHA, RootSHA256: strings.Repeat("a", 64), SelectedRecordSHA256: strings.Repeat("b", 64), Before: before, After: after}
				produced = loopruntime.CompletedSessionReadback{CustodyProjection: proof, ProfileSHA256: profileSHA, NativeAttemptID: "native-attempt", CandidateHead: "candidate", NativePhase: "Stopped", StopAcknowledged: true, MainProcessInstanceID: "original-main", Sources: map[string]loopruntime.SessionReadbackFileRef{"native_custody": after}}
				write(path, []byte("{}  "))
				switch fault {
				case "missing-proof":
					produced.CustodyProjection = nil
				case "quota-source":
					write(quotaRef.Path, append(quotaRaw, ' '))
				case "profile-source":
					raw, err := os.ReadFile(profilePath)
					if err != nil {
						t.Fatal(err)
					}
					write(profilePath, append(raw, ' '))
				}
				return loop.SessionReadback{SchemaVersion: 1, Status: "observed", OrderID: orderID, SessionID: sessionID, AttemptID: "canonical-attempt", AttemptStatus: "failed", Runtime: "openshell", Execution: produced, Effects: []string{}}, nil
			}
			reads.completion = func(e loopruntime.CompletedSessionReadback, p loopruntime.OpenShellProfile, contextID, orderID, sessionID string) error {
				completions++
				if p.SourcePath != profilePath || contextID != "context" || orderID != "order" || sessionID != "session" || e.CustodyProjection != produced.CustodyProjection {
					t.Fatal("Host lost qualified producer proof or original subject")
				}
				if fault == "completion-refused" {
					return fmt.Errorf("original selected completion changed")
				}
				return nil
			}
			result, err := observeHostSessionWithReads(t.Context(), project, "original-request", "order", "session", owner, binary, reads)
			if fault != "" {
				if err == nil || result.Status != "refused" || result.Observation != nil {
					t.Fatal("Host accepted invalid source/frame/completion")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != "observed" || result.SchemaVersion != 2 || result.Observer == nil || result.Observer.InactiveScope != loopruntime.SelectedSessionStopped || result.Observer.Binary != binary || result.Observer.ExecutionBinary != executor || result.Observation.Execution.CustodyProjection != produced.CustodyProjection || result.Observation.AttemptID == produced.NativeAttemptID || result.Observation.AttemptStatus != "failed" || len(result.Effects) != 0 || frames != 2 || completions != 1 {
				t.Fatal("Host response lost original execution, proof, scope or provenance")
			}
			raw, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			var packet struct {
				Observation struct {
					Execution struct {
						Proof map[string]any `json:"custody_projection"`
					} `json:"execution"`
				} `json:"observation"`
			}
			if err = json.Unmarshal(raw, &packet); err != nil || len(packet.Observation.Execution.Proof) != 8 {
				t.Fatal("schema2 response lost exact optional proof", err)
			}
		})
	}
}
