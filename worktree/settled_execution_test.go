package worktree

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
)

func TestRemoveSettledExecutionCopy(t *testing.T) {
	for _, branch := range []string{"worker", "noodle/worker"} {
		t.Run(branch, func(t *testing.T) {
			root := setupTestRepo(t)
			path := createWorktreeWithBranch(t, root, "worker", branch)
			writeFile(t, filepath.Join(path, "result.txt"), "settled result")
			runGitIn(t, path, "add", "result.txt")
			runGitIn(t, path, "commit", "-m", "settled result")
			head := gitOutputIn(t, path, "rev-parse", "HEAD")
			order := state.OrderNode{OrderID: "task", Stages: []state.StageNode{{Attempts: []state.AttemptNode{{WorktreeName: "worker"}}}}}
			snapshot := reducer.BuildSnapshot(state.State{Orders: map[string]state.OrderNode{"task": order}}, nil, time.Time{})
			if err := os.MkdirAll(filepath.Join(root, ".noodle"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := reducer.WriteSnapshotAtomic(filepath.Join(root, ".noodle/state.snapshot.json"), snapshot); err != nil {
				t.Fatal(err)
			}
			writeFile(t, filepath.Join(root, ".noodle.toml"), "[runtime]\nadmitted_orders=['task']\n")
			app := &App{Root: root, Quiet: true}
			if err := app.ObserveSettledExecutionAbsent("worker", head); err == nil {
				t.Fatal("existing worker was observed absent")
			}
			if err := app.Cleanup("worker", CleanupOpts{Force: true}); err == nil || !strings.Contains(err.Error(), "retained") {
				t.Fatalf("generic guard changed: %v", err)
			}
			if err := app.RemoveSettledExecutionCopy("worker", head); err != nil {
				t.Fatal(err)
			}
			if fileExists(path) {
				t.Fatal("execution copy retained")
			}
			if err := app.ObserveSettledExecutionAbsent("worker", head); err != nil {
				t.Fatalf("original owner could not read removal: %v", err)
			}
			if got := gitOutputIn(t, root, "branch", "--list", branch); got != "" {
				t.Fatalf("branch retained: %s", got)
			}
			if err := app.RemoveSettledExecutionCopy("worker", head); err != nil {
				t.Fatalf("retry failed: %v", err)
			}
		})
	}
}

func TestRemoveSettledExecutionCopyRefusals(t *testing.T) {
	for _, scenario := range []string{"head", "empty-head", "untracked", "tracked", "staged", "branch", "detached", "missing-registered", "symlink"} {
		t.Run(scenario, func(t *testing.T) {
			root := setupTestRepo(t)
			path := createWorktreeWithBranch(t, root, "worker", "noodle/worker")
			head := gitOutputIn(t, path, "rev-parse", "HEAD")
			switch scenario {
			case "head":
				head = strings.Repeat("0", len(head))
			case "empty-head":
				head = ""
			case "untracked":
				writeFile(t, filepath.Join(path, "unsaved.txt"), "keep")
			case "tracked":
				writeFile(t, filepath.Join(path, "README.md"), "keep")
			case "staged":
				writeFile(t, filepath.Join(path, "README.md"), "keep")
				runGitIn(t, path, "add", "README.md")
			case "branch":
				runGitIn(t, path, "switch", "-c", "unrelated")
			case "detached":
				runGitIn(t, path, "checkout", "--detach")
			case "missing-registered":
				if err := os.RemoveAll(path); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				moved := filepath.Join(t.TempDir(), "moved")
				runGitIn(t, root, "worktree", "move", path, moved)
				if err := os.Symlink(moved, path); err != nil {
					t.Skipf("symlink unavailable: %v", err)
				}
			}
			app := &App{Root: root, Quiet: true}
			if err := app.RemoveSettledExecutionCopy("worker", head); err == nil {
				t.Fatal("unsafe execution copy accepted")
			}
			if got := gitOutputIn(t, root, "branch", "--list", "noodle/worker"); got == "" {
				t.Fatal("branch lost after refusal")
			}
			if scenario != "missing-registered" && !fileExists(path) {
				t.Fatal("execution copy lost after refusal")
			}
		})
	}
}

func TestRemoveSettledExecutionCopyRejectsControlPaths(t *testing.T) {
	root := setupTestRepo(t)
	control := filepath.Join(t.TempDir(), "control")
	runGitIn(t, root, "worktree", "add", "-b", "noodle/control", control)
	head := gitOutputIn(t, root, "rev-parse", "HEAD")
	app := &App{Root: root, Quiet: true}
	for _, name := range []string{control, "../control", "a/../../control", ".", "", "a/../control", `a\control`, "control"} {
		if err := app.RemoveSettledExecutionCopy(name, head); err == nil {
			t.Fatalf("control path accepted: %q", name)
		}
	}
	if !fileExists(control) {
		t.Fatal("control checkout lost")
	}
}

func TestRemoveSettledExecutionCopyResumesBranchCleanup(t *testing.T) {
	for _, advanced := range []bool{false, true} {
		t.Run(map[bool]string{false: "same-tip", true: "advanced"}[advanced], func(t *testing.T) {
			root := setupTestRepo(t)
			path := createWorktreeWithBranch(t, root, "worker", "noodle/worker")
			head := gitOutputIn(t, path, "rev-parse", "HEAD")
			if advanced {
				runGitIn(t, path, "commit", "--allow-empty", "-m", "new work")
			}
			runGitIn(t, root, "worktree", "remove", path)
			app := &App{Root: root, Quiet: true}
			err := app.RemoveSettledExecutionCopy("worker", head)
			if advanced {
				if err == nil {
					t.Fatal("advanced branch deleted")
				}
				if got := gitOutputIn(t, root, "branch", "--list", "noodle/worker"); got == "" {
					t.Fatal("advanced ref lost")
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRemoveSettledExecutionCopyReportsFailures(t *testing.T) {
	for _, operation := range []string{"remove", "delete", "readback"} {
		t.Run(operation, func(t *testing.T) {
			root := setupTestRepo(t)
			path := createWorktreeWithBranch(t, root, "worker", "noodle/worker")
			head := gitOutputIn(t, path, "rev-parse", "HEAD")
			script := `if [ "$1" = worktree ] && [ "$2" = remove ]; then
 if [ "$FAIL_OPERATION" = readback ]; then exit 0; fi
 if [ "$FAIL_OPERATION" = remove ]; then echo 'injected removal refusal' >&2; exit 1; fi
fi
if [ "$1" = update-ref ] && [ "$FAIL_OPERATION" = delete ]; then echo 'injected deletion refusal' >&2; exit 1; fi`
			app := cleanupTestApp(t, root, script, "FAIL_OPERATION="+operation)
			if err := app.RemoveSettledExecutionCopy("worker", head); err == nil {
				t.Fatal("failed cleanup reported success")
			}
			if got := gitOutputIn(t, root, "branch", "--list", "noodle/worker"); got == "" {
				t.Fatal("branch deleted after failure")
			}
			if operation != "delete" && !fileExists(path) {
				t.Fatal("worktree disappeared")
			}
			if operation == "delete" {
				if err := (&App{Root: root, Quiet: true}).RemoveSettledExecutionCopy("worker", head); err != nil {
					t.Fatalf("resume after partial cleanup: %v", err)
				}
			}
		})
	}
}

func TestRemoveSettledExecutionCopyPreservesConcurrentChanges(t *testing.T) {
	for _, change := range []string{"dirty", "commit", "registration"} {
		t.Run(change, func(t *testing.T) {
			root := setupTestRepo(t)
			path := createWorktreeWithBranch(t, root, "worker", "noodle/worker")
			head := gitOutputIn(t, path, "rev-parse", "HEAD")
			app := cleanupTestApp(t, root, `if [ "$1" = worktree ] && [ "$2" = remove ]; then
 case "$CONCURRENT_CHANGE" in
 dirty) printf concurrent > "$CANDIDATE_PATH/concurrent.txt" ;;
 commit) "$REAL_GIT" -C "$CANDIDATE_PATH" commit --allow-empty -m concurrent >/dev/null || exit 1 ;;
 registration)
  "$REAL_GIT" "$@" || exit 1
  "$REAL_GIT" worktree add "$OTHER_PATH" noodle/worker >/dev/null || exit 1
  exit 0 ;;
 esac
fi`, "CONCURRENT_CHANGE="+change, "CANDIDATE_PATH="+path, "OTHER_PATH="+filepath.Join(t.TempDir(), "other"))
			if err := app.RemoveSettledExecutionCopy("worker", head); err == nil {
				t.Fatal("concurrent change returned success")
			}
			if got := gitOutputIn(t, root, "branch", "--list", "noodle/worker"); got == "" {
				t.Fatal("concurrent branch lost")
			}
			if change == "dirty" {
				if raw, err := os.ReadFile(filepath.Join(path, "concurrent.txt")); err != nil || string(raw) != "concurrent" {
					t.Fatalf("concurrent file lost: %q %v", raw, err)
				}
			}
			if change == "commit" && gitOutputIn(t, root, "rev-parse", "noodle/worker") == head {
				t.Fatal("concurrent commit lost")
			}
		})
	}
}

func TestRemoveSettledExecutionCopyReadsBackBranchDeletion(t *testing.T) {
	root := setupTestRepo(t)
	path := createWorktreeWithBranch(t, root, "worker", "noodle/worker")
	head := gitOutputIn(t, path, "rev-parse", "HEAD")
	app := cleanupTestApp(t, root, `if [ "$1" = update-ref ]; then exit 0; fi`)
	if err := app.RemoveSettledExecutionCopy("worker", head); err == nil {
		t.Fatal("branch deletion without readback accepted")
	}
}

func TestRemoveSettledExecutionCopyRejectsForeignMetadata(t *testing.T) {
	root := setupTestRepo(t)
	path := createWorktreeWithBranch(t, root, "worker", "noodle/worker")
	head := gitOutputIn(t, path, "rev-parse", "HEAD")
	foreign := setupTestRepo(t)
	foreignPath := createWorktreeWithBranch(t, foreign, "worker", "noodle/worker")
	metadata, err := os.ReadFile(filepath.Join(foreignPath, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, ".git"), metadata, 0600); err != nil {
		t.Fatal(err)
	}
	app := &App{Root: root, Quiet: true}
	if err := app.RemoveSettledExecutionCopy("worker", head); err == nil {
		t.Fatal("foreign project metadata accepted")
	}
	if !fileExists(path) || !fileExists(foreignPath) {
		t.Fatal("foreign metadata refusal lost worktree")
	}
}

func TestRemoveSettledExecutionCopyRejectsAmbiguousBranch(t *testing.T) {
	root := setupTestRepo(t)
	path := createWorktreeWithBranch(t, root, "worker", "noodle/worker")
	head := gitOutputIn(t, path, "rev-parse", "HEAD")
	runGitIn(t, root, "branch", "worker", head)
	app := &App{Root: root, Quiet: true}
	if err := app.RemoveSettledExecutionCopy("worker", head); err == nil {
		t.Fatal("ambiguous branches accepted")
	}
	if !fileExists(path) {
		t.Fatal("ambiguous candidate removed")
	}
	for _, branch := range []string{"worker", "noodle/worker"} {
		if got := gitOutputIn(t, root, "branch", "--list", branch); got == "" {
			t.Fatal("ambiguous branch removed")
		}
	}
}

func TestRemoveSettledExecutionCopyMissingRequiresFreshRegistrationRead(t *testing.T) {
	root := setupTestRepo(t)
	head := gitOutputIn(t, root, "rev-parse", "HEAD")
	app := cleanupTestApp(t, root, `if [ "$1" = worktree ] && [ "$2" = list ]; then echo 'injected list refusal' >&2; exit 1; fi`)
	if err := app.RemoveSettledExecutionCopy("missing", head); err == nil {
		t.Fatal("missing directory accepted without registration readback")
	}
}

func TestRemoveSettledExecutionCopyRejectsUnreadableStatus(t *testing.T) {
	root := setupTestRepo(t)
	path := createWorktreeWithBranch(t, root, "worker", "noodle/worker")
	head := gitOutputIn(t, path, "rev-parse", "HEAD")
	app := cleanupTestApp(t, root, `if [ "$1" = -C ] && [ "$3" = status ]; then echo 'injected status refusal' >&2; exit 1; fi`)
	if err := app.RemoveSettledExecutionCopy("worker", head); err == nil {
		t.Fatal("unreadable status accepted")
	}
	if !fileExists(path) {
		t.Fatal("unassessed candidate removed")
	}
}
