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
	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/worktree"
)

type hostStorageSource = worktree.StorageSource

// Runtime and worker bytes belong to the repository owner, outside disposable
// App checkouts. The original paths remain aliases, not new task identities.
func prepareHostStorage(ctx context.Context, req hostRequest, data []byte) (string, error) {
	return prepareHostStorageAt(ctx, req, data, "")
}

func prepareHostStorageAt(ctx context.Context, req hostRequest, data []byte, selectedStorage string) (string, error) {
	e, err := execution.New(req.ProjectDir, req.Environment)
	if err != nil {
		return "", err
	}
	if err = validateHostEnvironment(e); err != nil {
		return "", err
	}
	common, err := e.Command(ctx, req.ProjectDir, "git", "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		return "", nil
	} // Non-repository manual contexts have no Git custody owner.
	commonDir := strings.TrimSpace(string(common))
	if !filepath.IsAbs(commonDir) {
		return "", errors.New("Host storage Git owner is not absolute")
	}
	ownerDir := filepath.Join(commonDir, "noodle-contexts", hostDigest([]byte(e.Root())))
	if err = os.MkdirAll(ownerDir, 0700); err != nil {
		return "", err
	}
	lock, err := lockfile.Acquire(ctx, filepath.Join(ownerDir, "storage.lock"))
	if err != nil {
		return "", err
	}
	defer lock.Close()
	if selectedStorage == "" {
		if raw, readErr := os.ReadFile(filepath.Join(ownerDir, "source.json")); readErr == nil {
			var original hostStorageSource
			if json.Unmarshal(raw, &original) != nil || original.Project != e.Root() || original.CommonDir != commonDir || !filepath.IsAbs(original.StorageRoot) {
				return "", errors.New("original Host storage owner is invalid")
			}
			selectedStorage = original.StorageRoot
		} else if !os.IsNotExist(readErr) {
			return "", readErr
		}
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
		return "", errors.New("Host data directory source is unavailable")
	}
	storage := filepath.Join(dataDir, "noodle", "contexts", hostDigest([]byte(commonDir)), hostDigest([]byte(e.Root())))
	if selectedStorage != "" {
		storage = selectedStorage
	}
	if !filepath.IsAbs(storage) || strings.HasPrefix(storage, e.Root()+string(filepath.Separator)) || storage == e.Root() || strings.HasPrefix(storage, commonDir+string(filepath.Separator)) || storage == commonDir {
		return "", errors.New("Host storage remains inside disposable or protected source")
	}
	if err = os.MkdirAll(storage, 0700); err != nil {
		return "", err
	}
	storage, err = filepath.EvalSymlinks(storage)
	if err != nil {
		return "", err
	}
	source := hostStorageSource{Project: e.Root(), CommonDir: commonDir, StorageRoot: storage}
	raw, err := json.Marshal(source)
	if err != nil {
		return "", err
	}
	path := filepath.Join(ownerDir, "source.json")
	if before, readErr := os.ReadFile(path); readErr == nil {
		var original hostStorageSource
		if json.Unmarshal(before, &original) != nil || original != source {
			return "", errors.New("Host storage source binding changed")
		}
	} else if !os.IsNotExist(readErr) {
		return "", readErr
	} else if err = filex.WriteFileAtomicDurable(path, raw); err != nil {
		return "", err
	}
	if err = saveHostStorageBytes(filepath.Join(storage, "source.json"), raw); err != nil {
		return "", err
	}
	if hostDigest(data) != req.ConfigSHA256 {
		return "", errors.New("Host storage config source changed")
	}
	if err = os.MkdirAll(filepath.Join(storage, "configs"), 0700); err != nil {
		return "", err
	}
	if err = saveHostStorageBytes(filepath.Join(storage, "configs", req.ConfigSHA256+".toml"), data); err != nil {
		return "", err
	}
	// The original writer lock must be absent before a directory can move.
	runtimeDir := filepath.Join(req.ProjectDir, ".noodle")
	lockDir := runtimeDir
	if info, statErr := os.Lstat(runtimeDir); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
		lockDir, err = filepath.EvalSymlinks(runtimeDir)
		if err != nil || lockDir != filepath.Join(storage, ".noodle") {
			return "", errors.New("Host storage alias belongs to another owner")
		}
	} else if statErr != nil && !os.IsNotExist(statErr) {
		return "", statErr
	} else if os.IsNotExist(statErr) {
		if _, targetErr := os.Stat(filepath.Join(storage, ".noodle")); targetErr == nil {
			lockDir = filepath.Join(storage, ".noodle")
		} else if !os.IsNotExist(targetErr) {
			return "", targetErr
		}
	}
	if err = os.MkdirAll(lockDir, 0700); err != nil {
		return "", err
	}
	writer, lockErr := lockfile.TryLock(filepath.Join(lockDir, "noodle.lock"))
	if lockErr != nil {
		return "", fmt.Errorf("original runtime still owns storage: %w", lockErr)
	}
	defer writer.Close()
	for _, name := range []string{".noodle", ".worktrees"} {
		if err = moveHostStorageDirectory(filepath.Join(req.ProjectDir, name), filepath.Join(storage, name)); err != nil {
			return "", err
		}
	}
	if err = repairHostStorageWorktrees(ctx, e, storage, commonDir); err != nil {
		return "", err
	}
	return storage, nil
}

func repairHostStorageWorktrees(ctx context.Context, e *execution.Context, storage, common string) error {
	raw, err := e.Command(ctx, e.Root(), "git", "worktree", "list", "--porcelain", "-z").Output()
	if err != nil {
		return err
	}
	for _, field := range strings.Split(string(raw), "\x00") {
		path, ok := strings.CutPrefix(field, "worktree ")
		if !ok {
			continue
		}
		originalRoot, physicalRoot := filepath.Join(e.Root(), ".worktrees"), filepath.Join(storage, ".worktrees")
		var relative string
		if strings.HasPrefix(path, originalRoot+string(filepath.Separator)) {
			relative = strings.TrimPrefix(path, originalRoot+string(filepath.Separator))
		} else if strings.HasPrefix(path, physicalRoot+string(filepath.Separator)) {
			relative = strings.TrimPrefix(path, physicalRoot+string(filepath.Separator))
		} else {
			continue
		}
		original, physical := filepath.Join(originalRoot, relative), filepath.Join(physicalRoot, relative)
		admin, readErr := e.Command(ctx, physical, "git", "rev-parse", "--absolute-git-dir").Output()
		if readErr != nil {
			return readErr
		}
		adminDir := strings.TrimSpace(string(admin))
		if !strings.HasPrefix(adminDir, filepath.Join(common, "worktrees")+string(filepath.Separator)) {
			return errors.New("worker Git admin owner differs from repository custody")
		}
		binding, _ := json.Marshal(map[string]string{"original": original, "physical": physical, "git_admin": adminDir})
		bindings := filepath.Join(storage, "worktree-bindings")
		if err = os.MkdirAll(bindings, 0700); err != nil {
			return err
		}
		if err = saveHostStorageBytes(filepath.Join(bindings, hostDigest([]byte(original))+".json"), binding); err != nil {
			return err
		}
		backpointer, readErr := os.ReadFile(filepath.Join(adminDir, "gitdir"))
		if readErr != nil {
			return readErr
		}
		current := strings.TrimSpace(string(backpointer))
		if current == filepath.Join(physical, ".git") {
			continue
		}
		if current != filepath.Join(original, ".git") {
			return errors.New("original worker Git backpointer changed")
		}
		if output, repairErr := e.Command(ctx, e.Root(), "git", "worktree", "repair", physical).CombinedOutput(); repairErr != nil {
			return fmt.Errorf("worker Git custody repair: %s: %w", output, repairErr)
		}
		after, readErr := os.ReadFile(filepath.Join(adminDir, "gitdir"))
		if readErr != nil || strings.TrimSpace(string(after)) != filepath.Join(physical, ".git") {
			return errors.New("worker Git physical custody is not confirmed")
		}
		if err = syncHostStorageTree(adminDir); err != nil {
			return err
		}
	}
	return nil
}

func saveHostStorageBytes(path string, data []byte) error {
	before, err := os.ReadFile(path)
	if err == nil {
		if string(before) != string(data) {
			return errors.New("original Host storage bytes changed")
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	return filex.WriteFileAtomicDurable(path, data)
}

func moveHostStorageDirectory(source, target string) error {
	info, err := os.Lstat(source)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		resolved, readErr := filepath.EvalSymlinks(source)
		if readErr != nil || resolved != target {
			return errors.New("Host storage alias belongs to another owner")
		}
		return nil
	}
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil {
		if !info.IsDir() {
			return errors.New("Host storage input is not a directory")
		}
		if _, targetErr := os.Stat(target); targetErr == nil {
			return errors.New("both original and durable Host storage exist; readback required")
		} else if !os.IsNotExist(targetErr) {
			return targetErr
		}
		if err = os.Rename(source, target); err != nil {
			return err
		}
		if err = filex.SyncDir(filepath.Dir(target)); err != nil {
			return err
		}
		if err = filex.SyncDir(filepath.Dir(source)); err != nil {
			return err
		}
	} else if err = os.MkdirAll(target, 0700); err != nil {
		return err
	}
	if err = syncHostStorageTree(target); err != nil {
		return err
	}
	if err = os.Symlink(target, source); err != nil {
		return err
	}
	return filex.SyncDir(filepath.Dir(source))
}

func syncHostStorageTree(root string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		} // Preserve links; never follow their targets.
		if entry.IsDir() {
			return filex.SyncDir(path)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported runtime storage entry: %s", path)
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		err = file.Sync()
		return errors.Join(err, file.Close())
	})
}
