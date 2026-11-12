package runtime

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestGoalProfileKeepsOriginalCredentialOwnerAcrossHosts(t *testing.T) {
	template := goalProfileFixture(t)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"first", "replacement"} {
		host := filepath.Join(root, name)
		if err := os.Mkdir(host, 0700); err != nil {
			t.Fatal(err)
		}
		receipt, err := MaterializeGoalOpenShellProfile(template, host, "")
		if err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(receipt.Profile.Path)
		if err != nil {
			t.Fatal(err)
		}
		var profile struct {
			CredentialSource *struct {
				Gateway, Workspace string
				TLS                map[string]string
			} `json:"credential_source"`
		}
		if err := json.Unmarshal(data, &profile); err != nil {
			t.Fatal(err)
		}
		source := profile.CredentialSource
		if source == nil || source.Gateway != "https://127.0.0.1:17671" || source.Workspace != "default" || source.Gateway == receipt.Gateway {
			t.Fatal("Goal gateway lost or replaced the original login owner")
		}
		if source.TLS["key_file"] == "" || !filepath.IsAbs(source.TLS["key_file"]) || filepath.Dir(source.TLS["key_file"]) == filepath.Join(host, "openshell", "tls") {
			t.Fatal("credential source did not preserve original operator TLS")
		}
		template = receipt.Profile.Path
	}
}

func TestGoalCredentialSourcePinsTransportWithoutActivatingLegacyDelivery(t *testing.T) {
	template := goalProfileFixture(t)
	original, err := LoadOpenShellProfile(template)
	if err != nil {
		t.Fatal(err)
	}
	if original.Gateway.CredentialSource != nil {
		t.Fatal("original gateway unexpectedly receives external credentials")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := MaterializeGoalOpenShellProfile(template, root, "")
	if err != nil {
		t.Fatal(err)
	}
	selected, err := LoadOpenShellProfile(receipt.Profile.Path)
	if err != nil {
		t.Fatal(err)
	}
	if selected.Gateway.CredentialSource == nil || selected.CredentialOrigin.BindingSHA256 != original.CredentialOrigin.BindingSHA256 {
		t.Fatal("new gateway did not bind the original credential source")
	}
	key := filepath.Join(filepath.Dir(template), "client.key")
	data, err := os.ReadFile(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(key, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	changed, err := LoadOpenShellProfile(receipt.Profile.Path)
	if err != nil {
		t.Fatal(err)
	}
	if changed.CredentialOrigin.BindingSHA256 == selected.CredentialOrigin.BindingSHA256 || changed.Gateway.Binding.ConfigFingerprint == selected.Gateway.Binding.ConfigFingerprint || VerifyOpenShellProfile(selected) == nil {
		t.Fatal("credential source transport drift was not bound")
	}
}

func TestGoalCredentialSourceRejectsInvalidOrSelfEndpoint(t *testing.T) {
	for _, endpoint := range []string{"http://127.0.0.1:17671", "https://user@127.0.0.1:17671", "https://127.0.0.1:17671/path", "https://127.0.0.1:17671?token=invalid", "self"} {
		t.Run(endpoint, func(t *testing.T) {
			template := goalProfileFixture(t)
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			receipt, err := MaterializeGoalOpenShellProfile(template, root, "")
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(receipt.Profile.Path)
			if err != nil {
				t.Fatal(err)
			}
			var f openShellProfileFile
			if err := json.Unmarshal(raw, &f); err != nil {
				t.Fatal(err)
			}
			if endpoint == "self" {
				endpoint = receipt.Gateway
			}
			f.CredentialSource.Gateway = endpoint
			writeProfileFixture(t, receipt.Profile.Path, f)
			if _, err := LoadOpenShellProfile(receipt.Profile.Path); err == nil {
				t.Fatal("invalid credential source was admitted")
			}
		})
	}
}
