//go:build !windows

package admission

import (
	"os"
	"os/exec"
	"syscall"
)

func configureResident(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func stopResident(p *os.Process) error {
	// Only a live original child handle may authorize signalling its owned group.
	if err := p.Signal(syscall.Signal(0)); err != nil {
		return err
	}
	return syscall.Kill(-p.Pid, syscall.SIGTERM)
}
