package workspaceartifacts

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func publicationFixture(t *testing.T) (string, string) {
	t.Helper()
	root, base := fixture(t)
	hooks := filepath.Join(base, "hooks")
	if err := os.Mkdir(hooks, 0700); err != nil {
		t.Fatal(err)
	}
	testGit(t, root, "config", "core.hooksPath", hooks)
	testGit(t, root, "config", "commit.gpgsign", "false")
	return root, base
}

func appliedForPublication(t *testing.T, root, base string) (*Baseline, *Result) {
	t.Helper()
	b, returned := captureForTest(t, root, base)
	put(t, returned, "file.txt", []byte("worker result\n"), 0644)
	result, err := Apply(context.Background(), root, b, returned, filepath.Join(base, "evidence"))
	if err != nil {
		t.Fatal(err)
	}
	return b, result
}

func TestRequireCleanIncludesUntrackedAndPreservesIgnoredPrivateFiles(t *testing.T) {
	root, _ := publicationFixture(t)
	put(t, root, ".env", []byte("private"), 0600)
	if err := RequireClean(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	put(t, root, "untracked", []byte("local work"), 0644)
	if err := RequireClean(context.Background(), root); err == nil {
		t.Fatal("untracked file accepted")
	}
	if err := os.Remove(filepath.Join(root, "untracked")); err != nil {
		t.Fatal(err)
	}
	put(t, root, "file.txt", []byte("local work"), 0644)
	if err := RequireClean(context.Background(), root); err == nil {
		t.Fatal("modified tracked file accepted")
	}
	testGit(t, root, "add", "file.txt")
	if err := RequireClean(context.Background(), root); err == nil {
		t.Fatal("staged file accepted")
	}
}

func TestPublishCreatesCandidateWithHooksAndPreservesPrivateFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires executable bits and POSIX hook")
	}
	root, base := publicationFixture(t)
	put(t, root, ".env.production", []byte("tracked private\n"), 0600)
	testGit(t, root, "add", "-f", ".env.production")
	testGit(t, root, "commit", "--quiet", "-m", "private fixture")
	put(t, root, ".env", []byte("ignored private\n"), 0600)
	put(t, base, "hooks/pre-commit", []byte("#!/bin/sh\nprintf ran >> ../hook-ran\n"), 0755)
	if err := RequireClean(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	b, returned := captureForTest(t, root, base)
	if err := os.Remove(filepath.Join(returned, "file.txt")); err != nil {
		t.Fatal(err)
	}
	put(t, returned, "binary", []byte{0, 1, 2, 255, 0}, 0755)
	if err := os.Symlink("binary", filepath.Join(returned, "link")); err != nil {
		t.Fatal(err)
	}
	result, err := Apply(context.Background(), root, b, returned, filepath.Join(base, "evidence"))
	if err != nil {
		t.Fatal(err)
	}
	head, err := Publish(context.Background(), root, b, result, "feat: accept worker result\n\nPreserve the verified worker changes for review.")
	if err != nil {
		t.Fatal(err)
	}
	if head == b.Head || result.State != "published" || !result.Applied {
		t.Fatalf("head=%s result=%+v", head, result)
	}
	if hook, err := os.ReadFile(filepath.Join(base, "hook-ran")); err != nil || string(hook) != "ran" {
		t.Fatalf("hook not executed: %q %v", hook, err)
	}
	if err := RequireClean(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	if actual := strings.TrimSpace(string(testGit(t, root, "rev-parse", "HEAD^"))); actual != b.Head {
		t.Fatalf("parent=%s", actual)
	}
	if actual := testGit(t, root, "show", "HEAD:binary"); !bytes.Equal(actual, []byte{0, 1, 2, 255, 0}) {
		t.Fatalf("binary changed: %v", actual)
	}
	if actual := testGit(t, root, "show", "HEAD:.env.production"); string(actual) != "tracked private\n" {
		t.Fatal("private tracked blob changed")
	}
	if actual, err := os.ReadFile(filepath.Join(root, ".env")); err != nil || string(actual) != "ignored private\n" {
		t.Fatal("ignored private file changed")
	}
	for _, name := range []string{".env", ".env.production"} {
		if bytes.Contains(testGit(t, root, "diff", "--name-only", b.Head, "HEAD", "--"), []byte(name)) {
			t.Fatalf("private path staged: %s", name)
		}
	}
	var receipt publicationReceipt
	body, err := os.ReadFile(filepath.Join(base, "evidence", "publish.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Phase != "published" || receipt.ObservedHead != head || receipt.ExpectedTree == "" {
		t.Fatalf("receipt=%+v", receipt)
	}
}

func TestPublishHookRejectionRetainsUnknownAndNeverRetries(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX hook")
	}
	root, base := publicationFixture(t)
	put(t, base, "hooks/pre-commit", []byte("#!/bin/sh\nprintf ran >> ../hook-ran\necho hook-rejected >&2\nexit 1\n"), 0755)
	b, result := appliedForPublication(t, root, base)
	head, err := Publish(context.Background(), root, b, result, "test: candidate\n\nVerify hook rejection.")
	if err == nil || head != "" || result.State != "unknown" || result.Applied {
		t.Fatalf("head=%s result=%+v err=%v", head, result, err)
	}
	if actual := strings.TrimSpace(string(testGit(t, root, "rev-parse", "HEAD"))); actual != b.Head {
		t.Fatal("rejected hook changed HEAD")
	}
	if len(testGit(t, root, "diff", "--cached", "--name-only")) == 0 {
		t.Fatal("failed publication silently rolled back index")
	}
	for _, name := range []string{"publish.json", "publish-index.before", "publish-index.after", "publish-commit.log"} {
		if _, err := os.Stat(filepath.Join(base, "evidence", name)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Publish(context.Background(), root, b, result, "retry"); err == nil {
		t.Fatal("unknown publication replayed")
	}
	staleResult := *result
	staleResult.State, staleResult.Applied = "applied", true
	if _, err := Publish(context.Background(), root, b, &staleResult, "stale retry"); err == nil || !strings.Contains(err.Error(), "one-shot") {
		t.Fatalf("saved publication intent replayed: %v", err)
	}
	if hook, err := os.ReadFile(filepath.Join(base, "hook-ran")); err != nil || string(hook) != "ran" {
		t.Fatalf("hook repeated: %q %v", hook, err)
	}
}

func TestPublishRejectsHiddenDirtyBaseline(t *testing.T) {
	root, base := publicationFixture(t)
	testGit(t, root, "update-index", "--assume-unchanged", "file.txt")
	put(t, root, "file.txt", []byte("hidden local edits\n"), 0644)
	// Status can hide dirty bytes under an index flag. Independently proving
	// the captured baseline against HEAD prevents publishing those local edits.
	if err := RequireClean(context.Background(), root); err != nil {
		t.Fatal(err)
	}
	b, result := appliedForPublication(t, root, base)
	if _, err := Publish(context.Background(), root, b, result, "test: raw baseline"); err == nil || !strings.Contains(err.Error(), "baseline bytes differ") {
		t.Fatalf("raw baseline drift accepted: %v", err)
	}
}

func TestPublishRejectsDirtyBaselinesWithoutChangingIndex(t *testing.T) {
	for _, dirty := range []string{"working file", "staged file", "untracked file"} {
		t.Run(dirty, func(t *testing.T) {
			root, base := publicationFixture(t)
			switch dirty {
			case "working file":
				put(t, root, "file.txt", []byte("dirty"), 0644)
			case "staged file":
				put(t, root, "file.txt", []byte("dirty"), 0644)
				testGit(t, root, "add", "file.txt")
			case "untracked file":
				put(t, root, "local", []byte("dirty"), 0644)
			}
			b, result := appliedForPublication(t, root, base)
			beforeHead, beforeIndex, err := identity(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Publish(context.Background(), root, b, result, "test: reject dirty baseline"); err == nil {
				t.Fatal("dirty baseline published")
			}
			afterHead, afterIndex, err := identity(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			if beforeHead != afterHead || beforeIndex != afterIndex || result.State != "applied" {
				t.Fatal("preflight rejection mutated Git state")
			}
		})
	}
}

func TestPublishRejectsDriftBeforeGitMutation(t *testing.T) {
	for _, drift := range []string{"head", "index", "working bytes", "unrelated file", "returned bytes"} {
		t.Run(drift, func(t *testing.T) {
			root, base := publicationFixture(t)
			b, result := appliedForPublication(t, root, base)
			switch drift {
			case "head":
				testGit(t, root, "commit", "--quiet", "--allow-empty", "-m", "local commit")
			case "index":
				testGit(t, root, "update-index", "--assume-unchanged", "file.txt")
			case "working bytes":
				put(t, root, "file.txt", []byte("local drift"), 0644)
			case "unrelated file":
				put(t, root, "local", []byte("local drift"), 0644)
			case "returned bytes":
				put(t, result.ReturnedDirectory, "file.txt", []byte("local drift"), 0644)
			}
			beforeHead, beforeIndex, err := identity(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := Publish(context.Background(), root, b, result, "test: drift"); err == nil {
				t.Fatal("drift accepted")
			}
			afterHead, afterIndex, err := identity(context.Background(), root)
			if err != nil {
				t.Fatal(err)
			}
			if beforeHead != afterHead || beforeIndex != afterIndex {
				t.Fatal("drift rejection mutated target Git state")
			}
		})
	}
}

func TestPublishDetectsHookChangingCandidateTree(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX hook")
	}
	root, base := publicationFixture(t)
	put(t, base, "hooks/pre-commit", []byte("#!/bin/sh\nprintf hook-change > injected\ngit add injected\n"), 0755)
	b, result := appliedForPublication(t, root, base)
	head, err := Publish(context.Background(), root, b, result, "test: candidate mutation")
	if err == nil || head != "" || result.State != "unknown" || !strings.Contains(err.Error(), "tree differs") {
		t.Fatalf("head=%s result=%+v err=%v", head, result, err)
	}
	if actual := strings.TrimSpace(string(testGit(t, root, "rev-parse", "HEAD"))); actual == b.Head {
		t.Fatal("test hook did not produce candidate commit")
	}
}
