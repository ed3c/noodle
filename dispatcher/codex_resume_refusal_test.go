package dispatcher

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCodexResumeRefusalRequiresNativeMissingBeforeTurnAndExit(t *testing.T) {
	for _, which := range []string{"missing", "not_loaded", "transport", "stdout", "init", "active", "degraded", "foreign_process", "duplicate_error"} {
		t.Run(which, func(t *testing.T) {
			root := t.TempDir()
			dir := filepath.Join(root, "sessions", "failed-resume")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			write := func(name string, v any) {
				t.Helper()
				data, err := json.Marshal(v)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			spawn := dispatchMetadata{SessionID: "failed-resume", Provider: "codex", ResumeSessionID: "original", ResumeThreadID: resumeTestThread}
			if which == "degraded" {
				spawn.DegradedFromSessionID = "older"
			}
			write("spawn.json", spawn)
			startup := codexStartupReceipt{SessionID: "failed-resume"}
			if which == "init" {
				now := time.Now()
				startup.FirstInitAt = &now
			}
			write("codex-startup.json", startup)
			raw := []byte{}
			if which == "stdout" {
				raw = []byte("{\"type\":\"turn.started\"}\n")
			}
			if err := os.WriteFile(filepath.Join(dir, "raw.ndjson"), raw, 0600); err != nil {
				t.Fatal(err)
			}
			stderr := "Error: thread/resume: thread/resume failed: no rollout found for thread id " + resumeTestThread + " (code -32600)\n"
			switch which {
			case "not_loaded":
				stderr = "Error: thread not loaded: " + resumeTestThread
			case "transport":
				stderr = "Error: connection reset"
			case "duplicate_error":
				stderr += stderr
			}
			if err := os.WriteFile(filepath.Join(dir, "stderr.log"), []byte(stderr), 0600); err != nil {
				t.Fatal(err)
			}
			write("meta.json", map[string]any{"session_id": "failed-resume", "status": "exited", "alive": which == "active"})
			processID := "failed-resume"
			if which == "foreign_process" {
				processID = "foreign"
			}
			write("process.json", map[string]any{"session_id": processID, "pid": 99999999})
			observed, err := ReadCodexResumeRefusal(root, "failed-resume")
			if which == "missing" {
				if err != nil || observed == nil || observed.ThreadID != resumeTestThread || len(observed.Files) != 4 {
					t.Fatalf("native missing refused: %+v %v", observed, err)
				}
			} else if observed != nil {
				t.Fatalf("%s authorized degradation: %+v", which, observed)
			}
			if got, _ := os.ReadFile(filepath.Join(dir, "stderr.log")); string(got) != stderr {
				t.Fatal("original error rewritten")
			}
		})
	}
}
