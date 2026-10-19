//go:build windows

package admission

import (
	"errors"
	"os"
	"syscall"
)

func residentProcessAttributes() *syscall.SysProcAttr { return nil }

func waitResident(child *residentChild) error {
	state, err := child.process.Wait()
	child.mu.Lock()
	child.reaped = true
	child.mu.Unlock()
	if err == nil && !state.Success() {
		return errors.New(state.String())
	}
	return err
}

func forceResident(p *os.Process) error {
	return errors.New("owned resident group force stop unsupported on Windows; original owner readback required")
}

func stopResident(p *os.Process) error {
	return errors.New("graceful resident shutdown unsupported on Windows; original owner readback required")
}
