package worktree

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/poteto/noodle/internal/execution"
)

func TestBoundWorktreesUseExplicitRootAndSharedMergeLock(t *testing.T) {
	root := setupTestRepo(t)
	e, err := execution.New(root, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	a, err := NewAppWithExecution(e)
	if err != nil {
		t.Fatal(err)
	}
	canonical, _ := filepath.EvalSymlinks(root)
	if a.Root != canonical {
		t.Fatalf("discovered Host cwd instead: %s", a.Root)
	}
	linked := filepath.Join(t.TempDir(), "linked")
	if out, err := e.Command(context.Background(), root, "git", "worktree", "add", "-b", "linked", linked).CombinedOutput(); err != nil {
		t.Fatalf("fixture: %s %v", out, err)
	}
	linkedEnv, err := execution.New(linked, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewAppWithExecution(linkedEnv)
	if err != nil {
		t.Fatal(err)
	}
	if a.mergeLockPath() != b.mergeLockPath() {
		t.Fatalf("shared refs have separate locks: %s %s", a.mergeLockPath(), b.mergeLockPath())
	}
	if err := a.acquireMergeLock(); err != nil {
		t.Fatal(err)
	}
	defer a.releaseMergeLock()
	if err := writeMergeLock(b.mergeLockPath()); !os.IsExist(err) {
		t.Fatalf("same Host PID bypassed shared lock: %v", err)
	}
}
