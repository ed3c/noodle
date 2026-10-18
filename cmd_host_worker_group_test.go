//go:build !windows

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestHostRecoveryPreflightChecksRecordedStartIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, started string
		wantErr       bool
	}{
		{"reused-pid", "2000-01-01T00:00:00Z", false},
		{"same-process", time.Now().UTC().Format(time.RFC3339Nano), true},
		{"missing-start", "", true},
		{"invalid-start", "unknown", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			session := filepath.Join(dir, "sessions", "fixture")
			if err := os.MkdirAll(session, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(session, "meta.json"), []byte(`{"status":"completed"}`), 0600); err != nil {
				t.Fatal(err)
			}
			record := map[string]any{"pid": os.Getpid()}
			if tc.started != "" {
				record["started_at"] = tc.started
			}
			raw, err := json.Marshal(record)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(session, "process.json"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			if err := hostRecoveryPreflight(dir); (err != nil) != tc.wantErr {
				t.Fatalf("preflight: %v", err)
			}
			if tc.name == "reused-pid" {
				t.Setenv("PATH", t.TempDir())
				if err := hostRecoveryPreflight(dir); err == nil {
					t.Fatal("missing OS start readback accepted")
				}
			}
		})
	}
}

func TestHostRecoveryPreflightRetainsExitedWorkerGroup(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command("/bin/sh", "-c", "sleep 30 </dev/null >/dev/null 2>&1 & echo $! > child.pid")
	cmd.Dir = dir
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "child.pid"))
	if err != nil {
		t.Fatal(err)
	}
	child, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || child <= 1 {
		t.Fatalf("fixture child identity: %q %v", raw, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(child, syscall.SIGKILL) })
	session := filepath.Join(dir, "sessions", "fixture")
	if err := os.MkdirAll(session, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(session, "meta.json"), []byte(`{"status":"completed"}`), 0600); err != nil {
		t.Fatal(err)
	}
	raw, _ = json.Marshal(map[string]int{"pid": cmd.Process.Pid})
	if err := os.WriteFile(filepath.Join(session, "process.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := hostRecoveryPreflight(dir); err == nil {
		t.Fatal("recovery accepted a terminal launcher with a live worker group")
	}
	if err := syscall.Kill(child, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := hostRecoveryPreflight(dir); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("recovery did not accept confirmed worker group exit")
}
