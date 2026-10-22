package dispatcher

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const resumeTestThread = "01a1622d-1cdf-7c92-be2f-3613a06ecd1d"

func TestCodexResumeUsesExactBoundThread(t *testing.T) {
	dir := t.TempDir()
	prior := filepath.Join(dir, "sessions", "prior-attempt")
	if err := os.MkdirAll(prior, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(name string, value any) {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(prior, name), append(data, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
	req := DispatchRequest{Provider: "codex", Model: "test-model", Skill: "local-task", WorktreePath: "/retained/worktree", ResumeSessionID: "prior-attempt"}
	write("spawn.json", dispatchMetadata{SessionID: "prior-attempt", Provider: "codex", Skill: req.Skill, WorktreePath: req.WorktreePath})
	write("meta.json", map[string]any{"session_id": "prior-attempt", "status": "exited", "alive": false})
	write("process.json", map[string]any{"session_id": "prior-attempt", "pid": 99999999})
	write("raw.ndjson", map[string]any{"type": "thread.started", "thread_id": resumeTestThread})
	d := NewProcessDispatcher(ProcessDispatcherConfig{RuntimeDir: dir})
	bound, err := d.resolveCodexResume(req)
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := d.buildCmd(bound, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"exec", "resume", "--skip-git-repo-check", "--json", "--model", "test-model", resumeTestThread, "-"}
	if !reflect.DeepEqual(cmd.Args[1:], want) {
		t.Fatalf("resume argv = %q, want %q", cmd.Args[1:], want)
	}
	startup, err := newCodexStartup(prior, "prior-attempt", 0)
	if err != nil {
		t.Fatal(err)
	}
	startup.observeThread(filepath.Join(prior, "raw.ndjson"))
	observed := readCodexStartup(t, startup.path)
	if observed.NativeThreadID != resumeTestThread || observed.NativeThreadSourceSHA256 == "" || observed.NativeThreadError != "" {
		t.Fatalf("native identity lost: %+v", observed)
	}
	if _, err := d.resolveCodexResume(req); err != nil {
		t.Fatal(err)
	}
	write("raw.ndjson", map[string]any{"type": "thread.started", "thread_id": "01a16230-a1c2-7333-8948-c2a48d244833"})
	if _, err := d.resolveCodexResume(req); err == nil || !strings.Contains(err.Error(), "source changed") {
		t.Fatalf("native source drift accepted: %v", err)
	}
	write("raw.ndjson", map[string]any{"type": "thread.started", "thread_id": resumeTestThread})
	for _, tc := range []struct {
		name   string
		change func()
	}{
		{"live", func() {
			write("meta.json", map[string]any{"session_id": "prior-attempt", "status": "active", "alive": true})
		}},
		{"foreign worktree", func() {
			write("spawn.json", dispatchMetadata{SessionID: "prior-attempt", Provider: "codex", Skill: req.Skill, WorktreePath: "/another/worktree"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.change()
			if _, err := d.resolveCodexResume(req); err == nil {
				t.Fatal("unsafe resume accepted")
			}
		})
	}
}

func TestCodexNativeThreadReadbackPreservesRaw(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		valid     bool
	}{
		{"single", `{"type":"thread.started","thread_id":"` + resumeTestThread + `"}` + "\n", true},
		{"duplicate same identity", strings.Repeat(`{"type":"thread.started","thread_id":"`+resumeTestThread+`"}`+"\n", 2), true},
		{"missing", "{\"type\":\"turn.completed\"}\n", false},
		{"conflict", `{"type":"thread.started","thread_id":"` + resumeTestThread + `"}` + "\n" + `{"type":"thread.started","thread_id":"01a16230-a1c2-7333-8948-c2a48d244833"}` + "\n", false},
		{"truncated", `{"type":"thread.started","thread_id":"` + resumeTestThread + `"}` + "\n{", false},
		{"invalid id", "{\"type\":\"thread.started\",\"thread_id\":\"--last\"}\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "raw.ndjson")
			if err := os.WriteFile(path, []byte(tc.raw), 0600); err != nil {
				t.Fatal(err)
			}
			id, digest, err := readCodexThread(path)
			if tc.valid && (err != nil || id != resumeTestThread || len(digest) != 64) {
				t.Fatalf("readback = %q %q %v", id, digest, err)
			}
			if !tc.valid && err == nil {
				t.Fatal("unsupported identity accepted")
			}
			got, err := os.ReadFile(path)
			if err != nil || string(got) != tc.raw {
				t.Fatal("original raw changed")
			}
		})
	}
}
