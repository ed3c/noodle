package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGatewayProfileRetainsDeploymentBindingWithoutWorkerFiles(t *testing.T) {
	filename, _ := profileFixture(t)
	full, err := LoadOpenShellProfile(filename)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(filename)
	if err = os.RemoveAll(filepath.Join(root, "payload")); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(root, "spec.json")); err != nil {
		t.Fatal(err)
	}
	gateway, err := LoadOpenShellGatewaySpec(filename)
	if err != nil {
		t.Fatal(err)
	}
	if gateway.Binding != full.Gateway.Binding {
		t.Fatalf("gateway maintenance changed deployment identity: got %+v, want %+v", gateway.Binding, full.Gateway.Binding)
	}
	if _, err = LoadOpenShellProfile(filename); err == nil {
		t.Fatal("worker admission accepted missing payload and route specification")
	}
}

func TestGatewayProfileRetainsGatewayValidation(t *testing.T) {
	for _, dependency := range []string{"gateway", "gateway.toml", "ca.pem", "client.pem", "client.key", "client/openshell/gateways/dedicated/metadata.json"} {
		t.Run(dependency, func(t *testing.T) {
			filename, _ := profileFixture(t)
			if err := os.Remove(filepath.Join(filepath.Dir(filename), dependency)); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadOpenShellGatewaySpec(filename); err == nil {
				t.Fatalf("gateway maintenance accepted missing dependency %s", dependency)
			}
		})
	}
}

func TestGatewayProfileBindsPolicyAndRejectsClientMismatch(t *testing.T) {
	filename, profile := profileFixture(t)
	original, err := LoadOpenShellGatewaySpec(filename)
	if err != nil {
		t.Fatal(err)
	}
	profile.Gateway.Resident.Argv = append(profile.Gateway.Resident.Argv, "--enable-operator-auth", "true")
	writeProfileFixture(t, filename, profile)
	updated, err := LoadOpenShellGatewaySpec(filename)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Binding.ConfigFingerprint == original.Binding.ConfigFingerprint {
		t.Fatal("operator policy change did not change the deployment binding")
	}
	if updated.Binding.StateRoot != original.Binding.StateRoot || updated.Binding.Gateway != original.Binding.Gateway {
		t.Fatal("policy readback changed custody or endpoint")
	}
	metadata := filepath.Join(filepath.Dir(filename), "client", "openshell", "gateways", "dedicated", "metadata.json")
	if err = os.WriteFile(metadata, []byte(`{"name":"dedicated","gateway_endpoint":"https://127.0.0.1:1","auth_mode":"mtls"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadOpenShellGatewaySpec(filename); err == nil || !strings.Contains(err.Error(), "gateway") {
		t.Fatalf("gateway maintenance accepted mismatched native client: %v", err)
	}
}
