package worktree

import (
	"path/filepath"
	"testing"
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
