package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/poteto/noodle/internal/execution"
	"github.com/poteto/noodle/internal/procx"
)

func validateHostEnvironment(e *execution.Context) error {
	for _, key := range []string{"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR"} {
		if value, ok := e.Lookup(key); ok && value != "" {
			return fmt.Errorf("host context cannot override repository ownership through %s", key)
		}
	}
	return nil
}

func hostRecoveryPreflight(runtimeDir string) error {
	entries, err := os.ReadDir(filepath.Join(runtimeDir, "sessions"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(runtimeDir, "sessions", entry.Name())
		data, err := os.ReadFile(filepath.Join(dir, "meta.json"))
		if err != nil {
			return fmt.Errorf("session %s has unknown metadata; owner readback required", entry.Name())
		}
		var meta struct {
			Status string `json:"status"`
		}
		if err := json.Unmarshal(data, &meta); err != nil {
			return fmt.Errorf("session %s has invalid metadata; owner readback required", entry.Name())
		}
		switch strings.ToLower(meta.Status) {
		case "completed", "failed", "cancelled", "killed", "exited":
		default:
			return fmt.Errorf("session %s has unresolved status %q; owner readback required", entry.Name(), meta.Status)
		}
		pidPath := filepath.Join(dir, "process.json")
		if _, err := os.Stat(pidPath); os.IsNotExist(err) {
			continue
		} else if err != nil {
			return err
		}
		pid, err := procx.ReadPIDFile(pidPath)
		if err != nil {
			return fmt.Errorf("session %s has unknown process identity; owner readback required", entry.Name())
		}
		if procx.IsPIDAlive(pid) {
			return fmt.Errorf("session %s still has a live process; owner readback required", entry.Name())
		}
	}
	return nil
}
