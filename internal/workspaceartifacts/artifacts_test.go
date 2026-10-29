package workspaceartifacts

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func testGit(t *testing.T, root string, args ...string) []byte {
	t.Helper()
	out, err := git(context.Background(), root, nil, args...)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func put(t *testing.T, root, name string, data []byte, mode os.FileMode) {
	t.Helper()
	full := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(full), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, data, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(full, mode); err != nil {
		t.Fatal(err)
	}
}

func fixture(t *testing.T) (string, string) {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "target")
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	testGit(t, root, "init", "--quiet", "--template=")
	testGit(t, root, "config", "user.name", "Artifact Test")
	testGit(t, root, "config", "user.email", "artifact@example.invalid")
	put(t, root, "file.txt", []byte("original\n"), 0644)
	put(t, root, ".gitignore", []byte("ignored\n.env\n"), 0644)
	testGit(t, root, "add", ".")
	testGit(t, root, "commit", "--quiet", "-m", "test fixture")
	return root, base
}

func captureForTest(t *testing.T, root, base string) (*Baseline, string) {
	t.Helper()
	b, err := Capture(context.Background(), root, filepath.Join(base, "snapshot"))
	if err != nil {
		t.Fatal(err)
	}
	// Exercise persistence/reload, not just the in-memory object.
	body, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	var loaded Baseline
	if err := json.Unmarshal(body, &loaded); err != nil {
		t.Fatal(err)
	}
	returned := filepath.Join(base, "returned")
	if err := os.Mkdir(returned, 0700); err != nil {
		t.Fatal(err)
	}
	entries, data, err := scanDirectory(b.Directory)
	if err != nil {
		t.Fatal(err)
	}
	if err := materialize(returned, entries, data); err != nil {
		t.Fatal(err)
	}
	return &loaded, returned
}

func TestRoundTripPreservesBinaryModesDeletesSymlinksAndIndex(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires Unix executable bits and symlink creation")
	}
	root, base := fixture(t)
	put(t, root, "binary", []byte{0, 1, 2, 255, 0}, 0644)
	put(t, root, "delete", []byte("delete me"), 0644)
	put(t, root, "staged", []byte("index content"), 0644)
	testGit(t, root, "add", "binary", "delete", "staged")
	put(t, root, "staged", []byte("working content"), 0644)
	put(t, root, "untracked", []byte("local work"), 0644)
	put(t, root, "ignored", []byte("ignored local"), 0644)
	put(t, root, ".env", []byte("PRIVATE=kept-local\n"), 0600)
	if err := os.Symlink("file.txt", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	beforeIndex := testGit(t, root, "diff", "--cached", "--binary")
	b, returned := captureForTest(t, root, base)
	for _, name := range []string{".git", ".env", "ignored"} {
		if _, err := os.Lstat(filepath.Join(b.Directory, name)); !os.IsNotExist(err) {
			t.Fatalf("excluded %s exists: %v", name, err)
		}
	}
	put(t, returned, "binary", []byte{0, 99, 0, 255, 42}, 0644)
	put(t, returned, "file.txt", []byte("new\r\nraw bytes\n"), 0755)
	put(t, returned, "new/nul", []byte{0, 0, 3, 0}, 0644)
	if err := os.Remove(filepath.Join(returned, "delete")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(returned, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("new/nul", filepath.Join(returned, "link")); err != nil {
		t.Fatal(err)
	}
	result, err := Apply(context.Background(), root, b, returned, filepath.Join(base, "evidence"))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Applied || result.State != "applied" {
		t.Fatalf("result = %+v", result)
	}
	patch, err := os.ReadFile(result.PatchPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(patch, []byte("GIT binary patch")) {
		t.Fatalf("missing binary patch: %s", patch)
	}
	if !bytes.Equal(beforeIndex, testGit(t, root, "diff", "--cached", "--binary")) {
		t.Fatal("staged changes altered")
	}
	if body, err := os.ReadFile(filepath.Join(root, ".env")); err != nil || string(body) != "PRIVATE=kept-local\n" {
		t.Fatalf("private file changed: %q, %v", body, err)
	}
	if body, err := os.ReadFile(filepath.Join(root, "ignored")); err != nil || string(body) != "ignored local" {
		t.Fatalf("ignored file changed: %q, %v", body, err)
	}
	want, _, err := scanDirectory(returned)
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := scanRepository(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("roundtrip contents differ: got %#v want %#v", got, want)
	}
}

func TestConflictLeavesTargetUntouchedAndRetainsEvidence(t *testing.T) {
	for _, change := range []string{"bytes", "mode", "new file", "delete", "index", "head", "ignored collision"} {
		t.Run(change, func(t *testing.T) {
			if change == "mode" && runtime.GOOS == "windows" {
				t.Skip("executable modes")
			}
			root, base := fixture(t)
			b, returned := captureForTest(t, root, base)
			put(t, returned, "file.txt", []byte("worker\n"), 0644)
			switch change {
			case "bytes":
				put(t, root, "file.txt", []byte("local\n"), 0644)
			case "mode":
				if err := os.Chmod(filepath.Join(root, "file.txt"), 0755); err != nil {
					t.Fatal(err)
				}
			case "new file":
				put(t, root, "local-new", []byte("local"), 0644)
			case "delete":
				if err := os.Remove(filepath.Join(root, "file.txt")); err != nil {
					t.Fatal(err)
				}
			case "index":
				testGit(t, root, "update-index", "--assume-unchanged", "file.txt")
			case "head":
				testGit(t, root, "commit", "--allow-empty", "--quiet", "-m", "new local commit")
			case "ignored collision":
				put(t, root, "ignored", []byte("local"), 0644)
				put(t, returned, "ignored", []byte("worker"), 0644)
			}
			before, body, err := scanRepository(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			head, index, err := identity(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			result, err := Apply(context.Background(), root, b, returned, filepath.Join(base, "evidence"))
			if err == nil || result == nil || result.Applied || result.State != "rejected" {
				t.Fatalf("result=%+v error=%v", result, err)
			}
			after, afterBody, err := scanRepository(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			newHead, newIndex, err := identity(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(body, afterBody) || head != newHead || index != newIndex {
				t.Fatal("rejection mutated target")
			}
			if change == "ignored collision" {
				raw, _ := os.ReadFile(filepath.Join(root, "ignored"))
				if string(raw) != "local" {
					t.Fatal("ignored collision overwritten")
				}
			}
			for _, name := range []string{"baseline.json", "baseline/file.txt", "returned/file.txt", "changes.patch", "result.json"} {
				if _, err := os.Lstat(filepath.Join(base, "evidence", name)); err != nil {
					t.Fatalf("missing evidence %s: %v", name, err)
				}
			}
		})
	}
}

func TestReturnedUnsafeArtifactsRejected(t *testing.T) {
	for _, attack := range []string{".git/config", "sub/.GIT/config", ".env", "sub/.env.production", "colon:name", "back\\slash", "absolute symlink", "escaping symlink", "metadata symlink", "traversal symlink", "setuid mode"} {
		t.Run(attack, func(t *testing.T) {
			if strings.Contains(attack, "symlink") && runtime.GOOS == "windows" {
				t.Skip("requires symlink creation")
			}
			root, base := fixture(t)
			b, returned := captureForTest(t, root, base)
			put(t, returned, "file.txt", []byte("worker"), 0644)
			switch attack {
			case "absolute symlink":
				if err := os.Symlink(filepath.Join(base, "outside"), filepath.Join(returned, "escape")); err != nil {
					t.Fatal(err)
				}
			case "escaping symlink":
				if err := os.Symlink("../outside", filepath.Join(returned, "escape")); err != nil {
					t.Fatal(err)
				}
			case "metadata symlink":
				if err := os.Symlink(".git/config", filepath.Join(returned, "escape")); err != nil {
					t.Fatal(err)
				}
			case "traversal symlink":
				if err := os.Symlink("ignored/../file.txt", filepath.Join(returned, "escape")); err != nil {
					t.Fatal(err)
				}
			case "setuid mode":
				if runtime.GOOS == "windows" {
					t.Skip("setuid modes")
				}
				if err := os.Chmod(filepath.Join(returned, "file.txt"), 0755|os.ModeSetuid); err != nil {
					t.Fatal(err)
				}
			default:
				put(t, returned, attack, []byte("malicious"), 0644)
			}
			result, err := Apply(context.Background(), root, b, returned, filepath.Join(base, "evidence"))
			if err == nil || result == nil || result.State != "rejected" {
				t.Fatalf("unsafe accepted: %+v %v", result, err)
			}
			if err := unchanged(context.Background(), root, b); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(base, "evidence", "result.json")); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRejectTargetSymlinkAncestor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires symlink creation")
	}
	root, base := fixture(t)
	put(t, root, ".gitignore", []byte("ignored\n"), 0644)
	b, returned := captureForTest(t, root, base)
	outside := filepath.Join(base, "outside")
	if err := os.Mkdir(outside, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "ignored")); err != nil {
		t.Fatal(err)
	}
	put(t, returned, "ignored/payload", []byte("do not escape"), 0644)
	result, err := Apply(context.Background(), root, b, returned, filepath.Join(base, "evidence"))
	if err == nil || result.State != "rejected" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if _, err := os.Stat(filepath.Join(outside, "payload")); !os.IsNotExist(err) {
		t.Fatal("outside path written")
	}
}

func TestRejectRelativeLinkThroughIgnoredLocalSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires symlink creation")
	}
	for _, linkTarget := range []string{"ignored/private", "ignored"} {
		t.Run(linkTarget, func(t *testing.T) {
			root, base := fixture(t)
			b, returned := captureForTest(t, root, base)
			outside := filepath.Join(base, "outside")
			if err := os.Mkdir(outside, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(root, "ignored")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(linkTarget, filepath.Join(returned, "link")); err != nil {
				t.Fatal(err)
			}
			result, err := Apply(context.Background(), root, b, returned, filepath.Join(base, "evidence"))
			if err == nil || result.State != "rejected" {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if _, err := os.Lstat(filepath.Join(root, "link")); !os.IsNotExist(err) {
				t.Fatal("unsafe link applied")
			}
		})
	}
}

func TestCapturedTrackedDeletionAndPrivateFileStayAbsent(t *testing.T) {
	root, base := fixture(t)
	put(t, root, ".env.production", []byte("private"), 0600)
	testGit(t, root, "add", "-f", ".env.production")
	if err := os.Remove(filepath.Join(root, "file.txt")); err != nil {
		t.Fatal(err)
	}
	b, returned := captureForTest(t, root, base)
	if _, exists := b.Entries["file.txt"]; exists {
		t.Fatal("deleted file captured")
	}
	if _, exists := b.Entries[".env.production"]; exists {
		t.Fatal("tracked private file captured")
	}
	put(t, returned, "new", []byte("worker"), 0644)
	result, err := Apply(context.Background(), root, b, returned, filepath.Join(base, "evidence"))
	if err != nil || !result.Applied {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if _, err := os.Lstat(filepath.Join(root, "file.txt")); !os.IsNotExist(err) {
		t.Fatal("local deletion restored")
	}
	if body, err := os.ReadFile(filepath.Join(root, ".env.production")); err != nil || string(body) != "private" {
		t.Fatal("tracked private file changed")
	}
}

func TestBaselineTamperingRejected(t *testing.T) {
	for _, tamper := range []string{"bytes", "manifest mode", "manifest path"} {
		t.Run(tamper, func(t *testing.T) {
			root, base := fixture(t)
			b, returned := captureForTest(t, root, base)
			switch tamper {
			case "bytes":
				put(t, b.Directory, "file.txt", []byte("tampered"), 0644)
			case "manifest mode":
				entry := b.Entries["file.txt"]
				entry.Mode = "160000"
				b.Entries["file.txt"] = entry
			case "manifest path":
				b.Entries["../escape"] = b.Entries["file.txt"]
			}
			result, err := Apply(context.Background(), root, b, returned, filepath.Join(base, "evidence"))
			if err == nil || result.State != "rejected" {
				t.Fatalf("tampered accepted: %+v %v", result, err)
			}
			if body, _ := os.ReadFile(filepath.Join(root, "file.txt")); string(body) != "original\n" {
				t.Fatal("target mutated")
			}
		})
	}
}

func TestCaptureRejectsExternalSymlinkAndNestedDestination(t *testing.T) {
	root, base := fixture(t)
	if _, err := Capture(context.Background(), root, filepath.Join(root, "snapshot")); err == nil {
		t.Fatal("nested destination accepted")
	}
	if runtime.GOOS == "windows" {
		return
	}
	if err := os.Symlink("../private", filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := Capture(context.Background(), root, filepath.Join(base, "snapshot")); err == nil {
		t.Fatal("external symlink accepted")
	}
}

func TestRawBytesBypassGitFiltersAndIgnoreRules(t *testing.T) {
	root, base := fixture(t)
	put(t, root, ".gitattributes", []byte("*.txt text eol=crlf filter=broken\n"), 0644)
	testGit(t, root, "config", "filter.broken.clean", "false")
	testGit(t, root, "config", "filter.broken.smudge", "false")
	testGit(t, root, "config", "filter.broken.required", "true")
	b, returned := captureForTest(t, root, base)
	put(t, returned, "file.txt", []byte("raw\nbytes\r\n"), 0644)
	put(t, returned, "new.txt", []byte("raw\n"), 0644)
	put(t, returned, ".gitignore", []byte("new.txt\n"), 0644)
	result, err := Apply(context.Background(), root, b, returned, filepath.Join(base, "evidence"))
	if err != nil || !result.Applied {
		t.Fatalf("raw roundtrip: %+v %v", result, err)
	}
}

func TestUnchangedReturnDoesNotApply(t *testing.T) {
	root, base := fixture(t)
	b, returned := captureForTest(t, root, base)
	result, err := Apply(context.Background(), root, b, returned, filepath.Join(base, "evidence"))
	if err != nil || result.Applied || result.State != "unchanged" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
