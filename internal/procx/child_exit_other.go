//go:build !darwin && !linux

package procx

import "fmt"

func WaitChildExit(pid int) error {
	return fmt.Errorf("non-reaping child exit observation unavailable for PID %d", pid)
}
