package workspaceartifacts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func compactFixture(t *testing.T) (string, *Baseline, *Result, string) {
	t.Helper()
	return compactFixtureLayout(t, false)
}

func compactFixtureLayout(t *testing.T, inProject bool) (string, *Baseline, *Result, string) {
	t.Helper()
	root, base := publicationFixture(t)
	base, err := filepath.EvalSymlinks(base)
	if err != nil {
		t.Fatal(err)
	}
	root = filepath.Join(base, "target")
	session := filepath.Join(base, "session")
	if inProject {
		put(t, root, ".gitignore", []byte("ignored\n.env\n.noodle/\n"), 0644)
		testGit(t, root, "add", ".gitignore")
		testGit(t, root, "commit", "-m", "test: ignore runtime state")
		session = filepath.Join(root, ".noodle", "sessions", "session-01")
	}
	if err := os.MkdirAll(session, 0700); err != nil {
		t.Fatal(err)
	}
	source := root
	if inProject {
		source = filepath.Join(base, "execution")
		testGit(t, root, "worktree", "add", "--detach", source, "HEAD")
	}
	b, err := Capture(t.Context(), source, filepath.Join(session, "repo"))
	if err != nil {
		t.Fatal(err)
	}
	returned := filepath.Join(session, "returned")
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
	put(t, returned, "file.txt", []byte("worker\x00raw\r\n"), 0755)
	r, err := Apply(t.Context(), source, b, returned, filepath.Join(session, "delivery"))
	if err != nil {
		t.Fatal(err)
	}
	head, err := Publish(t.Context(), source, b, r, "test: retained compaction")
	if err != nil {
		t.Fatal(err)
	}
	if inProject {
		testGit(t, root, "worktree", "remove", source)
	}
	for _, name := range []string{"noodle-result.tar", "noodle-session.tar", "instructions/input.json", "logs/stderr.log"} {
		put(t, session, name, []byte(name), 0600)
	}
	return root, b, r, head
}

func TestCompactedPublicationReclaimsCopiesAndSurvivesGC(t *testing.T) {
	root, b, r, head := compactFixture(t)
	evidence := filepath.Dir(r.PatchPath)
	kept := map[string][]byte{}
	for _, name := range []string{"changes.patch", "baseline.json", "result.json", "publish.json", "publish-commit.log", "publish-index.before", "publish-index.after"} {
		raw, err := os.ReadFile(filepath.Join(evidence, name))
		if err != nil {
			t.Fatal(err)
		}
		kept[name] = raw
	}
	if err := CompactRetainedPublication(t.Context(), root, b, r, head); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{b.Directory, r.ReturnedDirectory, filepath.Join(evidence, "baseline"), filepath.Join(evidence, "returned"), filepath.Join(evidence, "patch-repository")} {
		if _, err := os.Lstat(dir); !os.IsNotExist(err) {
			t.Fatalf("copy survived: %s %v", dir, err)
		}
	}
	for name, want := range kept {
		got, err := os.ReadFile(filepath.Join(evidence, name))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("evidence changed: %s %v", name, err)
		}
	}
	for _, name := range []string{"noodle-result.tar", "noodle-session.tar", "instructions/input.json", "logs/stderr.log"} {
		got, err := os.ReadFile(filepath.Join(filepath.Dir(evidence), name))
		if err != nil || string(got) != name {
			t.Fatalf("protected evidence changed: %s %v", name, err)
		}
	}
	testGit(t, root, "reset", "--hard", b.Head)
	testGit(t, root, "reflog", "expire", "--expire=now", "--all")
	testGit(t, root, "gc", "--prune=now")
	if err := VerifyCompactedPublication(t.Context(), root, b, r, head); err != nil {
		t.Fatal(err)
	}
	if err := CompactRetainedPublication(t.Context(), root, b, r, head); err != nil {
		t.Fatal(err)
	}
	if err := VerifyRetainedPublication(t.Context(), root, b, r, head); err == nil {
		t.Fatal("old verifier accepted removed originals")
	}
}

func TestCompactionRejectsSourceDriftBeforeDeleting(t *testing.T) {
	for _, kind := range []string{"baseline", "returned", "saved-baseline", "saved-returned", "json", "symlink", "archive", "missing-object"} {
		t.Run(kind, func(t *testing.T) {
			root, b, r, head := compactFixture(t)
			evidence := filepath.Dir(r.PatchPath)
			switch kind {
			case "baseline":
				put(t, b.Directory, "file.txt", []byte("drift"), 0644)
			case "returned":
				put(t, r.ReturnedDirectory, "file.txt", []byte("drift"), 0644)
			case "saved-baseline":
				put(t, filepath.Join(evidence, "baseline"), "file.txt", []byte("drift"), 0644)
			case "saved-returned":
				put(t, filepath.Join(evidence, "returned"), "file.txt", []byte("drift"), 0644)
			case "json":
				put(t, evidence, "result.json", []byte("{}"), 0600)
			case "symlink":
				old := b.Directory + "-moved"
				if err := os.Rename(b.Directory, old); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(old, b.Directory); err != nil {
					t.Fatal(err)
				}
			case "archive":
				put(t, filepath.Join(evidence, "patch-repository"), "keep.tar", []byte("archive"), 0600)
			case "missing-object":
				oid := strings.TrimSpace(string(testGit(t, root, "rev-parse", head+":file.txt")))
				if err := os.Remove(filepath.Join(root, ".git", "objects", oid[:2], oid[2:])); err != nil {
					t.Fatal(err)
				}

			}
			if err := CompactRetainedPublication(t.Context(), root, b, r, head); err == nil {
				t.Fatal("changed source accepted")
			}
			if _, err := os.Lstat(r.ReturnedDirectory); err != nil {
				t.Fatal("deleted before verification", err)
			}
		})
	}
}

func TestCompactedPublicationRejectsChangedEvidenceAndObjects(t *testing.T) {
	for _, kind := range []string{"manifest", "json", "patch", "ref", "missing-blob", "rewritten-blob", "missing-tree", "missing-commit", "rewritten-commit", "parent"} {
		t.Run(kind, func(t *testing.T) {
			root, b, r, head := compactFixture(t)
			if err := CompactRetainedPublication(t.Context(), root, b, r, head); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "manifest":
				path := filepath.Join(filepath.Dir(r.PatchPath), compactionManifestName)
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var m map[string]any
				if err := json.Unmarshal(raw, &m); err != nil {
					t.Fatal(err)
				}
				m["head"] = b.Head
				raw, err = json.Marshal(m)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "json":
				put(t, filepath.Dir(r.PatchPath), "publish.json", []byte("{}"), 0600)
			case "patch":
				put(t, filepath.Dir(r.PatchPath), "changes.patch", []byte("drift"), 0600)
			case "ref":
				testGit(t, root, "update-ref", compactionRefPrefix(b, r)+"/head", b.Head)
			case "missing-tree", "missing-commit", "rewritten-commit":
				oid := head
				if kind == "missing-tree" {
					oid = strings.TrimSpace(string(testGit(t, root, "rev-parse", head+"^{tree}")))
				}
				path := filepath.Join(root, ".git", "objects", oid[:2], oid[2:])
				original := []byte(nil)
				if kind == "rewritten-commit" {
					original = testGit(t, root, "cat-file", "commit", head)
				}
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if kind == "rewritten-commit" {
					changed, err := git(t.Context(), root, append(original, []byte("modified message\n")...), "hash-object", "-w", "-t", "commit", "--stdin")
					if err != nil {
						t.Fatal(err)
					}
					other := strings.TrimSpace(string(changed))
					compressed, err := os.ReadFile(filepath.Join(root, ".git", "objects", other[:2], other[2:]))
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, compressed, 0600); err != nil {
						t.Fatal(err)
					}
				}
			case "parent":
				b.Head = head
			case "missing-blob", "rewritten-blob":
				oid := strings.TrimSpace(string(testGit(t, root, "rev-parse", head+":file.txt")))
				path := filepath.Join(root, ".git", "objects", oid[:2], oid[2:])
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
				if kind == "rewritten-blob" {
					other := strings.TrimSpace(string(testGit(t, root, "hash-object", "-w", "--stdin")))
					raw, err := os.ReadFile(filepath.Join(root, ".git", "objects", other[:2], other[2:]))
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, raw, 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := VerifyCompactedPublication(t.Context(), root, b, r, head); err == nil {
				t.Fatal("changed evidence accepted")
			}
		})
	}
}

func TestCompactionResumesSameIntentAfterPartialDeletion(t *testing.T) {
	root, b, r, head := compactFixture(t)
	injected := errors.New("injected deletion failure")
	calls := 0
	remove := func(path string) error {
		calls++
		if calls == 5 {
			return injected
		}
		return os.Remove(path)
	}
	if err := compactRetainedPublication(t.Context(), root, b, r, head, remove); !errors.Is(err, injected) {
		t.Fatalf("failure=%v", err)
	}
	manifest := filepath.Join(filepath.Dir(r.PatchPath), compactionManifestName)
	before, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(b.Directory); !os.IsNotExist(err) {
		t.Fatalf("first copy not reclaimed: %v", err)
	}
	if err := VerifyCompactedPublication(t.Context(), root, b, r, head); err != nil {
		t.Fatal(err)
	}
	if err := CompactRetainedPublication(t.Context(), root, b, r, head); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(manifest)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("intent changed on resume", err)
	}
}

func TestCompactionCancelledBeforeIntent(t *testing.T) {
	root, b, r, head := compactFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := CompactRetainedPublication(ctx, root, b, r, head); err == nil {
		t.Fatal("cancelled compaction continued")
	}
	if _, err := os.Stat(b.Directory); err != nil {
		t.Fatal(err)
	}
}

func TestCompactionUnpinnedIntentRequiresUnchangedOriginals(t *testing.T) {
	for _, variant := range []string{"resume", "missing", "changed", "edited-inventory"} {
		t.Run(variant, func(t *testing.T) {
			root, b, r, head := compactFixture(t)
			evidence := filepath.Dir(r.PatchPath)
			_, _, copies, err := compactionPaths(t.Context(), root, b, r, head)
			if err != nil {
				t.Fatal(err)
			}
			m, err := prepareCompaction(t.Context(), root, b, r, head, evidence, copies)
			if err != nil {
				t.Fatal(err)
			}
			if variant == "missing" {
				if err := os.Remove(filepath.Join(b.Directory, "file.txt")); err != nil {
					t.Fatal(err)
				}
			}
			if variant == "changed" || variant == "edited-inventory" {
				put(t, r.ReturnedDirectory, "file.txt", []byte("changed after intent"), 0755)
				if variant == "edited-inventory" {
					m.Copies[r.ReturnedDirectory], err = retentionInventory(t.Context(), r.ReturnedDirectory)
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			raw, err := json.Marshal(m)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(evidence, compactionManifestName), raw, 0600); err != nil {
				t.Fatal(err)
			}
			if err := VerifyCompactedPublication(t.Context(), root, b, r, head); err == nil {
				t.Fatal("unpinned receipt accepted")
			}
			err = CompactRetainedPublication(t.Context(), root, b, r, head)
			if variant == "resume" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				if err == nil {
					t.Fatal("unpinned drift accepted")
				}
				if _, err := os.Stat(r.ReturnedDirectory); err != nil {
					t.Fatal("deleted unpinned input", err)
				}
			}
		})
	}
}

func TestCompactionPartialSurvivorDriftRefusesResume(t *testing.T) {
	root, b, r, head := compactFixture(t)
	fail := errors.New("stop after first copy")
	remove := func(path string) error {
		if strings.HasPrefix(path, r.ReturnedDirectory+string(filepath.Separator)) {
			return fail
		}
		return os.Remove(path)
	}
	if err := compactRetainedPublication(t.Context(), root, b, r, head, remove); !errors.Is(err, fail) {
		t.Fatalf("failure=%v", err)
	}
	put(t, r.ReturnedDirectory, "file.txt", []byte("reused session"), 0755)
	if err := VerifyCompactedPublication(t.Context(), root, b, r, head); err == nil {
		t.Fatal("changed survivor accepted")
	}
	if err := CompactRetainedPublication(t.Context(), root, b, r, head); err == nil {
		t.Fatal("changed survivor deleted")
	}
	got, err := os.ReadFile(filepath.Join(r.ReturnedDirectory, "file.txt"))
	if err != nil || string(got) != "reused session" {
		t.Fatal("changed survivor lost", err)
	}
}

func TestCompactionIntentWriteFailurePreservesAllCopies(t *testing.T) {
	root, b, r, head := compactFixture(t)
	path := filepath.Join(filepath.Dir(r.PatchPath), compactionManifestName)
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := CompactRetainedPublication(t.Context(), root, b, r, head); err == nil {
		t.Fatal("invalid intent destination accepted")
	}
	for _, path := range []string{b.Directory, r.ReturnedDirectory, filepath.Join(filepath.Dir(r.PatchPath), "patch-repository")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal("deleted without durable intent", err)
		}
	}
}

func TestCompactionRawModesLinksAndIsolatedGit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX executable bits and symlinks")
	}
	root, b, r, head := compactFixture(t)
	// Create a separate publication with a nested tree and literal symlink blob.
	session := filepath.Join(filepath.Dir(b.Directory), "next")
	if err := os.Mkdir(session, 0700); err != nil {
		t.Fatal(err)
	}
	put(t, root, ".env.production", []byte("private"), 0600)
	testGit(t, root, "add", "-f", ".env.production")
	testGit(t, root, "commit", "-m", "private fixture")
	b, err := Capture(t.Context(), root, filepath.Join(session, "repo"))
	if err != nil {
		t.Fatal(err)
	}
	returned := filepath.Join(session, "returned")
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
	put(t, returned, "nested/executable", []byte{0, 255, 13, 10, 0}, 0755)
	if err := os.Symlink("nested/executable", filepath.Join(returned, "link")); err != nil {
		t.Fatal(err)
	}
	r, err = Apply(t.Context(), root, b, returned, filepath.Join(session, "delivery"))
	if err != nil {
		t.Fatal(err)
	}
	head, err = Publish(t.Context(), root, b, r, "test: preserve raw links")
	if err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Dir(session), "hook", []byte("#!/bin/sh\nexit 99\n"), 0755)
	testGit(t, root, "config", "core.fsmonitor", filepath.Join(filepath.Dir(session), "hook"))
	put(t, filepath.Join(filepath.Dir(session), "..", "hooks"), "reference-transaction", []byte("#!/bin/sh\nexit 99\n"), 0755)
	t.Setenv("GIT_DIR", filepath.Join(session, "foreign.git"))
	t.Setenv("GIT_INDEX_FILE", filepath.Join(session, "foreign.index"))
	if err := CompactRetainedPublication(t.Context(), root, b, r, head); err != nil {
		t.Fatal(err)
	}
	if err := VerifyCompactedPublication(t.Context(), root, b, r, head); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(r.PatchPath), compactionManifestName))
	if err != nil {
		t.Fatal(err)
	}
	var m compactionManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.HeadObjects["link"].Mode != "120000" || m.HeadObjects["nested/executable"].Mode != "100755" || m.HeadObjects["nested/executable"].SHA256 != digest([]byte{0, 255, 13, 10, 0}) {
		t.Fatalf("raw entry identities lost: %+v", m.HeadObjects)
	}
}

func TestCompactionRejectsConflictingRefsBeforeDeleting(t *testing.T) {
	root, b, r, head := compactFixture(t)
	testGit(t, root, "update-ref", compactionRefPrefix(b, r)+"/head", b.Head)
	if err := CompactRetainedPublication(t.Context(), root, b, r, head); err == nil {
		t.Fatal("conflicting partial ref set accepted")
	}
	if _, err := os.Stat(b.Directory); err != nil {
		t.Fatal("deleted with conflicting refs", err)
	}
}

func TestCompactionInsideOriginalProjectRuntime(t *testing.T) {
	root, b, r, head := compactFixtureLayout(t, true)
	// Other runtime/session data and the project's actual Git store stay intact.
	put(t, root, ".noodle/sessions/other/instructions/input.json", []byte("other session"), 0600)
	before := testGit(t, root, "rev-parse", "HEAD")
	if err := CompactRetainedPublication(t.Context(), root, b, r, head); err != nil {
		t.Fatal(err)
	}
	if err := VerifyCompactedPublication(t.Context(), root, b, r, head); err != nil {
		t.Fatal(err)
	}
	if err := CompactRetainedPublication(t.Context(), root, b, r, head); err != nil {
		t.Fatal(err)
	}
	if got := testGit(t, root, "rev-parse", "HEAD"); !bytes.Equal(before, got) {
		t.Fatal("project HEAD changed")
	}
	for _, dir := range []string{b.Directory, r.ReturnedDirectory, filepath.Join(filepath.Dir(r.PatchPath), "baseline"), filepath.Join(filepath.Dir(r.PatchPath), "returned"), filepath.Join(filepath.Dir(r.PatchPath), "patch-repository")} {
		if _, err := os.Lstat(dir); !os.IsNotExist(err) {
			t.Fatalf("copy survived: %s %v", dir, err)
		}
	}
	for _, path := range []string{"file.txt", ".git/config", ".noodle/sessions/session-01/noodle-result.tar", ".noodle/sessions/session-01/noodle-session.tar", ".noodle/sessions/session-01/instructions/input.json", ".noodle/sessions/session-01/logs/stderr.log", ".noodle/sessions/other/instructions/input.json"} {
		if _, err := os.Stat(filepath.Join(root, path)); err != nil {
			t.Fatalf("protected project/session evidence lost: %s %v", path, err)
		}
	}
	testGit(t, root, "reset", "--hard", b.Head)
	testGit(t, root, "reflog", "expire", "--expire=now", "--all")
	testGit(t, root, "gc", "--prune=now")
	if err := VerifyCompactedPublication(t.Context(), root, b, r, head); err != nil {
		t.Fatal(err)
	}
}

func TestCompactionRejectsUnboundedProjectLayouts(t *testing.T) {
	root, b, r, head := compactFixture(t)
	for _, session := range []string{root, filepath.Dir(root), filepath.VolumeName(root) + string(filepath.Separator), filepath.Join(root, "arbitrary"), filepath.Join(root, ".noodle"), filepath.Join(root, ".noodle", "sessions"), filepath.Join(root, ".noodle", "sessions", "nested", "id"), filepath.Join(root, ".noodle", "sessions", "unsafe:id")} {
		changedB, changedR := *b, *r
		changedB.Directory = filepath.Join(session, "repo")
		changedR.ReturnedDirectory = filepath.Join(session, "returned")
		changedR.PatchPath = filepath.Join(session, "delivery", "changes.patch")
		if _, _, _, err := compactionPaths(t.Context(), root, &changedB, &changedR, head); err == nil {
			t.Fatalf("unbounded session accepted: %s", session)
		}
	}
}
