package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestProcessRecoveryDoesNotInventNativeProcessExit(t *testing.T) {
	dir := t.TempDir()
	session := filepath.Join(dir, "sessions", "original-native")
	if err := os.MkdirAll(session, 0700); err != nil {
		t.Fatal(err)
	}
	meta := []byte(`{"session_id":"original-native","runtime":"codex-app-server","status":"running","alive":null}`)
	if err := os.WriteFile(filepath.Join(session, "meta.json"), meta, 0600); err != nil {
		t.Fatal(err)
	}
	r := &processRuntime{DispatcherRuntime: NewDispatcherRuntime("process", nil, dir)}
	rows, err := r.Recover(context.Background())
	if err != nil || len(rows) != 0 {
		t.Fatalf("process owner adopted native session: %v %v", rows, err)
	}
	actual, err := os.ReadFile(filepath.Join(session, "meta.json"))
	if err != nil || string(actual) != string(meta) {
		t.Fatalf("native history changed during process recovery: %s %v", actual, err)
	}
}
