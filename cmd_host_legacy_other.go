//go:build !darwin

package main

import (
	"context"
	"errors"
)

func legacyProcessIdentity(context.Context, int, string) (string, error) {
	return "", errors.New("legacy first migration has no verified process observation on this platform")
}

func verifyEmptyLegacyHost(context.Context, hostBootstrap) error {
	return errors.New("legacy first migration has no verified empty-Host observation on this platform")
}
