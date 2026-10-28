package workspaceartifacts

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func observationHooks(t *testing.T, root, base string) (string, string) {
	t.Helper()
	monitorMarker := filepath.Join(base, "fsmonitor-ran")
	filterMarker := filepath.Join(base, "filter-ran")
	hook := filepath.Join(base, "monitor-hook")
	script := "#!/bin/sh\nprintf ran > " + strconv.Quote(monitorMarker) + "\nprintf '\\0'\n"
	if err := os.WriteFile(hook, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	testGit(t, root, "config", "core.fsmonitor", hook)
	testGit(t, root, "config", "filter.sideeffect.clean", "printf ran > "+strconv.Quote(filterMarker)+"; cat")
	testGit(t, root, "config", "filter.sideeffect.smudge", "printf ran > "+strconv.Quote(filterMarker)+"; cat")
	testGit(t, root, "config", "filter.sideeffect.required", "true")
	return monitorMarker, filterMarker
}
func TestReadCleanHeadBypassesHooksFiltersAndIndexRefresh(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable hook fixture")
	}
	for _, variant := range []string{"clean", "modified", "assume-unchanged", "skip-worktree", "staged", "untracked", "deleted"} {
		t.Run(variant, func(t *testing.T) {
			root, base := fixture(t)
			put(t, root, ".gitattributes", []byte("file.txt filter=sideeffect\n"), 0644)
			testGit(t, root, "add", ".gitattributes")
			testGit(t, root, "commit", "--quiet", "-m", "attributes")
			expected := strings.TrimSpace(string(testGit(t, root, "rev-parse", "HEAD")))
			if variant == "assume-unchanged" || variant == "skip-worktree" {
				testGit(t, root, "update-index", "--"+variant, "file.txt")
			}
			if variant == "modified" || variant == "assume-unchanged" || variant == "skip-worktree" || variant == "staged" {
				put(t, root, "file.txt", []byte("dirty\n"), 0644)
			}
			if variant == "staged" {
				testGit(t, root, "add", "file.txt")
			}
			if variant == "untracked" {
				put(t, root, "new.txt", []byte("new"), 0644)
			}
			if variant == "deleted" {
				if err := os.Remove(filepath.Join(root, "file.txt")); err != nil {
					t.Fatal(err)
				}
			}
			put(t, root, ".env", []byte("ignored private content"), 0600)
			monitorMarker, filterMarker := observationHooks(t, root, base)
			indexPath := filepath.Join(root, ".git", "index")
			before, err := os.ReadFile(indexPath)
			if err != nil {
				t.Fatal(err)
			}
			info, err := os.Stat(indexPath)
			if err != nil {
				t.Fatal(err)
			}
			head, err := ReadCleanHead(context.Background(), root)
			if variant == "clean" {
				if err != nil || head != expected {
					t.Fatalf("clean observation: %q %v", head, err)
				}
			} else if err == nil {
				t.Fatal("raw drift accepted")
			}
			after, readErr := os.ReadFile(indexPath)
			if readErr != nil {
				t.Fatal(readErr)
			}
			next, statErr := os.Stat(indexPath)
			if statErr != nil {
				t.Fatal(statErr)
			}
			if !bytes.Equal(before, after) || !info.ModTime().Equal(next.ModTime()) {
				t.Fatal("observation rewrote the real index")
			}
			for _, marker := range []string{monitorMarker, filterMarker} {
				if _, err = os.Stat(marker); !os.IsNotExist(err) {
					t.Fatalf("read-only observation executed configured program: %s", marker)
				}
			}
		})
	}
}

func TestReadCleanHeadIgnoresInheritedGitSelection(t *testing.T) {
	root, _ := fixture(t)
	other, _ := fixture(t)
	put(t, other, "different.txt", []byte("different repo"), 0644)
	testGit(t, other, "add", ".")
	testGit(t, other, "commit", "--quiet", "-m", "different")
	expected := strings.TrimSpace(string(testGit(t, root, "rev-parse", "HEAD")))
	t.Setenv("GIT_DIR", filepath.Join(other, ".git"))
	t.Setenv("GIT_WORK_TREE", other)
	t.Setenv("GIT_INDEX_FILE", filepath.Join(other, ".git", "index"))
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.worktree")
	t.Setenv("GIT_CONFIG_VALUE_0", other)
	head, err := ReadCleanHead(t.Context(), root)
	if err != nil || head != expected {
		t.Fatalf("environment replaced original target: %q %v", head, err)
	}
	put(t, root, "file.txt", []byte("original target drift"), 0644)
	if _, err = ReadCleanHead(t.Context(), root); err == nil {
		t.Fatal("alternate index hid original target drift")
	}
}

func TestReadCleanHeadUsesOriginalObjectsRatherThanReplacementRefs(t *testing.T) {
	root, _ := fixture(t)
	original := strings.TrimSpace(string(testGit(t, root, "rev-parse", "HEAD")))
	put(t, root, "file.txt", []byte("replacement bytes\n"), 0644)
	testGit(t, root, "add", "file.txt")
	testGit(t, root, "commit", "--quiet", "-m", "replacement")
	replacement := strings.TrimSpace(string(testGit(t, root, "rev-parse", "HEAD")))
	testGit(t, root, "reset", "--hard", original)
	testGit(t, root, "replace", original, replacement)
	if head, err := ReadCleanHead(t.Context(), root); err != nil || head != original {
		t.Fatalf("replacement changed observed original objects: %q %v", head, err)
	}
	put(t, root, "file.txt", []byte("replacement bytes\n"), 0644)
	testGit(t, root, "add", "file.txt")
	if _, err := ReadCleanHead(t.Context(), root); err == nil {
		t.Fatal("replacement ref hid index/worktree drift")
	}
}
