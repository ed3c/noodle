package loop

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/dispatcher"
	"github.com/poteto/noodle/internal/execution"
)

func TestPiFormalDefaultsDispatch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake Pi uses POSIX shell")
	}
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "agent"), 0700); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
printf '%s\n' "$@" > argv
while [ "$#" -gt 0 ]; do
 if [ "$1" = "--session-dir" ]; then shift; session_dir="$1"; fi
 shift
done
IFS= read -r state
printf '%s\n' "$state" > received
printf '{"type":"response","id":"noodle-state","command":"get_state","success":true,"data":{"model":{"provider":"openrouter","id":"deepseek/deepseek-v4.1-flash-1017"},"sessionId":"native-defaults","sessionFile":"%s/native.jsonl","isStreaming":false,"isCompacting":false,"pendingMessageCount":0}}\n' "$session_dir"
IFS= read -r prompt
printf '%s\n' "$prompt" >> received
printf '%s\n' '{"type":"response","id":"noodle-prompt","command":"prompt","success":true,"data":{"disposition":"started"}}' '{"type":"agent_start"}' '{"type":"agent_end","willRetry":false,"messages":[{"role":"assistant","content":[],"stopReason":"stop"}]}' '{"type":"agent_settled","aborted":false}'
cat > eof
`
	if err := os.WriteFile(filepath.Join(root, "agent", "pi"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Parse([]byte(`[routing.defaults]
provider="pi"
model="openrouter/deepseek/deepseek-v4.1-flash-1017"
[agents.pi]
path="agent"
require_typed_outcome=true
`))
	if err != nil {
		t.Fatal(err)
	}
	e, err := execution.New(root, []string{"PATH=" + os.Getenv("PATH")})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err = config.Bind(cfg, e)
	if err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(root, "runtime")
	deps := defaultDependencies(root, runDir, "noodle", cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, e)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	session, err := deps.Runtimes["process"].Dispatch(ctx, dispatcher.DispatchRequest{Name: "defaults", Provider: cfg.Routing.Defaults.Provider, Model: cfg.Routing.Defaults.Model, Prompt: "defaults task", WorktreePath: root, AllowPrimaryCheckout: true})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-session.Done():
	case <-ctx.Done():
		t.Fatal("default Pi dispatch stalled")
	}
	if out := session.Outcome(); out.Status != dispatcher.StatusCompleted {
		t.Fatalf("outcome=%+v", out)
	}
	args, err := os.ReadFile(filepath.Join(root, "argv"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "--mode\nrpc\n--provider\nopenrouter\n--model\ndeepseek/deepseek-v4.1-flash-1017\n") {
		t.Fatalf("argv=%s", args)
	}
	input, err := os.ReadFile(filepath.Join(root, "received"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(input), `"type":"get_state"`) || !strings.Contains(string(input), `"type":"prompt"`) {
		t.Fatalf("stdin=%s", input)
	}
	l := &Loop{config: cfg}
	cook := &cookHandle{}
	if !l.requiresTypedOutcome(cook) {
		t.Fatal("Pi default typed outcome not wired")
	}
	cook.stage.Provider = "pi"
	cfg.Routing.Defaults.Provider = "claude"
	l.config = cfg
	if !l.requiresTypedOutcome(cook) {
		t.Fatal("Pi stage typed outcome not wired")
	}
	cfg.Agents.Pi.RequireTypedOutcome = false
	l.config = cfg
	if l.requiresTypedOutcome(cook) {
		t.Fatal("Pi typed outcome did not honor false")
	}
	// Args must also reach the formal dispatcher, where they are refused before launch.
	cfg.Agents.Pi.Args = []string{"--continue"}
	deps = defaultDependencies(root, filepath.Join(root, "refused"), "noodle", cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, e)
	_, err = deps.Runtimes["process"].Dispatch(ctx, dispatcher.DispatchRequest{Name: "refused", Provider: "pi", Model: "openrouter/model", Prompt: "task", WorktreePath: root, AllowPrimaryCheckout: true})
	if err == nil || !strings.Contains(err.Error(), "extra arguments") {
		t.Fatalf("Pi config args not wired: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "refused", "sessions")); !os.IsNotExist(err) {
		t.Fatalf("Pi extras created session: %v", err)
	}
}
