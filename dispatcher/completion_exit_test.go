package dispatcher

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCompletedOwnedWorkerStopsLingeringLauncher(t *testing.T) {
	dir := t.TempDir()
	cmd := exec.Command("sh", "-c", `printf '%s\n' '{"type":"thread.started","thread_id":"native-retained"}' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'; sleep 30`)
	h, err := startProcess(cmd, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.ForceKill(); _ = h.Stdout().Close(); _ = h.Stderr().Close() })
	s := newProcessSession(processSessionConfig{id: "retained", process: h,
		canonicalPath: filepath.Join(dir, "canonical.ndjson"), stampedPath: filepath.Join(dir, "raw.ndjson"), requireGroupExit: true})
	s.start(context.Background())
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		for _, name := range []string{"raw.ndjson", "canonical.ndjson"} {
			data, err := os.ReadFile(filepath.Join(dir, name))
			t.Logf("%s: %s (%v)", name, data, err)
		}
		select {
		case err := <-h.waitProblem:
			t.Logf("original wait diagnostic: %v", err)
		default:
		}
		t.Logf("owner pid=%d, retained=%v, observed=%+v", h.PID(), h.retainGroup, s.Outcome())
		rows, _ := exec.Command("ps", "-axo", "pid=,ppid=,pgid=,stat=,comm=").Output()
		for _, row := range strings.Split(string(rows), "\n") {
			fields := strings.Fields(row)
			if len(fields) >= 3 && fields[2] == strconv.Itoa(h.PID()) {
				t.Log(row)
			}
		}
		for len(s.events) > 0 {
			t.Logf("session event: %+v", <-s.events)
		}
		t.Fatal("canonical completion left the owned launcher running")
	}
	if s.Outcome().Status != StatusCompleted {
		t.Fatalf("cleanup replaced the native completion: %+v", s.Outcome())
	}
	raw, err := os.ReadFile(filepath.Join(dir, "raw.ndjson"))
	if err != nil || !strings.Contains(string(raw), "turn.completed") || !strings.Contains(string(raw), "native-retained") {
		t.Fatalf("native context record lost: %s, %v", raw, err)
	}
}

func TestCompletedTurnKeepsSteerableSessionAvailable(t *testing.T) {
	dir := t.TempDir()
	h, err := startProcess(exec.Command("sh", "-c", `printf '%s\n' '{"type":"turn.completed"}'; sleep 30`), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = h.ForceKill()
		_ = h.Stdout().Close()
		_ = h.Stderr().Close()
	})
	s := newProcessSession(processSessionConfig{id: "steerable", process: h,
		controller: newClaudeController(h.Stdin()), canonicalPath: filepath.Join(dir, "canonical.ndjson"),
		stampedPath: filepath.Join(dir, "raw.ndjson"), requireGroupExit: true})
	s.start(context.Background())
	select {
	case <-s.Done():
		t.Fatal("a single turn completion closed the resumable runtime session")
	case <-time.After(2300 * time.Millisecond):
	}
}

func TestForceCleanupPreservesObservedNativeCompletion(t *testing.T) {
	dir := t.TempDir()
	h, err := startProcess(exec.Command("sh", "-c", `printf '%s\n' '{"type":"turn.completed"}'; sleep 30`), true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.ForceKill(); _ = h.Stdout().Close(); _ = h.Stderr().Close() })
	s := newProcessSession(processSessionConfig{id: "cleanup", process: h,
		canonicalPath: filepath.Join(dir, "canonical.ndjson"), stampedPath: filepath.Join(dir, "raw.ndjson"), requireGroupExit: true})
	s.start(context.Background())
	select {
	case event := <-s.Events():
		if event.Type != "result" {
			t.Fatalf("native completion missing: %+v", event)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("native completion not observed")
	}
	if err := s.ForceKill(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-s.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("original group cleanup not confirmed")
	}
	if s.Outcome().Status != StatusCompleted {
		t.Fatalf("resource cleanup rewrote the observed completion: %+v", s.Outcome())
	}
}
