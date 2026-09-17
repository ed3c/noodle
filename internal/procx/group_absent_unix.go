//go:build !windows

package procx

import (
	"errors"
	"fmt"
	"syscall"
)

// ProcessGroupAbsent observes both the root PID and its original process group.
// Dispatcher children use Setpgid, so the group ID remains the root PID even
// after the root exits. Only ESRCH proves absence; other errors fail closed.
func ProcessGroupAbsent(pid int) (bool, error) {
	if pid <= 0 {
		return false, fmt.Errorf("invalid PID %d", pid)
	}
	for _, id := range []int{pid, -pid} {
		err := syscall.Kill(id, 0)
		if err == nil || errors.Is(err, syscall.EPERM) {
			return false, nil
		}
		if !errors.Is(err, syscall.ESRCH) {
			return false, fmt.Errorf("observe process %d: %w", id, err)
		}
	}
	return true, nil
}
