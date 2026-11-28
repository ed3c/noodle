package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/poteto/noodle/internal/lockfile"
	loopruntime "github.com/poteto/noodle/runtime"
)

var failedReviewBootstrapImage = regexp.MustCompile(`^sandbox-bootstrap-rootfs-ext4-v[0-9]+-openshell-[a-z0-9.-]+-guest-[a-f0-9]{64}-[a-f0-9]{64}$`)
var failedReviewOverlayVersion = regexp.MustCompile(`^sandbox-overlay-ext4-v[0-9]+$`)
var failedReviewOverlayImage = regexp.MustCompile(`^[1-9][0-9]*\.ext4$`)

// The caller holds the retired Goal's admission fence and rechecks both
// Gateway and Host exit witnesses; the original custody lock fences reopening.
func reclaimFailedReviewRuntimeStorage(hostRoot, instance string, profile, template, retiredCustody loopruntime.SessionReadbackFileRef, apply bool, verifyExit func() error) (remaining bool, err error) {
	dir := filepath.Join(hostRoot, "openshell")
	lockPath := filepath.Join(dir, "custody", "owner.lock")
	if verifyExit == nil || !validPoolID(instance) || retiredCustody.Path != filepath.Join(dir, "custody", "owner.json") || retiredCustody.SHA256 == "" {
		return true, errors.New("original retired Goal storage scope is incomplete")
	}
	if err = verifyExit(); err != nil {
		return true, err
	}
	if canonical, err := filepath.EvalSymlinks(lockPath); err != nil || canonical != lockPath {
		return true, errors.Join(errors.New("original retired Goal custody lock changed"), err)
	}
	ownerLock, err := lockfile.TryLockExisting(lockPath)
	if err != nil {
		return true, err
	}
	defer func() { err = errors.Join(err, ownerLock.Close()) }()
	return reclaimFailedReviewStorageTree(filepath.Join(hostRoot, "openshell"), instance, apply, func() error {
		if !ownerLock.HoldsExisting(lockPath) {
			return errors.New("original retired Goal custody fence changed")
		}
		if err := verifyExit(); err != nil {
			return err
		}
		current, err := loopruntime.ObserveRetiredGoalVMCustody(hostRoot, profile, template)
		if err != nil || current != retiredCustody {
			return errors.Join(errors.New("original retired Goal custody changed during storage reclamation"), err)
		}
		return nil
	})
}

// Only reconstructible generated images, the bundled supervisor and empty
// driver directories belong to the retirement scope. A nonempty sandbox,
// run or preparation directory is original data until its owner confirms it.
func failedReviewVMEntry(path string, directory bool) bool {
	parts := strings.Split(filepath.ToSlash(path), "/")
	switch len(parts) {
	case 1:
		switch parts[0] {
		case "images", "host-runtime", "rootfs-tar-staging", "run", "sandboxes":
			return directory
		}
	case 2:
		switch parts[0] {
		case "host-runtime":
			return !directory && parts[1] == "openshell-supervisor"
		case "images":
			if directory {
				return parts[1] == "overlay-templates" || parts[1] == "preparations" || failedReviewBootstrapImage.MatchString(parts[1])
			}
			return parts[1] == "preparation.lock"
		}
	case 3:
		return parts[0] == "images" && (parts[1] == "overlay-templates" && directory && failedReviewOverlayVersion.MatchString(parts[2]) ||
			failedReviewBootstrapImage.MatchString(parts[1]) && !directory && parts[2] == "rootfs.ext4")
	case 4:
		return parts[0] == "images" && parts[1] == "overlay-templates" && failedReviewOverlayVersion.MatchString(parts[2]) &&
			!directory && failedReviewOverlayImage.MatchString(parts[3])
	}
	return false
}

func checkFailedReviewVMTree(root *os.Root, name string) (bool, error) {
	info, err := root.Lstat(name)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil || !info.IsDir() {
		return false, errors.Join(fmt.Errorf("original VM storage is not a directory: %s", name), err)
	}
	tree, err := root.OpenRoot(name)
	if err != nil {
		return false, err
	}
	defer tree.Close()
	return true, fs.WalkDir(tree.FS(), ".", func(path string, item fs.DirEntry, err error) error {
		if err != nil || path == "." {
			return err
		}
		if (!item.IsDir() && !item.Type().IsRegular()) || !failedReviewVMEntry(path, item.IsDir()) {
			return fmt.Errorf("original VM storage has unknown or retained content: %s", path)
		}
		return nil
	})
}

func reclaimFailedReviewStorageTree(dir, instance string, apply bool, verify func() error) (bool, error) {
	if !validPoolID(instance) {
		return false, errors.New("original VM retirement generation is invalid")
	}
	if canonical, err := filepath.EvalSymlinks(dir); err != nil || canonical != dir || !filepath.IsAbs(dir) {
		return false, errors.Join(errors.New("original OpenShell runtime root changed"), err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return false, err
	}
	defer root.Close()
	directory, err := root.Open(".")
	if err != nil {
		return false, err
	}
	defer directory.Close()
	originalDir, err := directory.Stat()
	if err != nil {
		return false, err
	}
	verifyRoot := func() error {
		currentDir, err := os.Lstat(dir)
		if err != nil || !os.SameFile(currentDir, originalDir) {
			return errors.Join(errors.New("original OpenShell runtime root changed"), err)
		}
		return nil
	}
	staged := ".vm-retiring-" + instance
	inventory := func() (bool, bool, []string, error) {
		vm, err := checkFailedReviewVMTree(root, "vm")
		if err != nil {
			return false, false, nil, err
		}
		quarantine, err := checkFailedReviewVMTree(root, staged)
		if err != nil {
			return false, false, nil, err
		}
		if vm && quarantine {
			return false, false, nil, errors.New("original VM runtime and its retirement staging both exist")
		}
		entries, err := fs.ReadDir(root.FS(), ".")
		if err != nil {
			return false, false, nil, err
		}
		for _, entry := range entries {
			name := entry.Name()
			if strings.HasPrefix(name, ".vm-retiring-") && name != staged ||
				strings.HasPrefix(name, "gateway.db") && name != "gateway.db" && name != "gateway.db-wal" && name != "gateway.db-shm" {
				return false, false, nil, fmt.Errorf("unknown original runtime residue: %s", name)
			}
		}
		var databases []string
		for _, name := range []string{"gateway.db-shm", "gateway.db-wal", "gateway.db"} {
			info, err := root.Lstat(name)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil || !info.Mode().IsRegular() {
				return false, false, nil, errors.Join(fmt.Errorf("original Gateway database differs: %s", name), err)
			}
			databases = append(databases, name)
		}
		return vm, quarantine, databases, nil
	}
	vm, quarantine, databases, err := inventory()
	if err != nil {
		return false, err
	}
	present := vm || quarantine || len(databases) != 0
	if err := errors.Join(verifyRoot(), verify()); err != nil || !apply || !present {
		return present, err
	}
	if vm {
		if err := root.Rename("vm", staged); err != nil {
			return true, err
		}
		if err := directory.Sync(); err != nil {
			return true, err
		}
		quarantine = true
	}
	if quarantine {
		if present, err := checkFailedReviewVMTree(root, staged); err != nil || !present {
			return true, errors.Join(errors.New("original VM retirement staging changed"), err)
		}
		if err := errors.Join(verifyRoot(), verify()); err != nil {
			return true, err
		}
		if err := root.RemoveAll(staged); err != nil {
			return true, err
		}
	}
	for _, name := range databases {
		info, err := root.Lstat(name)
		if err != nil || !info.Mode().IsRegular() {
			return true, errors.Join(fmt.Errorf("original Gateway database changed: %s", name), err)
		}
		if err := root.Remove(name); err != nil {
			return true, err
		}
	}
	if err := directory.Sync(); err != nil {
		return true, err
	}
	vm, quarantine, databases, err = inventory()
	return vm || quarantine || len(databases) != 0, errors.Join(err, verifyRoot(), verify())
}
