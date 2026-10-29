package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/poteto/noodle/internal/execution"
)

func cleanupTestApp(t *testing.T, root, script string, extraEnv ...string) *App {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("POSIX command interposition fixture")
	}
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	wrapper := "#!/bin/sh\n" + script + "\nexec \"$REAL_GIT\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "git"), []byte(wrapper), 0755); err != nil {
		t.Fatal(err)
	}
	env := append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "REAL_GIT="+realGit)
	env = append(env, extraEnv...)
	ctx, err := execution.New(root, env)
	if err != nil {
		t.Fatal(err)
	}
	return &App{Root: root, Execution: ctx, Quiet: true}
}

func TestCleanupPreservesDirtyWorktree(t *testing.T) {
	for _, force := range []bool{false, true} {
		t.Run(map[bool]string{false: "ordinary", true: "force"}[force], func(t *testing.T) {
			root := setupTestRepo(t)
			path := createWorktreeWithBranch(t, root, "dirty", "dirty")
			writeFile(t, filepath.Join(path, "unsaved.txt"), "keep me")
			app := &App{Root: root, Quiet: true}
			if err := app.Cleanup("dirty", CleanupOpts{Force: force}); err == nil {
				t.Fatal("dirty cleanup returned success")
			}
			if got, err := os.ReadFile(filepath.Join(path, "unsaved.txt")); err != nil || string(got) != "keep me" {
				t.Fatalf("unsaved work lost: %q %v", got, err)
			}
			if got := gitOutputIn(t, root, "branch", "--list", "dirty"); got == "" {
				t.Fatal("dirty branch deleted")
			}
		})
	}
}

func TestCleanupRejectsUnknownSafety(t *testing.T) {
	root := setupTestRepo(t)
	path := createWorktreeWithBranch(t, root, "unknown", "unknown")
	app := &App{Root: root, Quiet: true, IntegrationBranch: "missing-base"}
	if err := app.Cleanup("unknown"); err == nil {
		t.Fatal("failed safety read returned success")
	}
	if !fileExists(path) {
		t.Fatal("unassessed worktree removed")
	}
}

func TestCleanupUsesActualCandidateBranch(t *testing.T) {
	root := setupTestRepo(t)
	path := createWorktreeWithBranch(t, root, "candidate", "noodle/candidate")
	runGitIn(t, root, "branch", "candidate")
	writeFile(t, filepath.Join(path, "unique.txt"), "unmerged candidate")
	runGitIn(t, path, "add", "unique.txt")
	runGitIn(t, path, "commit", "-m", "candidate work")
	app := &App{Root: root, Quiet: true}
	if err := app.Cleanup("candidate"); err == nil {
		t.Fatal("unmerged actual candidate removed")
	}
	if !fileExists(path) {
		t.Fatal("candidate lost")
	}
	if err := app.Cleanup("candidate", CleanupOpts{Force: true}); err != nil {
		t.Fatal(err)
	}
	if got := gitOutputIn(t, root, "branch", "--list", "candidate"); got == "" {
		t.Fatal("unrelated same-name branch removed")
	}
	if got := gitOutputIn(t, root, "branch", "--list", "noodle/candidate"); got != "" {
		t.Fatal("selected candidate branch retained")
	}
}

func TestCleanupReportsDeletionFailures(t *testing.T) {
	for _, operation := range []string{"remove", "branch", "prune"} {
		t.Run(operation, func(t *testing.T) {
			root := setupTestRepo(t)
			path := createWorktreeWithBranch(t, root, "failure", "failure")
			script := `if { [ "$1" = worktree ] && [ "$2" = "$FAIL_OPERATION" ]; } || { [ "$1" = update-ref ] && [ "$FAIL_OPERATION" = branch ]; }; then
 echo "injected $FAIL_OPERATION refusal" >&2
 exit 1
fi`
			app := cleanupTestApp(t, root, script, "FAIL_OPERATION="+operation)
			err := app.Cleanup("failure")
			if err == nil || !strings.Contains(err.Error(), "injected "+operation+" refusal") {
				t.Fatalf("missing diagnostic: %v", err)
			}
			if operation == "remove" {
				if !fileExists(path) {
					t.Fatal("failed removal lost worktree")
				}
				if got := gitOutputIn(t, root, "branch", "--list", "failure"); got == "" {
					t.Fatal("branch deleted after removal failure")
				}
			}
		})
	}
}

func TestPruneReportsSafetyAndDeletionFailures(t *testing.T) {
	for _, operation := range []string{"cherry", "status", "remove", "prune"} {
		t.Run(operation, func(t *testing.T) {
			root := setupTestRepo(t)
			path := createWorktreeWithBranch(t, root, "failure", "failure")
			script := `if [ "$1" = "$FAIL_OPERATION" ] || { [ "$1" = worktree ] && [ "$2" = "$FAIL_OPERATION" ]; } || { [ "$1" = -C ] && [ "$3" = "$FAIL_OPERATION" ]; }; then
 echo "injected $FAIL_OPERATION refusal" >&2
 exit 1
fi`
			app := cleanupTestApp(t, root, script, "FAIL_OPERATION="+operation)
			if err := app.Prune(); err == nil {
				t.Fatal("failed prune returned success")
			}
			if operation != "prune" && !fileExists(path) {
				t.Fatal("unassessed/refused candidate removed")
			}
		})
	}
}

func TestCleanupPreservesConcurrentDirtyWorktree(t *testing.T) {
	root := setupTestRepo(t)
	path := createWorktreeWithBranch(t, root, "candidate", "candidate")
	app := cleanupTestApp(t, root, `if [ "$1" = worktree ] && [ "$2" = remove ]; then
 printf concurrent > "$CANDIDATE_FILE"
fi`, "CANDIDATE_FILE="+filepath.Join(path, "concurrent.txt"))
	if err := app.Cleanup("candidate"); err == nil {
		t.Fatal("concurrent write silently discarded")
	}
	if got, err := os.ReadFile(filepath.Join(path, "concurrent.txt")); err != nil || string(got) != "concurrent" {
		t.Fatalf("concurrent work lost: %q %v", got, err)
	}
}

func TestPruneUsesActualCandidateBranch(t *testing.T) {
	root := setupTestRepo(t)
	path := createWorktreeWithBranch(t, root, "candidate", "noodle/candidate")
	runGitIn(t, root, "branch", "candidate")
	writeFile(t, filepath.Join(path, "unique.txt"), "unmerged candidate")
	runGitIn(t, path, "add", "unique.txt")
	runGitIn(t, path, "commit", "-m", "candidate work")
	app := &App{Root: root, Quiet: true}
	if err := app.Prune(); err != nil {
		t.Fatal(err)
	}
	if !fileExists(path) {
		t.Fatal("unmerged actual candidate removed")
	}
}

func TestCleanupPreservesConcurrentBranchAdvance(t *testing.T) {
	root := setupTestRepo(t)
	path := createWorktreeWithBranch(t, root, "candidate", "candidate")
	original := gitOutputIn(t, path, "rev-parse", "HEAD")
	app := cleanupTestApp(t, root, `if [ "$1" = worktree ] && [ "$2" = remove ]; then
 "$REAL_GIT" -C "$CANDIDATE_PATH" commit --allow-empty -m concurrent >/dev/null || exit 1
fi`, "CANDIDATE_PATH="+path)
	if err := app.Cleanup("candidate"); err == nil {
		t.Fatal("advanced candidate cleanup returned success")
	}
	if got := gitOutputIn(t, root, "rev-parse", "refs/heads/candidate"); got == original {
		t.Fatal("concurrent commit lost")
	}
}

func TestCleanupRejectsChangedCandidate(t *testing.T) {
	root := setupTestRepo(t)
	path := createWorktreeWithBranch(t, root, "candidate", "candidate")
	app := cleanupTestApp(t, root, `if [ "$1" = cherry ]; then
 "$REAL_GIT" "$@" || exit 1
 "$REAL_GIT" -C "$CANDIDATE_PATH" commit --allow-empty -m concurrent >/dev/null || exit 1
 exit 0
fi`, "CANDIDATE_PATH="+path)
	if err := app.Cleanup("candidate"); err == nil || !strings.Contains(err.Error(), "candidate changed") {
		t.Fatalf("changed candidate accepted: %v", err)
	}
	if !fileExists(path) {
		t.Fatal("changed candidate worktree removed")
	}
}
