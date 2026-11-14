package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/poteto/noodle/internal/execution"
)

func managedExcludeFixture(t *testing.T) (*App, string) {
	t.Helper()
	e, err := execution.New(setupTestRepo(t), os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	a, err := NewAppWithExecution(e)
	if err != nil {
		t.Fatal(err)
	}
	a.Quiet = true
	return a, bindTestStorageAliases(t, a)
}

func bindTestStorageAliases(t *testing.T, a *App) string {
	t.Helper()
	storage, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{".noodle", ".worktrees"} {
		if err = os.Mkdir(filepath.Join(storage, name), 0700); err != nil {
			t.Fatal(err)
		}
		if err = os.Symlink(filepath.Join(storage, name), filepath.Join(a.Root, name)); err != nil {
			t.Fatal(err)
		}
	}
	writeTestStorageReceipts(t, a, storage)
	return storage
}

func TestManagedStorageExcludesPreserveUserMetadataAndConverge(t *testing.T) {
	a, _ := managedExcludeFixture(t)
	linked := filepath.Join(t.TempDir(), "linked")
	runGitIn(t, a.Root, "worktree", "add", "-b", "other-goal", linked)
	e, err := execution.New(linked, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	other, err := NewAppWithExecution(e)
	if err != nil {
		t.Fatal(err)
	}
	bindTestStorageAliases(t, other)
	if other.CommonDir != a.CommonDir {
		t.Fatal("fixture does not share original Git metadata")
	}
	exclude := filepath.Join(a.CommonDir, "info", "exclude")
	before := []byte("# original user excludes\nuser-cache\n")
	if err := os.WriteFile(exclude, before, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(exclude, 0600); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, app := range []*App{a, other, a, other} {
		wg.Add(1)
		go func(a *App) {
			defer wg.Done()
			if managed, err := PrepareStorageExcludes(t.Context(), a.execution()); err != nil || !managed {
				t.Errorf("owned excludes: %v %v", managed, err)
			}
		}(app)
	}
	wg.Wait()
	got, err := os.ReadFile(exclude)
	want := string(before) + "/.noodle\n/.worktrees\n"
	if err != nil || string(got) != want {
		t.Fatalf("lost original content or duplicated rules: %q %v", got, err)
	}
	if info, err := os.Stat(exclude); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("original excludes mode changed", err)
	}
	for _, name := range []string{"user-cache", "user-file"} {
		if err = os.WriteFile(filepath.Join(a.Root, name), []byte("user bytes"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if status := gitOutputIn(t, a.Root, "status", "--porcelain", "--untracked-files=all"); status != "?? user-file" {
		t.Fatalf("user exclusions/unknown visibility changed: %q", status)
	}
	if _, err = os.Stat(filepath.Join(a.Root, ".gitignore")); !os.IsNotExist(err) {
		t.Fatal("managed preparation wrote tracked-source ignore", err)
	}
}

func TestManagedStorageCreateRejectsChangedOwnersAndExcludes(t *testing.T) {
	for _, change := range []string{"foreign", "partial", "missing-owner", "missing-receipt", "changed-receipt", "tracked-alias", "exclude-symlink", "exclude-directory", "exclude-lock-symlink", "negated-alias"} {
		t.Run(change, func(t *testing.T) {
			a, storage := managedExcludeFixture(t)
			exclude := filepath.Join(a.CommonDir, "info", "exclude")
			switch change {
			case "foreign", "partial":
				if err := os.Remove(filepath.Join(a.Root, ".worktrees")); err != nil {
					t.Fatal(err)
				}
				if change == "foreign" {
					if err := os.Symlink(t.TempDir(), filepath.Join(a.Root, ".worktrees")); err != nil {
						t.Fatal(err)
					}
				}
			case "missing-owner":
				matches, err := filepath.Glob(filepath.Join(a.CommonDir, "noodle-contexts", "*", "source.json"))
				if err != nil || len(matches) != 1 {
					t.Fatal("original owner fixture missing", err)
				}
				if err = os.Remove(matches[0]); err != nil {
					t.Fatal(err)
				}
			case "missing-receipt":
				if err := os.Remove(filepath.Join(storage, "source.json")); err != nil {
					t.Fatal(err)
				}
			case "changed-receipt":
				if err := os.WriteFile(filepath.Join(storage, "source.json"), []byte(`{}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "tracked-alias":
				runGitIn(t, a.Root, "add", ".worktrees")
			case "exclude-symlink", "exclude-directory":
				if err := os.Remove(exclude); err != nil {
					t.Fatal(err)
				}
				var err error
				if change == "exclude-symlink" {
					err = os.Symlink(filepath.Join(storage, "source.json"), exclude)
				} else {
					err = os.Mkdir(exclude, 0700)
				}
				if err != nil {
					t.Fatal(err)
				}
			case "exclude-lock-symlink":
				if err := os.Symlink(filepath.Join(storage, "source.json"), exclude+".noodle.lock"); err != nil {
					t.Fatal(err)
				}
			case "negated-alias":
				if err := os.WriteFile(filepath.Join(a.Root, ".gitignore"), []byte("!/.worktrees\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := a.Create("must-not-create"); err == nil {
				t.Fatal("changed original owner accepted")
			}
			if _, err := os.Stat(filepath.Join(storage, ".worktrees", "must-not-create")); !os.IsNotExist(err) {
				t.Fatal("created worker after failed owner preparation", err)
			}
			if branches := gitOutputIn(t, a.Root, "branch", "--list", "must-not-create"); branches != "" {
				t.Fatal("created branch after failed owner preparation")
			}
		})
	}
}

func TestOrdinaryCreateReportsIgnoreReadAndWriteErrors(t *testing.T) {
	for _, fault := range []string{"git-fatal", "ignore-open"} {
		t.Run(fault, func(t *testing.T) {
			root := setupTestRepo(t)
			a := &App{Root: root, Quiet: true}
			if fault == "git-fatal" {
				realGit, err := exec.LookPath("git")
				if err != nil {
					t.Fatal(err)
				}
				bin := t.TempDir()
				shim := "#!/bin/sh\nif [ \"$1\" = check-ignore ]; then echo fixture-ignore-fatal >&2; exit 128; fi\nexec \"$REAL_GIT\" \"$@\"\n"
				if err := os.WriteFile(filepath.Join(bin, "git"), []byte(shim), 0700); err != nil {
					t.Fatal(err)
				}
				e, err := execution.New(root, append(os.Environ(), "REAL_GIT="+realGit, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH")))
				if err != nil {
					t.Fatal(err)
				}
				a.Execution = e
			} else if err := os.Mkdir(filepath.Join(root, ".gitignore"), 0700); err != nil {
				t.Fatal(err)
			}
			err := a.Create("must-not-create")
			if err == nil || !strings.Contains(err.Error(), "ignore") {
				t.Fatalf("ignore failure silently accepted: %v", err)
			}
			if fault == "git-fatal" && !strings.Contains(err.Error(), "fixture-ignore-fatal") {
				t.Fatalf("Git fatal was not preserved: %v", err)
			}
			if branches := gitOutputIn(t, root, "branch", "--list", "must-not-create"); branches != "" {
				t.Fatal("created branch after failed ignore preparation")
			}
		})
	}
}
