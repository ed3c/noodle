package skill

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

func TestTreeSHA256MatchesSortedUTF8JSONManifest(t *testing.T) {
	root := t.TempDir()
	references := filepath.Join(root, "references")
	if err := os.Mkdir(references, 0o755); err != nil {
		t.Fatal(err)
	}
	main := []byte("# Entry")
	ref := []byte("method")
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), main, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(references, "a.md"), ref, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := TreeSHA256(root)
	if err != nil {
		t.Fatal(err)
	}
	m := sha256.Sum256(main)
	r := sha256.Sum256(ref)
	// Equivalent to Python's:
	// json.dumps([[path, sha256], ...], sort_keys=True, ensure_ascii=False,
	// separators=(",", ":")).encode("utf-8")
	rows := fmt.Sprintf("[[\"SKILL.md\",\"%x\"],[\"references/a.md\",\"%x\"]]", m, r)
	want := sha256.Sum256([]byte(rows))
	if got != hex.EncodeToString(want[:]) {
		t.Fatalf("tree digest mismatches normalized manifest: %s", got)
	}
	repeated, err := TreeSHA256(root)
	if err != nil || repeated != got {
		t.Fatalf("same-source recheck is not stable: %v %s", err, repeated)
	}
}

func TestTreeSHA256RejectsSymlinkAndAbsentSKILL(t *testing.T) {
	root := t.TempDir()
	if _, err := TreeSHA256(root); err == nil {
		t.Fatal("absent SKILL.md accepted")
	}
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte("entry"), 0o644); err != nil {
		t.Fatal(err)
	}
	foreign := t.TempDir()
	if err := os.WriteFile(filepath.Join(foreign, "escape.md"), []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(foreign, "escape.md"), filepath.Join(root, "references.md")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	if _, err := TreeSHA256(root); err == nil {
		t.Fatal("symlink outside selected Skill was accepted")
	}
}

func TestTreeSHA256RejectsOversizedFile(t *testing.T) {
	root := t.TempDir()
	payload := make([]byte, 2_000_001)
	if err := os.WriteFile(filepath.Join(root, "SKILL.md"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := TreeSHA256(root); err == nil {
		t.Fatal("oversized Skill file accepted")
	}
}

func TestTreeSHA256RejectsInvalidUTF8Paths(t *testing.T) {
	for _, name := range []string{"reference-\xff.md", "reference-\xfe.md"} {
		t.Run(fmt.Sprintf("%x", []byte(name)), func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "SKILL.md"), []byte("entry"), 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, name), []byte("method"), 0o644); err != nil {
				if runtime.GOOS == "darwin" && errors.Is(err, syscall.EILSEQ) {
					t.Skipf("filesystem could not create invalid UTF-8 filename bytes %x: %v", []byte(name), err)
				}
				t.Fatal(err)
			}
			entries, err := os.ReadDir(root)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, entry := range entries {
				if entry.Name() == name {
					found = true
				}
			}
			if !found {
				t.Fatalf("filesystem changed filename bytes %x", []byte(name))
			}
			digest, err := TreeSHA256(root)
			t.Logf("filesystem filename bytes %x, tree digest %s, error %v", []byte(name), digest, err)
			if err == nil || !strings.Contains(err.Error(), "cannot be represented consistently") {
				t.Fatalf("invalid UTF-8 path was not rejected: %v", err)
			}
		})
	}
}

func TestTreeSHA256MatchesValidUnicodePaths(t *testing.T) {
	for _, name := range []string{"reference-\ufffd.md", "參考.md"} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			main, ref := []byte("entry"), []byte("method")
			if err := os.WriteFile(filepath.Join(root, "SKILL.md"), main, 0o644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, name), ref, 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := TreeSHA256(root)
			if err != nil {
				t.Fatal(err)
			}
			m, r := sha256.Sum256(main), sha256.Sum256(ref)
			rows := fmt.Sprintf("[[\"SKILL.md\",\"%x\"],[\"%s\",\"%x\"]]", m, name, r)
			want := sha256.Sum256([]byte(rows))
			if got != hex.EncodeToString(want[:]) {
				t.Fatalf("valid Unicode path mismatches normalized manifest: %s", got)
			}
		})
	}
}
