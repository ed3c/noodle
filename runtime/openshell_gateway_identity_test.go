package runtime

import (
	"slices"
	"testing"
)

func TestGatewayGuardIdentityRetainsDeploymentBoundaries(t *testing.T) {
	path, _ := profileFixture(t)
	spec, err := LoadOpenShellGatewaySpec(path)
	if err != nil {
		t.Fatal(err)
	}
	original, err := OpenShellGatewayGuardID(spec)
	if err != nil || original == "" {
		t.Fatal(original, err)
	}
	for _, field := range []string{"binding", "version", "driver", "server-name", "certificate"} {
		t.Run(field, func(t *testing.T) {
			changed := spec
			switch field {
			case "binding":
				changed.Binding.ConfigFingerprint = "different-policy"
			case "version":
				changed.Version += "-different"
			case "driver":
				changed.ComputeDriver += "-different"
			case "server-name":
				changed.TLS.ServerName += "-different"
			case "certificate":
				changed.TLS.Certificate.Certificate = slices.Clone(spec.TLS.Certificate.Certificate)
				changed.TLS.Certificate.Certificate[0] = []byte("different certificate")
			}
			id, err := OpenShellGatewayGuardID(changed)
			if err != nil || id == original {
				t.Fatal("guard reused a different deployment identity", id, err)
			}
		})
	}
}
