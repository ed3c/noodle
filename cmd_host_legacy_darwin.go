//go:build darwin

package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

func legacyProcessIdentity(ctx context.Context, pid int, binary string) (string, error) {
	cmd := exec.CommandContext(ctx, "/bin/ps", "-p", strconv.Itoa(pid), "-o", "lstart=", "-o", "comm=")
	cmd.Env = append(os.Environ(), "LC_ALL=C", "TZ=UTC")
	raw, err := cmd.Output()
	if err != nil {
		return "", err
	}
	value := strings.TrimSpace(string(raw))
	if !strings.HasSuffix(value, " "+binary) {
		return "", errors.New("legacy process executable identity changed")
	}
	return value, nil
}

func verifyEmptyLegacyHost(ctx context.Context, b hostBootstrap) error {
	r, err := readLegacyResources(ctx, b.Source.Directory)
	if err != nil {
		return err
	}
	if r.Instance != b.Source.ID || r.PID != b.Source.Process.PID || r.Contexts != 0 || r.Resources.Workers != 0 || r.Resources.Residents != 0 || r.Resources.Allocations != 0 || r.Resources.Unresolved != 0 || r.Residents.Error != nil {
		return errors.New("original legacy Host has live or unresolved resources")
	}
	identity, err := legacyProcessIdentity(ctx, r.PID, b.Source.Binary)
	if err != nil || identity != b.ProcessIdentity {
		return errors.New("original empty Host process identity changed")
	}
	if err = verifyLegacyBootstrapBinary(b.Source.Binary, b.Source.SHA); err != nil {
		return err
	}
	// Accepted Unix connections include handlers queued before admission fencing.
	// Excluding them closes the gap between a resource snapshot and shutdown.
	raw, err := exec.CommandContext(ctx, "/usr/sbin/lsof", "-nP", "-a", "-p", strconv.Itoa(r.PID), "-U", "-Fftn").Output()
	if err != nil {
		return fmt.Errorf("legacy pending-connection observation: %w", err)
	}
	if err = validateLegacyUnixFDs(string(raw)); err != nil {
		return err
	}
	return nil
}

func validateLegacyUnixFDs(raw string) error {
	count := 0
	for _, line := range strings.Split(raw, "\n") {
		if line == "tunix" {
			count++
		}
		if strings.HasPrefix(line, "n") && (strings.Contains(line, "->") || !strings.HasSuffix(line, "/host.sock")) {
			return errors.New("legacy Host still has an accepted Unix connection")
		}
	}
	if count != 1 {
		return errors.New("legacy Host listener is not uniquely empty")
	}
	return nil
}
func signalEmptyLegacyHost(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }
