//go:build !windows

package admission

import (
	"os"
	"syscall"
)

func stopResident(p *os.Process) error { return p.Signal(syscall.SIGTERM) }
