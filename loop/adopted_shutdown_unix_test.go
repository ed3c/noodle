//go:build !windows

package loop

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestShutdownDoesNotSignalAdoptedPIDWithoutChildOwner(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	root := t.TempDir()
	dir := filepath.Join(root, "sessions", "recovered")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"pid": cmd.Process.Pid, "session_id": "recovered"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "process.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	var log bytes.Buffer
	l := &Loop{runtimeDir: root, logger: slog.New(slog.NewTextHandler(&log, nil)), cooks: cookTracker{adoptedSessions: []string{"recovered"}}}
	l.Shutdown()
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("unowned adopted process was stopped: %v", err)
	}
	if !strings.Contains(log.String(), "without original child owner; readback required") {
		t.Fatalf("missing unresolved lifecycle diagnostic: %s", log.String())
	}
	kept, err := os.ReadFile(filepath.Join(dir, "process.json"))
	if err != nil || !bytes.Equal(kept, raw) {
		t.Fatalf("original process record changed: %s, %v", kept, err)
	}
}
