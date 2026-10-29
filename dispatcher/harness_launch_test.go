package dispatcher

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func processCommandForTest(t *testing.T, d *ProcessDispatcher, req DispatchRequest, systemPrompt string) (*exec.Cmd, error) {
	t.Helper()
	harness, err := selectHarness(req.Provider)
	if err != nil {
		return nil, err
	}
	return d.buildCmd(req, d.planProcessLaunch(harness, req, systemPrompt))
}

// Like the existing startup control, these children run only POSIX shell code.
// Configured binary directories exercise the native launcher without a model.
func writeHarnessCLI(t *testing.T, dir, name, script string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake harness uses a POSIX shell")
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script), 0700); err != nil {
		t.Fatal(err)
	}
}

func waitHarnessSession(t *testing.T, session Session) {
	t.Helper()
	select {
	case <-session.Done():
	case <-time.After(5 * time.Second):
		s := session.(*processSession)
		for _, path := range []string{s.stampedPath, s.canonicalPath, s.stderrPath} {
			data, err := os.ReadFile(path)
			t.Logf("%s: %s (%v)", path, data, err)
		}
		select {
		case err := <-s.process.WaitProblems():
			t.Logf("wait: %v", err)
		default:
		}
		t.Fatal("fake harness session did not finish")
	}
}

func readHarnessArgs(t *testing.T, root string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "argv"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\x00"), "\x00")
}

func TestHarnessRejectionPrecedesSideEffects(t *testing.T) {
	for _, provider := range []string{"unknown", "openshell"} {
		for _, carrier := range []string{"process", "custom", "sprites"} {
			t.Run(provider+"/"+carrier, func(t *testing.T) {
				root := t.TempDir()
				runtimeDir := filepath.Join(root, "runtime-not-created")
				writeHarnessCLI(t, root, "claude", "echo launched > launched\nexit 1\n")
				writeHarnessCLI(t, root, "codex", "echo launched > launched\nexit 1\n")
				req := DispatchRequest{Name: "rejected", Provider: provider, Model: "fixture", Prompt: "prompt", WorktreePath: root, AllowPrimaryCheckout: true}
				var d Dispatcher
				if carrier == "sprites" {
					req.Runtime = "sprites"
					sprites := NewSpritesDispatcher(SpritesDispatcherConfig{ProjectDir: root, RuntimeDir: runtimeDir, SpriteName: "never-contact"})
					sprites.newSprite = func(string) spriteHandle {
						t.Fatal("unsupported harness acquired a remote Sprite handle")
						return nil
					}
					d = sprites
				} else {
					config := ProcessDispatcherConfig{ProjectDir: root, RuntimeDir: runtimeDir, ProviderConfigs: ProviderConfigs{Claude: ProviderConfig{Path: root}, Codex: ProviderConfig{Path: root}}}
					if carrier == "custom" {
						config.RuntimeDefault = "echo launched > launched"
					}
					d = NewProcessDispatcher(config)
				}
				for _, linked := range []bool{false, true} {
					// Also reject before worktree resolution could execute Git.
					req.AllowPrimaryCheckout = !linked
					session, err := d.Dispatch(context.Background(), req)
					if session != nil || err == nil || err.Error() != "harness "+strconv.Quote(provider)+" not supported" {
						t.Fatalf("unsupported harness = %v, %v", session, err)
					}
				}
				for _, path := range []string{runtimeDir, filepath.Join(root, "launched")} {
					if _, err := os.Stat(path); !os.IsNotExist(err) {
						t.Fatalf("unsupported harness left a side effect at %s: %v", path, err)
					}
				}
			})
		}
	}
}

// Owned-group lifetime is exercised by the existing owned_group/completion_exit controls.
func TestHarnessCodexNativeDispatchArgvStdinAndResume(t *testing.T) {
	root := t.TempDir()
	writeHarnessCLI(t, root, "codex", `printf '%s\0' "$@" > argv
cat > received
printf '%s\n' 'startup marker' >&2
printf '%s\n' '{"type":"thread.started","thread_id":"`+resumeTestThread+`"}' '{"type":"item.completed","item":{"type":"agent_message","text":"fixture done"}}' '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'
`)
	runtimeDir := filepath.Join(root, ".noodle")
	d := NewProcessDispatcher(ProcessDispatcherConfig{ProjectDir: root, RuntimeDir: runtimeDir,
		ProviderConfigs: ProviderConfigs{Codex: ProviderConfig{Path: root, Args: []string{"--sandbox", "workspace-write", "--profile", "fixture"}}}})
	req := DispatchRequest{Name: "native", Provider: "codex", Model: "fixture", Prompt: "literal prompt\nwith 'quotes' and $(no command)", WorktreePath: root, AllowPrimaryCheckout: true, Skill: "local-task"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for attempt := 0; attempt < 2; attempt++ {
		session, err := d.Dispatch(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = session.ForceKill() })
		waitHarnessSession(t, session)
		if session.Controller().Steerable() || session.Outcome().Status != StatusCompleted {
			t.Fatalf("Codex lifecycle changed: %+v", session.Outcome())
		}
		want := []string{"exec", "--skip-git-repo-check", "--json", "--model", "fixture", "--sandbox", "workspace-write", "--profile", "fixture"}
		if attempt == 1 {
			want = []string{"exec", "--sandbox", "workspace-write", "--profile", "fixture", "resume", "--skip-git-repo-check", "--json", "--model", "fixture", resumeTestThread, "-"}
		}
		if got := readHarnessArgs(t, root); !reflect.DeepEqual(got, want) {
			t.Fatalf("native argv = %q, want %q", got, want)
		}
		s := session.(*processSession)
		<-s.stderrDone
		input, err := os.ReadFile(filepath.Join(root, "received"))
		if err != nil {
			t.Fatal(err)
		}
		composed, err := os.ReadFile(filepath.Join(runtimeDir, "sessions", session.ID(), "input.txt"))
		if err != nil {
			t.Fatal(err)
		}
		r := readCodexStartup(t, s.startup.path)
		if !bytes.Equal(input, composed) || !bytes.Contains(input, []byte(req.Prompt)) || r.Stdin.CloseCompletedAt == nil || r.Stdin.CloseError != "" || r.Stdin.WriteError != "" || r.Stdin.WrittenBytes == nil || *r.Stdin.WrittenBytes != len(input) || r.FirstInitAt == nil || r.NativeThreadID != resumeTestThread {
			t.Fatalf("native stdin/startup changed: %s, %+v", input, r)
		}
		// The loop owns meta.json. Reproduce its terminal observation only
		// after this real child and session have both exited.
		if code, exited := s.process.ExitCode(); !exited || code != 0 {
			t.Fatalf("original child exit = %d, %v", code, exited)
		}
		meta, err := json.Marshal(map[string]any{"session_id": session.ID(), "status": "exited", "alive": false})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(runtimeDir, "sessions", session.ID(), "meta.json"), meta, 0600); err != nil {
			t.Fatal(err)
		}
		req.ResumeSessionID = session.ID()
	}
}

func TestHarnessClaudeNativeDispatchArgvAndSteering(t *testing.T) {
	root := t.TempDir()
	writeHarnessCLI(t, root, "claude", `printf '%s\0' "$@" > argv
IFS= read -r first || exit 1
printf '%s\n' "$first" > received
printf '%s\n' '{"type":"system","subtype":"init","session_id":"fixture"}' '{"type":"result","subtype":"success","result":"first done"}'
IFS= read -r second || exit 2
printf '%s\n' "$second" >> received
printf '%s\n' '{"type":"result","subtype":"success","result":"second done"}'
`)
	d := NewProcessDispatcher(ProcessDispatcherConfig{ProjectDir: root, RuntimeDir: filepath.Join(root, ".noodle"), ProviderConfigs: ProviderConfigs{Claude: ProviderConfig{Path: root, Args: []string{"--extra", "two words"}}}})
	req := DispatchRequest{Name: "native", Provider: "claude", Model: "fixture", Prompt: "initial\n'prompt'", WorktreePath: root, AllowPrimaryCheckout: true, MaxTurns: 3, BudgetCap: 1.25, ReasoningLevel: "high", SystemPrompt: "system fixture"}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := d.Dispatch(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.ForceKill() })
	if !session.Controller().Steerable() {
		t.Fatal("Claude controller is not steerable")
	}
	if err := session.Controller().SendMessage(ctx, "second\nsteer"); err != nil {
		t.Fatal(err)
	}
	waitHarnessSession(t, session)
	want := []string{"--output-format", "stream-json", "--verbose", "--permission-mode", "bypassPermissions", "--max-turns", "3", "--max-budget-usd", "1.25", "--model", "fixture", "--effort", "high", "--append-system-prompt", "system fixture", "--input-format", "stream-json", "--extra", "two words"}
	got := readHarnessArgs(t, root)
	if len(got) != len(want) || !strings.HasSuffix(got[14], "\n\nsystem fixture") || !strings.Contains(got[14], root) {
		t.Fatalf("Claude system prompt changed: %q", got)
	}
	want[14] = got[14]
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("native Claude argv = %q, want %q", got, want)
	}
	data, err := os.ReadFile(filepath.Join(root, "received"))
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	if len(lines) != 2 {
		t.Fatalf("stream-json messages = %q", data)
	}
	for i, prompt := range []string{req.Prompt, "second\nsteer"} {
		var message struct {
			Type    string `json:"type"`
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(lines[i], &message); err != nil || message.Type != "user" || message.Message.Role != "user" || message.Message.Content != prompt {
			t.Fatalf("native steering message = %s: %v", lines[i], err)
		}
	}
}

func TestHarnessSpritePipeContractWithNativeCLI(t *testing.T) {
	for _, provider := range []string{"claude", "codex"} {
		t.Run(provider, func(t *testing.T) {
			root := t.TempDir()
			writeHarnessCLI(t, root, provider, "printf '%s\\0' \"$@\" > argv\ncat > received\n")
			req := DispatchRequest{Model: "fixture"}
			args := buildSpriteClaudeArgs(req, "sprite system")
			want := []string{"-p", "--output-format", "stream-json", "--verbose", "--permission-mode", "bypassPermissions", "--model", "fixture", "--append-system-prompt", "sprite system"}
			if provider == "codex" {
				args = buildSpriteCodexArgs(req)
				want = []string{"exec", "--dangerously-bypass-approvals-and-sandbox", "--skip-git-repo-check", "--json", "--model", "fixture"}
			}
			if args[0] != provider {
				t.Fatalf("Sprite binary = %q, want %q", args[0], provider)
			}
			// Exercise the unmodified pipe-mode argv with plain stdin locally;
			// never acquire a Sprite handle, push a branch, or start a VM.
			cmd := exec.Command(filepath.Join(root, args[0]), args[1:]...)
			cmd.Dir = root
			cmd.Stdin = strings.NewReader("plain prompt\n'quoted'\n")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("fake Sprite CLI: %s, %v", out, err)
			}
			if got := readHarnessArgs(t, root); !reflect.DeepEqual(got, want) {
				t.Fatalf("Sprite argv = %q, want %q", got, want)
			}
			data, err := os.ReadFile(filepath.Join(root, "received"))
			if err != nil || string(data) != "plain prompt\n'quoted'\n" {
				t.Fatalf("Sprite pipe stdin = %q, %v", data, err)
			}
		})
	}
}
