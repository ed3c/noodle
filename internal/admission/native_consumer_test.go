package admission

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/poteto/noodle/internal/procx"
)

func TestNativeConsumerPeerHelper(t *testing.T) {
	socket := os.Getenv("NOODLE_NATIVE_PEER_TEST")
	if socket == "" {
		return
	}
	c, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err = c.Write([]byte("ready")); err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(io.Discard, c)
	if err != nil {
		t.Fatal(err)
	}
}

func TestNativeConsumerUsesKernelPeerAndOriginalRequest(t *testing.T) {
	parent, err := procx.ObserveNativeIdentity(os.Getpid())
	if err != nil {
		t.Skipf("native generation unavailable: %v", err)
	}
	dir, err := os.MkdirTemp("", "n-peer-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "peer.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestNativeConsumerPeerHelper$")
	cmd.Env = append(os.Environ(), "NOODLE_NATIVE_PEER_TEST="+socket)
	if err = cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		err := cmd.Wait()
		cancel()
		if err != nil {
			t.Error("native peer helper exit:", err)
		}
	})
	if err = listener.(*net.UnixListener).SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	conn, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err = conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err = io.ReadFull(conn, make([]byte, 5)); err != nil {
		t.Fatal(err)
	}
	peer, err := procx.PeerPID(conn)
	if err != nil || peer != cmd.Process.Pid {
		t.Fatalf("kernel peer: %d %v", peer, err)
	}
	project, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "task")
	if err = os.MkdirAll(filepath.Join(output, "admission"), 0700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(map[string]any{"repository": project, "output": output})
	request := filepath.Join(dir, "request.json")
	if err = os.WriteFile(request, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(output, "admission", "host-request.json"), []byte(`{"request_id":"original"}`), 0600); err != nil {
		t.Fatal(err)
	}
	registry := map[string]any{
		"mcp_connections": map[string]any{"native-instance": map[string]any{"session_id": "owner", "state": "connected", "native_worktree": map[string]any{"thread_id": "owner", "root": project, "source": "original_codex_app"}, "process": map[string]any{"pid": parent.PID, "started": fixtureStarted(parent)}}},
		"bindings":        map[string]any{"task": map[string]any{"session_id": "owner", "request": SourceReference{Path: request, SHA256: sourceDigest(raw)}}},
	}
	path := filepath.Join(dir, "registry.json")
	data, _ := json.Marshal(registry)
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	c, err := ResolveNativeConsumer(ctx, path, peer, "original", "context", project)
	if err != nil || !c.verified || c.Connection != "native-instance" || c.SessionID != "owner" || c.Process.PID != parent.PID {
		t.Fatalf("original producer lost: %+v %v", c, err)
	}
	if _, err = ResolveNativeConsumer(ctx, path, peer, "foreign-request", "context", project); err == nil {
		t.Fatal("borrowed original request")
	}
	callerTree := filepath.Join(dir, "actual-native-worktree")
	if err = os.Mkdir(callerTree, 0700); err != nil {
		t.Fatal(err)
	}
	callerTree, err = filepath.EvalSymlinks(callerTree)
	if err != nil {
		t.Fatal(err)
	}
	connections := registry["mcp_connections"].(map[string]any)
	row := connections["native-instance"].(map[string]any)
	row["native_worktree"] = map[string]any{"thread_id": "owner", "root": callerTree, "source": "original_codex_app"}
	writeRegistry := func() {
		t.Helper()
		data, err := json.Marshal(registry)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeRegistry()
	c, err = ResolveNativeConsumer(ctx, path, peer, "original", "context", project)
	if err != nil || c.Worktree.Root != callerTree {
		t.Fatalf("request directory substituted for native cwd: %+v %v", c, err)
	}
	binding := map[string]any{"session_id": "parent", "turn_id": "frozen", "cwd": dir, "request": SourceReference{Path: request, SHA256: sourceDigest(raw)}}
	registry["bindings"] = map[string]any{"task": binding}
	writeRegistry()
	if _, err = ResolveNativeConsumer(ctx, path, peer, "original", "context", project); err == nil {
		t.Fatal("unregistered independent thread borrowed parent request")
	}
	authorization := map[string]any{"parent_binding": binding, "request": binding["request"], "target_thread": "owner", "worktree": callerTree, "state": "authorized"}
	registry["consumer_authorizations"] = map[string]any{"membership": authorization}
	writeRegistry()
	c, err = ResolveNativeConsumer(ctx, path, peer, "original", "context", project)
	if err != nil || c.SessionID != "owner" || c.Worktree.Root != callerTree {
		t.Fatalf("authorized native consumer rejected: %+v %v", c, err)
	}
	authorization["worktree"] = project
	writeRegistry()
	if _, err = ResolveNativeConsumer(ctx, path, peer, "original", "context", project); err == nil {
		t.Fatal("membership accepted another worktree")
	}
	authorization["worktree"] = callerTree
	authorization["parent_binding"] = map[string]any{"session_id": "parent", "turn_id": "changed", "cwd": dir, "request": binding["request"]}
	writeRegistry()
	if _, err = ResolveNativeConsumer(ctx, path, peer, "original", "context", project); err == nil {
		t.Fatal("stale original binding authorized consumer")
	}
	authorization["parent_binding"] = binding
	writeRegistry()
	if err = os.WriteFile(request, []byte(`{"repository":"changed"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = ResolveNativeConsumer(ctx, path, peer, "original", "context", project); err == nil {
		t.Fatal("stale request bytes accepted")
	}
}
