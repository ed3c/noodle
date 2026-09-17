//go:build windows

package procx

import "fmt"

// ProcessGroupAbsent refuses recovery where the persisted PID alone cannot
// establish that the dispatched process tree has exited.
func ProcessGroupAbsent(pid int) (bool, error) {
	return false, fmt.Errorf("process tree absence unavailable on Windows for PID %d", pid)
}
