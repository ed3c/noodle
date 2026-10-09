package skill

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
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
