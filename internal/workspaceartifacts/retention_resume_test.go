package workspaceartifacts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func pinnedResumeFixture(t *testing.T) (string, *Baseline, *Result, string, string) {
	t.Helper()
	root, b, r, head := compactFixture(t)
	stop := errors.New("retain complete pinned fixture")
	err := compactRetainedPublication(t.Context(), root, b, r, head, func(string) error { return stop })
	if !errors.Is(err, stop) {
		t.Fatalf("prepare original pinned intent: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(r.PatchPath), compactionManifestName))
	if err != nil {
		t.Fatal(err)
	}
	return root, b, r, head, digest(raw)
}

func pinnedResumeSnapshot(t *testing.T, root string, b *Baseline, r *Result, copies bool) map[string]string {
	t.Helper()
	evidence := filepath.Dir(r.PatchPath)
	state := map[string]string{
		"pins": string(testGit(t, root, "for-each-ref", "--format=%(refname) %(objectname) %(symref)", compactionRefPrefix(b, r)+"/")),
	}
	for _, name := range []string{compactionManifestName, "changes.patch", "baseline.json", "result.json", "publish.json", "publish-index.before", "publish-index.after", "publish-commit.log"} {
		entry, err := retentionStat(filepath.Join(evidence, name))
		if os.IsNotExist(err) {
			state[name] = "absent"
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(entry)
		if err != nil {
			t.Fatal(err)
		}
		state[name] = string(raw)
	}
	for _, name := range []string{"noodle-result.tar", "noodle-session.tar", "instructions/input.json", "logs/stderr.log"} {
		raw, err := os.ReadFile(filepath.Join(filepath.Dir(evidence), name))
		if err != nil {
			t.Fatal(err)
		}
		state["original/"+name] = digest(raw)
	}
	if copies {
		for _, dir := range []string{b.Directory, r.ReturnedDirectory, filepath.Join(evidence, "baseline"), filepath.Join(evidence, "returned"), filepath.Join(evidence, "patch-repository")} {
			inventory, err := retentionInventory(t.Context(), dir)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := json.Marshal(inventory)
			if err != nil {
				t.Fatal(err)
			}
			state["copy/"+dir] = string(raw)
		}
	}
	return state
}

func TestPinnedCompactionResumeRefusesMissingManifestWithoutCreating(t *testing.T) {
	root, b, r, head := compactFixture(t)
	before := pinnedResumeSnapshot(t, root, b, r, true)
	if err := ResumePinnedRetainedPublication(t.Context(), root, b, r, head, strings.Repeat("a", 64)); err == nil {
		t.Fatal("resume created a first compaction intent")
	}
	if !reflect.DeepEqual(before, pinnedResumeSnapshot(t, root, b, r, true)) {
		t.Fatal("missing-intent refusal changed pins, evidence or original copies")
	}
}

func TestPinnedCompactionResumeRefusesChangedAuthorityBeforeDeletion(t *testing.T) {
	variants := []string{"manifest-missing", "manifest-digest", "manifest-unpinned-bytes", "all-pins-missing", "unexpected-pin", "survivor-change", "new-file", "preserved-change", "missing-copy-inventory", "cancelled"}
	for _, name := range []string{"base", "head", "manifest"} {
		for _, kind := range []string{"missing-", "changed-", "symbolic-"} {
			variants = append(variants, kind+name)
		}
	}
	for _, variant := range variants {
		t.Run(variant, func(t *testing.T) {
			root, b, r, head, expected := pinnedResumeFixture(t)
			prefix := compactionRefPrefix(b, r)
			path := filepath.Join(filepath.Dir(r.PatchPath), compactionManifestName)
			ctx := t.Context()
			switch {
			case variant == "manifest-missing":
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			case variant == "manifest-digest":
				expected = strings.Repeat("0", 64)
			case variant == "manifest-unpinned-bytes":
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				raw = append(raw, ' ')
				put(t, filepath.Dir(path), filepath.Base(path), raw, 0600)
				expected = digest(raw)
			case variant == "all-pins-missing":
				for _, name := range []string{"base", "head", "manifest"} {
					testGit(t, root, "update-ref", "-d", prefix+"/"+name)
				}
			case variant == "unexpected-pin":
				testGit(t, root, "update-ref", prefix+"/extra", head)
			case strings.HasPrefix(variant, "missing-") && variant != "missing-copy-inventory":
				testGit(t, root, "update-ref", "-d", prefix+"/"+strings.TrimPrefix(variant, "missing-"))
			case strings.HasPrefix(variant, "changed-"):
				name := strings.TrimPrefix(variant, "changed-")
				oid := head
				if name == "head" {
					oid = b.Head
				}
				testGit(t, root, "update-ref", prefix+"/"+name, oid)
			case strings.HasPrefix(variant, "symbolic-"):
				target := strings.TrimSpace(string(testGit(t, root, "symbolic-ref", "HEAD")))
				testGit(t, root, "symbolic-ref", prefix+"/"+strings.TrimPrefix(variant, "symbolic-"), target)
			case variant == "survivor-change":
				put(t, b.Directory, "file.txt", []byte("changed original survivor"), 0644)
			case variant == "new-file":
				put(t, b.Directory, "unreviewed-file", []byte("new file"), 0644)
			case variant == "preserved-change":
				put(t, filepath.Dir(r.PatchPath), "changes.patch", []byte("changed evidence"), 0600)
			case variant == "missing-copy-inventory":
				raw, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				var m compactionManifest
				if err := json.Unmarshal(raw, &m); err != nil {
					t.Fatal(err)
				}
				delete(m.Copies, b.Directory)
				raw, err = json.Marshal(m)
				if err != nil {
					t.Fatal(err)
				}
				put(t, filepath.Dir(path), filepath.Base(path), raw, 0600)
				expected = digest(raw)
				oid, err := compactionGit(t.Context(), root, raw, "hash-object", "-w", "--stdin", "--no-filters")
				if err != nil {
					t.Fatal(err)
				}
				testGit(t, root, "update-ref", prefix+"/manifest", strings.TrimSpace(string(oid)))
			case variant == "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(t.Context())
				cancel()
			}
			before := pinnedResumeSnapshot(t, root, b, r, true)
			removed := 0
			err := resumePinnedRetainedPublication(ctx, root, b, r, head, expected, func(string) error {
				removed++
				return errors.New("deletion crossed refused authority")
			})
			if err == nil || removed != 0 {
				t.Fatalf("invalid resume reached deletion: calls=%d error=%v", removed, err)
			}
			if !reflect.DeepEqual(before, pinnedResumeSnapshot(t, root, b, r, true)) {
				t.Fatal("resume refusal repaired pins/manifest or changed evidence/copies")
			}
		})
	}
}

func TestPinnedCompactionResumeCompletesOriginalPartialIntent(t *testing.T) {
	root, b, r, head, expected := pinnedResumeFixture(t)
	before := pinnedResumeSnapshot(t, root, b, r, false)
	stop := errors.New("interrupt original pinned deletion")
	calls := 0
	err := resumePinnedRetainedPublication(t.Context(), root, b, r, head, expected, func(path string) error {
		calls++
		if calls == 5 {
			return stop
		}
		return os.Remove(path)
	})
	if !errors.Is(err, stop) || calls != 5 {
		t.Fatalf("partial fixture did not reach its interruption: calls=%d error=%v", calls, err)
	}
	for range 2 {
		if err := ResumePinnedRetainedPublication(t.Context(), root, b, r, head, expected); err != nil {
			t.Fatal(err)
		}
		if err := VerifyCompactedPublication(t.Context(), root, b, r, head); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(before, pinnedResumeSnapshot(t, root, b, r, false)) {
			t.Fatal("resume changed original pins/manifest/preserved evidence")
		}
	}
	_, _, copies, err := compactionPaths(t.Context(), root, b, r, head)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range copies {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("original authorized copy remains: %s %v", path, err)
		}
	}
}

func TestPinnedCompactionResumePreservesPackedPins(t *testing.T) {
	root, b, r, head, expected := pinnedResumeFixture(t)
	testGit(t, root, "pack-refs", "--all")
	before := pinnedResumeSnapshot(t, root, b, r, false)
	packed := filepath.Join(root, ".git", "packed-refs")
	raw, err := os.ReadFile(packed)
	if err != nil {
		t.Fatal(err)
	}
	if err := ResumePinnedRetainedPublication(t.Context(), root, b, r, head, expected); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(packed)
	if err != nil || !bytes.Equal(raw, after) || !reflect.DeepEqual(before, pinnedResumeSnapshot(t, root, b, r, false)) {
		t.Fatal("resume rewrote packed refs or preserved evidence", err)
	}
}
