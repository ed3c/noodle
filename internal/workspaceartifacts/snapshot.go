// Package workspaceartifacts transfers repository contents without Git metadata.
// Callers own the target worktree exclusively while capturing and applying. The
// checks detect observed changes; they are not a CAS against arbitrary writers.
package workspaceartifacts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

// Entry records Git's file modes, not platform-specific permission bits.
type Entry struct {
	Mode   string `json:"mode"`
	SHA256 string `json:"sha256"`
}

// Baseline is local evidence. Persist it outside Directory; only Directory is
// uploaded to the worker. Its fields may be encoded and decoded as JSON.
type Baseline struct {
	Head        string           `json:"head"`
	Directory   string           `json:"directory"`
	IndexSHA256 string           `json:"index_sha256"`
	Entries     map[string]Entry `json:"entries"`
}

type contents map[string][]byte

// Capture copies tracked and nonignored untracked files into a new destination.
// Deleted tracked files remain absent. Private environment files stay local.
func Capture(ctx context.Context, source, destination string) (*Baseline, error) {
	source, err := repositoryRoot(ctx, source)
	if err != nil {
		return nil, err
	}
	destination, err = externalDirectory(source, destination)
	if err != nil {
		return nil, err
	}
	head, index, err := identity(ctx, source)
	if err != nil {
		return nil, err
	}
	entries, data, err := scanRepository(ctx, source)
	if err != nil {
		return nil, err
	}
	if err := os.Mkdir(destination, 0700); err != nil {
		return nil, fmt.Errorf("create snapshot: %w", err)
	}
	if err := materialize(destination, entries, data); err != nil {
		return nil, err
	}
	b := &Baseline{Head: head, Directory: destination, IndexSHA256: index, Entries: entries}
	if err := unchanged(ctx, source, b); err != nil {
		return nil, fmt.Errorf("source changed during capture: %w", err)
	}
	return b, nil
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func privatePath(name string) bool {
	for _, component := range strings.Split(name, "/") {
		lower := strings.ToLower(component)
		if lower == ".env" || strings.HasPrefix(lower, ".env.") {
			if lower != ".env.example" && lower != ".env.sample" && lower != ".env.template" {
				return true
			}
		}
	}
	return false
}

func validPath(name string) error {
	if name == "" || name == "." || path.Clean(name) != name || path.IsAbs(name) || strings.ContainsAny(name, "\\:\x00\r\n") {
		return fmt.Errorf("unsafe artifact path %q", name)
	}
	for _, component := range strings.Split(name, "/") {
		lower := strings.ToLower(component)
		if component == ".." || lower == ".git" || strings.TrimRight(lower, " .") == ".git" || strings.HasPrefix(lower, "git~") {
			return fmt.Errorf("unsafe artifact path %q", name)
		}
	}
	if privatePath(name) {
		return fmt.Errorf("private environment artifact %q", name)
	}
	return nil
}

func safeAncestors(root, name string) error {
	dir := ""
	components := strings.Split(name, "/")
	for _, component := range components[:len(components)-1] {
		dir = path.Join(dir, component)
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(dir)))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect ancestor %q: %w", dir, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("artifact ancestor is not a directory: %q", dir)
		}
	}
	return nil
}

func readEntry(root, name string) (Entry, []byte, error) {
	if err := validPath(name); err != nil {
		return Entry{}, nil, err
	}
	if err := safeAncestors(root, name); err != nil {
		return Entry{}, nil, err
	}
	full := filepath.Join(root, filepath.FromSlash(name))
	info, err := os.Lstat(full)
	if err != nil {
		return Entry{}, nil, err
	}
	var data []byte
	mode := "100644"
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		var target string
		target, err = os.Readlink(full)
		if err == nil {
			if path.IsAbs(target) || strings.ContainsAny(target, "\\:\x00\r\n") || target == "" {
				return Entry{}, nil, fmt.Errorf("unsafe symlink %q", name)
			}
			// Cleaning "ignored/../file" before checking would hide a
			// symlink traversal through ignored. Allow leading ../ only.
			seenName := false
			for _, component := range strings.Split(target, "/") {
				if component == ".." && seenName {
					return Entry{}, nil, fmt.Errorf("unsafe symlink traversal %q", name)
				}
				if component != "" && component != "." && component != ".." {
					seenName = true
				}
			}
			resolved := path.Clean(path.Join(path.Dir(name), target))
			if err := validPath(resolved); err != nil {
				return Entry{}, nil, fmt.Errorf("unsafe symlink %q: %w", name, err)
			}
			if err := safeAncestors(root, resolved); err != nil {
				return Entry{}, nil, fmt.Errorf("unsafe symlink %q: %w", name, err)
			}
			// Do not chase chained links, including links into ignored content
			// that is absent from the transferable snapshot.
			if targetInfo, targetErr := os.Lstat(filepath.Join(root, filepath.FromSlash(resolved))); targetErr == nil && targetInfo.Mode()&os.ModeSymlink != 0 {
				return Entry{}, nil, fmt.Errorf("symlink target is another symlink: %q", name)
			} else if targetErr != nil && !os.IsNotExist(targetErr) {
				return Entry{}, nil, targetErr
			}
			data, mode = []byte(target), "120000"
		}
	case info.Mode().IsRegular():
		if info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
			return Entry{}, nil, fmt.Errorf("unsupported file mode for %q", name)
		}
		data, err = os.ReadFile(full)
		if info.Mode().Perm()&0111 != 0 {
			mode = "100755"
		}
	default:
		return Entry{}, nil, fmt.Errorf("unsupported artifact type for %q", name)
	}
	if err != nil {
		return Entry{}, nil, fmt.Errorf("read artifact %q: %w", name, err)
	}
	return Entry{Mode: mode, SHA256: digest(data)}, data, nil
}

func scanRepository(ctx context.Context, root string) (map[string]Entry, contents, error) {
	out, err := git(ctx, root, nil, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return nil, nil, err
	}
	entries, data := map[string]Entry{}, contents{}
	for _, name := range strings.Split(string(out), "\x00") {
		if name == "" || privatePath(name) {
			continue
		}
		entry, body, err := readEntry(root, name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		entries[name], data[name] = entry, body
	}
	return entries, data, nil
}

func scanDirectory(root string) (map[string]Entry, contents, error) {
	info, err := os.Lstat(root)
	if err != nil {
		return nil, nil, err
	}
	if !info.IsDir() {
		return nil, nil, fmt.Errorf("artifact root is not a directory: %q", root)
	}
	entries, data := map[string]Entry{}, contents{}
	err = filepath.WalkDir(root, func(full string, item fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if full == root {
			return nil
		}
		rel, err := filepath.Rel(root, full)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(rel)
		if err := validPath(name); err != nil {
			return err
		}
		if item.IsDir() {
			return nil
		}
		entry, body, err := readEntry(root, name)
		if err != nil {
			return err
		}
		entries[name], data[name] = entry, body
		return nil
	})
	return entries, data, err
}

func materialize(root string, entries map[string]Entry, data contents) error {
	for _, name := range names(entries) {
		if err := validPath(name); err != nil {
			return err
		}
		if err := safeAncestors(root, name); err != nil {
			return err
		}
		full := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0700); err != nil {
			return err
		}
		entry := entries[name]
		if entry.Mode == "120000" {
			if err := os.Symlink(string(data[name]), full); err != nil {
				return err
			}
			continue
		}
		mode := os.FileMode(0644)
		if entry.Mode == "100755" {
			mode = 0755
		}
		if err := os.WriteFile(full, data[name], mode); err != nil {
			return err
		}
		if err := os.Chmod(full, mode); err != nil {
			return err
		}
	}
	return nil
}

func names(entries map[string]Entry) []string {
	result := make([]string, 0, len(entries))
	for name := range entries {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

func unchanged(ctx context.Context, target string, baseline *Baseline) error {
	head, index, err := identity(ctx, target)
	if err != nil {
		return err
	}
	if head != baseline.Head || index != baseline.IndexSHA256 {
		return fmt.Errorf("target HEAD or index changed since capture")
	}
	entries, _, err := scanRepository(ctx, target)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(entries, baseline.Entries) {
		return fmt.Errorf("target contents changed since capture")
	}
	return nil
}
