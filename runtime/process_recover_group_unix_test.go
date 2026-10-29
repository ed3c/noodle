//go:build !windows

package runtime

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestProcessRecoveryDoesNotLoseGroupWhenLauncherExited(t *testing.T) {
	dir := t.TempDir()
	sessionDir := filepath.Join(dir, "sessions", "leftover-group")
	if err := os.MkdirAll(sessionDir, 0755); err != nil {
		t.Fatal(err)
	}
	stop := filepath.Join(dir, "stop")
	ready := filepath.Join(dir, "ready")
	drained := filepath.Join(dir, "drained")
	cmd := exec.Command("sh", "-c", `(while [ ! -f "$1" ]; do sleep 0.05; done; echo drained > "$3") &
 echo ready > "$2"
 exit 0`, "fixture", stop, ready, drained)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.WriteFile(stop, []byte("stop"), 0600); err != nil {
			t.Error(err)
			return
		}
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(drained); err == nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Error("test descendant did not acknowledge cleanup before temporary directory removal")
	})
	record, _ := json.Marshal(map[string]any{"pid": cmd.Process.Pid, "session_id": "leftover-group", "started_at": time.Now().UTC()})
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ready); err != nil {
		t.Fatal(err)
	}
	meta := []byte(`{"session_id":"leftover-group","status":"running","retain":"original"}`)
	if err := os.WriteFile(filepath.Join(sessionDir, "meta.json"), meta, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "process.json"), record, 0644); err != nil {
		t.Fatal(err)
	}
	rt := &processRuntime{DispatcherRuntime: NewDispatcherRuntime("process", nil, dir)}
	recovered, err := rt.Recover(context.Background())
	if err == nil {
		t.Fatalf("launcher absence must not certify group exit: recovered=%v", recovered)
	}
	after, readErr := os.ReadFile(filepath.Join(sessionDir, "meta.json"))
	if readErr != nil || string(after) != string(meta) {
		t.Fatalf("unknown group rewrote original metadata: %s err=%v", after, readErr)
	}
}
