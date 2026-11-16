package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"

	"github.com/poteto/noodle/internal/execution"
	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/worktree"
	"github.com/poteto/noodle/worktree/archive"
)

// The original Host binds policy without changing its accepted environment.
// Main supplies semantic work, never per-task archive paths or signing bytes.
func prepareHostCheckoutArchive(ctx context.Context, e *execution.Context) (*archive.FileRef, error) {
	if _, err := archive.FileIdentity(e.Root()); err != nil {
		return nil, fmt.Errorf("original archive support is unavailable: %w", err)
	}
	if _, err := worktree.PrepareStorageExcludes(ctx, e); err != nil {
		return nil, err
	}
	base, err := filepath.EvalSymlinks(filepath.Join(e.Root(), ".worktrees"))
	if err != nil {
		return nil, fmt.Errorf("original Creator checkout base unavailable: %w", err)
	}
	if selected, ok := e.Lookup("NOODLE_WORKTREE_ARCHIVE_CONFIG"); ok && selected != "" {
		path, err := e.ResolvePath(selected)
		if err != nil {
			return nil, err
		}
		return boundCheckoutArchivePolicy(path, base)
	}
	home, _ := e.Lookup("HOME")
	if filepath.IsAbs(home) {
		original := filepath.Join(home, ".config", "noodle", "worktree-archive.json")
		if _, err := os.Lstat(original); err == nil {
			return boundCheckoutArchivePolicy(original, base)
		} else if !os.IsNotExist(err) {
			return nil, err
		}
	}
	data := filepath.Join(home, ".local", "share")
	if runtime.GOOS == "darwin" {
		data = filepath.Join(home, "Library", "Application Support")
	} else if xdg, ok := e.Lookup("XDG_DATA_HOME"); ok && xdg != "" {
		data = xdg
	}
	if !filepath.IsAbs(home) || !filepath.IsAbs(data) {
		return nil, fmt.Errorf("original Host archive data source is unavailable")
	}
	storageBase := filepath.Join(data, "noodle")
	if err = os.MkdirAll(storageBase, 0700); err != nil {
		return nil, err
	}
	storageBase, err = filepath.EvalSymlinks(storageBase)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(storageBase, "checkout-archives")
	if err = os.MkdirAll(root, 0700); err != nil {
		return nil, err
	}
	actual, err := filepath.EvalSymlinks(root)
	if err != nil || actual != root {
		return nil, fmt.Errorf("Host archive storage is aliased or unavailable: %v", err)
	}
	lock, err := lockfile.Acquire(ctx, filepath.Join(root, "setup.lock"))
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	codec := e.Command(ctx, e.Root(), "zstd")
	if codec.Err != nil {
		return nil, fmt.Errorf("original archive codec is unavailable: %w", codec.Err)
	}
	// Policies are specific to the Creator base; immutable blobs and Git pools
	// remain shared across all policies in this original user storage.
	key := hostDigest([]byte(base))
	cfg := archive.Config{Roots: []string{base}, ArchiveRoot: filepath.Join(root, "archive"), BlobRoot: filepath.Join(root, "blobs"), StateDir: filepath.Join(root, "owners", key), Codec: codec.Path, MaxItems: 1, ItemTimeoutSeconds: 120}
	policyDir := filepath.Join(root, "policies")
	for _, dir := range []string{cfg.ArchiveRoot, cfg.BlobRoot, cfg.StateDir, policyDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
		if actual, err := filepath.EvalSymlinks(dir); err != nil || actual != dir {
			return nil, fmt.Errorf("original archive directory changed: %s (%v)", dir, err)
		}
	}
	keyPath := filepath.Join(cfg.StateDir, "owner.key")
	path := filepath.Join(policyDir, key+".json")
	if info, err := os.Lstat(keyPath); err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() != 32 {
			return nil, fmt.Errorf("original archive owner key differs; preserve and reconcile")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	} else {
		if _, policyErr := os.Lstat(path); !os.IsNotExist(policyErr) {
			return nil, fmt.Errorf("existing archive policy lost its original key; preserve and reconcile: %v", policyErr)
		}
		var signing [32]byte
		if _, err := rand.Read(signing[:]); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return nil, err
		}
		if _, err = f.Write(signing[:]); err != nil {
			f.Close()
			return nil, err
		}
		if err = f.Sync(); err != nil {
			f.Close()
			return nil, err
		}
		if err = f.Close(); err != nil {
			return nil, err
		}
		if err = filex.SyncDir(cfg.StateDir); err != nil {
			return nil, err
		}
	}
	if original, err := archive.LoadConfig(path); err == nil {
		if !reflect.DeepEqual(original, cfg) {
			return nil, fmt.Errorf("original archive policy drift; preserve and reconcile")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	} else {
		raw, err := json.Marshal(cfg)
		if err != nil {
			return nil, err
		}
		if err := saveHostStorageBytes(path, raw); err != nil {
			return nil, err
		}
	}
	if err := filex.SyncDir(root); err != nil {
		return nil, err
	}
	return boundCheckoutArchivePolicy(path, base)
}

func boundCheckoutArchivePolicy(path, base string) (*archive.FileRef, error) {
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	cfg, err := archive.LoadConfig(path)
	if err != nil {
		return nil, err
	}
	covered := false
	for _, root := range cfg.Roots {
		rel, relErr := filepath.Rel(root, base)
		if relErr == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			covered = true
			break
		}
	}
	if !covered {
		return nil, fmt.Errorf("archive policy does not cover original Creator checkout base")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("original archive policy source is not regular: %v", err)
	}
	hash, err := archive.FileHash(path)
	if err != nil {
		return nil, err
	}
	return &archive.FileRef{Path: path, SHA256: hash}, nil
}
