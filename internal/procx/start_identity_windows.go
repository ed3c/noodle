//go:build windows

package procx

import (
	"fmt"
	"time"
)

func processStartedAfter(pid int, recorded time.Time) (bool, error) {
	return false, fmt.Errorf("process start identity unavailable on Windows for PID %d", pid)
}
