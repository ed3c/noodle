package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/poteto/noodle/internal/admission"
	loopruntime "github.com/poteto/noodle/runtime"
)

func TestProgressMaintenanceRequiresTheOriginalPausedSoleHost(t *testing.T) {
	for _, kind := range []string{"original", "foreign", "directory", "missing-shutdown", "missing-accounting", "other-worker", "unpaused", "pending-control", "foreign-context", "foreign-project", "foreign-mode", "foreign-cleanup", "foreign-attempt"} {
		t.Run(kind, func(t *testing.T) {
			root := hostConfigDir(t)
			host, err := os.MkdirTemp("", "nh-maint-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(host) })
			runtimeDir := filepath.Join(root, ".noodle")
			if err := os.Mkdir(runtimeDir, 0700); err != nil {
				t.Fatal(err)
			}
			write := func(name string, value any) {
				t.Helper()
				data, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(name, data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			request := filepath.Join(host, "request.json")
			write(request, hostRequestBinding{Schema: 1, Status: "accepted", Operation: "attach", ProjectDir: root, HostDir: host, RequiredHostInstanceID: "original", RequestID: "request", BinarySHA256: "binary", ConfigSHA256: "config"})
			ref, err := loopruntime.ReadSessionReadbackFile(request, nil)
			if err != nil {
				t.Fatal(err)
			}
			p := loopruntime.ProgressMaintenance{Schema: 1, HostDir: host, HostInstance: "original", HostPID: 12345, ContextID: "context", Request: ref, SessionDirectory: filepath.Join(runtimeDir, "sessions", "session")}
			p.Resident = admission.Resident{ID: "gateway", Name: "gateway", PID: 44444, ProcessGroup: 44444, Started: "original-start", Fingerprint: "source", GuardID: "guard", State: "running", Contexts: []string{"context"}}
			if err := os.MkdirAll(p.SessionDirectory, 0700); err != nil {
				t.Fatal(err)
			}
			write(filepath.Join(p.SessionDirectory, "openshell-dispatch.json"), map[string]any{"ContextID": "context", "OrderID": "order", "SessionID": "session"})
			write(filepath.Join(runtimeDir, "state.snapshot.json"), map[string]any{"state": map[string]any{"orders": map[string]any{"order": map[string]any{"order_id": "order", "stages": []any{map[string]any{"runtime": "openshell", "attempts": []any{map[string]any{"session_id": "session", "status": "running"}}}}}}}})
			receipt := hostReceipt{HostInstanceID: "original", HostPID: 12345, ContextID: "context", RequestID: "request", ContextState: "active", BinarySHA256: "binary", ConfigSHA256: "config", ProjectDir: root, HostDir: host}
			if kind == "foreign-context" {
				receipt.ContextID = "foreign"
			}
			switch kind {
			case "foreign-project":
				receipt.ProjectDir = "/foreign"
			case "foreign-mode":
				receipt.Mode = "auto"
			case "foreign-cleanup":
				receipt.CleanupCompleted = true
			case "foreign-attempt":
				write(filepath.Join(p.SessionDirectory, "openshell-dispatch.json"), map[string]any{"ContextID": "context", "OrderID": "other", "SessionID": "session"})
			}
			write(filepath.Join(host, "receipts.json"), []hostReceipt{receipt})
			status := "paused"
			if kind == "unpaused" {
				status = "running"
			}
			write(filepath.Join(runtimeDir, "status.json"), map[string]any{"loop_state": status})
			if kind == "pending-control" {
				if err := os.WriteFile(filepath.Join(runtimeDir, "control.ndjson"), []byte("pending\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			resources := map[string]any{"active_workers": 1, "active_residents": 1, "active_allocations": 2, "unresolved_residents": 0}
			packet := map[string]any{"schema": 1, "protocol": hostProtocol, "host_dir": host, "host_pid": 12345, "host_instance_id": "original", "binary_sha256": "binary", "contexts": 1, "shutdown": map[string]any{"requested": false}, "resources": resources}
			packet["residents"] = map[string]any{"records": []any{p.Resident}, "error": nil}
			switch kind {
			case "foreign":
				packet["host_instance_id"] = "foreign"
			case "directory":
				packet["host_dir"] = "/other"
			case "missing-shutdown":
				packet["shutdown"] = map[string]any{}
			case "missing-accounting":
				delete(resources, "unresolved_residents")
			case "other-worker":
				resources["active_workers"] = 2
			}
			listener, err := net.Listen("unix", filepath.Join(host, "host.sock"))
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			done := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					done <- err
					return
				}
				defer conn.Close()
				var req hostRequest
				if err := json.NewDecoder(conn).Decode(&req); err != nil {
					done <- err
					return
				}
				if req.Operation != "resources" {
					done <- os.ErrInvalid
					return
				}
				done <- json.NewEncoder(conn).Encode(packet)
			}()
			err = progressMaintenanceHost(p)
			if (err != nil) != (kind != "original") {
				t.Fatalf("maintenance ownership gate: %v", err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestProgressMaintenanceIncompleteTransactionBlocksHost(t *testing.T) {
	host := t.TempDir()
	if err := verifyProgressMaintenanceTransactions(host); err != nil {
		t.Fatal(err)
	}
	session := filepath.Join(t.TempDir(), "original-session")
	p := loopruntime.ProgressMaintenance{Schema: 1, HostDir: host, SessionDirectory: session}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	plan := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(plan, data, 0600); err != nil {
		t.Fatal(err)
	}
	ref, err := loopruntime.ReadSessionReadbackFile(plan, nil)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(host, "progress-maintenance")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	data, err = json.Marshal(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, filepath.Base(session)+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyProgressMaintenanceTransactions(host); err == nil {
		t.Fatal("Host admitted an incomplete original maintenance transaction")
	}
}
