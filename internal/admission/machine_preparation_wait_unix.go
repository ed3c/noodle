//go:build darwin || linux

package admission

import (
	"bytes"
	"io"
	"os"
	"syscall"
)

// Observe only an existing file. A stale/unlocked receipt must not create or
// mutate a lock during resource readback.
func preparationWaitLockHeld(path string, pid int, proof []byte) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer file.Close()
	before, err := file.Stat()
	if err != nil {
		return false
	}
	err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		return false
	}
	if err != syscall.EWOULDBLOCK {
		return false
	}
	data, err := io.ReadAll(io.LimitReader(file, 256))
	if err != nil {
		return false
	}
	after, statErr := os.Lstat(path)
	return bytes.Equal(data, preparationWaitWitness(pid, proof)) && statErr == nil && os.SameFile(before, after) && capacityFileIdentity(path, false) == nil
}
