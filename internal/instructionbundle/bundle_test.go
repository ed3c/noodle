package instructionbundle

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func canonicalTemp(t *testing.T) string {
	t.Helper()
	p, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func mode(value uint32) *uint32 { return &value }

func fixture(t *testing.T) (string, Manifest) {
	t.Helper()
	root := canonicalTemp(t)
	m := Manifest{SchemaVersion: 1, OrderID: "order-1", Bindings: []Binding{{Kind: "selected_skill", Name: "build", Path: "skills/build/SKILL.md"}, {Kind: "support_skill", Name: "review", Path: "skills/review/SKILL.md"}, {Kind: "agent", Name: "agent-1", Path: "agents/agent-1/AGENTS.md"}, {Kind: "source", Name: "source-1", Path: "sources/source-1/input.bin"}}}
	for name, body := range map[string][]byte{"skills/build/SKILL.md": []byte("Read scripts/run.sh"), "skills/build/scripts/run.sh": []byte("#!/bin/sh\necho fixture\n"), "skills/review/SKILL.md": []byte("review instructions"), "agents/agent-1/AGENTS.md": []byte("project contract"), "sources/source-1/input.bin": {0, 255, 1, 128, 0}} {
		p := filepath.Join(root, "frozen", filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
			t.Fatal(err)
		}
		permissions := uint32(0644)
		if strings.HasSuffix(name, ".sh") {
			permissions = 0751
		}
		if err := os.WriteFile(p, body, os.FileMode(permissions)); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(p, os.FileMode(permissions)); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		permissions = uint32(info.Mode().Perm())
		m.Files = append(m.Files, File{Source: FileRef{Path: p, SHA256: digest(body)}, OriginalSource: FileRef{Path: "/original-no-longer-present/" + name, SHA256: digest(body)}, Destination: name, Mode: mode(permissions)})
	}
	return root, m
}

func writeManifest(t *testing.T, root string, m Manifest) FileRef {
	t.Helper()
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(root, "bundle.json")
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	return FileRef{Path: p, SHA256: digest(b)}
}

func TestPreparePreservesCompleteFrozenFilesAndGuestVerifyIgnoresHostSources(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture requires POSIX permissions")
	}
	root, m := fixture(t)
	ref := writeManifest(t, root, m)
	p, err := Prepare(t.Context(), ref, m.OrderID, root)
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(ref.Path)
	if err != nil {
		t.Fatal(err)
	}
	materialized, err := os.ReadFile(filepath.Join(p.Directory, ManifestName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(original, materialized) {
		t.Fatal("manifest bytes rewritten")
	}
	for _, f := range m.Files {
		before, err := os.ReadFile(f.Source.Path)
		if err != nil {
			t.Fatal(err)
		}
		after, err := os.ReadFile(filepath.Join(p.Directory, filepath.FromSlash(f.Destination)))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Fatalf("bytes changed: %s", f.Destination)
		}
		if err := os.Remove(f.Source.Path); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Verify(t.Context(), p.Directory, ref.SHA256, m.OrderID); err != nil {
		t.Fatalf("guest tried reading a host source: %v", err)
	}
	if !strings.Contains(p.PromptSection(), "# Materialized task instructions") || !strings.Contains(p.PromptSection(), p.GuestDirectory+"/skills/build/SKILL.md") {
		t.Fatal("guest mapping missing")
	}
	if _, err := Prepare(t.Context(), ref, m.OrderID, root); err == nil {
		t.Fatal("missing frozen source accepted")
	}
}

func TestManifestRefusesAmbiguousOrPrivateInputsBeforeMaterialization(t *testing.T) {
	tests := map[string]func(*Manifest){
		"wrong order":           func(m *Manifest) { m.OrderID = "other" },
		"wrong schema":          func(m *Manifest) { m.SchemaVersion = 2 },
		"missing mode":          func(m *Manifest) { m.Files[0].Mode = nil },
		"special mode":          func(m *Manifest) { m.Files[0].Mode = mode(04755) },
		"uppercase digest":      func(m *Manifest) { m.Files[0].Source.SHA256 = strings.ToUpper(m.Files[0].Source.SHA256) },
		"absolute destination":  func(m *Manifest) { m.Files[0].Destination = "/escape" },
		"parent traversal":      func(m *Manifest) { m.Files[0].Destination = "a/../escape" },
		"backslash traversal":   func(m *Manifest) { m.Files[0].Destination = "..\\escape" },
		"colon":                 func(m *Manifest) { m.Files[0].Destination = "dir/name:stream" },
		"control":               func(m *Manifest) { m.Files[0].Destination = "dir/line\nfile" },
		"segment space":         func(m *Manifest) { m.Files[0].Destination = "dir /file" },
		"segment dot":           func(m *Manifest) { m.Files[0].Destination = "dir./file" },
		"duplicate destination": func(m *Manifest) { m.Files = append(m.Files, m.Files[0]) },
		"case destination": func(m *Manifest) {
			f := m.Files[0]
			f.Destination = strings.ToUpper(f.Destination)
			m.Files = append(m.Files, f)
		},
		"ancestor collision":       func(m *Manifest) { f := m.Files[0]; f.Destination = "skills"; m.Files = append(m.Files, f) },
		"directory case collision": func(m *Manifest) { f := m.Files[0]; f.Destination = "Skills/other/file"; m.Files = append(m.Files, f) },
		"reserved metadata":        func(m *Manifest) { m.Files[0].Destination = "MANIFEST.JSON/child" },
		"alias collision":          func(m *Manifest) { m.Bindings[1].Name = "BUILD" },
		"unicode alias collision":  func(m *Manifest) { m.Bindings[0].Name = "Straße"; m.Bindings[1].Name = "STRASSE" },
		"binding case mismatch":    func(m *Manifest) { m.Bindings[0].Path = "SKILLS/build/SKILL.md" },
		"unknown kind":             func(m *Manifest) { m.Bindings[0].Kind = "extension" },
		"no selected":              func(m *Manifest) { m.Bindings[0].Kind = "support_skill" },
		"multiple selected":        func(m *Manifest) { m.Bindings[1].Kind = "selected_skill" },
		"skill wrong entry":        func(m *Manifest) { m.Bindings[0].Path = "skills/build/scripts/run.sh" },
		"too many files":           func(m *Manifest) { m.Files = make([]File, MaxFiles+1) },
	}
	for _, private := range []string{".git/config", ".SSH/id", ".gnupg/pub", ".AWS/config", ".env", ".ENV.example", "x/auth.JSON", "x/credentials", "x/credentials.json", "x/credentials.toml", "x/file.PEM", "x/file.KEY"} {
		for _, field := range []string{"source", "original_source", "destination"} {
			name := field + " " + private
			tests[name] = func(m *Manifest) {
				switch field {
				case "source":
					m.Files[0].Source.Path = "/frozen/" + private
				case "original_source":
					m.Files[0].OriginalSource.Path = "/original/" + private
				default:
					m.Files[0].Destination = private
				}
			}
		}
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			root, m := fixture(t)
			change(&m)
			ref := writeManifest(t, root, m)
			if _, err := Prepare(t.Context(), ref, "order-1", root); err == nil {
				t.Fatal("unsafe manifest accepted")
			}
			if _, err := os.Lstat(filepath.Join(root, "instructions-"+ref.SHA256)); !os.IsNotExist(err) {
				t.Fatal("rejected manifest wrote materialized files")
			}
		})
	}
}

func TestManifestRejectsDuplicateUnknownAndMalformedMetadata(t *testing.T) {
	root, m := fixture(t)
	ref := writeManifest(t, root, m)
	valid, err := os.ReadFile(ref.Path)
	if err != nil {
		t.Fatal(err)
	}
	modeJSON := []byte(fmt.Sprintf(`"mode":%d`, *m.Files[0].Mode))
	for name, body := range map[string][]byte{
		"duplicate root":   bytes.Replace(valid, []byte(`"schema_version":1`), []byte(`"schema_version":1,"schema_version":1`), 1),
		"duplicate nested": bytes.Replace(valid, []byte(`"mode":`), []byte(`"mode":384,"mode":`), 1),
		"unknown":          append([]byte(`{"surprise":true,`), valid[1:]...),
		"case root key":    bytes.Replace(valid, []byte(`"schema_version"`), []byte(`"SCHEMA_VERSION"`), 1),
		"case file key":    bytes.Replace(valid, []byte(`"mode"`), []byte(`"MODE"`), 1),
		"trailing":         append(append([]byte{}, valid...), []byte(` {}`)...),
		"negative mode":    bytes.Replace(valid, modeJSON, []byte(`"mode":-1`), 1),
		"fractional mode":  bytes.Replace(valid, modeJSON, []byte(`"mode":420.5`), 1),
	} {
		t.Run(name, func(t *testing.T) {
			if bytes.Equal(body, valid) {
				t.Fatal("fixture mutation did not apply")
			}
			if _, err := decodeManifest(body, digest(body), m.OrderID); err == nil {
				t.Fatal("ambiguous manifest accepted")
			}
		})
	}
	if privatePath("/project/.codex/instructions.md") {
		t.Fatal("entire .codex directory was forbidden")
	}
}

func TestFrozenAndGuestFileDriftRefused(t *testing.T) {
	for _, where := range []string{"source", "guest"} {
		for _, change := range []string{"bytes", "mode", "missing", "symlink", "ancestor symlink", "extra"} {
			t.Run(where+"/"+change, func(t *testing.T) {
				if runtime.GOOS == "windows" {
					t.Skip("POSIX modes/symlinks")
				}
				root, m := fixture(t)
				ref := writeManifest(t, root, m)
				file := m.Files[0]
				name := file.Source.Path
				var p *Prepared
				if where == "guest" {
					var err error
					p, err = Prepare(t.Context(), ref, m.OrderID, root)
					if err != nil {
						t.Fatal(err)
					}
					name = filepath.Join(p.Directory, filepath.FromSlash(file.Destination))
				}
				switch change {
				case "bytes":
					if err := os.WriteFile(name, []byte("drift"), 0600); err != nil {
						t.Fatal(err)
					}
				case "mode":
					if err := os.Chmod(name, 0700); err != nil {
						t.Fatal(err)
					}
				case "missing":
					if err := os.Remove(name); err != nil {
						t.Fatal(err)
					}
				case "symlink":
					if err := os.Remove(name); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(ref.Path, name); err != nil {
						t.Fatal(err)
					}
				case "ancestor symlink":
					dir := filepath.Dir(name)
					if err := os.Rename(dir, dir+"-moved"); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(dir+"-moved", dir); err != nil {
						t.Fatal(err)
					}
				case "extra":
					if where == "source" {
						t.Skip("source authority is the file list, not a directory scan")
					}
					if err := os.WriteFile(filepath.Join(p.Directory, "extra"), []byte("extra"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				var err error
				if where == "source" {
					_, err = Prepare(t.Context(), ref, m.OrderID, root)
				} else {
					_, err = Verify(t.Context(), p.Directory, ref.SHA256, m.OrderID)
				}
				if err == nil {
					t.Fatal("drift accepted")
				}
			})
		}
	}
}

func TestBoundsAndManifestDrift(t *testing.T) {
	for _, kind := range []string{"manifest hash", "manifest bytes", "file bytes", "total bytes", "canceled", "manifest symlink", "parent symlink"} {
		t.Run(kind, func(t *testing.T) {
			root, m := fixture(t)
			if kind == "file bytes" {
				f, err := os.OpenFile(m.Files[0].Source.Path, os.O_WRONLY, 0600)
				if err != nil {
					t.Fatal(err)
				}
				if err := f.Truncate(MaxFileBytes + 1); err != nil {
					t.Fatal(err)
				}
				_ = f.Close()
			}
			if kind == "total bytes" {
				m.Files = nil
				for i := 0; i < 5; i++ {
					name := filepath.Join(root, string(rune('a'+i)))
					body := make([]byte, MaxFileBytes)
					if err := os.WriteFile(name, body, 0600); err != nil {
						t.Fatal(err)
					}
					dest := "skills/" + string(rune('a'+i)) + "/SKILL.md"
					m.Files = append(m.Files, File{Source: FileRef{name, digest(body)}, OriginalSource: FileRef{"/original/file", digest(body)}, Destination: dest, Mode: mode(0600)})
				}
				m.Bindings = []Binding{{Kind: "selected_skill", Name: "main", Path: m.Files[0].Destination}}
			}
			ref := writeManifest(t, root, m)
			switch kind {
			case "manifest hash":
				ref.SHA256 = strings.Repeat("0", 64)
			case "manifest bytes":
				if err := os.WriteFile(ref.Path, make([]byte, MaxManifestBytes+1), 0600); err != nil {
					t.Fatal(err)
				}
			case "manifest symlink":
				if runtime.GOOS == "windows" {
					t.Skip("symlink creation")
				}
				original := ref.Path
				ref.Path += "-link"
				if err := os.Symlink(original, ref.Path); err != nil {
					t.Fatal(err)
				}
			case "parent symlink":
				if runtime.GOOS == "windows" {
					t.Skip("symlink creation")
				}
				link := filepath.Join(root, "parent-link")
				if err := os.Symlink(root, link); err != nil {
					t.Fatal(err)
				}
				root = link
			}
			ctx := t.Context()
			if kind == "canceled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if _, err := Prepare(ctx, ref, m.OrderID, root); err == nil {
				t.Fatal("invalid input accepted")
			}
		})
	}
}
