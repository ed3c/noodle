package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/procx"
)

func sourceComputeLocalFixture(t *testing.T) (*hostOperatorNativeScope, []admission.ProcessObservation) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"sandboxes", "images/preparations"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	uid := os.Getuid()
	host := procx.NativeIdentity{PID: 10, UID: uid, ParentPID: 1, Group: 10}
	gateway := procx.NativeIdentity{PID: 20, UID: uid, ParentPID: 10, Group: 20}
	driver := procx.NativeIdentity{PID: 30, UID: uid, ParentPID: 20, Group: 20}
	scope := &hostOperatorNativeScope{StateDirectory: root, Host: hostOperatorActor{Process: host}, Gateway: hostOperatorActor{Process: gateway}, Driver: hostOperatorActor{Process: driver}}
	rows := []admission.ProcessObservation{}
	for _, actor := range []procx.NativeIdentity{host, gateway, driver} {
		rows = append(rows, admission.ProcessObservation{PID: actor.PID, UID: &uid, ParentPID: actor.ParentPID, ProcessGroup: actor.Group})
	}
	// Unrelated metadata/argv gaps do not become this Source's global gate.
	rows = append(rows, admission.ProcessObservation{PID: 100, ParentPID: 1, ProcessGroup: 100})
	return scope, rows
}

func TestSourceComputeLocalRequiresOriginalNamespaceAndNoOwnedProcesses(t *testing.T) {
	scope, rows := sourceComputeLocalFixture(t)
	if err := sourceComputeLocal(scope, rows); err != nil {
		t.Fatal("empty selected namespace refused", err)
	}
	for _, fault := range []string{"group", "descendant", "missing-actor", "lease", "sandbox", "indirect-parent"} {
		t.Run(fault, func(t *testing.T) {
			scope, rows := sourceComputeLocalFixture(t)
			switch fault {
			case "group":
				rows = append(rows, admission.ProcessObservation{PID: 40, ParentPID: 1, ProcessGroup: scope.Gateway.Process.Group})
			case "descendant":
				rows = append(rows, admission.ProcessObservation{PID: 40, ParentPID: 1, ProcessGroup: 40, AncestorPIDs: []int{scope.Driver.Process.PID}})
			case "missing-actor":
				rows = rows[1:]
			case "lease":
				if err := os.WriteFile(filepath.Join(scope.StateDirectory, "images/preparations/attempt.lease"), []byte("openshell-image-preparation-v1\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "sandbox":
				if err := os.Mkdir(filepath.Join(scope.StateDirectory, "sandboxes/original"), 0700); err != nil {
					t.Fatal(err)
				}
			case "indirect-parent":
				images := filepath.Join(scope.StateDirectory, "images")
				if err := os.Rename(images, images+"-original"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(images+"-original", images); err != nil {
					t.Fatal(err)
				}
			}
			if err := sourceComputeLocal(scope, rows); err == nil {
				t.Fatal("incomplete Source observation accepted", fault)
			}
		})
	}
}

func TestSourceMaintenanceStatusReadsAbsentJournalWithoutStartingIt(t *testing.T) {
	if !procx.NativeFixtureControlAvailable() {
		t.Skip("selected live process generation requires platform support")
	}
	p, _ := sourcePolicyTestPlan(t)
	t.Setenv("NOODLE_HOST_DIR", p.Root)
	path := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(path, sourcePolicyTestJSON(t, p), 0600); err != nil {
		t.Fatal(err)
	}
	cmd := newHostRestrictedCmd()
	cmd.SetArgs([]string{"--plan", path, "--status"})
	var output bytes.Buffer
	cmd.SetOut(&output)
	if err := cmd.ExecuteContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	var observed struct {
		JournalState string                   `json:"journal_state"`
		Journal      any                      `json:"journal"`
		Compute      sourceComputeObservation `json:"source_compute"`
	}
	if err := json.Unmarshal(output.Bytes(), &observed); err != nil || observed.JournalState != "absent" || observed.Journal != nil || observed.Compute.State != "unfenced" {
		t.Fatal("status inferred an execution or compute qualification", output.String(), err)
	}
	if _, err := os.Stat(filepath.Join(p.Root, "maintenance")); !os.IsNotExist(err) {
		t.Fatal("read-only status created a maintenance epoch", err)
	}
}
