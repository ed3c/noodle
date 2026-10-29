package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func ownerBindingFixture(t *testing.T) (string, string, hostReceipt) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	dir := filepath.Join(root, "g", id)
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(root, "admission-noodle")
	if err = os.WriteFile(old, []byte("admitted executable"), 0500); err != nil {
		t.Fatal(err)
	}
	oldRef, err := hostReadbackBinaryRef(old)
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	currentRef, err := hostReadbackBinaryRef(exe)
	if err != nil {
		t.Fatal(err)
	}
	exe = currentRef.Path
	original := hostReceipt{Schema: 1, Protocol: hostProtocol, ProjectDir: root, HostDir: root, RequestID: "request", ConfigSHA256: "config", Mode: "manual", BinaryPath: old, BinarySHA256: oldRef.SHA256}
	current := original
	current.BinaryPath = exe
	current.BinarySHA256 = currentRef.SHA256
	current.HostInstanceID = id
	current.HostPID = 123
	current.ContextID = "context"
	current.ContextState = "active"
	binding := hostRequestBinding{Schema: 1, Operation: "attach", ProjectDir: root, HostDir: root, RequestID: "request", ConfigSHA256: "config", Mode: "manual", BinarySHA256: oldRef.SHA256, EnvironmentSHA256: "environment", Status: "accepted"}
	request := filepath.Join(root, "request.json")
	writeOwnerJSON(t, request, binding)
	writeOwnerJSON(t, filepath.Join(dir, "receipts.json"), []hostReceipt{current})
	err = updateHostPool(root, func(p *hostPool) error {
		p.Active = id
		oldID := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		p.Generations[oldID] = hostGeneration{ID: oldID, Directory: filepath.Join(root, "g", oldID), Binary: old, SHA: oldRef.SHA256, State: "exited"}
		p.Generations[id] = hostGeneration{ID: id, Directory: dir, Binary: exe, SHA: currentRef.SHA256, State: "active"}
		p.Owners["request"] = hostTaskOwner{Original: original, Current: current, State: "owned", EnvironmentSHA: "environment"}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return root, request, current
}
func writeOwnerJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}
func TestHostOwnerBindingSeparatesAdmissionAndExecution(t *testing.T) {
	root, request, current := ownerBindingFixture(t)
	before, err := os.ReadFile(request)
	if err != nil {
		t.Fatal(err)
	}
	got, err := readHostOwnerBinding(request)
	if err != nil {
		t.Fatal(err)
	}
	if got.HostRoot != root || got.Current.ContextID != current.ContextID || got.AdmissionBinary == got.ExecutionBinary || got.GenerationDir != filepath.Join(root, "g", current.HostInstanceID) {
		t.Fatalf("wrong binding: %+v", got)
	}
	client, err := originalHostTaskClient(root, request, "order", "session", "task_session_readback")
	if err != nil {
		t.Fatal(err)
	}
	if client.HostDir != got.GenerationDir || client.Request.AdmissionSHA != got.AdmissionBinary.SHA256 || client.Request.BinarySHA256 != got.ExecutionBinary.SHA256 {
		t.Fatalf("reader lost native owner: %+v", client)
	}
	after, _ := os.ReadFile(request)
	if string(before) != string(after) {
		t.Fatal("readback rewrote original admission")
	}
	if err = client.verify(); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "pool.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = client.verify(); err == nil {
		t.Fatal("changed pool accepted")
	}
}
func TestHostOwnerBindingRefusesForeignAndAmbiguousSources(t *testing.T) {
	for _, variant := range []string{"config", "environment", "generation", "missing", "duplicate", "admission-binary", "maintenance"} {
		t.Run(variant, func(t *testing.T) {
			root, request, current := ownerBindingFixture(t)
			if err := updateHostPool(root, func(p *hostPool) error {
				o := p.Owners["request"]
				switch variant {
				case "config":
					o.Current.ConfigSHA256 = "foreign"
				case "environment":
					o.EnvironmentSHA = "foreign"
				case "generation":
					o.Current.HostInstanceID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
				case "maintenance":
					o.MaintenanceHold = "pending"
				}
				p.Owners["request"] = o
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "g", current.HostInstanceID, "receipts.json")
			switch variant {
			case "missing":
				writeOwnerJSON(t, path, []hostReceipt{})
			case "duplicate":
				writeOwnerJSON(t, path, []hostReceipt{current, current})
			case "admission-binary":
				if err := os.Chmod(filepath.Join(root, "admission-noodle"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(root, "admission-noodle"), []byte("changed"), 0500); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := readHostOwnerBinding(request); err == nil {
				t.Fatal("foreign source accepted")
			}
		})
	}
}

func TestHostOwnerBindingReadsTerminalReceiptWithoutRewritingPool(t *testing.T) {
	root, request, current := ownerBindingFixture(t)
	current.ContextState = "detached"
	writeOwnerJSON(t, filepath.Join(root, "g", current.HostInstanceID, "receipts.json"), []hostReceipt{current})
	before, err := os.ReadFile(filepath.Join(root, "pool.json"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := readHostOwnerBinding(request)
	if err != nil {
		t.Fatal(err)
	}
	if got.Current.ContextState != "detached" {
		t.Fatal("terminal native receipt hidden behind pool monitor state")
	}
	after, _ := os.ReadFile(filepath.Join(root, "pool.json"))
	if string(before) != string(after) {
		t.Fatal("readback mutated pool")
	}
}
