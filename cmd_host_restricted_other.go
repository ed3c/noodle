//go:build !darwin

package main

import (
	"errors"

	"github.com/poteto/noodle/internal/lockfile"
)

func acquireRestrictedRuntimeLocks(string, *restrictedMaintenance) ([]*lockfile.Lock, error) {
	return nil, errors.New("restricted native owner migration is unavailable on this platform")
}
