package workspaceartifacts

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
)

// projectGit deliberately uses the project's normal Git configuration. Unlike
// raw artifact plumbing, commit must honor local/global hooks and identity.
func projectGit(ctx context.Context, target string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", target}, args...)...)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(strings.ToUpper(key), "GIT_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("project git %s failed: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return out, nil
}

func publicationStatus(ctx context.Context, target string) ([]byte, error) {
	return projectGit(ctx, target, "--no-optional-locks", "status", "--porcelain=v1", "-z", "--untracked-files=all", "--no-renames")
}

func publicationIndexPath(ctx context.Context, target string) (string, error) {
	out, err := git(ctx, target, nil, "rev-parse", "--git-path", "index")
	if err != nil {
		return "", err
	}
	path := strings.TrimSpace(string(out))
	if !filepath.IsAbs(path) {
		path = filepath.Join(target, path)
	}
	return path, nil
}

type publicationTreeEntry struct{ Mode, Object string }

func cleanBaselineTree(ctx context.Context, target string, b *Baseline, data contents) (map[string]publicationTreeEntry, error) {
	if _, err := git(ctx, target, nil, "diff", "--cached", "--quiet", b.Head, "--"); err != nil {
		return nil, fmt.Errorf("publication baseline index differs from HEAD: %w", err)
	}
	out, err := git(ctx, target, nil, "ls-tree", "-r", "-z", "--full-tree", b.Head)
	if err != nil {
		return nil, err
	}
	tree, err := parsePublicationTree(out, false)
	if err != nil {
		return nil, err
	}
	count := 0
	for name, entry := range tree {
		if privatePath(name) {
			continue
		}
		captured, exists := b.Entries[name]
		if !exists || captured.Mode != entry.Mode {
			return nil, fmt.Errorf("publication baseline differs from HEAD at %q", name)
		}
		object, err := git(ctx, target, data[name], "hash-object", "--stdin", "--no-filters")
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(string(object)) != entry.Object {
			return nil, fmt.Errorf("publication baseline bytes differ from HEAD at %q", name)
		}
		count++
	}
	if count != len(b.Entries) {
		return nil, fmt.Errorf("publication baseline contains untracked files")
	}
	return tree, nil
}

func parsePublicationTree(out []byte, index bool) (map[string]publicationTreeEntry, error) {
	entries := make(map[string]publicationTreeEntry)
	for _, record := range strings.Split(string(out), "\x00") {
		if record == "" {
			continue
		}
		metadata, name, ok := strings.Cut(record, "\t")
		fields := strings.Fields(metadata)
		if !ok || len(fields) != 3 {
			return nil, fmt.Errorf("Git tree record is malformed")
		}
		object := fields[2]
		if index {
			if fields[2] != "0" {
				return nil, fmt.Errorf("publication index contains unresolved stages")
			}
			object = fields[1]
		} else if fields[1] != "blob" {
			return nil, fmt.Errorf("publication tree contains unsupported type at %q", name)
		}
		entries[name] = publicationTreeEntry{Mode: fields[0], Object: object}
	}
	return entries, nil
}

func verifyPublicationIndex(ctx context.Context, target string, expected map[string]publicationTreeEntry) error {
	out, err := git(ctx, target, nil, "ls-files", "--stage", "-z")
	if err != nil {
		return err
	}
	actual, err := parsePublicationTree(out, true)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(actual, expected) {
		return fmt.Errorf("publication index differs from verified candidate tree")
	}
	return nil
}

func changedEntries(before, after map[string]Entry) map[string]Entry {
	changed := make(map[string]Entry)
	for name, entry := range before {
		if current, exists := after[name]; !exists || current != entry {
			changed[name] = entry
		}
	}
	for name, entry := range after {
		if original, exists := before[name]; !exists || original != entry {
			changed[name] = entry
		}
	}
	return changed
}

func verifyPublicationWork(ctx context.Context, target string, before, after, changes map[string]Entry) error {
	actual, _, err := scanRepository(ctx, target)
	if err != nil {
		return err
	}
	for name, entry := range actual {
		if expected, exists := after[name]; !exists || expected != entry {
			return fmt.Errorf("publication worktree differs from returned artifacts at %q", name)
		}
	}
	for name, expected := range after {
		entry, _, err := readEntry(target, name)
		if err != nil {
			return err
		}
		if entry != expected {
			return fmt.Errorf("publication artifact bytes or mode changed at %q", name)
		}
	}
	for name := range before {
		if _, exists := after[name]; exists {
			continue
		}
		if _, err := os.Lstat(filepath.Join(target, filepath.FromSlash(name))); !os.IsNotExist(err) {
			return fmt.Errorf("publication deleted artifact exists or cannot be checked: %q: %v", name, err)
		}
	}
	status, err := publicationStatus(ctx, target)
	if err != nil {
		return err
	}
	for _, record := range strings.Split(string(status), "\x00") {
		if record == "" {
			continue
		}
		if len(record) < 4 {
			return fmt.Errorf("publication status record is malformed")
		}
		if _, allowed := changes[record[3:]]; !allowed {
			return fmt.Errorf("publication has unrelated local changes at %q", record[3:])
		}
	}
	return nil
}
