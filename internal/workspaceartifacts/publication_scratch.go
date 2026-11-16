package workspaceartifacts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
)

// This capability stays with the original Apply caller. Reloading a result does
// not grant ownership of a directory that may have acquired another consumer.
type publicationScratch struct {
	path         string
	root, parent os.FileInfo
	entries      map[string]scratchEntry
}

type scratchEntry struct {
	Mode os.FileMode
	Size int64
	SHA  string
}

func newPublicationScratch(path string) (*publicationScratch, error) {
	root, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	parent, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	s := &publicationScratch{path: path, root: root, parent: parent}
	return s, s.verifyLocation()
}

func (s *publicationScratch) verifyLocation() error {
	for path, original := range map[string]os.FileInfo{s.path: s.root, filepath.Dir(s.path): s.parent} {
		current, err := os.Lstat(path)
		if err != nil || !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(original, current) {
			return fmt.Errorf("publication scratch ownership changed: %s", path)
		}
		canonical, err := filepath.EvalSymlinks(path)
		if err != nil || canonical != path {
			return fmt.Errorf("publication scratch has a path alias: %s", path)
		}
	}
	return nil
}

func (s *publicationScratch) snapshot(ctx context.Context) (map[string]scratchEntry, error) {
	if err := s.verifyLocation(); err != nil {
		return nil, err
	}
	entries := map[string]scratchEntry{}
	err := filepath.WalkDir(s.path, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		entry := scratchEntry{Mode: info.Mode()}
		if !info.IsDir() {
			if !info.Mode().IsRegular() {
				return fmt.Errorf("publication scratch contains a foreign entry: %s", path)
			}
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			hash := sha256.New()
			entry.Size, err = io.Copy(hash, file)
			after, statErr := file.Stat()
			closeErr := file.Close()
			if err != nil || statErr != nil || closeErr != nil || !os.SameFile(info, after) || info.Size() != after.Size() || !info.ModTime().Equal(after.ModTime()) {
				return fmt.Errorf("publication scratch changed while reading: %s", path)
			}
			entry.SHA = hex.EncodeToString(hash.Sum(nil))
		}
		rel, err := filepath.Rel(s.path, path)
		if err == nil {
			entries[rel] = entry
		}
		return err
	})
	if err == nil {
		err = s.verifyLocation()
	}
	return entries, err
}

func (r *Result) sealScratch() error {
	if r.scratch == nil {
		return nil
	}
	var err error
	r.scratch.entries, err = r.scratch.snapshot(context.Background())
	return err
}

// ReleasePublicationScratch removes only this caller's completed diff workspace.
// Baselines, returned bytes and all publication/failed evidence stay in place.
func ReleasePublicationScratch(ctx context.Context, repository string, baseline *Baseline, result *Result, head string) error {
	if result == nil || result.Error != "" || result.scratch == nil || result.scratch.entries == nil {
		return fmt.Errorf("publication scratch has no original producer ownership")
	}
	if err := VerifyRetainedPublication(ctx, repository, baseline, result, head); err != nil {
		return err
	}
	evidence := filepath.Dir(result.PatchPath)
	if result.scratch.path != filepath.Join(evidence, "patch-repository") {
		return fmt.Errorf("publication scratch is outside original delivery")
	}
	var saved Result
	raw, err := os.ReadFile(filepath.Join(evidence, "result.json"))
	if err != nil || json.Unmarshal(raw, &saved) != nil || saved.Applied != result.Applied || saved.State != result.State || saved.Error != result.Error || saved.PatchPath != result.PatchPath || saved.ReturnedDirectory != result.ReturnedDirectory {
		return fmt.Errorf("publication scratch outcome is not durably retained")
	}
	var receipt publicationReceipt
	raw, err = os.ReadFile(filepath.Join(evidence, "publish.json"))
	if err != nil || json.Unmarshal(raw, &receipt) != nil || receipt.Phase != result.State || receipt.Parent != baseline.Head || receipt.ObservedHead != head || receipt.Error != "" {
		return fmt.Errorf("publication scratch has no original successful publication")
	}
	current, err := result.scratch.snapshot(ctx)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(current, result.scratch.entries) {
		return fmt.Errorf("publication scratch changed after original apply")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return os.RemoveAll(result.scratch.path)
}
