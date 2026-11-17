package main

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
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
	plan, err := planHostCheckoutArchive(ctx, e, base)
	if err != nil {
		return nil, err
	}
	if plan.Bound != "" {
		return boundCheckoutArchivePolicy(plan.Bound, base)
	}
	root := filepath.Dir(filepath.Dir(plan.Path))
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
	cfg := plan.Config
	policyDir := filepath.Dir(plan.Path)
	for _, dir := range []string{cfg.ArchiveRoot, cfg.BlobRoot, cfg.StateDir, policyDir} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
		if actual, err := filepath.EvalSymlinks(dir); err != nil || actual != dir {
			return nil, fmt.Errorf("original archive directory changed: %s (%v)", dir, err)
		}
	}
	keyPath := filepath.Join(cfg.StateDir, "owner.key")
	path := plan.Path
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
