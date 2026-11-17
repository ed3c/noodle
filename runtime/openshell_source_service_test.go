package runtime

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
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

func TestSourcePolicyServiceUsesNewDeploymentWithoutRewritingHistoricalCustody(t *testing.T) {
	f, ref, _ := sourceServiceFixture(t, "original business uncertainty")
	filename, profile := profileFixture(t)
	root := filepath.Dir(filename)
	key, err := x509.MarshalPKCS8PrivateKey(f.spec.TLS.Certificate.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	materials := map[string][]byte{
		"ca.pem":     pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.spec.TLS.Certificate.Certificate[1]}),
		"client.pem": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.spec.TLS.Certificate.Certificate[0]}),
		"client.key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}),
	}
	for name, data := range materials {
		if err := os.WriteFile(filepath.Join(root, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for name, destination := range map[string]string{"ca.pem": "ca.crt", "client.pem": "tls.crt", "client.key": "tls.key"} {
		if err := os.WriteFile(filepath.Join(root, "client", "openshell", "gateways", "dedicated", "mtls", destination), materials[name], 0600); err != nil {
			t.Fatal(err)
		}
	}
	metadata := fmt.Sprintf(`{"name":"dedicated","gateway_endpoint":%q,"auth_mode":"mtls"}`, f.spec.Binding.Gateway)
	if err := os.WriteFile(filepath.Join(root, "client", "openshell", "gateways", "dedicated", "metadata.json"), []byte(metadata), 0600); err != nil {
		t.Fatal(err)
	}
	profile.Gateway.Resident = f.spec.Resident
	// The fixture testing executable ignores native CLI arguments after --.
	profile.Gateway.Resident.Argv = append(profile.Gateway.Resident.Argv, "--")
	profile.Gateway.Binding.StateRoot = f.spec.Binding.StateRoot
	profile.Gateway.Binding.Gateway = f.spec.Binding.Gateway
	profile.Gateway.Binding.Workspace = f.spec.Binding.Workspace
	profile.Gateway.Binding.Owner = f.spec.Binding.Owner
	profile.Gateway.TLS.ServerName = f.spec.TLS.ServerName
	profile.Gateway.Version, profile.Gateway.ComputeDriver = f.spec.Version, f.spec.ComputeDriver
	profile.Gateway.ReadinessTimeoutSeconds = 3
	writeProfileFixture(t, filename, profile)
	oldSpec, err := LoadOpenShellGatewaySpec(filename)
	if err != nil {
		t.Fatal(err)
	}
	var state OpenShellCustodyState
	if _, err := ReadSessionReadbackFile(ref.Path, &state); err != nil {
		t.Fatal(err)
	}
	state.Binding = oldSpec.Binding
	for i := range state.Records {
		state.Records[i].Worker.Identity.ConfigSource = oldSpec.Binding.ConfigSource
	}
	ref = storageFailureWrite(t, ref.Path, state)
	before, err := os.ReadFile(ref.Path)
	if err != nil {
		t.Fatal(err)
	}
	beforeProfile, err := ReadSessionReadbackFile(filename, nil)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := PrepareSourceOperatorPolicy(beforeProfile, filename, filepath.Join(t.TempDir(), "operator.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, fault := range []string{"schema", "binding", "source"} {
		changed := policy
		switch fault {
		case "schema":
			changed.Schema++
		case "binding":
			changed.BeforeBinding.ConfigFingerprint = "another-history"
		case "source":
			changed.BeforeSource = changed.After.Path
		}
		if owner, err := NewOpenShellSourcePolicyService(f.residents, f.budget, changed, ref, f.spec.ControlRoot); err == nil || owner != nil {
			t.Fatal("unbound Source policy accepted", fault, err)
		}
	}
	f.spawns(t, 0)
	o, err := NewOpenShellSourcePolicyService(f.residents, f.budget, policy, ref, f.spec.ControlRoot)
	f.owner = o
	if err != nil {
		t.Fatal(err)
	}
	observation, err := o.AcquireSourceService(t.Context(), "a")
	if err != nil {
		t.Fatal(err)
	}
	if observation.ServiceBinding != policy.AfterBinding || observation.HistoricalCustodyBinding != policy.BeforeBinding || o.custody.state.Binding != oldSpec.Binding {
		t.Fatal("historical binding substituted for actual Source deployment", observation)
	}
	oldSpec.ControlRoot = f.spec.ControlRoot
	oldGuard, err := OpenShellGatewayGuardID(oldSpec)
	if err != nil || oldGuard == observation.Resident.GuardID {
		t.Fatal("Source retained the old deployment guard", err)
	}
	if custody, err := o.Readback(t.Context()); err == nil || custody != nil || o.custody.initialized || o.ready {
		t.Fatal("Source policy factory yielded a worker")
	}
	after, err := os.ReadFile(ref.Path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("operator policy rewrote historical custody", err)
	}
	f.spawns(t, 1)
}

func TestSourceServiceHealthPreservesMissingBusinessHistory(t *testing.T) {
	for _, blocked := range []string{"", "original business uncertainty"} {
		t.Run(blocked, func(t *testing.T) {
			f, ref, before := sourceServiceFixture(t, blocked)
			o, err := newOpenShellGateway(f.residents, f.budget, f.spec, &sourceServiceReservation{ref, f.spec.Binding})
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
			o, err := newOpenShellGateway(f.residents, f.budget, f.spec, &sourceServiceReservation{ref, f.spec.Binding})
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
			o, err := newOpenShellGateway(f.residents, f.budget, f.spec, &sourceServiceReservation{ref, f.spec.Binding})
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
