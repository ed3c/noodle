package workspaceartifacts

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/poteto/noodle/internal/filex"
)

// Re-sync on resume too: a previous process may have observed an error after
// making a ref visible but before acknowledging its persistence boundary.
func syncPinnedCompaction(ctx context.Context, root string, m *compactionManifest) error {
	raw, err := compactionGit(ctx, root, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	common := strings.TrimSpace(string(raw))
	if err := noLinkedPath(common); err != nil {
		return err
	}
	prefix := compactionRefPrefix(&m.Baseline, &m.Result)
	for _, name := range []string{"base", "head", "manifest"} {
		path := filepath.Join(common, filepath.FromSlash(prefix), name)
		if err := syncRetentionRegular(path); err != nil {
			if !os.IsNotExist(err) {
				return err
			}
			// A later normal Git GC can pack these refs. Their resolved values were
			// already checked; persist the packed representation rather than recreate.
			if err := syncRetentionRegular(filepath.Join(common, "packed-refs")); err != nil {
				return err
			}
		} else {
			for dir := filepath.Dir(path); dir != common; dir = filepath.Dir(dir) {
				if err := filex.SyncDir(dir); err != nil {
					return err
				}
			}
		}
	}
	return filex.SyncDir(common)
}

func syncRetentionRegular(path string) error {
	if err := noLinkedPath(path); err != nil {
		return err
	}
	before, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !before.Mode().IsRegular() {
		return fmt.Errorf("retained path is not a regular file: %s", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil || !os.SameFile(before, info) {
		return errors.Join(fmt.Errorf("retained file changed before sync: %s", path), err, f.Close())
	}
	return errors.Join(f.Sync(), f.Close())
}

func syncCompactionSources(ctx context.Context, root string, m *compactionManifest, oid string) error {
	evidence := filepath.Dir(m.Result.PatchPath)
	for name := range m.Preserved {
		if err := syncRetentionRegular(filepath.Join(evidence, name)); err != nil {
			return err
		}
	}
	if err := syncCompactionIntent(filepath.Join(evidence, compactionManifestName)); err != nil {
		return err
	}
	return syncRetainedGitObjects(ctx, root, m.Parent, m.Head, oid)
}

// syncRetainedGitObjects verifies and persists the complete object closure in
// the original repository. Callers still persist their own refs before deleting
// copies, and exclude concurrent repository mutation through the final deletion.
// Also used by diagnostic baseline compaction, which has no published head.
func syncRetainedGitObjects(ctx context.Context, root string, roots ...string) error {
	return syncRetainedGitObjectsWith(ctx, root, roots, syncRetentionRegular, filex.SyncDir)
}

func syncRetainedGitObjectsWith(ctx context.Context, root string, roots []string, syncFile, syncDir func(string) error) error {
	raw, err := compactionGit(ctx, root, nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	common := strings.TrimSpace(string(raw))
	objects := filepath.Join(common, "objects")
	if err := noLinkedPath(objects); err != nil {
		return err
	}
	// An alternate store is not protected by this repository's refs. Shallow or
	// grafted history would silently omit original ancestors from the traversal.
	for _, path := range []string{filepath.Join(objects, "info", "alternates"), filepath.Join(objects, "info", "http-alternates"), filepath.Join(common, "shallow"), filepath.Join(common, "info", "grafts")} {
		if err := noLinkedPath(path); err != nil {
			return err
		}
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("retained Git object closure has unsupported alternate/shallow/graft storage: %s", path)
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	oids, err := retainedReachableObjects(ctx, root, roots)
	if err != nil {
		return err
	}
	packFiles, err := retainedPackFiles(objects)
	if err != nil {
		return err
	}
	packed := map[string]bool{}
	dirs := map[string]bool{objects: true, common: true}
	for _, path := range packFiles {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := syncFile(path); err != nil {
			return err
		}
		if filepath.Ext(path) == ".idx" {
			index, err := retentionReadFile(path)
			if err != nil {
				return err
			}
			format := "sha1"
			if len(roots[0]) == 64 {
				format = "sha256"
			}
			listing, err := compactionGit(ctx, root, index, "show-index", "--object-format="+format)
			if err != nil {
				return err
			}
			for _, line := range strings.Split(strings.TrimSpace(string(listing)), "\n") {
				fields := strings.Fields(line)
				if len(fields) < 2 || !validObjectID(fields[1]) {
					return fmt.Errorf("retained Git pack index is malformed")
				}
				packed[fields[1]] = true
			}
		}
		dirs[filepath.Dir(path)] = true
	}
	for _, oid := range oids {
		if err := ctx.Err(); err != nil {
			return err
		}
		path := filepath.Join(objects, oid[:2], oid[2:])
		if err := noLinkedPath(path); err != nil {
			return err
		}
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			// Every object was independently read and hashed above. With alternates
			// excluded, a non-loose object belongs to the packs persisted above.
			if !packed[oid] {
				return fmt.Errorf("retained Git object has no persisted loose file or pack index: %s", oid)
			}
			continue
		} else if err != nil {
			return err
		}
		if err := syncFile(path); err != nil {
			return err
		}
		dirs[filepath.Dir(path)] = true
	}
	ordered := make([]string, 0, len(dirs))
	for dir := range dirs {
		ordered = append(ordered, dir)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ordered)))
	for _, dir := range ordered {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := syncDir(dir); err != nil {
			return err
		}
	}
	return nil
}

// Persist pack indexes as well as data; Git may resolve objects through a MIDX.
// Chained MIDX and an in-progress repack are refused rather than partially synced.
func retainedPackFiles(objects string) ([]string, error) {
	dir := filepath.Join(objects, "pack")
	if err := noLinkedPath(dir); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		ext := filepath.Ext(name)
		base := strings.TrimSuffix(name, ext)
		ordinary := strings.HasPrefix(base, "pack-") && validObjectID(strings.TrimPrefix(base, "pack-"))
		allowed := ext == ".pack" || ext == ".idx" || ext == ".rev" || ext == ".bitmap" || ext == ".keep" || ext == ".mtimes"
		if !entry.Type().IsRegular() || (name != "multi-pack-index" && (!ordinary || !allowed)) {
			return nil, fmt.Errorf("retained Git pack storage is unsupported or unsettled: %s", name)
		}
		names[name] = true
		paths = append(paths, filepath.Join(dir, name))
	}
	for name := range names {
		if name == "multi-pack-index" {
			continue
		}
		base := strings.TrimSuffix(name, filepath.Ext(name))
		if !names[base+".pack"] || !names[base+".idx"] {
			return nil, fmt.Errorf("retained Git pack or index is missing: %s", base)
		}
	}
	return paths, nil
}
