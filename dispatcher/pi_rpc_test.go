package dispatcher

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/poteto/noodle/skill"
)

// The native boundary is a real child with real pipes, not a mocked RPC call.
func TestPiRPCChild(t *testing.T) {
	if os.Getenv("NOODLE_FAKE_PI") != "1" {
		return
	}
	mode := os.Getenv("NOODLE_FAKE_PI_MODE")
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	_ = os.WriteFile("argv", []byte(strings.Join(args, "\x00")+"\x00"), 0600)
	dir := ""
	for i, a := range args {
		if a == "--session-dir" {
			dir = args[i+1]
		}
	}
	input, _ := os.Create("received")
	defer input.Close()
	scanner := bufio.NewScanner(io.TeeReader(os.Stdin, input))
	scanner.Buffer(make([]byte, 65536), 8<<20)
	out := json.NewEncoder(os.Stdout)
	if mode == "pressure" {
		_, _ = os.Stderr.WriteString(strings.Repeat("stderr\n", 25000))
		for i := 0; i < 1000; i++ {
			_ = out.Encode(map[string]string{"type": "session", "id": "native-pi", "padding": strings.Repeat("x", 1000)})
		}
	}
	if mode == "no-read" {
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	if !scanner.Scan() {
		os.Exit(21)
	}
	if mode == "early" {
		os.Exit(0)
	}
	if mode == "timeout" {
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	state := map[string]any{"model": map[string]string{"provider": "openrouter", "id": "deepseek/deepseek-v4.1-flash-1017"},
		"sessionId": "native-pi", "sessionFile": filepath.Join(dir, "native-pi.jsonl"), "isStreaming": false, "isCompacting": false, "pendingMessageCount": 0}
	id, success := "noodle-state", true
	switch mode {
	case "wrong-model":
		state["model"] = map[string]string{"provider": "openrouter", "id": "deepseek/wrong"}
	case "wrong-provider":
		state["model"] = map[string]string{"provider": "other", "id": "deepseek/deepseek-v4.1-flash-1017"}
	case "wrong-id":
		id = "wrong"
	case "busy":
		state["isStreaming"] = true
	case "compacting":
		state["isCompacting"] = true
	case "pending":
		state["pendingMessageCount"] = 1
	case "missing-id":
		delete(state, "sessionId")
	case "missing-file":
		delete(state, "sessionFile")
	case "outside-file":
		state["sessionFile"] = filepath.Join(dir, "..", "outside.jsonl")
	case "missing-streaming":
		delete(state, "isStreaming")
	case "missing-pending":
		delete(state, "pendingMessageCount")
	case "missing-idle":
		delete(state, "isCompacting")
	case "missing-model":
		delete(state, "model")
	case "state-error":
		success = false
	}
	_ = os.WriteFile(filepath.Join(dir, "native-pi.jsonl"), []byte("{\"type\":\"session\",\"id\":\"native-pi\"}\n"), 0600)
	if mode == "unsolicited" {
		_ = out.Encode(map[string]string{"type": "agent_start"})
	}
	_ = out.Encode(map[string]any{"type": "response", "command": "get_state", "id": id, "success": success, "data": state, "error": "fixture state error"})
	if mode == "input-block" {
		_ = os.WriteFile("input-block-ready", []byte("ready"), 0600)
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	if !scanner.Scan() {
		os.Exit(0)
	}
	disposition, ackID := "started", "noodle-prompt"
	switch mode {
	case "handled", "queued":
		disposition = mode
	case "wrong-ack":
		ackID = "wrong"
	}
	ack := map[string]any{"type": "response", "command": "prompt", "id": ackID, "success": mode != "prompt-error", "data": map[string]string{"disposition": disposition}}
	if mode == "missing-disposition" {
		ack["data"] = map[string]string{}
	}
	if mode != "late-ack" && mode != "late-error-ack" && mode != "no-ack" {
		_ = out.Encode(ack)
	}
	if mode == "prompt-exit" {
		os.Exit(0)
	}
	_ = out.Encode(map[string]string{"type": "agent_start"})
	_ = out.Encode(map[string]any{"type": "tool_execution_start", "toolName": "bash", "args": map[string]string{"command": "echo fixture"}})
	if mode == "ack-only" || mode == "no-ack" {
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
	final := map[string]any{"role": "assistant", "content": []map[string]string{{"type": "text", "text": "answer"}}, "stopReason": "stop", "usage": map[string]any{"input": 2, "output": 3, "cost": map[string]float64{"total": 0.25}}}
	if mode == "model-error" || mode == "late-error-ack" {
		final["stopReason"] = "error"
		final["errorMessage"] = "provider failed"
	}
	_ = out.Encode(map[string]any{"type": "message_end", "message": final})
	_ = out.Encode(map[string]any{"type": "agent_end", "willRetry": false, "messages": []any{final}})
	if mode == "agent-end-exit" {
		os.Exit(0)
	}
	if mode == "agent-end-only" {
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
	if mode == "truncated" {
		fmt.Fprint(os.Stdout, `{"type":`)
		os.Exit(0)
	}
	if mode == "stdout-eof" {
		os.Stdout.Close()
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	_ = out.Encode(map[string]any{"type": "agent_settled", "aborted": mode == "aborted"})
	if mode == "late-ack" || mode == "late-error-ack" {
		_ = out.Encode(ack)
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	_ = os.WriteFile("stdin-eof", []byte("observed"), 0600)
	if mode == "hold-exit" {
		time.Sleep(time.Minute)
	}
	if mode == "group" {
		child := exec.Command("sh", "-c", "trap '' TERM; echo ready > child-ready; while :; do sleep 1; done")
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(41)
		}
		_ = os.WriteFile("child-pid", []byte(strconv.Itoa(child.Process.Pid)), 0600)
		// Keep the group alive until the parent has actually observed the child.
		for i := 0; i < 100; i++ {
			if _, err := os.Stat("child-ready"); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if mode == "nonzero" {
		os.Exit(7)
	}
	os.Exit(0)
}

func piFixture(t *testing.T, mode string) (*ProcessDispatcher, DispatchRequest, string) {
	t.Helper()
	root := t.TempDir()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	writeHarnessCLI(t, root, "pi", "exec "+strconv.Quote(binary)+" -test.run=^TestPiRPCChild$ -- \"$@\"\n")
	d := NewProcessDispatcher(ProcessDispatcherConfig{ProjectDir: root, RuntimeDir: filepath.Join(root, "runtime"), ProviderConfigs: ProviderConfigs{Pi: ProviderConfig{Path: root}}})
	req := DispatchRequest{Name: "pi-rpc", Provider: "pi", Model: "openrouter/deepseek/deepseek-v4.1-flash-1017", Prompt: "literal task\n$(not executed)", SystemPrompt: "selected skill instructions", Skill: "local-task", WorktreePath: root, AllowPrimaryCheckout: true,
		EnvVars: map[string]string{"NOODLE_FAKE_PI": "1", "NOODLE_FAKE_PI_MODE": mode}}
	return d, req, root
}

func TestPiNativeDispatch(t *testing.T) {
	for _, mode := range []string{"success", "late-ack", "pressure", "model-error", "aborted", "handled", "queued", "prompt-error", "wrong-ack", "nonzero", "truncated", "stdout-eof", "prompt-exit", "agent-end-exit", "missing-disposition", "late-error-ack"} {
		t.Run(mode, func(t *testing.T) {
			d, req, root := piFixture(t, mode)
			req.ReasoningLevel = "high"
			if mode == "success" {
				// Resolve the real primary bundle, rather than only SystemPrompt override.
				req.SystemPrompt = ""
				dir := filepath.Join(root, "skills", "local-task")
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: local-task\ndescription: fixture\n---\nselected skill instructions\n"), 0600); err != nil {
					t.Fatal(err)
				}
				d.skillResolver = skill.Resolver{SearchPaths: []string{filepath.Join(root, "skills")}}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			session, err := d.Dispatch(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			waitHarnessSession(t, session)
			out := session.Outcome()
			want := mode == "success" || mode == "late-ack" || mode == "pressure"
			if (out.Status == StatusCompleted) != want {
				t.Fatalf("outcome=%+v", out)
			}
			args := readHarnessArgs(t, root)
			dir := filepath.Join(d.runtimeDir, "sessions", session.ID())
			expected := []string{"--mode", "rpc", "--provider", "openrouter", "--model", "deepseek/deepseek-v4.1-flash-1017", "--no-extensions", "--no-mcp", "--no-skills", "--no-prompt-templates", "--no-themes", "--no-context-files", "--thinking", "high", "--session-dir", filepath.Join(dir, "pi")}
			if !reflect.DeepEqual(args, expected) {
				t.Fatalf("argv=%q", args)
			}
			data, err := os.ReadFile(filepath.Join(root, "received"))
			if err != nil {
				t.Fatal(err)
			}
			var commands []struct{ Type, ID, Message string }
			for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
				var cmd struct{ Type, ID, Message string }
				if err := json.Unmarshal([]byte(line), &cmd); err != nil {
					t.Fatal(err)
				}
				commands = append(commands, cmd)
			}
			if len(commands) != 2 || commands[0].Type != "get_state" || commands[0].ID != "noodle-state" || commands[1].Type != "prompt" || commands[1].ID != "noodle-prompt" {
				t.Fatalf("commands=%+v", commands)
			}
			if !strings.Contains(commands[1].Message, req.Prompt) || !strings.Contains(commands[1].Message, "selected skill instructions") || !strings.Contains(commands[1].Message, "Noodle Runtime Context") {
				t.Fatalf("composed prompt=%q", commands[1].Message)
			}
			for _, name := range []string{"pi-stdout.jsonl", "pi-rpc.json", "pi/native-pi.jsonl", "stderr.log"} {
				if _, err := os.ReadFile(filepath.Join(dir, name)); err != nil {
					t.Fatal(err)
				}
			}
			raw, _ := os.ReadFile(filepath.Join(dir, "pi-stdout.jsonl"))
			if !strings.Contains(string(raw), `"command":"get_state"`) || !strings.Contains(string(raw), `"sessionId":"native-pi"`) {
				t.Fatalf("raw evidence=%s", raw)
			}
			if mode == "pressure" {
				stderr, err := os.ReadFile(filepath.Join(dir, "stderr.log"))
				if err != nil {
					t.Fatal(err)
				}
				if string(stderr) != strings.Repeat("stderr\n", 25000) || strings.Count(string(raw), `"padding"`) != 1000 {
					t.Fatal("backpressure lost native stdout/stderr evidence")
				}
			}
			if want {
				var receipt piReceipt
				bytes, _ := os.ReadFile(filepath.Join(dir, "pi-rpc.json"))
				if err := json.Unmarshal(bytes, &receipt); err != nil {
					t.Fatal(err)
				}
				if !receipt.Verified || receipt.Native.SessionID == session.ID() || !receipt.Result || !receipt.StdinClosed || receipt.Error != "" {
					t.Fatalf("receipt=%+v", receipt)
				}
				canonical, err := os.ReadFile(filepath.Join(dir, "canonical.ndjson"))
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(canonical), "$ echo fixture") {
					t.Fatalf("tool event missing: %s", canonical)
				}
				if session.TotalCost() != 0.25 {
					t.Fatalf("cost=%v", session.TotalCost())
				}
				if _, err := os.Stat(filepath.Join(root, "stdin-eof")); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestPiStateRejectsBeforePrompt(t *testing.T) {
	for _, mode := range []string{"wrong-model", "wrong-provider", "wrong-id", "busy", "compacting", "pending", "missing-id", "missing-file", "outside-file", "missing-idle", "missing-streaming", "missing-pending", "missing-model", "state-error", "early", "unsolicited"} {
		t.Run(mode, func(t *testing.T) {
			d, req, root := piFixture(t, mode)
			session, err := d.Dispatch(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			waitHarnessSession(t, session)
			if out := session.Outcome(); out.Status != StatusFailed || out.Reason == "" {
				t.Fatalf("outcome=%+v", out)
			}
			data, err := os.ReadFile(filepath.Join(root, "received"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), `"type":"prompt"`) {
				t.Fatalf("rejected identity received prompt: %s", data)
			}
			raw, err := os.ReadFile(filepath.Join(d.runtimeDir, "sessions", session.ID(), "pi-stdout.jsonl"))
			if err != nil {
				t.Fatal(err)
			}
			if mode != "early" && !strings.Contains(string(raw), `"command":"get_state"`) {
				t.Fatalf("lost rejection evidence: %s", raw)
			}
		})
	}
}

func TestPiUnsupportedPrecedesSideEffects(t *testing.T) {
	tests := map[string]func(*ProcessDispatcher, *DispatchRequest){
		"resume":      func(_ *ProcessDispatcher, r *DispatchRequest) { r.ResumeSessionID = "old" },
		"turns":       func(_ *ProcessDispatcher, r *DispatchRequest) { r.MaxTurns = 1 },
		"budget":      func(_ *ProcessDispatcher, r *DispatchRequest) { r.BudgetCap = 0.1 },
		"custom":      func(d *ProcessDispatcher, _ *DispatchRequest) { d.runtimeDefault = "touch launched" },
		"runtime":     func(_ *ProcessDispatcher, r *DispatchRequest) { r.Runtime = "sprites" },
		"no-provider": func(_ *ProcessDispatcher, r *DispatchRequest) { r.Model = "deepseek" },
		"fuzzy":       func(_ *ProcessDispatcher, r *DispatchRequest) { r.Model = "openrouter/*flash*" },
		"thinking":    func(_ *ProcessDispatcher, r *DispatchRequest) { r.ReasoningLevel = "invalid" },
	}
	for _, args := range [][]string{{"--model", "other"}, {"--mode=print"}, {"--continue"}, {"--session", "other"}, {"--provider", "other"}, {"login"}, {"prompt"}, {"-e", "extension"}, {"--api-key", "fixture"}, {"--"}} {
		args := args
		tests[strings.Join(args, " ")] = func(d *ProcessDispatcher, _ *DispatchRequest) { d.providerConfigs.Pi.Args = args }
	}
	for name, change := range tests {
		t.Run(name, func(t *testing.T) {
			d, r, root := piFixture(t, "success")
			change(d, &r)
			session, err := d.Dispatch(t.Context(), r)
			if session != nil || err == nil {
				t.Fatalf("session=%v err=%v", session, err)
			}
			for _, path := range []string{d.runtimeDir, filepath.Join(root, "argv")} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("side effect %s: %v", path, err)
				}
			}
		})
	}
	if runtime.GOOS != "windows" {
		root := t.TempDir()
		d := NewSpritesDispatcher(SpritesDispatcherConfig{RuntimeDir: filepath.Join(root, "runtime")})
		d.newSprite = func(string) spriteHandle { t.Fatal("Pi acquired Sprite"); return nil }
		_, err := d.Dispatch(t.Context(), DispatchRequest{Name: "pi", Provider: "pi", Model: "openrouter/model", Prompt: "task", WorktreePath: root, Runtime: "sprites"})
		if err == nil {
			t.Fatal("Pi sprites accepted")
		}
	}
}

func waitPiFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("fake Pi did not write %s", path)
}

func TestPiCancellationAndExitOwnership(t *testing.T) {
	for _, mode := range []string{"ack-only", "agent-end-only", "hold-exit", "input-block", "no-read"} {
		t.Run(mode, func(t *testing.T) {
			d, req, root := piFixture(t, mode)
			if mode == "input-block" {
				req.Prompt = strings.Repeat("task", 512*1024)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			session, err := d.Dispatch(ctx, req)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "hold-exit" {
				waitPiFile(t, filepath.Join(root, "stdin-eof"))
			} else {
				waitPiFile(t, filepath.Join(root, "argv"))
			}
			// Even settled+EOF cannot release the resource while Pi is still alive.
			select {
			case <-session.Done():
				t.Fatal("completed before original exit")
			case <-time.After(100 * time.Millisecond):
			}
			cancel()
			waitHarnessSession(t, session)
			if out := session.Outcome(); out.Status != StatusCancelled {
				t.Fatalf("outcome=%+v", out)
			}
			if _, exited := session.(*processSession).process.ExitCode(); !exited {
				t.Fatal("released without process exit")
			}
		})
	}
}

func TestPiControlTimeout(t *testing.T) {
	for _, mode := range []string{"timeout", "no-ack"} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()
			d, req, root := piFixture(t, mode)
			session, err := d.Dispatch(t.Context(), req)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-session.Done():
			case <-time.After(15 * time.Second):
				_ = session.ForceKill()
				t.Fatal("control timeout deadlocked")
			}
			if out := session.Outcome(); out.Status != StatusFailed || !strings.Contains(out.Reason, "timed out") {
				t.Fatalf("outcome=%+v", out)
			}
			data, err := os.ReadFile(filepath.Join(root, "received"))
			if err != nil {
				t.Fatal(err)
			}
			if mode == "timeout" && strings.Contains(string(data), `"type":"prompt"`) {
				t.Fatalf("timeout sent prompt: %s", data)
			}
		})
	}
}

func TestPiOwnedGroupRetainsAdmission(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("owned group observation unavailable")
	}
	d, req, root := piFixture(t, "group")
	req.RequireGroupExit = true
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	session, err := d.Dispatch(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	waitPiFile(t, filepath.Join(root, "child-ready"))
	select {
	case <-session.Done():
		t.Fatal("released while owned descendant remains")
	case <-time.After(250 * time.Millisecond):
	}
	// The real launcher has already exited, but Done and quota still belong to
	// its original process group. SIGKILL must reclaim that group before Done.
	if err := session.ForceKill(); err != nil {
		t.Fatal(err)
	}
	waitHarnessSession(t, session)
	if out := session.Outcome(); out.Status != StatusKilled {
		t.Fatalf("outcome=%+v", out)
	}
}
