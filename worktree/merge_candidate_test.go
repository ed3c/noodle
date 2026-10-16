package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/poteto/noodle/internal/execution"
)

func TestReviewedMergeUsesCandidateDespiteAmbiguousBranch(t *testing.T) {
	dir := setupTestRepo(t)
	app := &App{Root: dir}
	base := gitOutputIn(t, dir, "rev-parse", "HEAD")
	wt := WorktreePath(dir, "candidate")
	runGitIn(t, dir, "worktree", "add", "-b", "noodle/candidate", wt)
	writeFile(t, filepath.Join(wt, "reviewed.txt"), "reviewed")
	runGitIn(t, wt, "add", "reviewed.txt")
	runGitIn(t, wt, "commit", "-m", "reviewed")
	head := gitOutputIn(t, wt, "rev-parse", "HEAD")
	tree := gitOutputIn(t, wt, "rev-parse", "HEAD^{tree}")
	other := filepath.Join(t.TempDir(), "other")
	runGitIn(t, dir, "worktree", "add", "-b", "candidate", other, base)
	writeFile(t, filepath.Join(other, "unreviewed.txt"), "unreviewed")
	runGitIn(t, other, "add", "unreviewed.txt")
	runGitIn(t, other, "commit", "-m", "unreviewed")
	foreign := gitOutputIn(t, other, "rev-parse", "HEAD")
	runGitIn(t, dir, "worktree", "remove", other)
	if err := app.Merge("candidate", MergeOpts{ExpectedHead: head, ExpectedTree: tree}); err != nil {
		t.Fatal(err)
	}
	if got := gitOutputIn(t, dir, "rev-parse", "HEAD"); got != head {
		t.Fatalf("merged %s instead of reviewed %s", got, head)
	}
	if got := gitOutputIn(t, dir, "rev-parse", "candidate"); got != foreign {
		t.Fatal("unrelated branch changed")
	}
	if fileExists(filepath.Join(dir, "unreviewed.txt")) {
		t.Fatal("unreviewed content merged")
	}
}

func TestReviewedMergePreservesConcurrentDirtyWorktree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX command interposition fixture")
	}
	dir := setupTestRepo(t)
	wt := WorktreePath(dir, "candidate")
	runGitIn(t, dir, "worktree", "add", "-b", "noodle/candidate", wt)
	writeFile(t, filepath.Join(wt, "reviewed.txt"), "reviewed")
	runGitIn(t, wt, "add", "reviewed.txt")
	runGitIn(t, wt, "commit", "-m", "reviewed")
	head := gitOutputIn(t, wt, "rev-parse", "HEAD")
	tree := gitOutputIn(t, wt, "rev-parse", "HEAD^{tree}")
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	// Inject a real write after the initial clean check, before the merge.
	wrapper := "#!/bin/sh\nif [ \"$1\" = rev-list ]; then\n printf concurrent > \"$CANDIDATE_FILE\"\nfi\nexec \"$REAL_GIT\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(wrapper), 0755); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "REAL_GIT="+realGit, "CANDIDATE_FILE="+filepath.Join(wt, "reviewed.txt"))
	ctx, err := execution.New(dir, env)
	if err != nil {
		t.Fatal(err)
	}
	app := &App{Root: dir, Execution: ctx, Quiet: true}
	if err := app.Merge("candidate", MergeOpts{ExpectedHead: head, ExpectedTree: tree}); err != nil {
		t.Fatal(err)
	}
	if got := gitOutputIn(t, dir, "rev-parse", "HEAD"); got != head {
		t.Fatalf("reviewed commit not merged: %s", got)
	}
	data, err := os.ReadFile(filepath.Join(wt, "reviewed.txt"))
	if err != nil || string(data) != "concurrent" {
		t.Fatalf("concurrent work not preserved: %q %v", data, err)
	}
	if got := gitOutputIn(t, dir, "stash", "list"); got != "" {
		t.Fatalf("reviewed merge moved concurrent work into stash: %s", got)
	}
}
