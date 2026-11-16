//go:build !linux && (!darwin || !cgo)

package admission

import "errors"

func nativeMachineIdentity() (string, error) { return "", errors.New("machine capacity identity unsupported") }
func capacityFileIdentity(string, bool) error { return errors.New("machine capacity file identity unsupported") }
func nativeDiskCapacity(string) (diskCapacity, error) { return diskCapacity{}, errors.New("disk capacity observation unsupported") }
