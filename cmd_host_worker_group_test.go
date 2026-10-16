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
