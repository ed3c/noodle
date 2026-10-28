//go:build darwin

package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"

	"github.com/poteto/noodle/internal/lockfile"
)

// App deletion unlinked some original owner locks. Fence the current runtime
// path during capture without closing, adopting or replacing the old handle.
func acquireRestrictedRuntimeLocks(dir string, j *restrictedMaintenance) (held []*lockfile.Lock, err error) {
	defer func() {
		if err != nil {
			for _, lock := range held {
				_ = lock.Close()
			}
		}
	}()
	if j.RuntimeLocks == nil {
		j.RuntimeLocks = map[string]uint64{}
	}
	for _, task := range j.Tasks {
		if task.Plan.Files["noodle.lock"] != "absent" {
			continue
		}
		key := task.Plan.Original.RequestID
		path := filepath.Join(task.Plan.Original.ProjectDir, ".noodle", "noodle.lock")
		inode, recorded := j.RuntimeLocks[key]
		info, statErr := os.Lstat(path)
		if recorded {
			if inode == 0 || statErr != nil || !info.Mode().IsRegular() || uint64(info.Sys().(*syscall.Stat_t).Ino) != inode {
				return held, errors.New("runtime lock creation or identity is unresolved; readback only")
			}
		} else {
			if !os.IsNotExist(statErr) {
				return held, errors.New("runtime path has an unplanned lock; original owner must be read back")
			}
			j.RuntimeLocks[key] = 0
			if err = saveRestrictedMaintenance(dir, *j); err != nil {
				return held, err
			}
		}
		lock, lockErr := lockfile.TryLock(path)
		if lockErr != nil {
			return held, lockErr
		}
		held = append(held, lock)
		info, err = os.Lstat(path)
		if err != nil {
			return held, err
		}
		actual := uint64(info.Sys().(*syscall.Stat_t).Ino)
		if recorded && actual != inode {
			return held, errors.New("runtime lock changed during acquisition")
		}
		j.RuntimeLocks[key] = actual
		if err = saveRestrictedMaintenance(dir, *j); err != nil {
			return held, err
		}
	}
	return held, nil
}
