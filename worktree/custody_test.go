package worktree

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/poteto/noodle/internal/execution"
)

func TestCreatePublishesOriginalDurableWorkerCustody(t *testing.T) {
	root := setupTestRepo(t)
	e, err := execution.New(root, os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	a, err := NewAppWithExecution(e)
	if err != nil {
		t.Fatal(err)
	}
	a.Quiet = true
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
	source, err := json.Marshal(StorageSource{Project: a.Root, CommonDir: a.CommonDir, StorageRoot: storage})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(storage, "source.json"), source, 0600); err != nil {
		t.Fatal(err)
	}
	if err = a.Create("new-worker"); err != nil {
		t.Fatal(err)
	}
	logical := WorktreePath(a.Root, "new-worker")
	custody, err := ReadCustody(t.Context(), e, logical)
	if err != nil || custody.Logical != logical || custody.Physical != filepath.Join(storage, ".worktrees", "new-worker") {
		t.Fatalf("new worker lost original custody: %+v %v", custody, err)
	}
	bindings, err := os.ReadDir(filepath.Join(storage, "worktree-bindings"))
	if err != nil || len(bindings) != 1 {
		t.Fatalf("original binding missing: %v %v", bindings, err)
	}
	path := filepath.Join(storage, "worktree-bindings", bindings[0].Name())
	directory := filepath.Dir(path)
	if err = os.Chmod(directory, 0500); err != nil {
		t.Fatal(err)
	}
	probe, writeErr := os.CreateTemp(directory, "publication-probe")
	if writeErr == nil {
		probe.Close()
		os.Remove(probe.Name())
	}
	publicationErr := a.EnsureCustody("new-worker")
	if err = os.Chmod(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if (writeErr == nil) != (publicationErr == nil) {
		t.Fatalf("existing bytes skipped publication availability: write=%v publication=%v", writeErr, publicationErr)
	}
	if err = a.EnsureCustody("new-worker"); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, []byte(`{"original":"another-worker"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = ReadCustody(t.Context(), e, logical); err == nil {
		t.Fatal("changed original binding accepted")
	}
	if err = a.EnsureCustody("new-worker"); err == nil {
		t.Fatal("changed binding was replaced")
	}
	before, _ := os.ReadFile(path)
	if string(before) != `{"original":"another-worker"}` {
		t.Fatal("original binding overwritten")
	}
}

func TestCreateFromSubdirectoryKeepsOriginalProjectCustody(t *testing.T) {
	for _, durable := range []bool{false, true} {
		t.Run(fmt.Sprint(durable), func(t *testing.T) {
			root := setupTestRepo(t)
			subdir := filepath.Join(root, "src")
			if err := os.Mkdir(subdir, 0700); err != nil {
				t.Fatal(err)
			}
			e, err := execution.New(subdir, os.Environ())
			if err != nil {
				t.Fatal(err)
			}
			a, err := NewAppWithExecution(e)
			if err != nil {
				t.Fatal(err)
			}
			a.Quiet = true
			if durable {
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
				source, _ := json.Marshal(StorageSource{Project: a.Root, CommonDir: a.CommonDir, StorageRoot: storage})
				if err = os.WriteFile(filepath.Join(storage, "source.json"), source, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err = a.Create("from-subdir"); err != nil {
				t.Fatal(err)
			}
			if err = a.EnsureCustody("from-subdir"); err != nil {
				t.Fatal(err)
			}
			if _, err = os.Stat(WorktreePath(subdir, "from-subdir")); !os.IsNotExist(err) {
				t.Fatal("created custody under caller cwd")
			}
		})
	}
}
