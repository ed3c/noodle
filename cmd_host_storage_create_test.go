package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/poteto/noodle/internal/execution"
	"github.com/poteto/noodle/worktree"
)

func TestManagedControlCreateCandidateAndNormalMergeKeepRootClean(t *testing.T) {
	req := hostTestRequest(t, t.TempDir())
	req.Environment = []string{"HOME=" + t.TempDir(), "PATH=" + os.Getenv("PATH")}
	git := func(root string, args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", root}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %s %v", args, out, err)
		}
		return strings.TrimSpace(string(out))
	}
	git(req.ProjectDir, "init", "-b", "main")
	git(req.ProjectDir, "config", "user.name", "fixture")
	git(req.ProjectDir, "config", "user.email", "fixture@example.test")
	ignore := []byte(".noodle/\n.worktrees/\n")
	if err := os.WriteFile(filepath.Join(req.ProjectDir, ".gitignore"), ignore, 0600); err != nil {
		t.Fatal(err)
	}
	git(req.ProjectDir, "add", ".")
	git(req.ProjectDir, "commit", "-m", "Original source and tracked directory ignores")
	raw, err := readHostRequestConfig(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = prepareHostStorage(context.Background(), req, raw); err != nil {
		t.Fatal(err)
	}
	e, err := execution.New(req.ProjectDir, req.Environment)
	if err != nil {
		t.Fatal(err)
	}
	a, err := worktree.NewAppWithExecution(e)
	if err != nil {
		t.Fatal(err)
	}
	a.Quiet = true
	if err = a.Create("candidate"); err != nil {
		t.Fatal(err)
	}
	worker := worktree.WorktreePath(a.Root, "candidate")
	if err = os.WriteFile(filepath.Join(worker, "result.txt"), []byte("complete candidate\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git(worker, "add", "result.txt")
	git(worker, "commit", "-m", "Deliver the original bounded candidate")
	head, tree := git(worker, "rev-parse", "HEAD"), git(worker, "rev-parse", "HEAD^{tree}")
	unexpected := filepath.Join(worker, "unknown-user-file")
	if err = os.WriteFile(unexpected, []byte("unknown candidate bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = a.Merge("candidate", worktree.MergeOpts{ExpectedHead: head, ExpectedTree: tree, RetainWorktree: true}); err == nil || !strings.Contains(err.Error(), "candidate worktree changed") {
		t.Fatal("unknown candidate bytes were accepted: ", err)
	}
	if err = os.Remove(unexpected); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(a.Root, ".noodle.toml")
	if err = os.WriteFile(configPath, append(append([]byte{}, raw...), []byte("\n# unknown root change\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	if err = a.Merge("candidate", worktree.MergeOpts{ExpectedHead: head, ExpectedTree: tree, RetainWorktree: true}); err == nil || !strings.Contains(err.Error(), "root checkout has uncommitted changes") {
		t.Fatal("unknown tracked root change was accepted: ", err)
	}
	if err = os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err = a.Merge("candidate", worktree.MergeOpts{ExpectedHead: head, ExpectedTree: tree, RetainWorktree: true}); err != nil {
		t.Fatal("original normal merge refused its producer's control: ", err)
	}
	if git(a.Root, "rev-parse", "HEAD") != head || git(a.Root, "status", "--porcelain", "--untracked-files=all") != "" {
		t.Fatal("merge did not preserve clean original root")
	}
	if got, err := os.ReadFile(filepath.Join(a.Root, ".gitignore")); err != nil || string(got) != string(ignore) {
		t.Fatalf("tracked original ignore changed: %q %v", got, err)
	}
	if err = os.WriteFile(filepath.Join(a.Root, "user-unknown.txt"), []byte("unrelated user bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := git(a.Root, "status", "--porcelain", "--untracked-files=all"); got != "?? user-unknown.txt" {
		t.Fatalf("unexpected user file was hidden: %q", got)
	}
}
