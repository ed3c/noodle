//go:build windows

package admission

import (
	"errors"
	"os"
	"os/exec"
)

func configureResident(cmd *exec.Cmd) {}

func stopResident(p *os.Process) error {
	return errors.New("graceful resident shutdown unsupported on Windows; Session readback of retained process records required")
}
