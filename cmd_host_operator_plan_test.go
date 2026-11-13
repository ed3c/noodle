package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/poteto/noodle/internal/admission"
	loopruntime "github.com/poteto/noodle/runtime"
)

func TestOperatorPlanDerivesOriginalSourceWithoutWorkerAdmission(t *testing.T) {
	for _, fault := range []string{"", "request-not-accepted", "generation-changed", "context-missing", "pool-identity-changed", "config-changed", "operator-already-explicit", "custody-binding-changed", "custody-control-root-changed", "resident-missing", "resident-guard-changed", "resident-extra-context", "source-not-leased"} {
		t.Run(fault, func(t *testing.T) {
			root, err := os.MkdirTemp("/tmp", "n-op-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(root) })
			profile := hostDeferralProfile(t)
			// Gateway-only preparation must not traverse these worker inputs.
			if err := os.RemoveAll(filepath.Join(filepath.Dir(profile), "payload")); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(filepath.Join(filepath.Dir(profile), "spec.json")); err != nil {
				t.Fatal(err)
			}
			write := func(path string, value any) []byte {
				t.Helper()
				data, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				return data
			}
			gateway, err := loopruntime.LoadOpenShellGatewaySpec(profile)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.MkdirAll(gateway.Binding.StateRoot, 0700); err != nil {
				t.Fatal(err)
			}
			custody := loopruntime.OpenShellCustodyState{Version: 4, ControlRoot: root, Binding: gateway.Binding, Blocked: "original unknown remains unresolved"}
			if fault == "custody-binding-changed" {
				custody.Binding.ConfigFingerprint = "changed"
			}
			if fault == "custody-control-root-changed" {
				custody.ControlRoot = "/different/host"
			}
			custodyPath := filepath.Join(gateway.Binding.StateRoot, "owner.json")
			custodyBytes := write(custodyPath, custody)
			generation, requestID, contextID := strings.Repeat("a", 32), strings.Repeat("b", 32), strings.Repeat("c", 32)
			guardID, err := loopruntime.OpenShellGatewayGuardID(gateway)
			if err != nil {
				t.Fatal(err)
			}
			resident := admission.Resident{ID: gateway.Resident.Name, Name: gateway.Resident.Name, GuardID: guardID, Sharing: "shared", State: "running", PID: 123, ProcessGroup: 123, Started: "original-start", Contexts: []string{contextID}}
			if fault == "resident-guard-changed" {
				resident.GuardID = "different-deployment"
			}
			if fault == "resident-extra-context" {
				resident.Contexts = append(resident.Contexts, strings.Repeat("e", 32))
			}
			if fault == "source-not-leased" {
				resident.Contexts = nil
			}
			residents := []admission.Resident{resident}
			if fault == "resident-missing" {
				residents = nil
			}
			directory := filepath.Join(root, "g", generation)
			if err := os.MkdirAll(directory, 0700); err != nil {
				t.Fatal(err)
			}
			cfg := []byte(fmt.Sprintf("[runtime.openshell]\nprofile=%q\n", profile))
			configPath := filepath.Join(root, "noodle.toml")
			if err := os.WriteFile(configPath, cfg, 0600); err != nil {
				t.Fatal(err)
			}
			receipt := hostReceipt{Schema: 1, Protocol: hostProtocol, HostPID: os.Getpid(), HostInstanceID: generation, ContextID: contextID, ProjectDir: root, ContextState: "failed", Mode: "manual", ConfigPath: configPath, ConfigSHA256: hostDigest(cfg), BinaryPath: "/original/noodle", BinarySHA256: "original-binary", RequestID: requestID, HostDir: directory}
			request := hostRequestBinding{Schema: 1, RequestID: requestID, Operation: "attach", ProjectDir: root, ConfigPath: configPath, ConfigSHA256: receipt.ConfigSHA256, Mode: receipt.Mode, BinarySHA256: receipt.BinarySHA256, EnvironmentSHA256: "original-environment", Status: "accepted", HostDir: root}
			if fault == "request-not-accepted" {
				request.Status = "pending"
			}
			requestPath := filepath.Join(root, "host-request.json")
			requestBytes := write(requestPath, request)
			historyPath := filepath.Join(directory, "receipts.json")
			historyBytes := write(historyPath, []hostReceipt{receipt})
			current := receipt
			current.ContextState = "active" // A historical state disagreement is not new execution.
			if fault == "pool-identity-changed" {
				current.ContextID = strings.Repeat("d", 32)
			}
			poolPath := filepath.Join(root, "pool.json")
			poolBytes := write(poolPath, hostPool{Schema: 1, Active: generation, Generations: map[string]hostGeneration{generation: {ID: generation, Directory: directory}}, Owners: map[string]hostTaskOwner{requestID: {Original: current, Current: current, State: "unknown"}}})
			if fault == "config-changed" {
				if err := os.WriteFile(configPath, []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if fault == "operator-already-explicit" {
				data, err := os.ReadFile(profile)
				if err != nil {
					t.Fatal(err)
				}
				var raw map[string]any
				if err = json.Unmarshal(data, &raw); err != nil {
					t.Fatal(err)
				}
				resident := raw["gateway"].(map[string]any)["resident"].(map[string]any)
				resident["argv"] = append(resident["argv"].([]any), "--enable-operator-auth=false")
				write(profile, raw)
			}
			listener, err := net.Listen("unix", filepath.Join(root, "host.sock"))
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				var rpc hostRequest
				if err = json.NewDecoder(conn).Decode(&rpc); err != nil {
					t.Error(err)
					return
				}
				if rpc.Operation != "resources" {
					t.Errorf("preparation attempted service effect: %s", rpc.Operation)
					return
				}
				instance, contexts := generation, 1
				if fault == "generation-changed" {
					instance = strings.Repeat("d", 32)
				}
				if fault == "context-missing" {
					contexts = 2
				}
				response := map[string]any{"protocol": hostProtocol, "host_root": root, "host_dir": directory, "host_instance_id": instance, "host_pid": receipt.HostPID, "rolling_host_version": 1, "binary_sha256": receipt.BinarySHA256, "contexts": contexts, "resources": map[string]any{"active_workers": 0}, "residents": map[string]any{"records": residents, "error": nil}}
				if err = json.NewEncoder(conn).Encode(response); err != nil {
					t.Error(err)
				}
			}()
			plan, err := planHostOperatorMaintenance(context.Background(), requestPath)
			if fault == "" {
				if err != nil {
					t.Fatal(err)
				}
				if plan.Executable || len(plan.Effects) != 0 || len(plan.Contexts) != 1 || plan.Contexts[0].ContextState != "failed" || plan.Binding.StateRoot == "" || plan.Resident.GuardID != guardID || len(plan.Requires) == 0 || strings.Join(plan.OperatorArgv[len(plan.OperatorArgv)-2:], " ") != "--enable-operator-auth true" {
					t.Fatalf("preparation lost its identity or effect boundary: %+v", plan)
				}
			} else if err == nil {
				t.Fatal("preparation accepted changed original input", fault)
			}
			if fault == "operator-already-explicit" && (err == nil || !strings.Contains(err.Error(), "already explicit")) {
				t.Fatal("explicit policy control did not reach its own refusal", err)
			}
			for path, before := range map[string][]byte{requestPath: requestBytes, historyPath: historyBytes, poolPath: poolBytes, custodyPath: custodyBytes} {
				after, err := os.ReadFile(path)
				if err != nil || string(after) != string(before) {
					t.Fatal("preparation changed original custody", path, err)
				}
			}
		})
	}
}
