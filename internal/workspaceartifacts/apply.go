package workspaceartifacts

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
)

type Result struct {
	Applied           bool   `json:"applied"`
	State             string `json:"state"`
	PatchPath         string `json:"patch_path"`
	ReturnedDirectory string `json:"returned_directory"`
	Error             string `json:"error"`
}

// Apply validates a complete returned directory and applies its binary diff.
// Evidence must be a new directory outside target. Failures retain evidence and
// are never retried. An error after git apply starts has an unknown outcome.
func Apply(ctx context.Context, target string, baseline *Baseline, returned, evidence string) (result *Result, resultErr error) {
	target, err := repositoryRoot(ctx, target)
	if err != nil {
		return nil, err
	}
	evidence, err = externalDirectory(target, evidence)
	if err != nil {
		return nil, err
	}
	if err := os.Mkdir(evidence, 0700); err != nil {
		return nil, fmt.Errorf("create artifact evidence: %w", err)
	}
	result = &Result{State: "rejected", PatchPath: filepath.Join(evidence, "changes.patch"), ReturnedDirectory: returned}
	defer func() {
		if resultErr != nil {
			result.Error = resultErr.Error()
		}
		if err := writeJSON(filepath.Join(evidence, "result.json"), result); err != nil {
			if result.Applied {
				result.State, result.Applied = "unknown", false
			}
			resultErr = errors.Join(resultErr, fmt.Errorf("persist artifact outcome: %w", err))
		}
	}()
	if baseline == nil {
		return result, fmt.Errorf("artifact baseline is missing")
	}
	if err := writeJSON(filepath.Join(evidence, "baseline.json"), baseline); err != nil {
		return result, err
	}
	before, beforeData, err := scanDirectory(baseline.Directory)
	if err != nil {
		return result, fmt.Errorf("validate baseline: %w", err)
	}
	if !reflect.DeepEqual(before, baseline.Entries) {
		return result, fmt.Errorf("baseline snapshot bytes or modes changed")
	}
	after, afterData, err := scanDirectory(returned)
	if err != nil {
		return result, fmt.Errorf("validate returned artifacts: %w", err)
	}
	for _, item := range []struct {
		name    string
		entries map[string]Entry
		data    contents
	}{{"baseline", before, beforeData}, {"returned", after, afterData}} {
		dir := filepath.Join(evidence, item.name)
		if err := os.Mkdir(dir, 0700); err != nil {
			return result, err
		}
		if err := materialize(dir, item.entries, item.data); err != nil {
			return result, err
		}
	}
	build := filepath.Join(evidence, "patch-repository")
	if err := os.Mkdir(build, 0700); err != nil {
		return result, err
	}
	if _, err := git(ctx, build, nil, "init", "--quiet", "--template="); err != nil {
		return result, err
	}
	// info/attributes overrides worker-controlled .gitattributes. The isolated
	// Git directory also keeps target filters/config and its index out of apply.
	if err := os.MkdirAll(filepath.Join(build, ".git", "info"), 0700); err != nil {
		return result, err
	}
	if err := os.WriteFile(filepath.Join(build, ".git", "info", "attributes"), []byte("* -text -filter -ident -working-tree-encoding\n"), 0600); err != nil {
		return result, err
	}
	from, err := writeTree(ctx, build, before, beforeData)
	if err != nil {
		return result, err
	}
	to, err := writeTree(ctx, build, after, afterData)
	if err != nil {
		return result, err
	}
	patch, err := git(ctx, build, nil, "diff", "--binary", "--full-index", "--no-renames", "--no-ext-diff", "--no-textconv", "--src-prefix=a/", "--dst-prefix=b/", from, to, "--")
	if err != nil {
		return result, err
	}
	if err := os.WriteFile(result.PatchPath, patch, 0600); err != nil {
		return result, err
	}
	if err := unchanged(ctx, target, baseline); err != nil {
		return result, err
	}
	// ls-files omits ignored files; a new artifact must not overwrite one.
	for name := range after {
		if err := safeAncestors(target, name); err != nil {
			return result, err
		}
		if _, existed := before[name]; existed {
			continue
		}
		if _, err := os.Lstat(filepath.Join(target, filepath.FromSlash(name))); err == nil {
			return result, fmt.Errorf("new artifact collides with local path %q", name)
		} else if !os.IsNotExist(err) {
			return result, err
		}
	}
	// A safe-looking relative link must not acquire an external destination
	// through an ignored local symlink that the returned tree did not contain.
	for name, entry := range after {
		if entry.Mode != "120000" {
			continue
		}
		resolved := filepath.Clean(filepath.Join(filepath.Dir(filepath.FromSlash(name)), string(afterData[name])))
		if err := safeAncestors(target, filepath.ToSlash(resolved)); err != nil {
			return result, err
		}
		if info, err := os.Lstat(filepath.Join(target, resolved)); err == nil && info.Mode()&os.ModeSymlink != 0 {
			return result, fmt.Errorf("returned symlink points to local symlink: %q", name)
		} else if err != nil && !os.IsNotExist(err) {
			return result, err
		}
	}
	if len(patch) == 0 {
		result.State = "unchanged"
		return result, nil
	}
	if err := applyGit(ctx, target, build, patch, true); err != nil {
		return result, err
	}
	// Narrow the gap after potentially expensive validation. This is still a
	// single-writer contract, not an atomic filesystem compare-and-swap.
	if err := unchanged(ctx, target, baseline); err != nil {
		return result, err
	}
	result.State = "unknown"
	if err := writeJSON(filepath.Join(evidence, "result.json"), result); err != nil {
		return result, err
	}
	if err := applyGit(ctx, target, build, patch, false); err != nil {
		return result, err
	}
	head, index, err := identity(ctx, target)
	if err != nil {
		return result, err
	}
	if head != baseline.Head || index != baseline.IndexSHA256 {
		return result, fmt.Errorf("target HEAD or index changed during artifact apply")
	}
	// Check explicit paths even if returned .gitignore now hides them.
	for name, expected := range after {
		actual, _, err := readEntry(target, name)
		if err != nil {
			return result, err
		}
		if actual != expected {
			return result, fmt.Errorf("applied artifact bytes or mode differ: %q", name)
		}
	}
	for name := range before {
		if _, exists := after[name]; exists {
			continue
		}
		if _, err := os.Lstat(filepath.Join(target, filepath.FromSlash(name))); !os.IsNotExist(err) {
			return result, fmt.Errorf("deleted artifact still exists or cannot be checked: %q: %v", name, err)
		}
	}
	result.State, result.Applied = "applied", true
	return result, nil
}

func writeJSON(path string, value any) error {
	body, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(body, '\n'), 0600)
}
