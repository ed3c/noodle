//go:build !darwin || !cgo

package archive

import (
	"errors"
	"os"
)

func FileIdentity(string) (string, error) {
	return "", errors.New("archive file identity requires a supported macOS build")
}

func AvailableBytes(string) (uint64, error) {
	return 0, errors.New("archive capacity readback is unsupported on this platform")
}

func CheckIndependentFile(string) error {
	return errors.New("source alias readback is unsupported on this platform")
}

func ReadMetadata(string) (Metadata, error) {
	return Metadata{}, errors.New("archive metadata requires a macOS cgo build; source retained")
}
func ApplyMetadata(string, Metadata, *os.FileMode) error {
	return errors.New("archive metadata requires a macOS cgo build; destination unverified")
}
