package runtime

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func templatePairingFixture(t *testing.T) (string, string, string, string) {
	t.Helper()
	template := goalProfileFixture(t)
	writableDirectories := func(root string) {
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err == nil && info.IsDir() {
				return os.Chmod(path, 0700)
			}
			return err
		})
	}
	t.Cleanup(func() { writableDirectories(filepath.Dir(template)) })
	profile, err := LoadOpenShellProfile(template)
	if err != nil {
		t.Fatal(err)
	}
	if err := filepath.Walk(profile.PayloadDirectory, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Chmod(path, 0555)
	}); err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { writableDirectories(root) })
	progress := filepath.Join(root, "noodle-progress")
	cmd := exec.CommandContext(t.Context(), "go", "build", "-o", progress, "./cmd/noodle-progress")
	cmd.Dir = ".."
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOARCH=arm64", "CGO_ENABLED=0", "GOMAXPROCS=2", "GOFLAGS=-p=1")
	if raw, err := cmd.CombinedOutput(); err != nil {
		t.Fatal(err, string(raw))
	}
	if err := os.Chmod(progress, 0555); err != nil {
		t.Fatal(err)
	}
	gateway := filepath.Join(root, "openshell-gateway")
	if err := os.WriteFile(gateway, []byte("#!/bin/sh\nprintf 'openshell-gateway fixture-native\\n'\n"), 0555); err != nil {
		t.Fatal(err)
	}
	manifest := func(binary string, native bool) string {
		t.Helper()
		sha, err := pairingDigestFile(binary)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(binary)
		if err != nil {
			t.Fatal(err)
		}
		var m templatePairingArtifact
		m.Schema, m.Source.Repository, m.Source.Head, m.Source.Tree, m.Source.Clean = 1, root, strings.Repeat("1", 40), strings.Repeat("2", 40), true
		b := &templatePairingBinary{Path: binary, SHA256: sha, Bytes: info.Size()}
		if native {
			b.Version = "openshell-gateway fixture-native"
			m.Gateway = b
		} else {
			b.ArchivePolicy = 2
			m.Progress = b
		}
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		path := binary + ".json"
		if err := os.WriteFile(path, raw, 0444); err != nil {
			t.Fatal(err)
		}
		return path
	}
	return template, root, manifest(gateway, true), manifest(progress, false)
}

func TestTemplatePairingPreservesOriginalAuthorityAndPayloadInodes(t *testing.T) {
	template, root, native, progress := templatePairingFixture(t)
	raw, err := os.ReadFile(template)
	if err != nil {
		t.Fatal(err)
	}
	var original map[string]json.RawMessage
	if err := json.Unmarshal(raw, &original); err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(original)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(template, raw, 0600); err != nil {
		t.Fatal(err)
	}
	before, err := LoadOpenShellProfile(template)
	if err != nil {
		t.Fatal(err)
	}
	paired, err := PairGoalOpenShellTemplate(t.Context(), filepath.Join(root, "pairs"), template, native, progress)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadOpenShellProfile(paired)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Routes["codex"].Binary != before.Routes["codex"].Binary || loaded.CredentialOrigin.BindingSHA256 != before.CredentialOrigin.BindingSHA256 || loaded.Gateway.Binding.Gateway != before.Gateway.Binding.Gateway || loaded.Gateway.Binding.StateRoot != before.Gateway.Binding.StateRoot || loaded.Gateway.Binding.Owner != before.Gateway.Binding.Owner || loaded.Gateway.Binding.Workspace != before.Gateway.Binding.Workspace || loaded.Gateway.Version != "fixture-native" {
		t.Fatal("pairing changed original model/credential/deployment authority")
	}
	originalInfo, err := os.Stat(filepath.Join(before.PayloadDirectory, "codex"))
	if err != nil {
		t.Fatal(err)
	}
	pairedInfo, err := os.Stat(filepath.Join(loaded.PayloadDirectory, "codex"))
	if err != nil || !os.SameFile(originalInfo, pairedInfo) {
		t.Fatal("immutable payload was copied", err)
	}
	var after map[string]json.RawMessage
	data, _ := os.ReadFile(paired)
	if err := json.Unmarshal(data, &after); err != nil {
		t.Fatal(err)
	}
	if string(after["environment"]) != string(original["environment"]) || string(after["progress_supervisor"]) != string(original["progress_supervisor"]) {
		t.Fatal("original command environment or progress consumer changed")
	}
	if again, err := PairGoalOpenShellTemplate(t.Context(), filepath.Join(root, "pairs"), template, native, progress); err != nil || again != paired {
		t.Fatal("pairing reentry changed original output", again, err)
	}
	if err := VerifyOpenShellProfile(before); err != nil {
		t.Fatal("old Frozen template changed", err)
	}
	for _, phase := range []string{"partial", "unknown"} {
		t.Run(phase, func(t *testing.T) {
			parent := filepath.Join(root, phase)
			path := filepath.Join(parent, filepath.Base(filepath.Dir(paired)))
			if phase == "partial" {
				path += ".partial"
			}
			if err := os.MkdirAll(path, 0700); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(path, "retain-unknown")
			if err := os.WriteFile(marker, []byte("original unknown output"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := PairGoalOpenShellTemplate(t.Context(), parent, template, native, progress); err == nil {
				t.Fatal("unknown or partial output was replaced")
			}
			if data, err := os.ReadFile(marker); err != nil || string(data) != "original unknown output" {
				t.Fatal("unknown output was changed", err)
			}
		})
	}
	host := filepath.Join(root, "new-host")
	if err := os.Mkdir(host, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := MaterializeGoalOpenShellProfile(paired, host, "127.0.0.1:12567"); err != nil {
		t.Fatal("original Goal materializer rejected paired template", err)
	}
}

func TestTemplatePairingRefusesPartialForeignTamperedOrWritableSources(t *testing.T) {
	template, root, native, progress := templatePairingFixture(t)
	if got, err := PairGoalOpenShellTemplate(t.Context(), "", template, "", ""); err != nil || got != template {
		t.Fatal("legacy NOOP changed", got, err)
	}
	for _, variant := range []string{"missing", "foreign-role", "binary-drift", "writable-payload", "unknown-source", "old-policy", "wrong-platform", "duplicate-manifest", "relative-manifest"} {
		t.Run(variant, func(t *testing.T) {
			gateway, guest := native, progress
			raw, err := os.ReadFile(progress)
			if err != nil {
				t.Fatal(err)
			}
			var m templatePairingArtifact
			if err := json.Unmarshal(raw, &m); err != nil {
				t.Fatal(err)
			}
			switch variant {
			case "missing":
				guest = ""
			case "relative-manifest":
				cwd, err := os.Getwd()
				if err != nil {
					t.Fatal(err)
				}
				guest, err = filepath.Rel(cwd, progress)
				if err != nil {
					t.Fatal(err)
				}
			case "foreign-role":
				gateway = progress
			case "binary-drift":
				m.Progress.SHA256 = strings.Repeat("0", 64)
			case "unknown-source":
				m.Source.Clean = false
			case "old-policy":
				m.Progress.ArchivePolicy = 1
			case "wrong-platform":
				nativeBytes, err := os.ReadFile(native)
				if err != nil {
					t.Fatal(err)
				}
				var n templatePairingArtifact
				if err := json.Unmarshal(nativeBytes, &n); err != nil {
					t.Fatal(err)
				}
				m.Progress = n.Gateway
				m.Progress.ArchivePolicy = 2
			case "duplicate-manifest":
				changed := filepath.Join(root, variant+".json")
				if err := os.WriteFile(changed, append([]byte(`{"schema":1,`), raw[1:]...), 0444); err != nil {
					t.Fatal(err)
				}
				guest = changed
			case "writable-payload":
				p, err := LoadOpenShellProfile(template)
				if err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(p.PayloadDirectory, "codex")
				if err := os.Chmod(path, 0755); err != nil {
					t.Fatal(err)
				}
				defer os.Chmod(path, 0555)
			}
			if variant == "binary-drift" || variant == "unknown-source" || variant == "old-policy" || variant == "wrong-platform" {
				changed := filepath.Join(root, variant+".json")
				bytes, _ := json.Marshal(m)
				if err := os.WriteFile(changed, bytes, 0444); err != nil {
					t.Fatal(err)
				}
				guest = changed
			}
			if _, err := PairGoalOpenShellTemplate(context.Background(), filepath.Join(root, variant), template, gateway, guest); err == nil {
				t.Fatal("unqualified template pairing accepted", variant)
			}
		})
	}
	paired, err := PairGoalOpenShellTemplate(t.Context(), filepath.Join(root, "sealed"), template, native, progress)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(paired, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := PairGoalOpenShellTemplate(t.Context(), filepath.Join(root, "sealed"), template, native, progress); err == nil {
		t.Fatal("writable published profile was accepted")
	}
}
