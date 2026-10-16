//go:build !windows

package dispatcher

import (
	"errors"
	"os/exec"
	"syscall"
)

func configureChildProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func terminateProcess(cmd *exec.Cmd) error {
	return signalProcessGroup(cmd, syscall.SIGTERM)
}

func forceKillProcess(cmd *exec.Cmd) error {
	return signalProcessGroup(cmd, syscall.SIGKILL)
}

func signalProcessGroup(cmd *exec.Cmd, signal syscall.Signal) error {
	if cmd.Process == nil {
		return nil
	}
	// StartProcess creates a group whose ID is the original child PID. Keep
	// that identity after the launcher exits; Getpgid would lose its children.
	err := syscall.Kill(-cmd.Process.Pid, signal)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	return err
}
