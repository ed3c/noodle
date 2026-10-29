package runtime

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestProcessRuntimeRecoverLivePID(t *testing.T) {
	runtimeDir := t.TempDir()
	sessionID := "cook-live"
	sessionDir := filepath.Join(runtimeDir, "sessions", sessionID)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Write process.json with our own PID (alive).
	procMeta, _ := json.Marshal(map[string]any{
		"pid":        os.Getpid(),
		"session_id": sessionID,
		"started_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err := os.WriteFile(filepath.Join(sessionDir, "process.json"), procMeta, 0o644); err != nil {
		t.Fatalf("write process.json: %v", err)
	}

	// Write meta.json with running status.
	metaJSON, _ := json.Marshal(map[string]any{
		"session_id": sessionID,
		"status":     "running",
	})
	if err := os.WriteFile(filepath.Join(sessionDir, "meta.json"), metaJSON, 0o644); err != nil {
		t.Fatalf("write meta.json: %v", err)
	}

	// Write prompt.txt with order tag.
	if err := os.WriteFile(filepath.Join(sessionDir, "prompt.txt"), []byte("[order:fix-bug-42]\nFix the bug."), 0o644); err != nil {
		t.Fatalf("write prompt.txt: %v", err)
	}

	rt := &processRuntime{DispatcherRuntime: NewDispatcherRuntime("process", nil, runtimeDir)}
	recovered, err := rt.Recover(context.Background())
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(recovered) != 1 {
		t.Fatalf("recovered %d sessions, want 1", len(recovered))
	}
	rs := recovered[0]
	if rs.OrderID != "fix-bug-42" {
		t.Fatalf("order ID = %q, want %q", rs.OrderID, "fix-bug-42")
	}
	if rs.SessionHandle.ID() != sessionID {
		t.Fatalf("session ID = %q, want %q", rs.SessionHandle.ID(), sessionID)
	}
	if rs.Reason != "original process/group exit requires owner readback" {
		t.Fatalf("reason = %q, want %q", rs.Reason, "original process/group exit requires owner readback")
	}
}

func TestProcessRuntimeRecoverDeadPIDUpdatesMetaToExited(t *testing.T) {
	runtimeDir := t.TempDir()
	sessionID := "cook-dead"
	sessionDir := filepath.Join(runtimeDir, "sessions", sessionID)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Write process.json with a bogus PID.
	procMeta, _ := json.Marshal(map[string]any{
		"pid":        2147483647,
		"session_id": sessionID,
		"started_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err := os.WriteFile(filepath.Join(sessionDir, "process.json"), procMeta, 0o644); err != nil {
		t.Fatalf("write process.json: %v", err)
	}

	// Write meta.json with running status.
	metaJSON, _ := json.Marshal(map[string]any{
		"session_id": sessionID,
		"status":     "running",
	})
	if err := os.WriteFile(filepath.Join(sessionDir, "meta.json"), metaJSON, 0o644); err != nil {
		t.Fatalf("write meta.json: %v", err)
	}

	rt := &processRuntime{DispatcherRuntime: NewDispatcherRuntime("process", nil, runtimeDir)}
	recovered, err := rt.Recover(context.Background())
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(recovered) != 0 {
		t.Fatalf("recovered %d sessions, want 0", len(recovered))
	}

	// Verify meta.json was updated to "exited".
	data, err := os.ReadFile(filepath.Join(sessionDir, "meta.json"))
	if err != nil {
		t.Fatalf("read meta.json: %v", err)
	}
	if !strings.Contains(string(data), `"status":"exited"`) {
		t.Fatalf("meta.json not updated to exited: %s", string(data))
	}
}

func TestProcessRuntimeRecoverMissingProcessIdentityStaysUnknown(t *testing.T) {
	runtimeDir := t.TempDir()
	sessionID := "cook-no-process-json"
	sessionDir := filepath.Join(runtimeDir, "sessions", sessionID)
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// meta.json with running status but no process.json.
	metaJSON, _ := json.Marshal(map[string]any{
		"session_id": sessionID,
		"status":     "running",
	})
	if err := os.WriteFile(filepath.Join(sessionDir, "meta.json"), metaJSON, 0o644); err != nil {
		t.Fatalf("write meta.json: %v", err)
	}

	rt := &processRuntime{DispatcherRuntime: NewDispatcherRuntime("process", nil, runtimeDir)}
	recovered, err := rt.Recover(context.Background())
	if err == nil || !strings.Contains(err.Error(), "process identity") {
		t.Fatalf("missing original process identity must remain unknown: recovered=%v err=%v", recovered, err)
	}
	if len(recovered) != 0 {
		t.Fatalf("recovered %d sessions, want 0 (no process.json)", len(recovered))
	}

	// meta.json should not have been modified.
	data, err := os.ReadFile(filepath.Join(sessionDir, "meta.json"))
	if err != nil {
		t.Fatalf("read meta.json: %v", err)
	}
	if !strings.Contains(string(data), `"status":"running"`) {
		t.Fatalf("meta.json should still be running: %s", string(data))
	}
}

func TestProcessRuntimeRecoverEmptySessionsDir(t *testing.T) {
	runtimeDir := t.TempDir()
	rt := &processRuntime{DispatcherRuntime: NewDispatcherRuntime("process", nil, runtimeDir)}
	recovered, err := rt.Recover(context.Background())
	if err != nil {
		t.Fatalf("recover: %v", err)
	}
	if len(recovered) != 0 {
		t.Fatalf("recovered %d sessions, want 0", len(recovered))
	}
}

func TestRecoveredIdentityCannotAcknowledgeStopOrExit(t *testing.T) {
	s := &recoveredSessionHandle{id: "unowned", status: "running"}
	if err := s.Terminate(); err == nil {
		t.Fatal("recovered identity acknowledged a stop without its original child owner")
	}
	if err := s.ForceKill(); err == nil {
		t.Fatal("recovered identity acknowledged force kill without its original child owner")
	}
	select {
	case <-s.Done():
		t.Fatal("recovered identity falsely certified process exit")
	default:
	}
}

func TestProcessRecoveryReadsFormattedMetadataAndPreservesRaw(t *testing.T) {
	dir := t.TempDir()
	sessionDir := filepath.Join(dir, "sessions", "formatted")
	if err := os.MkdirAll(sessionDir, 0755); err != nil {
		t.Fatal(err)
	}
	meta := []byte("{\n  \"session_id\": \"formatted\",\n  \"status\": \"running\",\n  \"retained\": {\"raw\": true}\n}\n")
	process, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "session_id": "formatted", "started_at": time.Now().UTC()})
	if err := os.WriteFile(filepath.Join(sessionDir, "process.json"), process, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "meta.json"), meta, 0644); err != nil {
		t.Fatal(err)
	}
	rt := &processRuntime{DispatcherRuntime: NewDispatcherRuntime("process", nil, dir)}
	recovered, err := rt.Recover(context.Background())
	if err != nil || len(recovered) != 1 {
		t.Fatalf("formatted native metadata lost pending identity: recovered=%v err=%v", recovered, err)
	}
	after, err := os.ReadFile(filepath.Join(sessionDir, "meta.json"))
	if err != nil || string(after) != string(meta) {
		t.Fatalf("live metadata modified: %s err=%v", after, err)
	}
	raw, err := os.ReadFile(filepath.Join(sessionDir, "process.json"))
	if err != nil || string(raw) != string(process) {
		t.Fatal("original process identity changed")
	}
}

func TestProcessRecoveryTerminalMetadataDoesNotHideLiveIdentity(t *testing.T) {
	dir := t.TempDir()
	sessionDir := filepath.Join(dir, "sessions", "terminal-live")
	if err := os.MkdirAll(sessionDir, 0755); err != nil {
		t.Fatal(err)
	}
	meta := []byte(`{"session_id":"terminal-live","status":"completed"}`)
	raw, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "session_id": "terminal-live", "started_at": time.Now().UTC()})
	if err := os.WriteFile(filepath.Join(sessionDir, "meta.json"), meta, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "process.json"), raw, 0644); err != nil {
		t.Fatal(err)
	}
	rt := &processRuntime{DispatcherRuntime: NewDispatcherRuntime("process", nil, dir)}
	recovered, err := rt.Recover(context.Background())
	if err != nil || len(recovered) != 1 {
		t.Fatalf("terminal claim hid original live process: recovered=%v err=%v", recovered, err)
	}
	wrong, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "session_id": "different-session", "started_at": time.Now().UTC()})
	if err := os.WriteFile(filepath.Join(sessionDir, "process.json"), wrong, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := rt.Recover(context.Background()); err == nil {
		t.Fatal("conflicting native Session identity accepted")
	}
	after, _ := os.ReadFile(filepath.Join(sessionDir, "meta.json"))
	if string(after) != string(meta) {
		t.Fatalf("unknown ownership rewrote history: %s", after)
	}
}
