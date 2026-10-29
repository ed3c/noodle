package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/poteto/noodle/internal/execution"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/worktree"
)

func TestHostStoragePreservesRuntimeAndDirtyBytesOutsideCheckout(t *testing.T) {
	req := hostTestRequest(t, t.TempDir())
	req.Environment = []string{"HOME=" + t.TempDir(), "PATH=" + os.Getenv("PATH")}
	if err := exec.Command("git", "init", req.ProjectDir).Run(); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{".noodle/sessions/original/raw.ndjson": "original process history\n", ".noodle/state.snapshot.json": "original snapshot", ".worktrees/original/dirty.txt": "uncommitted bytes", ".worktrees/original/.git": "original index owner reference"}
	for name, data := range files {
		path := filepath.Join(req.ProjectDir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := readHostRequestConfig(req)
	if err != nil {
		t.Fatal(err)
	}
	storage, err := prepareHostStorage(context.Background(), req, raw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(storage, req.ProjectDir+string(filepath.Separator)) || strings.Contains(storage, string(filepath.Separator)+".git"+string(filepath.Separator)) {
		t.Fatalf("custody is still inside disposable/protected source: %s", storage)
	}
	if again, err := prepareHostStorage(context.Background(), req, raw); err != nil || again != storage {
		t.Fatalf("repeat: %s %v", again, err)
	}
	if err = os.RemoveAll(req.ProjectDir); err != nil {
		t.Fatal(err)
	}
	for name, data := range files {
		got, err := os.ReadFile(filepath.Join(storage, name))
		if err != nil || string(got) != data {
			t.Fatalf("archive lost %s: %q %v", name, got, err)
		}
	}
}

func TestHostStorageRefusesLiveWriterAndForeignAlias(t *testing.T) {
	req := hostTestRequest(t, t.TempDir())
	req.Environment = []string{"HOME=" + t.TempDir(), "PATH=" + os.Getenv("PATH")}
	if err := exec.Command("git", "init", req.ProjectDir).Run(); err != nil {
		t.Fatal(err)
	}
	runtimeDir := filepath.Join(req.ProjectDir, ".noodle")
	if err := os.MkdirAll(runtimeDir, 0700); err != nil {
		t.Fatal(err)
	}
	writer, err := lockfile.TryLock(filepath.Join(runtimeDir, "noodle.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	raw, err := readHostRequestConfig(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = prepareHostStorage(context.Background(), req, raw); err == nil {
		t.Fatal("moved a live actor's runtime")
	}
	if info, err := os.Lstat(runtimeDir); err != nil || !info.IsDir() {
		t.Fatal("live storage changed", err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.RemoveAll(runtimeDir); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(t.TempDir(), runtimeDir); err != nil {
		t.Fatal(err)
	}
	if _, err = prepareHostStorage(context.Background(), req, raw); err == nil {
		t.Fatal("foreign runtime alias accepted")
	}
}

func TestHostStoragePreservesRealGitIndexAndLaterConfig(t *testing.T) {
	req := hostTestRequest(t, t.TempDir())
	req.Environment = []string{"HOME=" + t.TempDir(), "PATH=" + os.Getenv("PATH")}
	git := func(root string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s %v", args, out, err)
		}
		return string(out)
	}
	git(req.ProjectDir, "init")
	git(req.ProjectDir, "-c", "user.name=Probe", "-c", "user.email=probe@invalid", "commit", "--allow-empty", "-m", "fixture")
	worker := filepath.Join(req.ProjectDir, ".worktrees", "original")
	git(req.ProjectDir, "worktree", "add", "-b", "worker", worker)
	if err := os.WriteFile(filepath.Join(worker, "staged"), []byte("staged bytes"), 0755); err != nil {
		t.Fatal(err)
	}
	git(worker, "add", "staged")
	if err := os.WriteFile(filepath.Join(worker, "staged"), []byte("later dirty bytes"), 0755); err != nil {
		t.Fatal(err)
	}
	indexPath := strings.TrimSpace(git(worker, "rev-parse", "--path-format=absolute", "--git-path", "index"))
	index, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatal(err)
	}
	data, err := readHostRequestConfig(req)
	if err != nil {
		t.Fatal(err)
	}
	storage, err := prepareHostStorage(context.Background(), req, data)
	if err != nil {
		t.Fatal(err)
	}
	// A new legal task can use another config in the same repository custody.
	later := append(append([]byte{}, data...), []byte("\n# later task\n")...)
	req.ConfigSHA256 = hostDigest(later)
	if _, err = prepareHostStorage(context.Background(), req, later); err != nil {
		t.Fatal(err)
	}

	e, err := execution.New(req.ProjectDir, req.Environment)
	if err != nil {
		t.Fatal(err)
	}
	custody, err := worktree.ReadCustody(context.Background(), e, worker)
	if err != nil || custody.Physical != filepath.Join(storage, ".worktrees", "original") {
		t.Fatalf("durable custody: %+v %v", custody, err)
	}
	bindingPath := filepath.Join(storage, "worktree-bindings", hostDigest([]byte(filepath.Join(e.Root(), ".worktrees", "original")))+".json")
	bindingBytes, err := os.ReadFile(bindingPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(bindingPath, []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = worktree.ReadCustody(context.Background(), e, worker); err == nil {
		t.Fatal("foreign alias binding accepted")
	}
	if err = os.WriteFile(bindingPath, bindingBytes, 0600); err != nil {
		t.Fatal(err)
	}
	// Simulate App deleting its checkout; the Git common owner is normally a
	// separate primary repository, so remove only the disposable aliases here.
	if err = os.Remove(filepath.Join(req.ProjectDir, ".worktrees")); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(filepath.Join(req.ProjectDir, ".noodle")); err != nil {
		t.Fatal(err)
	}
	git(req.ProjectDir, "worktree", "prune", "--expire", "now")
	physical := filepath.Join(storage, ".worktrees", "original")
	if got := git(physical, "show", ":staged"); got != "staged bytes" {
		t.Fatal(got)
	}
	if got, err := os.ReadFile(filepath.Join(physical, "staged")); err != nil || string(got) != "later dirty bytes" {
		t.Fatal(string(got), err)
	}
	if got, err := os.ReadFile(indexPath); err != nil || string(got) != string(index) {
		t.Fatal("original Git index changed", err)
	}
}

func TestHostStorageRejectsGitOwnershipOverrideBeforeMoving(t *testing.T) {
	req := hostTestRequest(t, t.TempDir())
	req.Environment = append(req.Environment, "GIT_DIR="+t.TempDir())
	if err := os.MkdirAll(filepath.Join(req.ProjectDir, ".noodle"), 0700); err != nil {
		t.Fatal(err)
	}
	data, err := readHostRequestConfig(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = prepareHostStorage(context.Background(), req, data); err == nil {
		t.Fatal("Git ownership override accepted")
	}
	if info, err := os.Lstat(filepath.Join(req.ProjectDir, ".noodle")); err != nil || !info.IsDir() {
		t.Fatal("rejected context moved original runtime", err)
	}
}

func TestHostStoragePartialAliasStillRequiresOriginalWriterRelease(t *testing.T) {
	req := hostTestRequest(t, t.TempDir())
	req.Environment = []string{"HOME=" + t.TempDir(), "PATH=" + os.Getenv("PATH")}
	if err := exec.Command("git", "init", req.ProjectDir).Run(); err != nil {
		t.Fatal(err)
	}
	data, err := readHostRequestConfig(req)
	if err != nil {
		t.Fatal(err)
	}
	storage, err := prepareHostStorage(context.Background(), req, data)
	if err != nil {
		t.Fatal(err)
	}
	// Recreate the state after publishing runtime alias but before moving workers.
	if err = os.Remove(filepath.Join(req.ProjectDir, ".worktrees")); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(filepath.Join(storage, ".worktrees"), filepath.Join(req.ProjectDir, ".worktrees")); err != nil {
		t.Fatal(err)
	}
	writer, err := lockfile.TryLock(filepath.Join(storage, ".noodle", "noodle.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err = prepareHostStorage(context.Background(), req, data); err == nil {
		t.Fatal("moved workers through a live runtime alias")
	}
	if info, err := os.Lstat(filepath.Join(req.ProjectDir, ".worktrees")); err != nil || !info.IsDir() {
		t.Fatal("original worker directory moved", err)
	}
}
