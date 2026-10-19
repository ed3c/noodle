//go:build !windows

package admission

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// Reaping and group signals share the child lock. Until reap, the original
// child (including a zombie) reserves its PID, so the group cannot be reused.
func waitResident(child *residentChild) error {
	for {
		child.mu.Lock()
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(child.process.Pid, &status, syscall.WNOHANG, nil)
		if errors.Is(err, syscall.EINTR) {
			child.mu.Unlock()
			continue
		}
		if pid != 0 || err != nil {
			child.reaped = true
			if err != nil {
				err = errors.Join(err, child.process.Release())
				child.mu.Unlock()
				return err
			}
			// All streams are files. Never wait a second time on a reusable PID.
			finishErr := child.process.Release()
			child.mu.Unlock()
			if finishErr != nil {
				return finishErr
			}
			if status.Signaled() {
				return fmt.Errorf("resident terminated by %s", status.Signal())
			}
			if status.ExitStatus() != 0 {
				return fmt.Errorf("resident exited with status %d", status.ExitStatus())
			}
			return nil
		}
		child.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
}

func forceResident(p *os.Process) error {
	if err := p.Signal(syscall.Signal(0)); err != nil {
		return err
	}
	group, err := syscall.Getpgid(p.Pid)
	if err != nil {
		return err
	}
	if group != p.Pid {
		return fmt.Errorf("original child group changed")
	}
	return syscall.Kill(-group, syscall.SIGKILL)
}

func residentProcessAttributes() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }

func stopResident(p *os.Process) error {
	// Only a live original child handle may authorize signalling its owned group.
	if err := p.Signal(syscall.Signal(0)); err != nil {
		return err
	}
	group, err := syscall.Getpgid(p.Pid)
	if err != nil {
		return err
	}
	if group != p.Pid {
		return fmt.Errorf("original child group changed")
	}
	return syscall.Kill(-p.Pid, syscall.SIGTERM)
}
