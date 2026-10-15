package loop

import (
	"github.com/poteto/noodle/internal/execution"
	"github.com/poteto/noodle/worktree"
	"path/filepath"
	"testing"
)

func TestLocalTaskIntegrationPinsControlBranchAcrossRestart(t *testing.T) {
	root := t.TempDir()
	runGitInRepo(t, root, "init", "-b", "control")
	runGitInRepo(t, root, "config", "user.email", "test@noodle.dev")
	runGitInRepo(t, root, "config", "user.name", "Noodle Test")
	requestChangesWrite(t, filepath.Join(root, ".gitignore"), []byte(".noodle/\n.worktrees/\n"))
	runGitInRepo(t, root, "add", ".gitignore")
	runGitInRepo(t, root, "commit", "-m", "base")
	app, err := worktree.NewAppWithExecution(execution.Capture(root))
	if err != nil {
		t.Fatal(err)
	}
	if err := bindContextIntegration(app, filepath.Join(root, ".noodle")); err != nil {
		t.Fatal(err)
	}
	if app.IntegrationBranch != "control" {
		t.Fatal(app.IntegrationBranch)
	}
	if err := app.Create("candidate"); err != nil {
		t.Fatal(err)
	}
	wt := worktree.WorktreePath(root, "candidate")
	requestChangesWrite(t, filepath.Join(wt, "candidate.txt"), []byte("candidate\n"))
	runGitInRepo(t, wt, "add", "candidate.txt")
	runGitInRepo(t, wt, "commit", "-m", "candidate")
	if err := app.Merge("candidate"); err != nil {
		t.Fatal(err)
	}
	if string(recoveryRead(t, filepath.Join(root, "candidate.txt"))) != "candidate\n" {
		t.Fatal("not integrated into control")
	}
	runGitInRepo(t, root, "checkout", "-b", "foreign")
	restarted, err := worktree.NewAppWithExecution(execution.Capture(root))
	if err != nil {
		t.Fatal(err)
	}
	if err := bindContextIntegration(restarted, filepath.Join(root, ".noodle")); err == nil {
		t.Fatal("adopted drifted branch")
	}
}
