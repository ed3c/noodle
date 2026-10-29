package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/lockfile"
	loopruntime "github.com/poteto/noodle/runtime"
)

func artifactCompactionFile(path string, value any) (loopruntime.SessionReadbackFileRef, error) {
	raw, err := config.ReadExplicitSource(path)
	if err != nil {
		return loopruntime.SessionReadbackFileRef{}, err
	}
	if value != nil {
		if err := json.Unmarshal(raw, value); err != nil {
			return loopruntime.SessionReadbackFileRef{}, err
		}
	}
	return loopruntime.SessionReadbackFileRef{Path: path, SHA256: hostDigest(raw)}, nil
}

func artifactCompactionCheckRefs(refs map[string]loopruntime.SessionReadbackFileRef) error {
	for name, ref := range refs {
		actual, err := artifactCompactionFile(ref.Path, nil)
		if err != nil {
			return err
		}
		if actual != ref {
			return fmt.Errorf("artifact continuation source changed: %s", name)
		}
	}
	return nil
}

func artifactCompactionInside(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	return err != nil || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// Validate missing-copy ancestors too. A missing leaf cannot hide a symlink.
func artifactCompactionCanonical(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("artifact continuation path is not absolute and clean")
	}
	ancestor := path
	for {
		_, err := os.Lstat(ancestor)
		if os.IsNotExist(err) {
			parent := filepath.Dir(ancestor)
			if parent == ancestor {
				return err
			}
			ancestor = parent
			continue
		}
		if err != nil {
			return err
		}
		resolved, err := filepath.EvalSymlinks(ancestor)
		if err != nil {
			return err
		}
		if resolved != ancestor {
			return fmt.Errorf("artifact continuation path contains a symlink")
		}
		return nil
	}
}

func artifactCompactionRoots(p artifactCompactionRequest) []string {
	delivery := filepath.Dir(p.Owner.Result.PatchPath)
	return []string{p.Owner.Baseline.Directory, p.Owner.Result.ReturnedDirectory, filepath.Join(delivery, "baseline"), filepath.Join(delivery, "returned"), filepath.Join(delivery, "patch-repository")}
}

func artifactCompactionEvidence(p artifactCompactionRequest, refs map[string]loopruntime.SessionReadbackFileRef) error {
	roots := append(artifactCompactionRoots(p), p.Owner.Binding.WorktreePath)
	for _, root := range roots {
		if err := artifactCompactionCanonical(root); err != nil {
			return err
		}
	}
	for name, ref := range refs {
		for _, root := range roots {
			if artifactCompactionInside(ref.Path, root) {
				return fmt.Errorf("artifact continuation evidence overlaps deletion data: %s", name)
			}
		}
	}
	return artifactCompactionCheckRefs(refs)
}

func artifactCompactionSync(refs map[string]loopruntime.SessionReadbackFileRef) error {
	if err := artifactCompactionCheckRefs(refs); err != nil {
		return err
	}
	dirs := map[string]bool{}
	for _, ref := range refs {
		before, err := os.Lstat(ref.Path)
		if err != nil {
			return err
		}
		f, err := os.Open(ref.Path)
		if err != nil {
			return err
		}
		opened, err := f.Stat()
		if err != nil || !os.SameFile(before, opened) {
			return errors.Join(err, f.Close(), fmt.Errorf("artifact continuation evidence replaced before sync"))
		}
		if err := errors.Join(f.Sync(), f.Close()); err != nil {
			return err
		}
		for dir := filepath.Dir(ref.Path); ; dir = filepath.Dir(dir) {
			dirs[dir] = true
			if filepath.Dir(dir) == dir {
				break
			}
		}
	}
	ordered := make([]string, 0, len(dirs))
	for dir := range dirs {
		ordered = append(ordered, dir)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ordered)))
	for _, dir := range ordered {
		if err := filex.SyncDir(dir); err != nil {
			return err
		}
	}
	return artifactCompactionCheckRefs(refs)
}

func artifactCompactionWrite(path string, value any) (loopruntime.SessionReadbackFileRef, error) {
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		return loopruntime.SessionReadbackFileRef{}, fmt.Errorf("artifact continuation evidence destination already exists or is unresolved")
	}
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return loopruntime.SessionReadbackFileRef{}, err
	}
	if err := filex.WriteFileAtomicDurable(path, append(raw, '\n')); err != nil {
		return loopruntime.SessionReadbackFileRef{}, err
	}
	return artifactCompactionFile(path, nil)
}

func artifactCompactionLock(project string) (*lockfile.Lock, error) {
	path := filepath.Join(project, ".noodle", "control.lock")
	if _, err := artifactCompactionFile(path, nil); err != nil {
		return nil, fmt.Errorf("original control lock is unavailable: %w", err)
	}
	return lockfile.TryLock(path)
}

func artifactCompactionExternal(dir string, host hostReceipt) error {
	if err := artifactCompactionCanonical(dir); err != nil {
		return err
	}
	for _, protected := range []string{host.ProjectDir, host.HostDir} {
		if artifactCompactionInside(dir, protected) || artifactCompactionInside(protected, dir) {
			return fmt.Errorf("artifact continuation evidence directory overlaps original owner storage")
		}
	}
	return nil
}
