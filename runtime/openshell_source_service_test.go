package runtime

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"google.golang.org/protobuf/proto"
)

func sourceServiceFixture(t *testing.T, blocked string) (*managedFixture, SessionReadbackFileRef, []byte) {
	t.Helper()
	f := newManagedFixture(t, true)
	if err := os.MkdirAll(f.spec.Binding.StateRoot, 0700); err != nil {
		t.Fatal(err)
	}
	var err error
	f.spec.Binding.StateRoot, err = filepath.EvalSymlinks(f.spec.Binding.StateRoot)
	if err != nil {
		t.Fatal(err)
	}
	f.spec.ControlRoot, err = filepath.EvalSymlinks(f.spec.ControlRoot)
	if err != nil {
		t.Fatal(err)
	}
	state := OpenShellCustodyState{Version: 4, ControlRoot: f.spec.ControlRoot, Binding: f.spec.Binding, Blocked: blocked}
	spec, err := proto.Marshal(&pb.SandboxSpec{Command: []string{"historical"}})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 33 {
		name := fmt.Sprintf("historical-%02d", i)
		state.Records = append(state.Records, OpenShellCustodyRecord{Name: name, Retained: true, Spec: spec, MainProcessInstanceID: name + "-main", Worker: OpenShellWorkerRecord{Identity: OpenShellIdentity{SandboxName: name, Sandbox: OpenShellSandboxRef{Gateway: f.spec.Binding.Gateway, Workspace: f.spec.Binding.Workspace, SandboxID: name + "-id"}, ConfigSource: f.spec.Binding.ConfigSource}, Boundary: OpenShellBoundaryUnknown}})
	}
	path := filepath.Join(f.spec.Binding.StateRoot, "owner.json")
	ref := storageFailureWrite(t, path, state)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return f, ref, raw
}

func TestSourceServiceHealthPreservesMissingBusinessHistory(t *testing.T) {
	for _, blocked := range []string{"", "original business uncertainty"} {
		t.Run(blocked, func(t *testing.T) {
			f, ref, before := sourceServiceFixture(t, blocked)
			o, err := NewOpenShellSourceService(f.residents, f.budget, f.spec, ref)
			f.owner = o
			if err != nil {
				t.Fatal(err)
			}
			if _, err := o.ReadSourceService(t.Context()); err == nil {
				t.Fatal("readback spawned a service")
			}
			observed, err := o.AcquireSourceService(t.Context(), "a")
			if err != nil || observed.Resident.State != "running" {
				t.Fatal(observed, err)
			}
			if _, err := o.ReadSourceService(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, err := o.AcquireSourceService(t.Context(), "a"); err == nil {
				t.Fatal("Source start was replayed")
			}
			if custody, err := o.Acquire(t.Context(), "b"); err == nil || custody != nil {
				t.Fatal("Source owner yielded worker custody")
			}
			if custody, err := o.Readback(t.Context()); err == nil || custody != nil {
				t.Fatal("Source owner promoted itself through worker readback")
			}
			if o.ready || o.custody.initialized || o.credentialCancel != nil {
				t.Fatal("Source service initialized a worker or credential delivery")
			}
			if _, err := o.Release("a"); err == nil {
				t.Fatal("Source service entered ordinary last-lease release")
			}
			if _, err := f.residents.Release(observed.Resident.ID, "a"); err == nil {
				t.Fatal("Host bypassed the Source service release guard")
			}
			f.retained(t, observed.Resident)
			after, err := os.ReadFile(ref.Path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("Source readiness changed business bytes", err)
			}
			events, err := os.ReadFile(filepath.Join(f.root, "events"))
			if err != nil || strings.Contains(string(events), "list\n") || strings.Contains(string(events), "get\n") {
				t.Fatal("Source readiness ran worker inventory", string(events), err)
			}
			f.spawns(t, 1)
		})
	}
}

func TestSourceServiceReadinessFailureKeepsBytesAndOriginalChild(t *testing.T) {
	for _, fault := range []string{"wrong-version", "wrong-driver", "stall"} {
		t.Run(fault, func(t *testing.T) {
			f, ref, before := sourceServiceFixture(t, "original business uncertainty")
			managedMarker(t, f.root, fault)
			o, err := NewOpenShellSourceService(f.residents, f.budget, f.spec, ref)
			f.owner = o
			if err != nil {
				t.Fatal(err)
			}
			if _, err := o.AcquireSourceService(t.Context(), "a"); err == nil {
				t.Fatal("unknown native readiness accepted", fault)
			}
			original := o.resident
			if _, err := o.AcquireSourceService(t.Context(), "a"); err == nil {
				t.Fatal("failed Source start was replayed")
			}
			if err := os.Remove(filepath.Join(f.root, fault)); err != nil {
				t.Fatal(err)
			}
			if _, err := o.ReadSourceService(t.Context()); err != nil {
				t.Fatal("original native service could not be read back", err)
			}
			f.retained(t, original)
			f.spawns(t, 1)
			after, err := os.ReadFile(ref.Path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("native failure wrote a business block", err)
			}
		})
	}
}

func TestSourceServiceConstructorRefusesWithoutBusinessWrites(t *testing.T) {
	for _, fault := range []string{"bytes", "binding", "control", "retained", "ledger", "v5"} {
		t.Run(fault, func(t *testing.T) {
			f, ref, _ := sourceServiceFixture(t, "original business uncertainty")
			switch fault {
			case "bytes":
				if err := os.WriteFile(ref.Path, []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			case "binding":
				f.spec.Binding.ConfigFingerprint = "other-policy"
			case "control":
				var err error
				f.spec.ControlRoot, err = filepath.EvalSymlinks(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
			case "retained":
				f.spec.Retained = []OpenShellRetainedSandbox{{Name: "foreign", SandboxID: "foreign-id", Workspace: f.spec.Binding.Workspace}}
			case "ledger", "v5":
				var state OpenShellCustodyState
				if _, err := ReadSessionReadbackFile(ref.Path, &state); err != nil {
					t.Fatal(err)
				}
				if fault == "ledger" {
					state.Records[0].Spec = []byte{255}
				} else {
					state.Version = 5
				}
				ref = storageFailureWrite(t, ref.Path, state)
			}
			before, err := os.ReadFile(ref.Path)
			if err != nil {
				t.Fatal(err)
			}
			o, err := NewOpenShellSourceService(f.residents, f.budget, f.spec, ref)
			f.owner = o
			if err == nil || o != nil {
				t.Fatal("Source constructor accepted invalid original binding", fault, err)
			}
			after, err := os.ReadFile(ref.Path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("Source constructor persisted a rejection", err)
			}
			f.spawns(t, 0)
		})
	}
}
