package workspaceartifacts

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

// VerifyRetainedPublication observes original Git objects and retained return
// bytes after a worktree was removed or reused. It never changes refs/index or
// treats the current checkout as the historical attempt's output.
func VerifyRetainedPublication(ctx context.Context, repository string, baseline *Baseline, result *Result, head string) error {
	if baseline == nil || result == nil || result.Error != "" || !validObjectID(head) || !validObjectID(baseline.Head) {
		return fmt.Errorf("retained publication identity is incomplete")
	}
	unchanged := !result.Applied && result.State == "unchanged"
	if !unchanged && !(result.Applied && result.State == "published") {
		return fmt.Errorf("retained publication outcome is unresolved")
	}
	root, err := repositoryRoot(ctx, repository)
	if err != nil {
		return err
	}
	if unchanged {
		if head != baseline.Head {
			return fmt.Errorf("retained unchanged HEAD differs")
		}
		patch, err := os.ReadFile(result.PatchPath)
		if err != nil {
			return err
		}
		if len(patch) != 0 {
			return fmt.Errorf("retained unchanged patch is nonempty")
		}
	} else {
		raw, err := git(ctx, root, nil, "--no-lazy-fetch", "--no-replace-objects", "cat-file", "commit", head)
		if err != nil {
			return err
		}
		parents := []string{}
		for _, line := range strings.Split(strings.SplitN(string(raw), "\n\n", 2)[0], "\n") {
			if strings.HasPrefix(line, "parent ") {
				parents = append(parents, strings.TrimPrefix(line, "parent "))
			}
		}
		if len(parents) != 1 || parents[0] != baseline.Head {
			return fmt.Errorf("retained publication parent differs")
		}
	}
	before, beforeData, err := scanDirectory(baseline.Directory)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(before, baseline.Entries) {
		return fmt.Errorf("retained baseline bytes or modes changed")
	}
	after, afterData, err := scanDirectory(result.ReturnedDirectory)
	if err != nil {
		return err
	}
	saved, _, err := scanDirectory(filepath.Join(filepath.Dir(result.PatchPath), "returned"))
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(after, saved) {
		return fmt.Errorf("retained return copies differ")
	}
	baseTree, err := retainedTree(ctx, root, baseline.Head)
	if err != nil {
		return err
	}
	finalTree, err := retainedTree(ctx, root, head)
	if err != nil {
		return err
	}
	if err = retainedTreeBytes(ctx, baseTree, before, beforeData); err != nil {
		return err
	}
	if err = retainedTreeBytes(ctx, finalTree, after, afterData); err != nil {
		return err
	}
	for name, entry := range baseTree {
		if privatePath(name) && finalTree[name] != entry {
			return fmt.Errorf("retained private tree identity changed")
		}
	}
	for name := range finalTree {
		if privatePath(name) {
			if _, ok := baseTree[name]; !ok {
				return fmt.Errorf("retained publication added private tree entry")
			}
		}
	}
	return nil
}

func validObjectID(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func retainedTree(ctx context.Context, root, head string) (map[string]publicationTreeEntry, error) {
	raw, err := git(ctx, root, nil, "--no-lazy-fetch", "--no-replace-objects", "ls-tree", "-r", "-z", "--full-tree", head)
	if err != nil {
		return nil, err
	}
	return parsePublicationTree(raw, false)
}
func retainedTreeBytes(ctx context.Context, tree map[string]publicationTreeEntry, entries map[string]Entry, data contents) error {
	count := 0
	for name, object := range tree {
		if err := ctx.Err(); err != nil {
			return err
		}
		if privatePath(name) {
			continue
		}
		entry, ok := entries[name]
		if !ok || entry.Mode != object.Mode || !matchesRawBlob(data[name], object.Object) {
			return fmt.Errorf("retained publication bytes differ at %q", name)
		}
		count++
	}
	if count != len(entries) {
		return fmt.Errorf("retained publication contains uncommitted entries")
	}
	return nil
}
