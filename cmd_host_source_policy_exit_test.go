package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/procx"
)

func TestSourcePolicyStatusConsumesOriginalParentNormalExitWithoutEffects(t *testing.T) {
	if _, err := procx.ObserveNativeIdentity(os.Getpid()); err != nil {
		t.Skip("native parent generation observation unavailable")
	}
	for _, fault := range []string{"", "running", "nonzero", "absence", "cleared-history", "parent-instance", "parent-binary", "persist-error", "missing-persist-result", "parent-generation"} {
		t.Run(fault, func(t *testing.T) {
			p, _ := sourcePolicyTestPlan(t)
			op, err := readSourcePolicySnapshot(p.SourcePolicy.Snapshot)
			if err != nil {
				t.Fatal(err)
			}
			op.Resident = admission.Resident{ID: "source-gateway", Name: "source-gateway", PID: 77, ProcessGroup: 77, Started: "original child start", Fingerprint: "original deployment", GuardID: "original guard", Sharing: "shared", State: "running", Contexts: []string{op.Contexts[0].ContextID}, Error: "resident release guard refused: original execution unknown", Errors: []string{"resident release guard refused: original execution unknown"}}
			op.NativeScope.Gateway.Process.PID = op.Resident.PID
			if fault == "parent-generation" {
				op.NativeScope.Host.Process.StartSec--
			}
			snapshot := filepath.Join(p.Root, "exit-snapshot.json")
			if err = os.WriteFile(snapshot, sourcePolicyTestJSON(t, op), 0600); err != nil {
				t.Fatal(err)
			}
			p, err = prepareSourcePolicyHold(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(p.Root, "maintenance", p.PlanSHA)
			if err = os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err = saveRestrictedMaintenance(dir, restrictedMaintenance{Schema: 1, Plan: p, Phase: "source-policy-admission-held"}); err != nil {
				t.Fatal(err)
			}
			planPath := filepath.Join(p.Root, "plan.json")
			if err = os.WriteFile(planPath, sourcePolicyTestJSON(t, p), 0600); err != nil {
				t.Fatal(err)
			}
			journalPath := filepath.Join(dir, "journal.json")
			beforeJournal, err := os.ReadFile(journalPath)
			if err != nil {
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
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				var request hostRequest
				if json.NewDecoder(conn).Decode(&request) != nil || request.Operation != "resources" {
					t.Error("status attempted control instead of original resource readback")
					return
				}
				after := op.Resident
				after.State, after.Contexts = "exited", nil
				after.Released = append(slices.Clone(op.Resident.Released), op.Resident.Contexts...)
				if fault == "running" {
					after = op.Resident
				} else if fault == "nonzero" {
					after.Error = "resident exited with status 2"
					after.Errors = append(slices.Clone(after.Errors), after.Error)
				} else if fault == "absence" {
					after.Error = "original process and owned group absent; no adoption or signal"
				} else if fault == "cleared-history" {
					after.Error, after.Errors = "", nil
				}
				resident := map[string]any{"records": []admission.Resident{after}, "error": nil}
				if fault == "persist-error" {
					resident["error"] = "original persistence failed"
				} else if fault == "missing-persist-result" {
					delete(resident, "error")
				}
				response := map[string]any{"protocol": hostProtocol, "rolling_host_version": 1, "host_root": p.Root, "host_dir": generation, "host_pid": p.Source.PID, "host_instance_id": p.Source.Instance, "binary_sha256": op.NativeScope.Host.Binary.SHA256, "residents": resident}
				if fault == "parent-instance" {
					response["host_instance_id"] = "another instance"
				} else if fault == "parent-binary" {
					response["binary_sha256"] = "another binary"
				}
				if err = json.NewEncoder(conn).Encode(response); err != nil {
					t.Error(err)
				}
			}()
			t.Setenv("NOODLE_HOST_DIR", p.Root)
			var output bytes.Buffer
			cmd := newHostRestrictedCmd()
			cmd.SetOut(&output)
			cmd.SetArgs([]string{"--plan", planPath, "--status"})
			if err = cmd.ExecuteContext(context.Background()); err != nil {
				t.Fatal("original status consumer failed", err)
			}
			var result struct {
				Completion  sourcePolicyNormalExit `json:"source_gateway_normal_exit"`
				Effects     []string               `json:"effects"`
				SourceError string                 `json:"source_error"`
			}
			if err = json.Unmarshal(output.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if fault == "" && (result.Completion.State != "observed" || result.SourceError != "") {
				t.Fatal("normal exit with original diagnostics was lost", output.String())
			}
			if fault != "" && result.Completion.State == "observed" {
				t.Fatal("unproved exit passed original status", fault)
			}
			actual, err := os.ReadFile(journalPath)
			if err != nil || !bytes.Equal(actual, beforeJournal) || len(result.Effects) != 0 {
				t.Fatal("status changed the original journal or reported effects", err)
			}
		})
	}
}
