package main

import (
	"errors"
	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/lockfile"
	"os"
	"path/filepath"
)

func newHostBudget() (*admission.Budget, error) {
	limit, err := hostSwapUsedLimit()
	if err != nil {
		return nil, err
	}
	return admission.NewBudgetWithSwapUsedLimit(0, limit), nil
}

func hostGenerationFence(dir string) (*lockfile.Lock, error) {
	root, generation := os.Getenv("NOODLE_HOST_POOL_ROOT"), os.Getenv("NOODLE_HOST_GENERATION")
	if root == "" && generation == "" {
		return lockfile.TryLock(filepath.Join(dir, "host.lock"))
	}
	if !filepath.IsAbs(root) || !validPoolID(generation) || dir != filepath.Join(root, "g", generation) {
		return nil, errors.New("generation scope is invalid")
	}
	return lockfile.TryLockShared(filepath.Join(root, "host.lock"))
}
