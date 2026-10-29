package workspaceartifacts

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/poteto/noodle/internal/filex"
)

func retainedHistoryFixture(t *testing.T) (string, []string, string) {
	t.Helper()
	root, _ := fixture(t)
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	put(t, root, "old/only-in-ancestor", []byte("original ancestor bytes\x00\xff\n"), 0644)
	testGit(t, root, "add", ".")
	testGit(t, root, "commit", "--quiet", "-m", "retain original ancestor")
	oldBlob := strings.TrimSpace(string(testGit(t, root, "rev-parse", "HEAD:old/only-in-ancestor")))
	if err := os.RemoveAll(filepath.Join(root, "old")); err != nil {
		t.Fatal(err)
	}
	testGit(t, root, "add", "-A")
	testGit(t, root, "commit", "--quiet", "-m", "baseline after removal")
	base := strings.TrimSpace(string(testGit(t, root, "rev-parse", "HEAD")))
	put(t, root, "nested/result", []byte("published result"), 0644)
	testGit(t, root, "add", ".")
	testGit(t, root, "commit", "--quiet", "-m", "publish candidate")
	head := strings.TrimSpace(string(testGit(t, root, "rev-parse", "HEAD")))
	manifest, err := compactionGit(t.Context(), root, []byte("retained manifest"), "hash-object", "-w", "--stdin", "--no-filters")
	if err != nil {
		t.Fatal(err)
	}
	return root, []string{base, head, strings.TrimSpace(string(manifest))}, oldBlob
}

func TestRetainedGitDurabilitySyncsCompleteOriginalHistory(t *testing.T) {
	root, roots, oldBlob := retainedHistoryFixture(t)
	files, dirs := map[string]bool{}, map[string]bool{}
	err := syncRetainedGitObjectsWith(t.Context(), root, roots, func(path string) error {
		files[path] = true
		return syncRetentionRegular(path)
	}, func(path string) error {
		dirs[path] = true
		return filex.SyncDir(path)
	})
	if err != nil {
		t.Fatal(err)
	}
	objects := filepath.Join(root, ".git", "objects")
	args := append([]string{"rev-list", "--objects", "--no-object-names"}, roots...)
	for _, oid := range strings.Fields(string(testGit(t, root, args...))) {
		path := filepath.Join(objects, oid[:2], oid[2:])
		if !files[path] || !dirs[filepath.Dir(path)] {
			t.Fatalf("reachable object or containing directory was not synced: %s", oid)
		}
	}
	if !files[filepath.Join(objects, oldBlob[:2], oldBlob[2:])] || !dirs[objects] || !dirs[filepath.Dir(objects)] {
		t.Fatal("ancestor-only bytes or object-store directory persistence missing")
	}
	// The baseline-only caller shares exactly the same ancestry boundary.
	if err := syncRetainedGitObjects(t.Context(), root, roots[0]); err != nil {
		t.Fatal(err)
	}
}

func TestRetainedGitDurabilitySyncsPackedDataIndexesAndMIDX(t *testing.T) {
	root, roots, _ := retainedHistoryFixture(t)
	testGit(t, root, "update-ref", "refs/test/retained-manifest", roots[2])
	testGit(t, root, "repack", "-a", "-d")
	testGit(t, root, "multi-pack-index", "write")
	files := map[string]bool{}
	if err := syncRetainedGitObjectsWith(t.Context(), root, roots, func(path string) error {
		files[path] = true
		return syncRetentionRegular(path)
	}, filex.SyncDir); err != nil {
		t.Fatal(err)
	}
	packDir := filepath.Join(root, ".git", "objects", "pack")
	entries, err := os.ReadDir(packDir)
	if err != nil {
		t.Fatal(err)
	}
	packs, indexes := 0, 0
	for _, entry := range entries {
		if !files[filepath.Join(packDir, entry.Name())] {
			t.Fatalf("pack storage file was not synced: %s", entry.Name())
		}
		if strings.HasSuffix(entry.Name(), ".pack") {
			packs++
		}
		if strings.HasSuffix(entry.Name(), ".idx") {
			indexes++
		}
	}
	if packs == 0 || packs != indexes || !files[filepath.Join(packDir, "multi-pack-index")] {
		t.Fatal("packed fixture did not exercise data, indexes and MIDX")
	}
}

func TestRetainedGitDurabilityRefusesIncompleteOrRedirectedStorage(t *testing.T) {
	for _, variant := range []string{"alternates", "http-alternates", "shallow", "grafts", "ancestor-missing", "ancestor-corrupt", "ancestor-symlink", "pack-without-index", "pack-symlink", "index-symlink", "chained-midx"} {
		t.Run(variant, func(t *testing.T) {
			root, roots, oldBlob := retainedHistoryFixture(t)
			common := filepath.Join(root, ".git")
			object := filepath.Join(common, "objects", oldBlob[:2], oldBlob[2:])
			switch variant {
			case "alternates", "http-alternates":
				// An empty indirection file is refused too; it must not silently
				// become an external object dependency later in the deletion flow.
				put(t, common, "objects/info/"+variant, nil, 0644)
			case "shallow":
				put(t, common, "shallow", []byte(roots[0]+"\n"), 0644)
			case "grafts":
				put(t, common, "info/grafts", []byte(roots[0]+"\n"), 0644)
			case "ancestor-missing", "ancestor-corrupt", "ancestor-symlink":
				var original []byte
				if variant == "ancestor-symlink" {
					var err error
					original, err = os.ReadFile(object)
					if err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Remove(object); err != nil {
					t.Fatal(err)
				}
				if variant == "ancestor-corrupt" {
					put(t, filepath.Dir(object), filepath.Base(object), []byte("damaged original object"), 0644)
				}
				if variant == "ancestor-symlink" {
					if runtime.GOOS == "windows" {
						t.Skip("requires symlink permission")
					}
					other := filepath.Join(t.TempDir(), "original-object")
					if err := os.WriteFile(other, original, 0600); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(other, object); err != nil {
						t.Fatal(err)
					}
				}
			case "pack-without-index":
				put(t, common, "objects/pack/pack-"+strings.Repeat("a", 40)+".pack", []byte("orphan"), 0644)
			case "pack-symlink", "index-symlink":
				if runtime.GOOS == "windows" {
					t.Skip("requires symlink permission")
				}
				testGit(t, root, "repack", "-a", "-d")
				ext := ".pack"
				if variant == "index-symlink" {
					ext = ".idx"
				}
				paths, err := filepath.Glob(filepath.Join(common, "objects", "pack", "*"+ext))
				if err != nil || len(paths) != 1 {
					t.Fatal("missing pack fixture", err)
				}
				other := filepath.Join(t.TempDir(), filepath.Base(paths[0]))
				if err := os.Rename(paths[0], other); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(other, paths[0]); err != nil {
					t.Fatal(err)
				}
			case "chained-midx":
				if err := os.MkdirAll(filepath.Join(common, "objects", "pack", "multi-pack-index.d"), 0755); err != nil {
					t.Fatal(err)
				}
			}
			if err := syncRetainedGitObjects(t.Context(), root, roots...); err == nil {
				t.Fatal("incomplete or redirected Git storage authorized retirement")
			}
		})
	}
}

func TestRetainedGitDurabilityPropagatesFileAndDirectorySyncFailure(t *testing.T) {
	for _, variant := range []string{"ancestor-file", "pack-index", "object-directory"} {
		t.Run(variant, func(t *testing.T) {
			root, roots, oldBlob := retainedHistoryFixture(t)
			if variant == "pack-index" {
				testGit(t, root, "repack", "-a", "-d")
			}
			injected := errors.New("injected persistence failure")
			err := syncRetainedGitObjectsWith(t.Context(), root, roots, func(path string) error {
				if (variant == "ancestor-file" && filepath.Base(path) == oldBlob[2:]) || (variant == "pack-index" && filepath.Ext(path) == ".idx") {
					return injected
				}
				return syncRetentionRegular(path)
			}, func(path string) error {
				if variant == "object-directory" && filepath.Base(path) == "objects" {
					return injected
				}
				return filex.SyncDir(path)
			})
			if !errors.Is(err, injected) {
				t.Fatalf("persistence failure was not propagated: %v", err)
			}
		})
	}
}

func TestRetainedGitDurabilityBlocksCompactionBeforeAnyCopyDeletion(t *testing.T) {
	root, b, r, head := compactFixtureLayout(t, true)
	// This blob is absent from both current trees, but reachable through their
	// original ancestor. The old two-tree verifier alone cannot detect its loss.
	oid := strings.TrimSpace(string(testGit(t, root, "rev-parse", b.Head+"^:.gitignore")))
	if err := os.Remove(filepath.Join(root, ".git", "objects", oid[:2], oid[2:])); err != nil {
		t.Fatal(err)
	}
	if err := compactRetainedPublication(t.Context(), root, b, r, head, func(string) error {
		t.Fatal("deletion started before complete Git durability proof")
		return nil
	}); err == nil {
		t.Fatal("missing original ancestor object authorized deletion")
	}
	evidence := filepath.Dir(r.PatchPath)
	for _, dir := range []string{b.Directory, r.ReturnedDirectory, filepath.Join(evidence, "baseline"), filepath.Join(evidence, "returned"), filepath.Join(evidence, "patch-repository")} {
		if _, err := os.Stat(dir); err != nil {
			t.Fatal("copy deleted before complete Git durability proof", err)
		}
	}
}

func TestRetainedGitDurabilityCancellationRefusesPersistence(t *testing.T) {
	root, roots, _ := retainedHistoryFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := syncRetainedGitObjects(ctx, root, roots...); err == nil {
		t.Fatal("cancelled Git durability check succeeded")
	}
}

func TestRetainedGitDurabilitySHA256AndOriginalLinkedRepository(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sha256-project")
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	testGit(t, root, "init", "--quiet", "--template=", "--object-format=sha256")
	testGit(t, root, "config", "user.name", "Retention Test")
	testGit(t, root, "config", "user.email", "retention@example.invalid")
	put(t, root, "result", []byte("original sha256 repository"), 0644)
	testGit(t, root, "add", ".")
	testGit(t, root, "commit", "--quiet", "-m", "original sha256 bytes")
	head := strings.TrimSpace(string(testGit(t, root, "rev-parse", "HEAD")))
	if err := syncRetainedGitObjects(t.Context(), root, head); err != nil {
		t.Fatal(err)
	}
	testGit(t, root, "repack", "-a", "-d")
	linked := filepath.Join(filepath.Dir(root), "linked-execution")
	testGit(t, root, "worktree", "add", "--quiet", "--detach", linked, head)
	if err := syncRetainedGitObjects(t.Context(), linked, head); err != nil {
		t.Fatal(err)
	}
}
