package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/poteto/noodle/internal/admission"
	loopruntime "github.com/poteto/noodle/runtime"
)

func unacceptedAdmissionFixture(t *testing.T) (*noodleHost, hostRequest) {
	t.Helper()
	root, project := t.TempDir(), t.TempDir()
	req := hostTestRequest(t, project)
	req.HostDir, req.RequestFile = root, filepath.Join(t.TempDir(), "original.json")
	req.RequiredHostInstanceID = strings.Repeat("a", 32)
	binding, unlock, err := bindHostRequest(&req)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	if binding.Status != "pending" {
		t.Fatal("fixture was accepted")
	}
	if err := updateHostPool(root, func(pool *hostPool) error { pool.Active = req.RequiredHostInstanceID; return nil }); err != nil {
		t.Fatal(err)
	}
	h := &noodleHost{dir: root, instanceID: req.RequiredHostInstanceID, binarySHA: req.BinarySHA256,
		binaryPath: "/fixture/noodle", blocked: true, rolling: &hostRolling{root: root}}
	return h, req
}

func TestAdmissionReadbackRequiresOriginalRefusalNotAbsence(t *testing.T) {
	h, req := unacceptedAdmissionFixture(t)
	if result := h.admissionReadback(req).(map[string]any); result["status"] != "input_required" {
		t.Fatalf("missing receipt became no-effect proof: %+v", result)
	}
	response := h.recordAdmissionRefusal(req, hostError("unknown_effect", errors.New("prior active or failed context requires readback; attach refused")))
	if response.Error == "" {
		t.Fatal("failure history lost")
	}
	if result := h.admissionReadback(req).(map[string]any); result["status"] != "not_admitted" {
		t.Fatalf("original refusal not readable: %+v", result)
	}
	before, err := os.ReadFile(req.RequestFile)
	if err != nil {
		t.Fatal(err)
	}
	h.admissionReadback(req)
	after, err := os.ReadFile(req.RequestFile)
	if err != nil || string(before) != string(after) {
		t.Fatal("readback rewrote original request")
	}
}

func TestAdmissionReadbackRefusesExecutedForgedAndChangedSources(t *testing.T) {
	for _, fault := range []string{"receipt", "context", "runtime-context", "reason", "source", "unblocked"} {
		t.Run(fault, func(t *testing.T) {
			h, req := unacceptedAdmissionFixture(t)
			if fault == "unblocked" {
				h.blocked = false
			}
			h.recordAdmissionRefusal(req, hostError("unknown_effect", errors.New("original blocked guard")))
			switch fault {
			case "runtime-context":
				h.contexts = map[string]*hostContext{"executed": {receipt: hostReceipt{RequestID: req.RequestID}}}
			case "receipt":
				h.receipts = append(h.receipts, hostReceipt{RequestID: req.RequestID})
			case "context", "reason":
				if err := updateHostPool(h.controlRoot(), func(pool *hostPool) error {
					owner := pool.Owners[req.RequestID]
					if fault == "context" {
						owner.Current.ContextID = "executed"
					} else {
						owner.NotAdmitted.Reason = "forged"
					}
					pool.Owners[req.RequestID] = owner
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			case "source":
				raw, err := os.ReadFile(req.RequestFile)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(req.RequestFile, append(raw, '\n'), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if result := h.admissionReadback(req).(map[string]any); result["status"] != "input_required" {
				t.Fatalf("unsupported proof accepted: %+v", result)
			}
		})
	}
}

func admissionTargetFixture(t *testing.T) string {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "nat-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "noodle")
	if err := os.WriteFile(binary, []byte("fixture binary"), 0700); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(root, "host.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			func() {
				defer connection.Close()
				var req hostRequest
				if json.NewDecoder(connection).Decode(&req) != nil {
					return
				}
				if req.Operation != "resources" {
					return
				}
				json.NewEncoder(connection).Encode(map[string]any{"protocol": hostProtocol, "host_pid": os.Getpid(),
					"rolling_host_version": 1, "host_root": root, "host_instance_id": strings.Repeat("b", 32),
					"binary_path": binary, "binary_sha256": hostDigest([]byte("fixture binary")),
					"resources": admission.Snapshot{AdmissionPolicy: admission.WorkerAdmissionPolicy, Maximum: admission.DefaultWorkerLimit},
					"admission": map[string]any{"can_accept": true}})
			}()
		}
	}()
	return root
}

func TestUnacceptedAdmissionTransferPreservesOriginalAndResumesSamePreparation(t *testing.T) {
	h, req := unacceptedAdmissionFixture(t)
	h.recordAdmissionRefusal(req, hostError("unknown_effect", errors.New("original blocked guard")))
	before, err := os.ReadFile(req.RequestFile)
	if err != nil {
		t.Fatal(err)
	}
	req.AdmissionTargetRoot = admissionTargetFixture(t)
	req.AdmissionConfigPath = filepath.Join(t.TempDir(), "successor.toml")
	original, err := readHostRequestConfig(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(req.AdmissionConfigPath, original, 0600); err != nil {
		t.Fatal(err)
	}
	first := h.admissionTransfer(context.Background(), req).(map[string]any)
	if first["status"] != "prepared" {
		t.Fatalf("transfer: %+v", first)
	}
	transfer := first["transfer"].(*hostAdmissionTransfer)
	executor, err := hostReadbackBinaryRef(filepath.Join(req.AdmissionTargetRoot, "noodle"))
	if err != nil || transfer.Noodle != executor || transfer.Noodle.SHA256 != transfer.Successor.BinarySHA256 {
		t.Fatalf("transfer lost canonical target executable: %+v, %v", transfer.Noodle, err)
	}
	var initialCurrent hostRequestBinding
	initialRef, err := loopruntime.ReadSessionReadbackFile(transfer.RequestPath, &initialCurrent)
	if err != nil || first["current_request"] != initialRef || !reflect.DeepEqual(first["current_binding"], initialCurrent) {
		t.Fatalf("prepared result did not return one native current request snapshot: %+v, %v", first, err)
	}
	if transfer.Successor.Status != "busy" || transfer.Successor.RequestID == req.RequestID {
		t.Fatal("successor was accepted or reused original identity")
	}
	second := h.admissionTransfer(context.Background(), req).(map[string]any)
	if second["status"] != "prepared" || second["transfer"].(*hostAdmissionTransfer).Successor.RequestID != transfer.Successor.RequestID {
		t.Fatalf("repeat created another successor: %+v", second)
	}
	after, err := os.ReadFile(req.RequestFile)
	if err != nil || string(before) != string(after) {
		t.Fatal("transfer rewrote original request")
	}
	if len(h.receipts) != 0 || len(h.contexts) != 0 {
		t.Fatal("transfer launched runtime")
	}
	if err := validateTransferredAdmission(transfer.RequestPath, transfer.Successor); err != nil {
		t.Fatal(err)
	}
	prepared := transfer.Successor
	successorRequest := hostRequest{Operation: prepared.Operation, RequestFile: transfer.RequestPath,
		ProjectDir: prepared.ProjectDir, ConfigPath: prepared.ConfigPath, ConfigSHA256: prepared.ConfigSHA256,
		Mode: prepared.Mode, BinarySHA256: prepared.BinarySHA256, RequiredHostInstanceID: prepared.RequiredHostInstanceID}
	bound, unlock, err := bindHostRequest(&successorRequest)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	if bound.Status != "pending" || successorRequest.Operation != prepared.Operation {
		t.Fatal("prepared successor was not offered once through original admission")
	}
	_, unlock, err = bindHostRequest(&successorRequest)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	if successorRequest.Operation != "status" {
		t.Fatal("pending successor was offered a second time")
	}
	for _, status := range []string{"pending", "accepted", "unrecognized-unknown"} {
		bound.Status = status
		bound.EnvironmentSHA256 = hostDigest([]byte("native-current-environment"))
		if err := saveHostRequest(transfer.RequestPath, *bound); err != nil {
			t.Fatal(err)
		}
		observed := h.admissionReadback(req).(map[string]any)
		var current hostRequestBinding
		currentRef, err := loopruntime.ReadSessionReadbackFile(transfer.RequestPath, &current)
		if err != nil || observed["status"] != "not_admitted" || observed["current_request"] != currentRef ||
			!reflect.DeepEqual(observed["current_binding"], current) || current.Status != status || currentRef == initialRef {
			t.Fatalf("successor current state was rewritten or lost: %+v, %v", observed, err)
		}
	}
	h.blocked = false
	if response := h.attachRolling(context.Background(), req, nil); response.Status != "unknown_effect" || !strings.Contains(response.Error, "fixed transfer") {
		t.Fatalf("original request restarted after transfer: %+v", response)
	}
	changed := transfer.Successor
	changed.ConfigSHA256 = "foreign"
	if err := validateTransferredAdmission(transfer.RequestPath, changed); err == nil {
		t.Fatal("foreign successor accepted")
	}
	if err := updateHostPool(h.controlRoot(), func(pool *hostPool) error {
		owner := pool.Owners[req.RequestID]
		owner.NotAdmitted.Transfer.Noodle.Path = "/foreign/noodle"
		pool.Owners[req.RequestID] = owner
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if result := h.admissionTransfer(context.Background(), req).(map[string]any); result["status"] != "input_required" {
		t.Fatalf("transfer changed its fixed executable: %+v", result)
	}
	if err := updateHostPool(h.controlRoot(), func(pool *hostPool) error {
		owner := pool.Owners[req.RequestID]
		owner.NotAdmitted.Transfer.Noodle = executor
		pool.Owners[req.RequestID] = owner
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(transfer.RequestPath); err != nil {
		t.Fatal(err)
	}
	if result := h.admissionTransfer(context.Background(), req).(map[string]any); result["status"] != "input_required" {
		t.Fatalf("missing successor silently repaired after completion: %+v", result)
	}
}

func TestAdmissionTransferRejectsChangedTaskPolicy(t *testing.T) {
	h, req := unacceptedAdmissionFixture(t)
	h.recordAdmissionRefusal(req, hostError("unknown_effect", errors.New("original blocked guard")))
	req.AdmissionTargetRoot = admissionTargetFixture(t)
	req.AdmissionConfigPath = filepath.Join(t.TempDir(), "changed.toml")
	if err := os.WriteFile(req.AdmissionConfigPath, []byte("mode='manual'\n[server]\nenabled=false\n[runtime]\nadmitted_orders=['foreign']\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if result := h.admissionTransfer(context.Background(), req).(map[string]any); result["status"] != "input_required" {
		t.Fatalf("transfer widened original task policy: %+v", result)
	}
	pool, err := readHostPool(h.controlRoot())
	if err != nil {
		t.Fatal(err)
	}
	if pool.Owners[req.RequestID].NotAdmitted.Transfer != nil {
		t.Fatal("refused policy persisted transfer")
	}
}

func TestAdmissionTransferRefusesBoundReturnHandoffBeforePersisting(t *testing.T) {
	h, req := unacceptedAdmissionFixture(t)
	descriptor := filepath.Join(t.TempDir(), "return.json")
	if err := os.WriteFile(descriptor, []byte(`{"schema":1,"host_instance_id":"`+req.RequiredHostInstanceID+`"}`), 0600); err != nil {
		t.Fatal(err)
	}
	ref, err := loopruntime.ReadSessionReadbackFile(descriptor, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := readHostRequestConfig(req)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, []byte(fmt.Sprintf("\n[runtime.return_handoff]\npath=%q\nsha256=%q\n", ref.Path, ref.SHA256))...)
	req.ConfigPath, req.ConfigSHA256 = filepath.Join(t.TempDir(), "original.toml"), hostDigest(raw)
	if err := os.WriteFile(req.ConfigPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	configRef, err := loopruntime.ReadSessionReadbackFile(req.ConfigPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.ConfigPath = configRef.Path
	var binding hostRequestBinding
	if _, err := loopruntime.ReadSessionReadbackFile(req.RequestFile, &binding); err != nil {
		t.Fatal(err)
	}
	binding.ConfigPath, binding.ConfigSHA256 = req.ConfigPath, req.ConfigSHA256
	if err := saveHostRequest(req.RequestFile, binding); err != nil {
		t.Fatal(err)
	}
	h.recordAdmissionRefusal(req, hostError("unknown_effect", errors.New("original blocked guard")))
	before, err := os.ReadFile(req.RequestFile)
	if err != nil {
		t.Fatal(err)
	}
	req.AdmissionTargetRoot = admissionTargetFixture(t)
	req.AdmissionConfigPath = filepath.Join(t.TempDir(), "successor.toml")
	if err := os.WriteFile(req.AdmissionConfigPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	result := h.admissionTransfer(context.Background(), req).(map[string]any)
	if result["status"] != "input_required" || !strings.Contains(fmt.Sprint(result["error"]), "return handoff") {
		t.Fatalf("bound return handoff transferred without original-owner rebind: %+v", result)
	}
	pool, err := readHostPool(h.controlRoot())
	if err != nil || pool.Owners[req.RequestID].NotAdmitted.Transfer != nil {
		t.Fatalf("refused handoff persisted a transfer: %+v, %v", pool, err)
	}
	after, err := os.ReadFile(req.RequestFile)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("refused handoff changed the original request")
	}
}

func TestAdmissionCLIKeepsLegacyMissingCapabilityInputRequired(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "nal-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	listener, err := net.Listen("unix", filepath.Join(root, "host.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for range 2 {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			var request hostRequest
			if json.NewDecoder(connection).Decode(&request) == nil {
				_ = json.NewEncoder(connection).Encode(hostError("refused", errors.New("unsupported host operation")))
			}
			connection.Close()
		}
	}()
	requestFile := filepath.Join(root, "original.json")
	binding := hostRequestBinding{Schema: 1, RequestID: strings.Repeat("a", 32), Operation: "attach", HostDir: root, Status: "pending"}
	if err := saveHostRequest(requestFile, binding); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(requestFile)
	for _, transfer := range []bool{false, true} {
		command := newHostAdmissionCmd(&App{}, transfer)
		arguments := []string{"--request-file", requestFile}
		if transfer {
			arguments = append(arguments, "--target-host-root", "/unused-target", "--config-file", "/unused-config")
		}
		command.SetArgs(arguments)
		var output bytes.Buffer
		command.SetOut(&output)
		if err := command.Execute(); err != nil {
			t.Fatal(err)
		}
		var result map[string]any
		if err := json.Unmarshal(output.Bytes(), &result); err != nil || result["status"] != "input_required" || result["missing_capability"] != "unaccepted_admission_readback" {
			t.Fatalf("legacy missing capability became transferable: %s, %v", output.String(), err)
		}
	}
	after, _ := os.ReadFile(requestFile)
	if !bytes.Equal(before, after) {
		t.Fatal("legacy readback or transfer rewrote the pending original")
	}
}

func TestRollingTargetAcceptsCurrentNativeCapabilitiesWithoutWarningGrant(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("rolling Host is not supported on Windows")
	}
	for _, fault := range []string{"none", "quota", "native", "roles", "legacy-policy", "missing-policy"} {
		t.Run(fault, func(t *testing.T) {
			capabilities := hostCapabilities(runtime.GOOS)
			if fault == "quota" {
				delete(capabilities, "shared_worker_quota")
			}
			if fault == "native" {
				delete(capabilities, "codex_native_runtime")
			}
			if fault == "legacy-policy" {
				capabilities["worker_admission_policy"] = "swap-used-critical-guard-v2"
			}
			if fault == "missing-policy" {
				delete(capabilities, "worker_admission_policy")
			}
			caps, err := json.Marshal(capabilities)
			if err != nil {
				t.Fatal(err)
			}
			root := NewRootCmd()
			var output bytes.Buffer
			root.SetOut(&output)
			root.SetArgs([]string{"host", "task", "review-capabilities"})
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			var review map[string]any
			if err := json.Unmarshal(output.Bytes(), &review); err != nil {
				t.Fatal(err)
			}
			if fault == "roles" {
				delete(review, "local_task_roles")
			}
			reviewRaw, err := json.Marshal(review)
			if err != nil {
				t.Fatal(err)
			}
			binary := filepath.Join(t.TempDir(), "target")
			script := []byte("#!/bin/sh\ncase \"$*\" in\n'host capabilities') cat \"$0.caps\";;\n'host task review-capabilities') cat \"$0.review\";;\n*) exit 2;;\nesac\n")
			for path, raw := range map[string][]byte{binary: script, binary + ".caps": caps, binary + ".review": reviewRaw} {
				if err := os.WriteFile(path, raw, 0700); err != nil {
					t.Fatal(err)
				}
			}
			err = verifyRollingTarget(context.Background(), binary, hostDigest(script))
			if (fault == "none") != (err == nil) {
				t.Fatalf("current capability consumer (%s): %v", fault, err)
			}
		})
	}
}

func TestAdmissionTargetConfigPreservesIsolatedDispatchPolicy(t *testing.T) {
	for _, fault := range []string{"none", "image", "provider", "gateway-driver"} {
		t.Run(fault, func(t *testing.T) {
			root, _, template, _ := goalHostFixture(t)
			originalRoot, targetRoot := filepath.Join(root, "original"), filepath.Join(root, "target")
			for _, directory := range []string{originalRoot, targetRoot} {
				if err := os.Mkdir(directory, 0700); err != nil {
					t.Fatal(err)
				}
			}
			original, err := loopruntime.MaterializeGoalOpenShellProfile(template, originalRoot, "")
			if err != nil {
				t.Fatal(err)
			}
			targetTemplate := template
			if fault == "gateway-driver" {
				filename := filepath.Join(filepath.Dir(template), "gateway.toml")
				raw, err := os.ReadFile(filename)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filename, bytes.Replace(raw, []byte("/immutable/driver"), []byte("/different/driver"), 1), 0600); err != nil {
					t.Fatal(err)
				}
			} else if fault != "none" {
				raw, err := os.ReadFile(template)
				if err != nil {
					t.Fatal(err)
				}
				var profile map[string]any
				if err := json.Unmarshal(raw, &profile); err != nil {
					t.Fatal(err)
				}
				route := profile["routes"].(map[string]any)["codex"].(map[string]any)
				specFile := filepath.Join(filepath.Dir(template), "changed-spec.json")
				specRaw, err := os.ReadFile(filepath.Join(filepath.Dir(template), route["spec_file"].(string)))
				if err != nil {
					t.Fatal(err)
				}
				var spec map[string]any
				if err := json.Unmarshal(specRaw, &spec); err != nil {
					t.Fatal(err)
				}
				if fault == "image" {
					spec["template"].(map[string]any)["image"] = "different-legal-image"
				} else {
					spec["providers"] = []string{"different-legal-provider"}
				}
				specRaw, err = json.Marshal(spec)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(specFile, specRaw, 0600); err != nil {
					t.Fatal(err)
				}
				route["spec_file"] = specFile
				raw, err = json.Marshal(profile)
				if err != nil {
					t.Fatal(err)
				}
				targetTemplate = filepath.Join(filepath.Dir(template), "changed-profile.json")
				if err := os.WriteFile(targetTemplate, raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			target, err := loopruntime.MaterializeGoalOpenShellProfile(targetTemplate, targetRoot, "")
			if err != nil {
				t.Fatal(err)
			}
			originalConfig := []byte(fmt.Sprintf("mode='manual'\n[server]\nenabled=false\n[runtime]\nadmitted_orders=['selected-task']\n[runtime.openshell]\nprofile=%q\n", original.Profile.Path))
			configPath, targetConfigPath := filepath.Join(root, "original.toml"), filepath.Join(root, "target.toml")
			if err := os.WriteFile(configPath, originalConfig, 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(targetConfigPath, bytes.Replace(originalConfig, []byte(original.Profile.Path), []byte(target.Profile.Path), 1), 0600); err != nil {
				t.Fatal(err)
			}
			binding := hostRequestBinding{ProjectDir: root, ConfigPath: configPath, ConfigSHA256: hostDigest(originalConfig)}
			_, err = targetAdmissionConfig(binding, targetRoot, targetConfigPath)
			if (fault == "none") != (err == nil) || err != nil && !strings.Contains(err.Error(), "policy differs") {
				t.Fatalf("isolated dispatch policy (%s): %v", fault, err)
			}
		})
	}
}

func TestSealedAdmissionTransferReadbackNeedsNoRunningHostAndCannotMintProof(t *testing.T) {
	for _, fault := range []string{"none", "not-sealed", "no-proof", "original-changed", "successor-changed", "noodle-changed"} {
		t.Run(fault, func(t *testing.T) {
			h, req := unacceptedAdmissionFixture(t)
			if fault != "no-proof" {
				h.recordAdmissionRefusal(req, hostError("unknown_effect", errors.New("original blocked guard")))
			}
			var transfer *hostAdmissionTransfer
			if fault != "not-sealed" && fault != "no-proof" {
				req.AdmissionTargetRoot = admissionTargetFixture(t)
				req.AdmissionConfigPath = filepath.Join(t.TempDir(), "target.toml")
				original, err := readHostRequestConfig(req)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(req.AdmissionConfigPath, original, 0600); err != nil {
					t.Fatal(err)
				}
				prepared := h.admissionTransfer(context.Background(), req).(map[string]any)
				if prepared["status"] != "prepared" {
					t.Fatalf("fixture transfer failed: %+v", prepared)
				}
				transfer = prepared["transfer"].(*hostAdmissionTransfer)
				current := transfer.Successor
				current.Status, current.EnvironmentSHA256 = "pending", hostDigest([]byte("native pending environment"))
				if fault == "successor-changed" {
					current.ConfigSHA256 = hostDigest([]byte("foreign config"))
				}
				if err := saveHostRequest(transfer.RequestPath, current); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(filepath.Join(req.AdmissionTargetRoot, "host.sock")); err != nil {
					t.Fatal(err)
				}
			}
			switch fault {
			case "original-changed":
				raw, err := os.ReadFile(req.RequestFile)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(req.RequestFile, append(raw, '\n'), 0600); err != nil {
					t.Fatal(err)
				}
			case "noodle-changed":
				if err := os.WriteFile(transfer.Noodle.Path, []byte("changed executable"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := os.Stat(filepath.Join(h.controlRoot(), "host.sock")); !os.IsNotExist(err) {
				t.Fatal("fixture unexpectedly has a live original Host socket")
			}
			originalBefore, err := os.ReadFile(req.RequestFile)
			if err != nil {
				t.Fatal(err)
			}
			poolBefore, err := os.ReadFile(filepath.Join(h.controlRoot(), "pool.json"))
			if err != nil {
				t.Fatal(err)
			}
			command := NewRootCmd()
			command.SetArgs([]string{"host", "admission-transfer-readback", "--request-file", req.RequestFile})
			var output bytes.Buffer
			command.SetOut(&output)
			if err := command.Execute(); err != nil {
				t.Fatal(err)
			}
			var result struct {
				Status         string               `json:"status"`
				CurrentRequest hostAdmissionFileRef `json:"current_request"`
				CurrentBinding hostRequestBinding   `json:"current_binding"`
				Effects        []string             `json:"effects"`
			}
			if err := json.Unmarshal(output.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if fault == "none" {
				var current hostRequestBinding
				reference, err := loopruntime.ReadSessionReadbackFile(transfer.RequestPath, &current)
				if err != nil || result.Status != "not_admitted" || result.CurrentRequest != reference ||
					!reflect.DeepEqual(result.CurrentBinding, current) || result.CurrentBinding.Status != "pending" {
					t.Fatalf("sealed pending readback without daemon differs: %s, %v", output.String(), err)
				}
			} else if result.Status != "input_required" {
				t.Fatalf("unproven or changed transfer became usable: %s", output.String())
			}
			originalAfter, err := os.ReadFile(req.RequestFile)
			if err != nil || !bytes.Equal(originalBefore, originalAfter) {
				t.Fatal("sealed reader rewrote original request")
			}
			poolAfter, err := os.ReadFile(filepath.Join(h.controlRoot(), "pool.json"))
			if err != nil || !bytes.Equal(poolBefore, poolAfter) || len(result.Effects) != 0 || len(h.receipts) != 0 || len(h.contexts) != 0 {
				t.Fatal("sealed reader wrote pool or launched a context")
			}
		})
	}
	if hostCapabilities(runtime.GOOS)["unaccepted_admission_transfer_readback"] != 1 {
		t.Fatal("sealed transfer reader capability missing")
	}
}

func TestAdmissionTransferPinsProfileDependenciesAcrossNativeReadbacks(t *testing.T) {
	for _, drift := range []string{"none", "spec", "payload"} {
		t.Run(drift, func(t *testing.T) {
			h, req := unacceptedAdmissionFixture(t)
			root, _, template, _ := goalHostFixture(t)
			originalRoot := filepath.Join(root, "original-profile")
			if err := os.Mkdir(originalRoot, 0700); err != nil {
				t.Fatal(err)
			}
			originalProfile, err := loopruntime.MaterializeGoalOpenShellProfile(template, originalRoot, "")
			if err != nil {
				t.Fatal(err)
			}
			req.AdmissionTargetRoot = admissionTargetFixture(t)
			targetProfile, err := loopruntime.MaterializeGoalOpenShellProfile(template, req.AdmissionTargetRoot, "")
			if err != nil {
				t.Fatal(err)
			}
			originalConfig := []byte(fmt.Sprintf("mode='manual'\n[server]\nenabled=false\n[runtime]\nadmitted_orders=['selected-task']\n[runtime.openshell]\nprofile=%q\n", originalProfile.Profile.Path))
			if err := os.WriteFile(filepath.Join(req.ProjectDir, ".noodle.toml"), originalConfig, 0600); err != nil {
				t.Fatal(err)
			}
			var originalBinding hostRequestBinding
			if _, err := loopruntime.ReadSessionReadbackFile(req.RequestFile, &originalBinding); err != nil {
				t.Fatal(err)
			}
			req.ConfigSHA256 = hostDigest(originalConfig)
			originalBinding.ConfigSHA256 = req.ConfigSHA256
			if err := saveHostRequest(req.RequestFile, originalBinding); err != nil {
				t.Fatal(err)
			}
			h.recordAdmissionRefusal(req, hostError("unknown_effect", errors.New("original blocked guard")))
			req.AdmissionConfigPath = filepath.Join(root, "target.toml")
			if err := os.WriteFile(req.AdmissionConfigPath, bytes.Replace(originalConfig, []byte(originalProfile.Profile.Path), []byte(targetProfile.Profile.Path), 1), 0600); err != nil {
				t.Fatal(err)
			}
			prepared := h.admissionTransfer(context.Background(), req).(map[string]any)
			if prepared["status"] != "prepared" {
				t.Fatalf("isolated profile transfer failed: %+v", prepared)
			}
			transfer := prepared["transfer"].(*hostAdmissionTransfer)
			loadedProfile, err := loopruntime.LoadOpenShellProfile(targetProfile.Profile.Path)
			if err != nil || transfer.Profile == nil || *transfer.Profile != targetProfile.Profile ||
				transfer.ProfileSourceSHA256 != loadedProfile.SourceSHA256 || transfer.ProfileSourceSHA256 == transfer.Profile.SHA256 {
				t.Fatalf("native profile or full dependency proof missing: %+v, %v", transfer, err)
			}
			current := transfer.Successor
			current.Status, current.EnvironmentSHA256 = "pending", hostDigest([]byte("original current environment"))
			if err := saveHostRequest(transfer.RequestPath, current); err != nil {
				t.Fatal(err)
			}
			profileBefore, err := os.ReadFile(targetProfile.Profile.Path)
			if err != nil {
				t.Fatal(err)
			}
			switch drift {
			case "spec":
				filename := filepath.Join(filepath.Dir(template), "spec.json")
				raw, err := os.ReadFile(filename)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filename, bytes.Replace(raw, []byte("fixture"), []byte("post-seal-image"), 1), 0600); err != nil {
					t.Fatal(err)
				}
			case "payload":
				if err := os.WriteFile(filepath.Join(filepath.Dir(template), "payload", "codex"), []byte("post-seal payload change"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			profileAfter, err := os.ReadFile(targetProfile.Profile.Path)
			if err != nil || !bytes.Equal(profileBefore, profileAfter) {
				t.Fatal("fixture changed JSON instead of a dependency")
			}
			if err := saveHostRequest(transfer.RequestPath, transfer.Successor); err != nil {
				t.Fatal(err)
			}
			requestBefore, err := os.ReadFile(transfer.RequestPath)
			if err != nil {
				t.Fatal(err)
			}
			successor := transfer.Successor
			attach := hostRequest{Operation: successor.Operation, RequestFile: transfer.RequestPath,
				ProjectDir: successor.ProjectDir, ConfigPath: successor.ConfigPath, ConfigSHA256: successor.ConfigSHA256,
				Mode: successor.Mode, BinarySHA256: successor.BinarySHA256, RequiredHostInstanceID: successor.RequiredHostInstanceID}
			bound, unlock, err := bindHostRequest(&attach)
			if err == nil {
				unlock()
			}
			if drift == "none" {
				if err != nil || bound.Status != "pending" || attach.Operation != "attach" {
					t.Fatalf("unchanged successor admission refused: %+v, %v", bound, err)
				}
			} else {
				if err == nil || !strings.Contains(err.Error(), "profile dependency source changed") {
					t.Fatalf("post-seal %s drift reached direct successor admission: %v", drift, err)
				}
				requestAfter, err := os.ReadFile(transfer.RequestPath)
				if err != nil || !bytes.Equal(requestBefore, requestAfter) {
					t.Fatal("refused successor admission changed the native request")
				}
			}
			var output bytes.Buffer
			command := newHostAdmissionTransferReadbackCmd()
			command.SetArgs([]string{"--request-file", req.RequestFile})
			command.SetOut(&output)
			if err := command.Execute(); err != nil {
				t.Fatal(err)
			}
			var sealed map[string]any
			if err := json.Unmarshal(output.Bytes(), &sealed); err != nil {
				t.Fatal(err)
			}
			want := "not_admitted"
			if drift != "none" {
				want = "input_required"
			}
			for _, result := range []map[string]any{h.admissionReadback(req).(map[string]any), sealed} {
				if result["status"] != want {
					t.Fatalf("profile dependency drift (%s) readback: %+v", drift, result)
				}
			}
			if drift != "none" {
				if result := h.admissionTransfer(context.Background(), req).(map[string]any); result["status"] != "input_required" {
					t.Fatalf("transfer rebound its sealed profile policy: %+v", result)
				}
			}
		})
	}
}
