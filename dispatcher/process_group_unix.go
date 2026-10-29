//go:build !windows

package dispatcher

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
)

func configureChildProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateProcess(cmd *exec.Cmd, retained bool) error {
	return signalProcessGroup(cmd, syscall.SIGTERM, retained)
}

func forceKillProcess(cmd *exec.Cmd, retained bool) error {
	return signalProcessGroup(cmd, syscall.SIGKILL, retained)
}

func signalProcessGroup(cmd *exec.Cmd, signal syscall.Signal, retained bool) error {
	if cmd.Process == nil {
		return nil
	}
	if !retained {
		group, err := syscall.Getpgid(cmd.Process.Pid)
		if err != nil {
			return err
		}
		if group != cmd.Process.Pid {
			return fmt.Errorf("original child process group changed")
		}
	}
	// The admitted ProcessHandle keeps the leader unreaped until its group
	// has drained. Its lock serializes this signal with the final Wait.
	// Use the group created at spawn, never a newly queried replacement group.
	err := syscall.Kill(-cmd.Process.Pid, signal)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
