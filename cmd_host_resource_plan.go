package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/poteto/noodle/internal/execution"
	"github.com/poteto/noodle/worktree/archive"
)

type hostStoragePlan struct {
	Source   hostStorageSource
	OwnerDir string
}

// Resolve the existing native ancestor without creating the planned leaf.
func hostPlannedPath(path string) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", errors.New("original planned storage path is not absolute and clean")
	}
	parent := path
	for {
		if _, err := os.Lstat(parent); err == nil {
			canonical, err := filepath.EvalSymlinks(parent)
			if err != nil {
				return "", err
			}
			relative, err := filepath.Rel(parent, path)
			if err != nil {
				return "", err
			}
			return filepath.Join(canonical, relative), nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
		next := filepath.Dir(parent)
		if next == parent {
			return "", errors.New("original planned storage ancestor unavailable")
		}
		parent = next
	}
}

// The repository override is only the original native control planner's Git
// owner before its checkout exists. Host attach reads its actual Git owner.
func planHostStorage(ctx context.Context, req hostRequest, selectedStorage, repository string) (hostStoragePlan, error) {
	var plan hostStoragePlan
	e, err := execution.New(req.ProjectDir, req.Environment)
	if err != nil {
		return plan, err
	}
	if err = validateHostEnvironment(e); err != nil {
		return plan, err
	}
	query := e.Root()
	if repository != "" {
		query = repository
	}
	common, err := e.Command(ctx, query, "git", "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		if repository == "" {
			return plan, nil
		}
		return plan, fmt.Errorf("original preparation Git owner unavailable: %w", err)
	}
	commonDir, err := hostPlannedPath(strings.TrimSpace(string(common)))
	if err != nil {
		return plan, err
	}
	plan.OwnerDir = filepath.Join(commonDir, "noodle-contexts", hostDigest([]byte(e.Root())))
	if raw, err := os.ReadFile(filepath.Join(plan.OwnerDir, "source.json")); err == nil {
		var original hostStorageSource
		if json.Unmarshal(raw, &original) != nil || original.Project != e.Root() || original.CommonDir != commonDir || !filepath.IsAbs(original.StorageRoot) || selectedStorage != "" && selectedStorage != original.StorageRoot {
			return plan, errors.New("original Host storage source differs")
		}
		selectedStorage = original.StorageRoot
	} else if !os.IsNotExist(err) {
		return plan, err
	}
	home, _ := e.Lookup("HOME")
	dataDir := filepath.Join(home, ".local", "share")
	if runtime.GOOS == "darwin" {
		dataDir = filepath.Join(home, "Library", "Application Support")
	} else if runtime.GOOS == "windows" {
		dataDir, _ = os.UserConfigDir()
	} else if xdg, ok := e.Lookup("XDG_DATA_HOME"); ok && xdg != "" {
		dataDir = xdg
	}
	if !filepath.IsAbs(dataDir) {
		return plan, errors.New("Host data directory source is unavailable")
	}
	storage := filepath.Join(dataDir, "noodle", "contexts", hostDigest([]byte(commonDir)), hostDigest([]byte(e.Root())))
	if selectedStorage != "" {
		storage = selectedStorage
	}
	storage, err = hostPlannedPath(storage)
	if err != nil {
		return plan, err
	}
	if strings.HasPrefix(storage, e.Root()+string(filepath.Separator)) || storage == e.Root() || strings.HasPrefix(storage, commonDir+string(filepath.Separator)) || storage == commonDir {
		return plan, errors.New("Host storage remains inside disposable or protected source")
	}
	plan.Source = hostStorageSource{Project: e.Root(), CommonDir: commonDir, StorageRoot: storage}
	return plan, nil
}

type hostArchivePlan struct {
	Config archive.Config
	Bound  string
	Path   string
}

func planHostCheckoutArchive(ctx context.Context, e *execution.Context, base string) (hostArchivePlan, error) {
	var plan hostArchivePlan
	selected, _ := e.Lookup("NOODLE_WORKTREE_ARCHIVE_CONFIG")
	home, _ := e.Lookup("HOME")
	if selected == "" && filepath.IsAbs(home) {
		original := filepath.Join(home, ".config", "noodle", "worktree-archive.json")
		if _, err := os.Lstat(original); err == nil {
			selected = original
		} else if !os.IsNotExist(err) {
			return plan, err
		}
	}
	if selected != "" {
		path, err := e.ResolvePath(selected)
		if err != nil {
			return plan, err
		}
		ref, err := boundCheckoutArchivePolicy(path, base)
		if err != nil {
			return plan, err
		}
		cfg, err := archive.LoadConfig(ref.Path)
		if err != nil {
			return plan, err
		}
		return hostArchivePlan{Config: cfg, Bound: ref.Path, Path: ref.Path}, nil
	}
	data := filepath.Join(home, ".local", "share")
	if runtime.GOOS == "darwin" {
		data = filepath.Join(home, "Library", "Application Support")
	} else if xdg, ok := e.Lookup("XDG_DATA_HOME"); ok && xdg != "" {
		data = xdg
	}
	if !filepath.IsAbs(home) || !filepath.IsAbs(data) {
		return plan, errors.New("original Host archive data source is unavailable")
	}
	storageBase, err := hostPlannedPath(filepath.Join(data, "noodle"))
	if err != nil {
		return plan, err
	}
	root := filepath.Join(storageBase, "checkout-archives")
	codec := e.Command(ctx, e.Root(), "zstd")
	if codec.Err != nil {
		return plan, fmt.Errorf("original archive codec is unavailable: %w", codec.Err)
	}
	key := hostDigest([]byte(base))
	plan.Config = archive.Config{Roots: []string{base}, ArchiveRoot: filepath.Join(root, "archive"), BlobRoot: filepath.Join(root, "blobs"), StateDir: filepath.Join(root, "owners", key), Codec: codec.Path, MaxItems: 1, ItemTimeoutSeconds: 120}
	plan.Path = filepath.Join(root, "policies", key+".json")
	return plan, nil
}

func (p hostArchivePlan) diskRoots() []string {
	return []string{p.Config.ArchiveRoot, p.Config.BlobRoot, p.Config.StateDir, filepath.Dir(p.Path)}
}
