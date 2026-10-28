//go:build !darwin && !linux

package lockfile

import "errors"

func TryLockShared(string) (*Lock, error) {
	return nil, errors.New("rolling Host shared fencing is unavailable on this platform")
}
