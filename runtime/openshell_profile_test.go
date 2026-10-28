package runtime

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/poteto/noodle/internal/admission"
)

func profileFixture(t *testing.T) (string, openShellProfileFile) {
	t.Helper()
	root := t.TempDir()
	write := func(name string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), data, 0700); err != nil {
			t.Fatal(err)
		}
	}
	security, _ := openShellTestTLS(t)
	write("ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: security.Certificate.Certificate[1]}))
	write("client.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: security.Certificate.Certificate[0]}))
	key, err := x509.MarshalPKCS8PrivateKey(security.Certificate.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	write("client.key", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}))
	write("gateway", []byte("fixture executable"))
	write("gateway.toml", []byte("driver = 'vm'"))
	write("spec.json", []byte(`{"template":{"image":"image@sha256:example"},"providers":["noodle-codex"],"command":["sleep","infinity"]}`))
	if err := os.Mkdir(filepath.Join(root, "payload"), 0700); err != nil {
		t.Fatal(err)
	}
	write("payload/codex", []byte("guest executable"))
	var f openShellProfileFile
	f.SchemaVersion = 1
	f.CLI = "gateway"
	f.GatewayName = "dedicated"
	f.PayloadDirectory = "payload"
	f.Routes = map[string]struct {
		Binary   string `json:"binary"`
		SpecFile string `json:"spec_file"`
	}{"codex": {Binary: "/sandbox/payload/codex", SpecFile: "spec.json"}}
	f.Gateway.Resident = admission.ResidentSpec{Name: "openshell", Argv: []string{filepath.Join(root, "gateway")}, Directory: root, Sharing: "shared", Transport: "network"}
	f.Gateway.Binding.StateRoot = "custody"
	f.Gateway.Binding.Gateway = "https://127.0.0.1:17671"
	f.Gateway.Binding.Workspace = "default"
	f.Gateway.Binding.Owner = "noodle"
	f.Gateway.TLS.CAFile = "ca.pem"
	f.Gateway.TLS.CertificateFile = "client.pem"
	f.Gateway.TLS.KeyFile = "client.key"
	f.Gateway.TLS.ServerName = "gateway.test"
	f.Gateway.Version = "0.1.5"
	f.Gateway.ComputeDriver = "vm"
	f.Gateway.ReadinessTimeoutSeconds = 10
	f.Gateway.IdentitySource = "gateway.toml"
	f.Gateway.ConfigFiles = []string{"gateway.toml"}
	f.Environment = []string{"XDG_CONFIG_HOME=" + filepath.Join(root, "client")}
	client := filepath.Join(root, "client", "openshell", "gateways", "dedicated")
	if err := os.MkdirAll(filepath.Join(client, "mtls"), 0700); err != nil {
		t.Fatal(err)
	}
	for source, dest := range map[string]string{"ca.pem": "ca.crt", "client.pem": "tls.crt", "client.key": "tls.key"} {
		data, err := os.ReadFile(filepath.Join(root, source))
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(client, "mtls", dest), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(client, "metadata.json"), []byte(`{"name":"dedicated","gateway_endpoint":"https://127.0.0.1:17671","auth_mode":"mtls"}`), 0600); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(root, "profile.json")
	writeProfileFixture(t, filename, f)
	return filename, f
}
func writeProfileFixture(t *testing.T, filename string, f openShellProfileFile) {
	t.Helper()
	data, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filename, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestOpenShellProfilePinsPolicyPayloadAndNativeConfiguration(t *testing.T) {
	filename, _ := profileFixture(t)
	p, err := LoadOpenShellProfile(filename)
	if err != nil {
		t.Fatal(err)
	}
	identitySource, err := filepath.EvalSymlinks(filepath.Join(filepath.Dir(filename), "gateway.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Gateway.Binding.ConfigSource != identitySource || p.Routes["codex"].Spec.GetProviders()[0] != "noodle-codex" {
		t.Fatal("profile binding lost")
	}
	if err = VerifyOpenShellPayload(p); err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(filename)
	for _, name := range []string{"gateway.toml", "spec.json", "payload/codex"} {
		original, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		changed := append(append([]byte(nil), original...), '\n')
		if err = os.WriteFile(filepath.Join(root, name), changed, 0700); err != nil {
			t.Fatal(err)
		}
		updated, err := LoadOpenShellProfile(filename)
		if err != nil {
			t.Fatal(err)
		}
		if updated.SourceSHA256 == p.SourceSHA256 {
			t.Fatalf("%s task drift not bound", name)
		}
		if (updated.Gateway.Binding.ConfigFingerprint == p.Gateway.Binding.ConfigFingerprint) == (name == "gateway.toml") {
			t.Fatalf("%s gateway/task boundary differs", name)
		}
		if VerifyOpenShellProfile(p) == nil {
			t.Fatalf("%s source drift accepted", name)
		}
		if name == "payload/codex" && VerifyOpenShellPayload(p) == nil {
			t.Fatal("payload drift accepted")
		}
		if err = os.WriteFile(filepath.Join(root, name), original, 0700); err != nil {
			t.Fatal(err)
		}
	}
}
func TestOpenShellProfileRefusesMalformedOrUnboundedConfiguration(t *testing.T) {
	for _, test := range []struct {
		name   string
		change func(*openShellProfileFile)
	}{
		{"schema", func(f *openShellProfileFile) { f.SchemaVersion = 2 }},
		{"missing route", func(f *openShellProfileFile) { f.Routes = nil }},
		{"outside payload", func(f *openShellProfileFile) {
			r := f.Routes["codex"]
			r.Binary = "/usr/local/bin/codex"
			f.Routes["codex"] = r
		}},
		{"force gateway", func(f *openShellProfileFile) { f.Gateway.Resident.ForceAfterGrace = true }},
		{"timeout", func(f *openShellProfileFile) { f.Gateway.ReadinessTimeoutSeconds = 31 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			filename, f := profileFixture(t)
			test.change(&f)
			writeProfileFixture(t, filename, f)
			if _, err := LoadOpenShellProfile(filename); err == nil {
				t.Fatal("invalid profile accepted")
			}
		})
	}
	filename, _ := profileFixture(t)
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, []byte(" {}")...)
	if err = os.WriteFile(filename, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = LoadOpenShellProfile(filename); err == nil || !strings.Contains(err.Error(), "trailing") {
		t.Fatalf("trailing content: %v", err)
	}
}
func TestOpenShellPayloadRejectsEscapingSymlink(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := openShellPayloadHash(root); err == nil {
		t.Fatal("outside symlink accepted")
	}
}

func TestOpenShellProfilesSeparateWorkerPolicyFromSharedGateway(t *testing.T) {
	filename, f := profileFixture(t)
	first, err := LoadOpenShellProfile(filename)
	if err != nil {
		t.Fatal(err)
	}
	spec := `{"template":{"image":"image@sha256:example"},"providers":["noodle-codex"],"command":["sleep","infinity"],"policy":{"networkPolicies":{"docs":{"endpoints":[{"host":"docs.example.invalid","port":443}]}}}}`
	if err = os.WriteFile(filepath.Join(filepath.Dir(filename), "second-spec.json"), []byte(spec), 0600); err != nil {
		t.Fatal(err)
	}
	route := f.Routes["codex"]
	route.SpecFile = "second-spec.json"
	f.Routes["codex"] = route
	secondFile := filepath.Join(filepath.Dir(filename), "second-task.json")
	writeProfileFixture(t, secondFile, f)
	second, err := LoadOpenShellProfile(secondFile)
	if err != nil {
		t.Fatal(err)
	}
	if first.Gateway.Binding != second.Gateway.Binding {
		t.Fatal("worker policy or profile path reconfigured shared gateway")
	}
	if first.SourceSHA256 == second.SourceSHA256 {
		t.Fatal("distinct worker sources lost")
	}
	a, err := OpenShellDispatchSHA256(first)
	if err != nil {
		t.Fatal(err)
	}
	b, err := OpenShellDispatchSHA256(second)
	if err != nil {
		t.Fatal(err)
	}
	if a == b {
		t.Fatal("distinct worker dispatch profiles not bound")
	}
	if got := second.Routes["codex"].Spec.GetPolicy().GetNetworkPolicies()["docs"].GetEndpoints()[0].GetHost(); got != "docs.example.invalid" {
		t.Fatalf("wrong selected worker policy: %s", got)
	}
	f.Gateway.Resident.Environment = []string{"DEPLOYMENT=changed"}
	writeProfileFixture(t, secondFile, f)
	changed, err := LoadOpenShellProfile(secondFile)
	if err != nil {
		t.Fatal(err)
	}
	if changed.Gateway.Binding == first.Gateway.Binding {
		t.Fatal("shared foreground contract drift accepted")
	}
}

func TestOpenShellProfileRequiresExplicitGatewayIdentitySource(t *testing.T) {
	for _, source := range []string{"", "spec.json"} {
		t.Run(source, func(t *testing.T) {
			filename, f := profileFixture(t)
			f.Gateway.IdentitySource = source
			writeProfileFixture(t, filename, f)
			if _, err := LoadOpenShellProfile(filename); err == nil {
				t.Fatal("missing or unrelated gateway identity source accepted")
			}
		})
	}
}
