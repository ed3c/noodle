package guestprogress

import (
	"archive/tar"
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/poteto/noodle/internal/rebuildable"
)

func testArchivePolicy(t *testing.T) string {
	t.Helper()
	proofs, err := rebuildable.Dependencies(map[string]bool{"package.json": true, "pnpm-lock.yaml": true})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(ArchivePolicy{Schema: 1, Dependencies: proofs})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestQualifiedArchiveSkipsLateDependenciesPreservingChangedSources(t *testing.T) {
	root, output := canonicalTemp(t), canonicalTemp(t)
	policy, err := ParseArchivePolicy(testArchivePolicy(t))
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"package.json": `{"dependencies":{"updated":"2"}}`, "pnpm-lock.yaml": "new legitimate lockfile bytes\n", "program": "new program\n", "dist/required.bin": "required product output", "node_modules/dependency": strings.Repeat("unread dependency", 1000)} {
		full := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(data), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("program", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	excluded, actual, err := repositoryExclusions(root, policy)
	if err != nil || len(actual) != 1 || !excluded["node_modules"] {
		t.Fatal("late dependencies were not qualified", actual, err)
	}
	archive, err := archiveTree(root, output, "noodle-result.tar", excluded)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(output, archive.Name))
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	reader := tar.NewReader(bytes.NewReader(raw))
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(header.Name, "node_modules") {
			t.Fatal("dependency contents entered immutable archive")
		}
		seen[header.Name] = true
		body, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == "pnpm-lock.yaml" && string(body) != "new legitimate lockfile bytes\n" {
			t.Fatal("legitimate lockfile update was lost")
		}
		if header.Name == "program" && header.Mode&0111 == 0 {
			t.Fatal("executable mode was lost")
		}
		if header.Name == "link" && (header.Typeflag != tar.TypeSymlink || header.Linkname != "program") {
			t.Fatal("source link changed")
		}
	}
	for _, name := range []string{"package.json", "pnpm-lock.yaml", "program", "link", "dist/required.bin"} {
		if !seen[name] {
			t.Fatal("required source or output omitted", name)
		}
	}
}

func TestArchivePolicyMissingProofPreservesAndAliasRefuses(t *testing.T) {
	for _, variant := range []string{"missing-lock", "missing-manifest", "alias-dependency", "alias-manifest", "legacy"} {
		t.Run(variant, func(t *testing.T) {
			root := canonicalTemp(t)
			for _, name := range []string{"package.json", "pnpm-lock.yaml"} {
				if err := os.WriteFile(filepath.Join(root, name), []byte("changed source"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Mkdir(filepath.Join(root, "node_modules"), 0700); err != nil {
				t.Fatal(err)
			}
			policy, _ := ParseArchivePolicy(testArchivePolicy(t))
			switch variant {
			case "missing-lock":
				_ = os.Remove(filepath.Join(root, "pnpm-lock.yaml"))
			case "missing-manifest":
				_ = os.Remove(filepath.Join(root, "package.json"))
			case "alias-dependency", "alias-manifest":
				name := "node_modules"
				if variant == "alias-manifest" {
					name = "package.json"
				}
				if err := os.Remove(filepath.Join(root, name)); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(t.TempDir(), filepath.Join(root, name)); err != nil {
					t.Fatal(err)
				}
			case "legacy":
				policy = nil
			}
			excluded, _, err := repositoryExclusions(root, policy)
			if strings.HasPrefix(variant, "alias") {
				if err == nil {
					t.Fatal("alias acquired rebuild exclusion")
				}
			} else if err != nil || len(excluded) != 0 {
				t.Fatal("missing proof silently discarded dependencies", excluded, err)
			}
		})
	}
}

func TestArchivePolicyReceiptRequiresActualNewProducer(t *testing.T) {
	encoded := testArchivePolicy(t)
	sha, err := ArchivePolicySHA256(encoded)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := fixtureIdentity()
	result := Receipt{Schema: 2, Identity: id, Status: "archived", ChildrenReaped: true, ArchivePolicySHA256: sha, ExcludedDependencies: []string{"node_modules"}, Archives: []Archive{{Name: "noodle-result.tar", Bytes: 1024, SHA256: strings.Repeat("a", 64)}, {Name: "noodle-session.tar", Bytes: 1024, SHA256: strings.Repeat("b", 64)}}}
	if err := ValidateReceiptForPolicy(result, id, "", encoded); err != nil {
		t.Fatal(err)
	}
	for _, variant := range []string{"old-binary", "changed-policy", "foreign-exclusion", "unexpected-new-policy"} {
		changed, policy := result, encoded
		switch variant {
		case "old-binary":
			changed.Schema, changed.ArchivePolicySHA256, changed.ExcludedDependencies = 1, "", nil
		case "changed-policy":
			changed.ArchivePolicySHA256 = strings.Repeat("c", 64)
		case "foreign-exclusion":
			changed.ExcludedDependencies = []string{"dist"}
		case "unexpected-new-policy":
			policy = ""
		}
		if err := ValidateReceiptForPolicy(changed, id, "", policy); err == nil {
			t.Fatal("unconsumed or foreign policy accepted", variant)
		}
	}
	for _, invalid := range []string{encoded + " ", strings.Replace(encoded, "node_modules", "../node_modules", 1), strings.Replace(encoded, `"schema":1`, `"schema":9`, 1)} {
		if _, err := ParseArchivePolicy(invalid); err == nil {
			t.Fatal("invalid policy accepted", invalid)
		}
	}
}
