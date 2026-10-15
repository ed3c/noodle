//go:build windows

package admission

import (
	"errors"
	"os"
)

func stopResident(p *os.Process) error {
	return errors.New("graceful resident shutdown unsupported on Windows; original owner readback required")
}
