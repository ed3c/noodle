package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenShellClientSelectionRejectsOtherGatewayBeforeTransfer(t *testing.T) {
	filename, f := profileFixture(t)
	p, err := LoadOpenShellProfile(filename)
	if err != nil {
		t.Fatal(err)
	}
	if err = VerifyOpenShellClient(p); err != nil {
		t.Fatal(err)
	}
	metadata := filepath.Join(filepath.Dir(filename), "client", "openshell", "gateways", "dedicated", "metadata.json")
	original, err := os.ReadFile(metadata)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []struct{ old, new string }{{"17671", "17672"}, {"dedicated", "another"}, {"mtls", "none"}} {
		if err = os.WriteFile(metadata, []byte(strings.ReplaceAll(string(original), change.old, change.new)), 0600); err != nil {
			t.Fatal(err)
		}
		if err = VerifyOpenShellClient(p); err == nil {
			t.Fatal("client gateway selection drift accepted")
		}
		if _, err = LoadOpenShellProfile(filename); err == nil {
			t.Fatal("mismatched client binding admitted")
		}
	}
	if err = os.WriteFile(metadata, original, 0600); err != nil {
		t.Fatal(err)
	}
	f.Environment = nil
	writeProfileFixture(t, filename, f)
	if _, err = LoadOpenShellProfile(filename); err == nil {
		t.Fatal("implicit client configuration accepted")
	}
}
func TestOpenShellClientIgnoresUnselectedMutableMarkers(t *testing.T) {
	filename, _ := profileFixture(t)
	p, err := LoadOpenShellProfile(filename)
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(filepath.Dir(filename), "client", "openshell")
	for _, name := range []string{"active_gateway", "gateways/dedicated/last_sandbox"} {
		if err = os.WriteFile(filepath.Join(root, name), []byte("unrelated"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err = VerifyOpenShellClient(p); err != nil {
		t.Fatal(err)
	}
	updated, err := LoadOpenShellProfile(filename)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Gateway.Binding.ConfigFingerprint != p.Gateway.Binding.ConfigFingerprint {
		t.Fatal("unselected marker changed gateway binding")
	}
}
func TestOpenShellClientTrustAndKeyAreBound(t *testing.T) {
	for _, name := range []string{"ca.crt", "tls.crt", "tls.key"} {
		t.Run(name, func(t *testing.T) {
			filename, _ := profileFixture(t)
			p, err := LoadOpenShellProfile(filename)
			if err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(filepath.Dir(filename), "client", "openshell", "gateways", "dedicated", "mtls", name)
			if err = os.WriteFile(file, []byte("invalid"), 0600); err != nil {
				t.Fatal(err)
			}
			if err = VerifyOpenShellClient(p); err == nil {
				t.Fatal("changed client credentials accepted")
			}
		})
	}
}
