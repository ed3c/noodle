package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/poteto/noodle/internal/loopview"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
)

func viewSuccessorFixture(t *testing.T) (string, hostViewBinding, hostViewBinding, []hostReceipt, string) {
	t.Helper()
	dir, root := t.TempDir(), t.TempDir()
	t.Setenv("NOODLE_HOST_DIR", dir)
	view := loopview.Empty()
	view.Runs = append(view.Runs, loopview.Run{ID: "run", Repository: root, Mode: "inner-only", Status: "source_observed", Stages: []loopview.Stage{}, Cards: []loopview.Card{}, Notes: []loopview.Note{}, Breakpoints: []loopview.Breakpoint{}})
	if _, _, err := loopview.Import(dir, view); err != nil {
		t.Fatal(err)
	}
	old := hostViewBinding{RunID: "run", OrderID: "order", Request: hostRequestBinding{Schema: 1, RequestID: "old-request", Operation: "attach", ProjectDir: root, ConfigSHA256: "config", BinarySHA256: "old-binary", HostDir: dir, Mode: "manual"}}
	next := old
	next.Request.RequestID = "new-request"
	next.Request.BinarySHA256 = "new-binary"
	path := filepath.Join(t.TempDir(), "request.json")
	if err := saveHostRequest(path, old.Request); err != nil {
		t.Fatal(err)
	}
	if err := runViewSuccessorBind(path, "order"); err != nil {
		t.Fatal(err)
	}
	if err := saveHostRequest(path, next.Request); err != nil {
		t.Fatal(err)
	}
	oldReceipt := hostReceipt{Schema: 1, Protocol: hostProtocol, HostPID: 1234, HostInstanceID: "old-host", ContextID: "old-context", ProjectDir: root, ContextState: "detached", Mode: "manual", ConfigSHA256: "config", BinaryPath: "/immutable/old", BinarySHA256: "old-binary", RequestID: "old-request", HostDir: dir}
	newReceipt := oldReceipt
	newReceipt.HostPID = 5678
	newReceipt.HostInstanceID = "new-host"
	newReceipt.ContextID = "new-context"
	newReceipt.ContextState = "active"
	newReceipt.BinaryPath = "/immutable/new"
	newReceipt.BinarySHA256 = "new-binary"
	newReceipt.RequestID = "new-request"
	return dir, old, next, []hostReceipt{oldReceipt, newReceipt}, path
}
func runViewSuccessorBind(path, order string) error {
	cmd := NewRootCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"host", "views", "bind", "--run-id", "run", "--request-file", path, "--order-id", order})
	return cmd.Execute()
}
func writeViewReceipts(t *testing.T, dir string, receipts []hostReceipt) []byte {
	t.Helper()
	data, err := json.Marshal(receipts)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "receipts.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	return data
}

func TestHostViewsSuccessorRetainsHistoryAndOldServerMap(t *testing.T) {
	dir, old, next, receipts, path := viewSuccessorFixture(t)
	before := writeViewReceipts(t, dir, receipts)
	// An interrupted bind may have written history before replacing the map.
	if err := retainHostViewSuccessor(dir, old, next); err != nil {
		t.Fatal(err)
	}
	// The already-running server has the original view and reloads only bindings.
	view, err := loopview.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := runViewSuccessorBind(path, "order"); err != nil {
			t.Fatal(err)
		}
	}
	bindings, err := loadViewBindings(dir)
	if err != nil || len(bindings) != 1 || bindings["run"] != next {
		t.Fatalf("map incompatible: %+v %v", bindings, err)
	}
	history, err := filepath.Glob(filepath.Join(dir, "loop-view-binding-history", "run", "*.json"))
	if err != nil || len(history) != 1 {
		t.Fatalf("history %v %v", history, err)
	}
	data, err := os.ReadFile(history[0])
	if err != nil {
		t.Fatal(err)
	}
	var saved hostViewSuccessor
	if err := json.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.Previous != old || saved.Successor != next || saved.PreviousReceipt == nil || *saved.PreviousReceipt != receipts[0] || saved.SuccessorReceipt == nil || *saved.SuccessorReceipt != receipts[1] {
		t.Fatal("lost original binding or receipts")
	}
	root := next.Request.ProjectDir
	if err := os.MkdirAll(filepath.Join(root, ".noodle"), 0700); err != nil {
		t.Fatal(err)
	}
	snapshot := reducer.DurableSnapshot{State: state.State{Orders: map[string]state.OrderNode{"order": {OrderID: "order", Status: state.OrderActive, Stages: []state.StageNode{{StageIndex: 0, Status: state.StageReview, Prompt: "new native readback"}}}}}}
	if err := reducer.WriteSnapshotAtomic(filepath.Join(root, ".noodle", "state.snapshot.json"), snapshot); err != nil {
		t.Fatal(err)
	}
	if err := enrichHostView(dir, &view, hostViewObservation{Receipts: receipts[1:]}); err != nil {
		t.Fatal(err)
	}
	if len(view.Runs[0].Stages) != 1 || view.Runs[0].Stages[0].Prompt != "new native readback" {
		t.Fatal("old server reader did not consume new binding")
	}
	after, err := os.ReadFile(filepath.Join(dir, "receipts.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("bind changed native receipt")
	}
	for _, name := range []string{"host.sock", "host.lock"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatal("bind started Host")
		}
	}
}

func TestHostViewsSuccessorRefusesForeignOrUnknownReceipt(t *testing.T) {
	for _, which := range []string{"order", "repository", "duplicate", "binary", "instance", "context"} {
		t.Run(which, func(t *testing.T) {
			dir, _, next, receipts, path := viewSuccessorFixture(t)
			order := "order"
			switch which {
			case "order":
				order = "foreign"
			case "repository":
				next.Request.ProjectDir = t.TempDir()
			case "config":
				next.Request.ConfigSHA256 = "foreign"
				receipts[1].ConfigSHA256 = "foreign"
			case "old-active":
				receipts[0].ContextState = "active"
			case "new-detached":
				receipts[1].ContextState = "detached"
			case "missing":
				receipts = receipts[1:]
			case "duplicate":
				receipts = append(receipts, receipts[0])
			case "binary":
				receipts[1].BinarySHA256 = "foreign"
			case "instance":
				receipts[1].HostInstanceID = ""
			case "context":
				receipts[1].ContextID = receipts[0].ContextID
			}
			if err := saveHostRequest(path, next.Request); err != nil {
				t.Fatal(err)
			}
			before, err := loadViewBindings(dir)
			if err != nil {
				t.Fatal(err)
			}
			originalReceipts := writeViewReceipts(t, dir, receipts)
			if err := runViewSuccessorBind(path, order); err == nil {
				t.Fatal("accepted foreign or unknown successor")
			}
			after, err := loadViewBindings(dir)
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("refusal changed bindings")
			}
			retained, err := os.ReadFile(filepath.Join(dir, "receipts.json"))
			if err != nil || !bytes.Equal(originalReceipts, retained) {
				t.Fatal("refusal changed native receipts")
			}
			if _, err := os.Stat(filepath.Join(dir, "loop-view-binding-history")); !os.IsNotExist(err) {
				t.Fatal("refusal archived an invalid successor")
			}
		})
	}
}

func TestHostViewsSuccessorDoesNotRequirePriorActorOrLiveContext(t *testing.T) {
	for _, scenario := range []string{"old-active", "new-detached", "old-missing", "receipts-missing", "config-changed"} {
		t.Run(scenario, func(t *testing.T) {
			dir, old, next, receipts, path := viewSuccessorFixture(t)
			switch scenario {
			case "old-active":
				receipts[0].ContextState = "active"
			case "new-detached":
				receipts[1].ContextState = "detached"
			case "old-missing":
				receipts = receipts[1:]
			case "config-changed":
				next.Request.ConfigSHA256 = "updated-config"
				receipts[1].ConfigSHA256 = "updated-config"
			}
			if scenario != "receipts-missing" {
				writeViewReceipts(t, dir, receipts)
			}
			if err := saveHostRequest(path, next.Request); err != nil {
				t.Fatal(err)
			}
			if err := runViewSuccessorBind(path, "order"); err != nil {
				t.Fatalf("retained source became actor eligibility: %v", err)
			}
			bindings, err := loadViewBindings(dir)
			if err != nil || bindings["run"] != next {
				t.Fatalf("selected successor lost: %+v %v", bindings, err)
			}
			paths, err := filepath.Glob(filepath.Join(dir, "loop-view-binding-history", "run", "*.json"))
			if err != nil || len(paths) != 1 {
				t.Fatalf("prior association not retained: %v %v", paths, err)
			}
			data, err := os.ReadFile(paths[0])
			if err != nil {
				t.Fatal(err)
			}
			var history hostViewSuccessor
			if err := json.Unmarshal(data, &history); err != nil || history.Previous != old {
				t.Fatalf("prior data lost: %+v %v", history, err)
			}
			if (scenario == "old-missing" || scenario == "receipts-missing") && len(history.Gaps) == 0 {
				t.Fatal("missing receipt was silently inferred")
			}
		})
	}
}
