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
	cmd, err := processCommandForTest(t, d, bound, "")
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
		{"foreign role", func() {
			write("spawn.json", dispatchMetadata{SessionID: "prior-attempt", Provider: "codex", Skill: req.Skill, WorktreePath: req.WorktreePath, TaskRole: "review"})
		}},
		{"foreign worktree", func() {
			write("spawn.json", dispatchMetadata{SessionID: "prior-attempt", Provider: "codex", Skill: req.Skill, WorktreePath: "/another/worktree"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			write("meta.json", map[string]any{"session_id": "prior-attempt", "status": "exited", "alive": false})
			write("spawn.json", dispatchMetadata{SessionID: "prior-attempt", Provider: "codex", Skill: req.Skill, WorktreePath: req.WorktreePath})
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

func TestCodexResumeConfiguredArgsPrecedeSubcommand(t *testing.T) {
	for _, extra := range [][]string{{"--sandbox", "workspace-write"}, {"--profile", "ci"}, {"--sandbox", "invalid-mode"}} {
		d := NewProcessDispatcher(ProcessDispatcherConfig{ProviderConfigs: ProviderConfigs{Codex: ProviderConfig{Args: extra}}})
		cmd, err := processCommandForTest(t, d, DispatchRequest{Provider: "codex", Model: "test-model", codexThreadID: resumeTestThread}, "")
		if err != nil {
			t.Fatal(err)
		}
		want := append([]string{"exec"}, extra...)
		want = append(want, "resume", "--skip-git-repo-check", "--json", "--model", "test-model", resumeTestThread, "-")
		if !reflect.DeepEqual(cmd.Args[1:], want) {
			t.Fatalf("resume argv = %q, want %q", cmd.Args[1:], want)
		}
	}
}

func TestCodexResumeRetryRetainsThreadAfterPreTurnCLIRefusal(t *testing.T) {
	for _, variant := range []string{"parser-refusal", "transport-unknown", "stdout-observed", "source-drift", "alive", "malformed-raw", "cycle"} {
		t.Run(variant, func(t *testing.T) {
			root := t.TempDir()
			write := func(id, name string, value any) {
				t.Helper()
				dir := filepath.Join(root, "sessions", id)
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				var data []byte
				if text, ok := value.(string); ok {
					data = []byte(text)
				} else {
					var err error
					data, err = json.Marshal(value)
					if err != nil {
						t.Fatal(err)
					}
				}
				if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
					t.Fatal(err)
				}
			}
			req := DispatchRequest{Provider: "codex", Skill: "local-task", WorktreePath: "/same/worktree", ResumeSessionID: "parse-failed"}
			for _, id := range []string{"native-prior", "parse-failed"} {
				status := "exited"
				if id == "parse-failed" {
					status = "failed"
				}
				write(id, "meta.json", map[string]any{"session_id": id, "status": status, "alive": variant == "alive" && id == "parse-failed"})
				write(id, "process.json", map[string]any{"session_id": id, "pid": 99999999})
				write(id, "spawn.json", dispatchMetadata{SessionID: id, Provider: "codex", Skill: req.Skill, WorktreePath: req.WorktreePath})
			}
			write("native-prior", "raw.ndjson", `{"type":"thread.started","thread_id":"`+resumeTestThread+`"}`+"\n")
			_, digest, err := readCodexThread(filepath.Join(root, "sessions/native-prior/raw.ndjson"))
			if err != nil {
				t.Fatal(err)
			}
			parent := "native-prior"
			if variant == "cycle" {
				parent = "parse-failed"
			}
			if variant == "source-drift" {
				digest = strings.Repeat("0", 64)
			}
			write("parse-failed", "spawn.json", dispatchMetadata{SessionID: "parse-failed", Provider: "codex", Skill: req.Skill, WorktreePath: req.WorktreePath, ResumeSessionID: parent, ResumeThreadID: resumeTestThread, ResumeSourceSHA: digest})
			raw := ""
			if variant == "malformed-raw" {
				raw = "{"
			}
			write("parse-failed", "raw.ndjson", raw)
			stderr := "error: unexpected argument '--sandbox' found\n\nUsage: codex exec resume [OPTIONS] [SESSION_ID] [PROMPT]\n\nFor more information, try '--help'.\n"
			if variant == "transport-unknown" {
				stderr = "connection ended\n"
			}
			write("parse-failed", "stderr.log", stderr)
			startup := codexStartupReceipt{SessionID: "parse-failed", FirstStderr: &codexFirstRead{Bytes: len(stderr)}}
			if variant == "stdout-observed" {
				startup.FirstStdout = &codexFirstRead{Bytes: 1}
			}
			write("parse-failed", "codex-startup.json", startup)
			d := NewProcessDispatcher(ProcessDispatcherConfig{RuntimeDir: root})
			got, err := d.resolveCodexResume(req)
			if variant == "parser-refusal" {
				if err != nil || got.codexThreadID != resumeTestThread || got.ResumeSessionID != "parse-failed" {
					t.Fatalf("original thread not retained: %+v, %v", got, err)
				}
			} else if err == nil {
				t.Fatalf("unsafe retry accepted: %s", variant)
			}
		})
	}
}
