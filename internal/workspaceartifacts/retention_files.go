package workspaceartifacts

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/poteto/noodle/internal/filex"
)

type retentionEntry struct {
	Kind   string `json:"kind"`
	Mode   uint32 `json:"mode"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

func compactionPaths(ctx context.Context, repository string, b *Baseline, r *Result, head string) (string, string, []string, error) {
	if b == nil || r == nil || !r.Applied || r.State != "published" || r.Error != "" || !validObjectID(head) || !validObjectID(b.Head) {
		return "", "", nil, fmt.Errorf("compaction publication identity is incomplete")
	}
	if err := noLinkedPath(repository); err != nil {
		return "", "", nil, err
	}
	root, err := repositoryRoot(ctx, repository)
	if err != nil {
		return "", "", nil, err
	}
	evidence := filepath.Dir(r.PatchPath)
	session := filepath.Dir(evidence)
	if filepath.Base(evidence) != "delivery" || r.PatchPath != filepath.Join(evidence, "changes.patch") || b.Directory != filepath.Join(session, "repo") || r.ReturnedDirectory != filepath.Join(session, "returned") {
		return "", "", nil, fmt.Errorf("compaction paths differ from original session layout")
	}
	// A control project owns runtime evidence under this one bounded layout;
	// the execution worktree may already be gone. Other project descendants
	// are not session storage. An external independent session is also valid.
	relativeSession, err := filepath.Rel(root, session)
	if err != nil {
		return "", "", nil, err
	}
	insideProject := relativeSession != ".." && !strings.HasPrefix(relativeSession, ".."+string(filepath.Separator))
	if insideProject && (filepath.Dir(session) != filepath.Join(root, ".noodle", "sessions") || validPath(filepath.Base(session)) != nil) {
		return "", "", nil, fmt.Errorf("compaction session is outside bounded project runtime storage")
	}
	relativeRoot, err := filepath.Rel(session, root)
	if err != nil {
		return "", "", nil, err
	}
	if relativeRoot != ".." && !strings.HasPrefix(relativeRoot, ".."+string(filepath.Separator)) {
		return "", "", nil, fmt.Errorf("compaction session contains project repository")
	}
	copies := []string{b.Directory, r.ReturnedDirectory, filepath.Join(evidence, "baseline"), filepath.Join(evidence, "returned"), filepath.Join(evidence, "patch-repository")}
	for _, path := range append(copies, filepath.Join(evidence, compactionManifestName)) {
		if err := noLinkedPath(path); err != nil {
			return "", "", nil, err
		}
	}
	return root, evidence, copies, nil
}

// Lstat every component, including missing-copy ancestors. Unlike EvalSymlinks,
// this never turns a redirected session path into an authorized deletion path.
func noLinkedPath(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("retention path is not absolute and clean: %q", path)
	}
	current := filepath.VolumeName(path) + string(filepath.Separator)
	rest := strings.TrimPrefix(path, current)
	for _, part := range strings.Split(rest, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("retention path contains symlink: %q", current)
		}
	}
	return nil
}

func retentionReadFile(path string) ([]byte, error) {
	if err := noLinkedPath(path); err != nil {
		return nil, err
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("retained evidence is not a regular file: %q", path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	after, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !os.SameFile(before, after) || before.Mode() != after.Mode() || before.Size() != int64(len(raw)) || after.Size() != int64(len(raw)) {
		return nil, fmt.Errorf("retained evidence changed while reading: %q", path)
	}
	return raw, nil
}

func retentionStat(path string) (retentionEntry, error) {
	// A leaf symlink is recorded as literal link bytes; ancestors never followed.
	if err := noLinkedPath(filepath.Dir(path)); err != nil {
		return retentionEntry{}, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return retentionEntry{}, err
	}
	entry := retentionEntry{Mode: uint32(info.Mode())}
	switch {
	case info.IsDir():
		entry.Kind = "directory"
	case info.Mode().IsRegular():
		raw, err := retentionReadFile(path)
		if err != nil {
			return entry, err
		}
		entry.Kind, entry.Bytes, entry.SHA256 = "file", int64(len(raw)), digest(raw)
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(path)
		if err != nil {
			return entry, err
		}
		entry.Kind, entry.Bytes, entry.SHA256 = "symlink", int64(len(target)), digest([]byte(target))
	default:
		return entry, fmt.Errorf("retained copy contains unsupported file: %q", path)
	}
	return entry, nil
}

func protectedRetentionName(name string) bool {
	name = strings.ToLower(name)
	return name == "instructions" || name == "instructions.md" || name == "instructions.json" || name == "logs" || name == "log" || strings.HasSuffix(name, ".log") || strings.HasSuffix(name, ".tar") || strings.Contains(name, ".tar.")
}

func retentionInventory(ctx context.Context, root string) (map[string]retentionEntry, error) {
	if err := noLinkedPath(root); err != nil {
		return nil, err
	}
	entries := map[string]retentionEntry{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if os.IsNotExist(walkErr) && path == root {
			return nil
		}
		if walkErr != nil {
			return walkErr
		}
		name, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if name != "." && protectedRetentionName(d.Name()) {
			return fmt.Errorf("retained copy contains protected archive/instructions/log: %q", path)
		}
		entry, err := retentionStat(path)
		if err != nil {
			return err
		}
		if name == "." && entry.Kind != "directory" {
			return fmt.Errorf("retained copy root is not a directory: %q", root)
		}
		entries[filepath.ToSlash(name)] = entry
		return nil
	})
	return entries, err
}

func removeRetainedCopy(ctx context.Context, root string, inventory map[string]retentionEntry, remove func(string) error) error {
	current, err := retentionInventory(ctx, root)
	if err != nil {
		return err
	}
	paths := make([]string, 0, len(current))
	for name, entry := range current {
		if original, ok := inventory[name]; !ok || original != entry {
			return fmt.Errorf("retained copy changed before deletion: %q", name)
		}
		paths = append(paths, name)
	}
	// Children before parents, root last. Missing paths from an interrupted
	// deletion stay absent; unexpected or rewritten survivors are never removed.
	sort.Sort(sort.Reverse(sort.StringSlice(paths)))
	for _, name := range paths {
		if err := ctx.Err(); err != nil {
			return err
		}
		path := filepath.Join(root, filepath.FromSlash(name))
		actual, err := retentionStat(path)
		if err != nil {
			return err
		}
		if actual != inventory[name] {
			return fmt.Errorf("retained copy changed during deletion: %q", path)
		}
		if err := remove(path); err != nil {
			return err
		}
		if err := filex.SyncDir(filepath.Dir(path)); err != nil {
			return err
		}
	}
	// Also persist absence when resuming after a lost directory-sync ACK.
	return filex.SyncDir(filepath.Dir(root))
}

func syncCompactionIntent(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	if err := errors.Join(f.Sync(), f.Close()); err != nil {
		return err
	}
	return filex.SyncDir(filepath.Dir(path))
}
