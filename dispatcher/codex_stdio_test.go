//go:build !windows

package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCodexStdioEOFAndProcessExitRemainSeparate(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "codex")
	script := `#!/bin/sh
[ "$*" = app-server ] || exit 99
read -r init
printf '%s\n' '{"id":"1","result":{}}'
read -r initialized
read -r request
printf '%s\n' '{"id":"2","result":{"identity":"original"}}'
while read -r remaining; do :; done
printf '%s\n' 'native EOF observed' >&2
exit 7
`
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, process, drained, err := startCodexStdio(ctx, binary, dir, os.Environ(), &stderr, nil)
	if err != nil {
		t.Fatal(err)
	}
	var result struct{ Identity string }
	if err := client.call(ctx, "thread/read", map[string]string{"threadId": "original"}, &result); err != nil {
		t.Fatal(err)
	}
	if result.Identity != "original" {
		t.Fatalf("response lost: %+v", result)
	}
	if _, exited := process.ExitCode(); exited {
		t.Fatal("live transport reported exited")
	}
	client.close()
	select {
	case <-process.Done():
	case <-ctx.Done():
		t.Fatal("EOF did not reach child")
	}
	if err := <-drained; err != nil {
		t.Fatal(err)
	}
	if code, exited := process.ExitCode(); !exited || code != 7 {
		t.Fatalf("exit evidence lost: %d %v", code, exited)
	}
	if !strings.Contains(stderr.String(), "native EOF observed") {
		t.Fatal("stderr not preserved")
	}
}

func TestCodexStdioUnknownInitializationRetainsOriginalHandle(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "codex")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nread -r init\nexit 9\n"), 0700); err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, process, drained, err := startCodexStdio(ctx, binary, dir, os.Environ(), &stderr, func(json.RawMessage) error { return nil })
	if !errors.Is(err, errCodexRPCUnknown) || client == nil || process == nil {
		t.Fatalf("unknown custody discarded: client=%v process=%v err=%v", client, process, err)
	}
	client.close()
	select {
	case <-process.Done():
	case <-ctx.Done():
		t.Fatal("original exit unavailable")
	}
	if err := <-drained; err != nil {
		t.Fatal(err)
	}
	if code, exited := process.ExitCode(); !exited || code != 9 {
		t.Fatalf("original exit lost: %d %v", code, exited)
	}
}

func TestCodexStdioNativeOffline(t *testing.T) {
	binary := os.Getenv("NOODLE_NATIVE_OFFLINE_BINARY")
	if binary == "" {
		t.Skip("exact native binary not selected")
	}
	dir := t.TempDir()
	home := filepath.Join(dir, "home")
	if err := os.Mkdir(home, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(""), 0600); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=/usr/bin:/bin", "CODEX_HOME=" + home, "HOME=" + home, "TMPDIR=" + dir}
	var stderr strings.Builder
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, process, drained, err := startCodexStdio(ctx, binary, dir, env, &stderr, nil)
	if err != nil {
		t.Fatalf("native initialization: %v", err)
	}
	var result struct {
		Data []string `json:"data"`
	}
	if err := client.call(ctx, "thread/loaded/list", map[string]any{}, &result); err != nil {
		t.Fatal(err)
	}
	if result.Data == nil || len(result.Data) != 0 {
		t.Fatalf("isolated server has unexpected threads: %+v", result)
	}
	if err := client.beginDrain(); err != nil {
		t.Fatal(err)
	}
	var refused any
	if err := client.call(ctx, "thread/start", map[string]any{"cwd": dir}, &refused); err == nil || !strings.Contains(err.Error(), "not admitted") {
		t.Fatalf("new work admitted during native drain: %v", err)
	}
	if err := client.call(ctx, "thread/loaded/list", map[string]any{}, &result); err != nil || len(result.Data) != 0 {
		t.Fatalf("drain started work or blocked readback: %+v %v", result, err)
	}
	// Closing this owned input is the only shutdown action. No model turn,
	// archive, production connection, process signal, or retry is involved.
	if err := client.conn.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-process.Done():
	case <-ctx.Done():
		t.Fatal("native server did not exit after EOF")
	}
	select {
	case <-client.done:
	case <-ctx.Done():
		t.Fatal("native RPC reader did not drain")
	}
	if err := <-drained; err != nil {
		t.Fatal(err)
	}
	if code, exited := process.ExitCode(); !exited || code != 0 {
		t.Fatalf("native EOF exit: code=%d exited=%v stderr=%s", code, exited, stderr.String())
	}
}

func TestCodexDrainRefusesSharedPendingAndUnknown(t *testing.T) {
	shared := &codexRPC{}
	if err := shared.beginDrain(); err == nil {
		t.Fatal("shared ownership inferred")
	}
	owned := &codexRPC{conn: &codexStdio{}, pending: map[string]chan codexRPCReply{"original": make(chan codexRPCReply, 1)}}
	if err := owned.beginDrain(); err == nil {
		t.Fatal("pending work ignored")
	}
	delete(owned.pending, "original")
	owned.uncertain = true
	if err := owned.beginDrain(); err == nil {
		t.Fatal("lost response treated as completed")
	}
	owned.uncertain = false
	if err := owned.beginDrain(); err != nil {
		t.Fatal(err)
	}
	params := map[string]string{"threadId": nativeTestThread}
	if err := owned.allowDrainCall("thread/unsubscribe", params); err != nil {
		t.Fatal(err)
	}
	if err := owned.allowDrainCall("thread/unsubscribe", params); err == nil {
		t.Fatal("unsubscribe replay admitted")
	}
	for _, method := range []string{"turn/start", "thread/resume", "mcpServer/tool/call", "config/mcpServer/reload", "thread/archive"} {
		if err := owned.allowDrainCall(method, params); err == nil {
			t.Fatalf("effect admitted while draining: %s", method)
		}
	}
}
