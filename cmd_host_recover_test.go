package main

import (
	"encoding/json"
	"github.com/poteto/noodle/internal/lockfile"
	"os"
	"path/filepath"
	"testing"
)

func TestUnstartedRecoveryPreservesFailureAndRefusesUncertainty(t *testing.T) {
	for _, scenario := range []string{"clean", "live_host", "session", "control", "attempt", "effect", "active", "binding"} {
		t.Run(scenario, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			dir := filepath.Join(root, ".noodle")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			host := t.TempDir()
			write := func(path string, value any) {
				t.Helper()
				data, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			request := hostRequestBinding{Schema: 1, RequestID: "request", Operation: "attach", ProjectDir: root, ConfigSHA256: hostDigest(nil), Mode: "manual", BinarySHA256: "old", HostDir: host}
			receipt := hostReceipt{Schema: 1, Protocol: hostProtocol, HostPID: 99999999, ContextID: "context", HostInstanceID: "instance", ProjectDir: root, ContextState: "failed", ConfigSHA256: request.ConfigSHA256, Mode: "manual", BinarySHA256: "old", RequestID: "request", HostDir: host}
			stage := map[string]any{"status": "pending"}
			entry := map[string]any{"effect_id": "e", "status": "done", "effect": map[string]any{"effect_id": "e", "type": "initial_admission", "payload": map[string]string{"order_id": "task"}}, "result": map[string]string{"effect_id": "e", "status": "completed"}}
			switch scenario {
			case "live_host":
				lock, err := lockfile.TryLock(filepath.Join(host, "host.lock"))
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
			case "session":
				os.MkdirAll(filepath.Join(dir, "sessions", "worker"), 0700)
			case "control":
				os.WriteFile(filepath.Join(dir, "control.ndjson"), []byte("{}"), 0600)
			case "attempt":
				stage["attempts"] = []any{map[string]string{"status": "failed"}}
			case "effect":
				entry["status"] = "pending"
			case "active":
				receipt.ContextState = "active"
			case "binding":
				request.BinarySHA256 = "changed"
			}
			write(filepath.Join(dir, "state.snapshot.json"), map[string]any{"state": map[string]any{"schema_version": 1, "orders": map[string]any{"task": map[string]any{"order_id": "task", "stages": []any{stage}}}}, "effect_ledger": []any{entry}})
			write(filepath.Join(dir, "pending-review.json"), map[string]any{"items": []any{}})
			write(filepath.Join(dir, "tickets.json"), []any{})
			requestPath := filepath.Join(root, "request.json")
			write(requestPath, request)
			write(filepath.Join(host, "receipts.json"), []hostReceipt{receipt})
			before, _ := os.ReadFile(filepath.Join(host, "receipts.json"))
			bindingBefore, _ := os.ReadFile(requestPath)
			got, err := recoverUnstarted(root, requestPath)
			after, _ := os.ReadFile(filepath.Join(host, "receipts.json"))
			bindingAfter, _ := os.ReadFile(requestPath)
			if string(bindingBefore) != string(bindingAfter) {
				t.Fatal("original binding changed")
			}
			if scenario != "clean" {
				if err == nil || string(before) != string(after) {
					t.Fatalf("unsafe recovery: %+v %v", got, err)
				}
				return
			}
			if err != nil || got.ContextState != "detached" {
				t.Fatalf("recovery: %+v %v", got, err)
			}
			var proof struct{ Original hostReceipt }
			b, err := os.ReadFile(filepath.Join(host, "unstarted-context.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(b, &proof); err != nil || proof.Original != receipt {
				t.Fatal("original failure not retained")
			}
			if got, err = recoverUnstarted(root, requestPath); err != nil || got.ContextState != "detached" {
				t.Fatalf("idempotent recovery %+v %v", got, err)
			}
		})
	}
}
