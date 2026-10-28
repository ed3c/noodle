//go:build darwin || linux

package lockfile

import (
	"fmt"
	"os"
	"syscall"
)

// TryLockShared fences legacy exclusive Host launchers while supported Host
// generations share their original durable quota. It does not elect an owner.
func TryLockShared(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("Host shared fence: %w", err)
	}
	return &Lock{file: f}, nil
}
