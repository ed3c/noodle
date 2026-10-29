package workspaceartifacts

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"github.com/poteto/noodle/internal/filex"
)

const compactionManifestName = "compaction.json"

type compactedObject struct {
	Entry
	Bytes  int64  `json:"bytes"`
	Object string `json:"object"`
}

type compactionManifest struct {
	Version     int                                  `json:"version"`
	Repository  string                               `json:"repository"`
	Head        string                               `json:"head"`
	Parent      string                               `json:"parent"`
	Baseline    Baseline                             `json:"baseline"`
	Result      Result                               `json:"result"`
	BaseObjects map[string]compactedObject           `json:"base_objects"`
	HeadObjects map[string]compactedObject           `json:"head_objects"`
	Preserved   map[string]retentionEntry            `json:"preserved"`
	Copies      map[string]map[string]retentionEntry `json:"copies"`
}

// CompactRetainedPublication retires only the five reconstructable copies of a
// publication. The caller owns eligibility (accepted or explicitly closed), and
// exclusively owns the session and repository for the duration of this call.
// Layout is session/{repo,returned,delivery}; evidence stays in delivery.
// Session may be external or project/.noodle/sessions/<safe-id>. All supplied
// paths are absolute and canonical; symlink components are rejected.
// The immutable delivery/compaction.json is a durable deletion intent, anchored
// by fixed project Git refs before any deletion. Repeating the same call checks
// the original intent and remaining bytes, then resumes interrupted deletion.
func CompactRetainedPublication(ctx context.Context, repository string, baseline *Baseline, result *Result, head string) error {
	return compactRetainedPublication(ctx, repository, baseline, result, head, os.Remove)
}

func compactRetainedPublication(ctx context.Context, repository string, b *Baseline, r *Result, head string, remove func(string) error) error {
	root, evidence, copies, err := compactionPaths(ctx, repository, b, r, head)
	if err != nil {
		return err
	}
	path := filepath.Join(evidence, compactionManifestName)
	raw, err := retentionReadFile(path)
	if os.IsNotExist(err) {
		m, buildErr := prepareCompaction(ctx, root, b, r, head, evidence, copies)
		if buildErr != nil {
			return buildErr
		}
		raw, err = json.MarshalIndent(m, "", "  ")
		if err != nil {
			return err
		}
		raw = append(raw, '\n')
		// Atomic rename + file and directory fsync: a failed persistence boundary
		// leaves every input in place. Session exclusion is the caller's contract.
		if err := filex.WriteFileAtomicDurable(path, raw); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	m, err := readCompaction(raw, root, b, r, head)
	if err != nil {
		return err
	}
	if err := verifyCompaction(ctx, root, m, raw, false); err != nil {
		return err
	}
	if err := pinCompaction(ctx, root, m, raw); err != nil {
		return err
	}
	if err := verifyCompaction(ctx, root, m, raw, true); err != nil {
		return err
	}
	for _, dir := range copies {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := removeRetainedCopy(ctx, dir, m.Copies[dir], remove); err != nil {
			return fmt.Errorf("compact retained copy %q: %w", dir, err)
		}
	}
	return nil
}

// VerifyCompactedPublication verifies the immutable intent, preserved original
// evidence, fixed refs, exact parent/tree, and every surviving raw Git blob's
// object ID, SHA256, byte length and mode. Original copies may be absent or
// partly removed; any remaining files are checked against the original intent.
// It never grants task acceptance, removes files, fetches objects or repairs refs.
func VerifyCompactedPublication(ctx context.Context, repository string, b *Baseline, r *Result, head string) error {
	root, evidence, _, err := compactionPaths(ctx, repository, b, r, head)
	if err != nil {
		return err
	}
	raw, err := retentionReadFile(filepath.Join(evidence, compactionManifestName))
	if err != nil {
		return err
	}
	m, err := readCompaction(raw, root, b, r, head)
	if err != nil {
		return err
	}
	return verifyCompaction(ctx, root, m, raw, true)
}

func prepareCompaction(ctx context.Context, root string, b *Baseline, r *Result, head, evidence string, copies []string) (*compactionManifest, error) {
	if err := VerifyRetainedPublication(ctx, root, b, r, head); err != nil {
		return nil, err
	}
	saved, _, err := scanDirectory(filepath.Join(evidence, "baseline"))
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(saved, b.Entries) {
		return nil, fmt.Errorf("retained baseline copies differ")
	}
	// Bind original JSON semantics before freezing its exact bytes.
	for _, item := range []struct {
		name     string
		expected any
		target   any
	}{
		{"baseline.json", b, &Baseline{}}, {"result.json", r, &Result{}},
	} {
		raw, err := retentionReadFile(filepath.Join(evidence, item.name))
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, item.target); err != nil {
			return nil, err
		}
		if !reflect.DeepEqual(item.expected, item.target) {
			return nil, fmt.Errorf("retained %s differs", item.name)
		}
	}
	raw, err := retentionReadFile(filepath.Join(evidence, "publish.json"))
	if err != nil {
		return nil, err
	}
	var published publicationReceipt
	if err := json.Unmarshal(raw, &published); err != nil {
		return nil, err
	}
	if published.Phase != "published" || published.Parent != b.Head || published.ObservedHead != head || published.Error != "" || published.OriginalIndexSHA256 != b.IndexSHA256 {
		return nil, fmt.Errorf("retained publication receipt differs")
	}
	tree, err := git(ctx, root, nil, "--no-lazy-fetch", "--no-replace-objects", "rev-parse", head+"^{tree}")
	if err != nil {
		return nil, err
	}
	if string(tree) != published.ExpectedTree+"\n" {
		return nil, fmt.Errorf("retained publication tree receipt differs")
	}
	m := &compactionManifest{Version: 1, Repository: root, Head: head, Parent: b.Head, Baseline: *b, Result: *r, Copies: map[string]map[string]retentionEntry{}, Preserved: map[string]retentionEntry{}}
	m.BaseObjects, err = retainedObjects(ctx, root, b.Head)
	if err != nil {
		return nil, err
	}
	m.HeadObjects, err = retainedObjects(ctx, root, head)
	if err != nil {
		return nil, err
	}
	for _, dir := range copies {
		inventory, err := retentionInventory(ctx, dir)
		if err != nil {
			return nil, err
		}
		if _, ok := inventory["."]; !ok {
			return nil, fmt.Errorf("retained copy is missing: %q", dir)
		}
		m.Copies[dir] = inventory
	}
	for _, name := range []string{"changes.patch", "baseline.json", "result.json", "publish.json", "publish-index.before", "publish-index.after", "publish-commit.log"} {
		entry, err := retentionStat(filepath.Join(evidence, name))
		if err != nil {
			return nil, err
		}
		if entry.Kind != "file" {
			return nil, fmt.Errorf("retained evidence is not a regular file: %s", name)
		}
		m.Preserved[name] = entry
	}
	// Re-observe sources after the expensive object reads; no deletion has begun.
	if err := verifyCompaction(ctx, root, m, nil, false); err != nil {
		return nil, err
	}
	return m, nil
}

func readCompaction(raw []byte, root string, b *Baseline, r *Result, head string) (*compactionManifest, error) {
	var m compactionManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("read compaction intent: %w", err)
	}
	if m.Version != 1 || m.Repository != root || m.Head != head || m.Parent != b.Head || !reflect.DeepEqual(&m.Baseline, b) || !reflect.DeepEqual(&m.Result, r) {
		return nil, fmt.Errorf("compaction intent identity differs")
	}
	return &m, nil
}

func verifyCompaction(ctx context.Context, root string, m *compactionManifest, raw []byte, requireRefs bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if requireRefs {
		if err := verifyCompactionRefs(ctx, root, m, raw); err != nil {
			return err
		}
	}
	if err := retainedParent(ctx, root, m.Head, m.Parent); err != nil {
		return err
	}
	for _, item := range []struct {
		head     string
		expected map[string]compactedObject
	}{{m.Parent, m.BaseObjects}, {m.Head, m.HeadObjects}} {
		actual, err := retainedObjects(ctx, root, item.head)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(actual, item.expected) {
			return fmt.Errorf("compacted publication objects differ")
		}
	}
	evidence := filepath.Dir(m.Result.PatchPath)
	for name, want := range m.Preserved {
		if err := validPath(name); err != nil {
			return err
		}
		actual, err := retentionStat(filepath.Join(evidence, name))
		if err != nil {
			return err
		}
		if actual != want {
			return fmt.Errorf("preserved publication evidence changed: %s", name)
		}
	}
	for dir, want := range m.Copies {
		actual, err := retentionInventory(ctx, dir)
		if err != nil {
			return err
		}
		for name, entry := range actual {
			if original, ok := want[name]; !ok || original != entry {
				return fmt.Errorf("retained copy changed at %q", filepath.Join(dir, name))
			}
		}
	}
	return nil
}
