package runtime

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestGoalOpenShellProfileNativeGatewayMetadata(t *testing.T) {
	template := goalProfileFixture(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := MaterializeGoalOpenShellProfile(template, root, "127.0.0.1:12345")
	if err != nil {
		t.Fatal(err)
	}
	profile, err := LoadOpenShellProfile(receipt.Profile.Path)
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(root, "openshell", "client")
	filename := filepath.Join(config, "openshell", "gateways", profile.GatewayName, "metadata.json")
	raw, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	var metadata map[string]json.RawMessage
	if err := json.Unmarshal(raw, &metadata); err != nil {
		t.Fatal(err)
	}
	if string(metadata["is_remote"]) != "false" || string(metadata["gateway_port"]) != "12345" {
		t.Fatalf("native gateway metadata omits its local type or selected port: %s", raw)
	}
	t.Run("native-cli", func(t *testing.T) {
		cli := os.Getenv("NOODLE_TEST_OPENSHELL_CLI")
		if cli == "" {
			t.Skip("set NOODLE_TEST_OPENSHELL_CLI to the selected native executable")
		}
		list := func() bool {
			t.Helper()
			cmd := exec.Command(cli, "gateway", "list", "-o", "json")
			cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+config)
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("native gateway list: %v", err)
			}
			var gateways []struct {
				Name     string `json:"name"`
				Endpoint string `json:"endpoint"`
				Remote   bool   `json:"is_remote"`
			}
			if err := json.Unmarshal(out, &gateways); err != nil {
				t.Fatalf("native gateway list output: %v: %s", err, out)
			}
			for _, gateway := range gateways {
				if gateway.Name == profile.GatewayName {
					if gateway.Endpoint != receipt.Gateway || gateway.Remote {
						t.Fatalf("native gateway selection differs: %+v", gateway)
					}
					return true
				}
			}
			return false
		}
		if !list() {
			t.Fatal("native CLI did not recognize the generated gateway")
		}
		for _, test := range []struct{ key, value string }{{"is_remote", ""}, {"gateway_port", ""}, {"is_remote", `"false"`}, {"gateway_port", `"12345"`}} {
			value := metadata[test.key]
			if test.value == "" {
				delete(metadata, test.key)
			} else {
				metadata[test.key] = json.RawMessage(test.value)
			}
			invalid, err := json.Marshal(metadata)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filename, invalid, 0600); err != nil {
				t.Fatal(err)
			}
			if list() {
				t.Fatalf("native CLI accepted invalid %s=%q", test.key, test.value)
			}
			metadata[test.key] = value
		}
	})
}

func goalProfileFixture(t *testing.T) string {
	t.Helper()
	filename, f := profileFixture(t)
	configuration := []byte("[openshell]\nversion=2\n[openshell.gateway]\nname='dedicated'\nbind_address='127.0.0.1:17671'\ncompute_driver='vm'\n[openshell.drivers.vm]\ndriver_dir='/immutable/driver'\nstate_dir='/original/vm'\n")
	if err := os.WriteFile(filepath.Join(filepath.Dir(filename), "gateway.toml"), configuration, 0600); err != nil {
		t.Fatal(err)
	}
	f.Gateway.Resident.Argv = append(f.Gateway.Resident.Argv, "--config", f.Gateway.IdentitySource, "--tls-cert", "server.crt", "--tls-key", "server.key", "--tls-client-ca", f.Gateway.TLS.CAFile)
	f.Gateway.Resident.Environment = []string{"OPENSHELL_DB_URL=sqlite:///original/gateway.db", "XDG_STATE_HOME=/original/state"}
	writeProfileFixture(t, filename, f)
	return filename
}

func TestGoalOpenShellProfilesIsolateMutableStateAndReuseImmutablePayload(t *testing.T) {
	template := goalProfileFixture(t)
	original, err := os.ReadFile(template)
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	payload, err := filepath.EvalSymlinks(filepath.Join(filepath.Dir(template), "payload"))
	if err != nil {
		t.Fatal(err)
	}
	var receipts []GoalProfileReceipt
	for _, name := range []string{"host-a", "host-b"} {
		host := filepath.Join(root, name)
		if err = os.Mkdir(host, 0700); err != nil {
			t.Fatal(err)
		}
		receipt, e := MaterializeGoalOpenShellProfile(template, host, "")
		if e != nil {
			t.Fatal(e)
		}
		profile, e := LoadOpenShellProfile(receipt.Profile.Path)
		if e != nil {
			t.Fatal(e)
		}
		if profile.Gateway.Binding.StateRoot != filepath.Join(host, "openshell", "custody") || profile.Gateway.Resident.Directory != filepath.Join(host, "openshell") {
			t.Fatal("Goal Host shares mutable custody")
		}
		if len(profile.Gateway.Retained) != 0 {
			t.Fatal("new Host adopted old retained sandboxes")
		}
		if profile.PayloadDirectory != payload {
			t.Fatal("immutable payload was copied")
		}
		var config struct {
			OpenShell struct {
				Gateway struct {
					BindAddress string `toml:"bind_address"`
				}
				Drivers struct {
					VM struct {
						StateDir string `toml:"state_dir"`
					}
				}
			}
		}
		if _, e = toml.DecodeFile(profile.Gateway.Binding.ConfigSource, &config); e != nil {
			t.Fatal(e)
		}
		if config.OpenShell.Drivers.VM.StateDir != filepath.Join(host, "openshell", "vm") {
			t.Fatal("VM state directory was shared")
		}
		if e = VerifyOpenShellClient(profile); e != nil {
			t.Fatal(e)
		}
		again, e := MaterializeGoalOpenShellProfile(template, host, "")
		if e != nil || again.Profile != receipt.Profile || again.Gateway != receipt.Gateway {
			t.Fatalf("materialization replayed or changed: %+v %v", again, e)
		}
		receipts = append(receipts, receipt)
	}
	if receipts[0].Gateway == receipts[1].Gateway {
		t.Fatal("independent Host endpoints collided")
	}
	a, err := os.ReadFile(filepath.Join(root, "host-a", "openshell", "keys", "credential.key"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(root, "host-b", "openshell", "keys", "credential.key"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a, b) {
		t.Fatal("independent gateways share encryption keys")
	}
	current, err := os.ReadFile(template)
	if err != nil || !bytes.Equal(current, original) {
		t.Fatal("template was modified")
	}
}

func TestGoalOpenShellProfileRejectsChangedDependenciesAndForeignDestination(t *testing.T) {
	template := goalProfileFixture(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := MaterializeGoalOpenShellProfile(template, root, "")
	if err != nil {
		t.Fatal(err)
	}
	configuration := filepath.Join(filepath.Dir(receipt.Profile.Path), "gateway.toml")
	data, err := os.ReadFile(configuration)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(configuration, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = MaterializeGoalOpenShellProfile(template, root, ""); err == nil {
		t.Fatal("changed gateway config was accepted as original materialization")
	}
	foreign, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(filepath.Join(foreign, "openshell"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err = MaterializeGoalOpenShellProfile(template, foreign, ""); err == nil {
		t.Fatal("foreign existing destination was replaced")
	}
}

func TestGoalOpenShellProfileUsesSelectedEndpointAndReadbackNeverRecreates(t *testing.T) {
	template := goalProfileFixture(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	defer listener.Close()
	receipt, err := MaterializeGoalOpenShellProfile(template, root, address)
	if err != nil || receipt.Gateway != "https://"+address {
		t.Fatalf("selected endpoint changed: %+v %v", receipt, err)
	}
	if _, err := MaterializeGoalOpenShellProfile(template, root, "127.0.0.1:1"); err == nil {
		t.Fatal("existing profile accepted another endpoint")
	}
	readback, err := ReadGoalOpenShellProfile(root)
	if err != nil || readback.Profile != receipt.Profile || readback.Gateway != receipt.Gateway {
		t.Fatalf("original profile readback differs: %+v %v", readback, err)
	}
	if err := os.RemoveAll(filepath.Join(root, "openshell")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadGoalOpenShellProfile(root); err == nil {
		t.Fatal("missing original profile read back successfully")
	}
	if _, err := os.Stat(filepath.Join(root, "openshell")); !os.IsNotExist(err) {
		t.Fatalf("readback recreated the missing original profile: %v", err)
	}
}

func TestGoalOpenShellProfileRejectsUnselectedEndpoint(t *testing.T) {
	template := goalProfileFixture(t)
	for _, address := range []string{"127.0.0.1:0", "0.0.0.0:12345", "127.0.0.1:65536", "127.0.0.1:invalid"} {
		t.Run(address, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := MaterializeGoalOpenShellProfile(template, root, address); err == nil {
				t.Fatal("unselected endpoint accepted")
			}
			if _, err := os.Stat(filepath.Join(root, "openshell")); !os.IsNotExist(err) {
				t.Fatalf("invalid endpoint published a profile: %v", err)
			}
		})
	}
}

func TestGoalOpenShellProfileReadbackRejectsChangedReceiptEndpoint(t *testing.T) {
	template := goalProfileFixture(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := MaterializeGoalOpenShellProfile(template, root, "127.0.0.1:12345")
	if err != nil {
		t.Fatal(err)
	}
	receipt.Gateway = "https://127.0.0.1:12346"
	raw, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "openshell", "materialization.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadGoalOpenShellProfile(root); err == nil {
		t.Fatal("receipt endpoint differs from the original profile but read back successfully")
	}
}
