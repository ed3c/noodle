package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/poteto/noodle/internal/execution"
	"github.com/poteto/noodle/worktree/archive"
)

func TestHostCheckoutArchiveUsesPhysicalCreatorWithoutChangingEnvironment(t *testing.T) {
	req := hostTestRequest(t, t.TempDir())
	req.Environment = []string{"HOME=" + t.TempDir(), "PATH=" + os.Getenv("PATH"), "ORIGINAL_ARCHIVE_CONTEXT=unchanged"}
	if err := exec.Command("git", "init", req.ProjectDir).Run(); err != nil {
		t.Fatal(err)
	}
	raw, err := readHostRequestConfig(req)
	if err != nil {
		t.Fatal(err)
	}
	storage, err := prepareHostStorage(context.Background(), req, raw)
	if err != nil {
		t.Fatal(err)
	}
	e, err := execution.New(req.ProjectDir, req.Environment)
	if err != nil {
		t.Fatal(err)
	}
	before := e.Environ()
	policy, err := prepareHostCheckoutArchive(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := archive.LoadConfig(policy.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Roots, []string{filepath.Join(storage, ".worktrees")}) || !reflect.DeepEqual(before, e.Environ()) {
		t.Fatal("archive setup guessed physical root or changed execution environment")
	}
	if again, err := prepareHostCheckoutArchive(context.Background(), e); err != nil || *again != *policy {
		t.Fatal("same Host policy was not read back", again, err)
	}
	key := filepath.Join(cfg.StateDir, "owner.key")
	if info, err := os.Stat(key); err != nil || info.Size() != 32 || info.Mode().Perm()&0077 != 0 {
		t.Fatal("original archive owner key was not safely provisioned", err)
	}
	originalKey, err := os.ReadFile(key)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(policy.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(policy.Path, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	// Different formatting remains the same original policy; setup preserves
	// its bytes and binds the actual file instead of overwriting it.
	changed, err := prepareHostCheckoutArchive(context.Background(), e)
	if err != nil || changed.SHA256 == policy.SHA256 {
		t.Fatal("setup overwrote the original policy bytes", changed, err)
	}
	if err = os.WriteFile(key, []byte("foreign"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = prepareHostCheckoutArchive(context.Background(), e); err == nil {
		t.Fatal("setup overwrote or accepted a foreign key")
	}
	if err = os.WriteFile(key, originalKey, 0600); err != nil {
		t.Fatal(err)
	}
	home, _ := e.Lookup("HOME")
	defaultPath := filepath.Join(home, ".config", "noodle", "worktree-archive.json")
	if err = os.MkdirAll(filepath.Dir(defaultPath), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(defaultPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	selected, err := prepareHostCheckoutArchive(context.Background(), e)
	actualDefault, resolveErr := filepath.EvalSymlinks(defaultPath)
	if err != nil || resolveErr != nil || selected.Path != actualDefault {
		t.Fatal("Host ignored the existing original default policy", selected, err)
	}
}
