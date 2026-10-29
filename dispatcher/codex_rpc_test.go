//go:build !windows

package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func nativeRPCTestServer(t *testing.T, serve func(*websocket.Conn)) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "noodle-rpc-")
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, "s")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		os.RemoveAll(dir)
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, req, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var init map[string]json.RawMessage
		if conn.ReadJSON(&init) != nil {
			return
		}
		if conn.WriteJSON(map[string]any{"id": init["id"], "result": map[string]any{"userAgent": "native-test"}}) != nil {
			return
		}
		var initialized map[string]any
		if conn.ReadJSON(&initialized) != nil || initialized["method"] != "initialized" {
			return
		}
		serve(conn)
	})}
	go server.Serve(listener)
	t.Cleanup(func() { server.Close(); os.RemoveAll(dir) })
	return socket
}

func TestCodexRPCNativeResponseAndEvent(t *testing.T) {
	event := `{"method":"turn/completed","params":{"threadId":"original","turn":{"id":"round-1","status":"completed"}}}`
	socket := nativeRPCTestServer(t, func(conn *websocket.Conn) {
		var req map[string]json.RawMessage
		if conn.ReadJSON(&req) != nil {
			return
		}
		_ = conn.WriteMessage(websocket.TextMessage, []byte(event))
		_ = conn.WriteJSON(map[string]any{"id": req["id"], "result": map[string]any{"thread": map[string]string{"id": "original"}}})
		_, _, _ = conn.ReadMessage()
	})
	events := make(chan string, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, err := dialCodexRPC(ctx, socket, func(raw json.RawMessage) error { events <- string(raw); return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	var result struct{ Thread struct{ ID string } }
	if err := client.call(ctx, "thread/read", map[string]any{"threadId": "original"}, &result); err != nil {
		t.Fatal(err)
	}
	if result.Thread.ID != "original" {
		t.Fatalf("original response lost: %+v", result)
	}
	select {
	case actual := <-events:
		if actual != event {
			t.Fatalf("native event bytes changed: %s", actual)
		}
	case <-ctx.Done():
		t.Fatal("native event not delivered")
	}
}

func TestCodexRPCMutationDisconnectIsUnknownWithoutReplay(t *testing.T) {
	var calls atomic.Int32
	socket := nativeRPCTestServer(t, func(conn *websocket.Conn) {
		var req map[string]any
		if conn.ReadJSON(&req) == nil {
			calls.Add(1)
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, err := dialCodexRPC(ctx, socket, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	var result map[string]any
	err = client.call(ctx, "thread/archive", map[string]string{"threadId": "original"}, &result)
	if !errors.Is(err, errCodexRPCUnknown) || calls.Load() != 1 {
		t.Fatalf("unknown effect must not replay: error=%v calls=%d", err, calls.Load())
	}
}

func TestCodexRPCExplicitRefusalRemainsDistinct(t *testing.T) {
	socket := nativeRPCTestServer(t, func(conn *websocket.Conn) {
		var req map[string]json.RawMessage
		if conn.ReadJSON(&req) == nil {
			_ = conn.WriteJSON(map[string]any{"id": req["id"], "error": map[string]any{"code": -32000, "message": "wrong original backend"}})
			_, _, _ = conn.ReadMessage()
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, err := dialCodexRPC(ctx, socket, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	var result any
	err = client.call(ctx, "thread/archive", map[string]string{"threadId": "original"}, &result)
	if err == nil || errors.Is(err, errCodexRPCUnknown) || !strings.Contains(err.Error(), "wrong original backend") {
		t.Fatalf("original native refusal lost: %v", err)
	}
}

func TestCodexRPCNativeRequestRetainedWithoutDecision(t *testing.T) {
	observed := make(chan struct{}, 1)
	session := &nativeCodexSession{dir: t.TempDir(), record: NativeCodexRecord{SessionID: "approval", ThreadID: nativeTestThread, State: "running"}}
	socket := nativeRPCTestServer(t, func(conn *websocket.Conn) {
		_ = conn.WriteJSON(map[string]any{"id": "approval-1", "method": "item/commandExecution/requestApproval", "params": map[string]string{"threadId": nativeTestThread}})
		for {
			var call struct {
				ID     json.RawMessage
				Method string
			}
			if conn.ReadJSON(&call) != nil {
				return
			}
			if call.Method != "thread/read" {
				t.Errorf("pending approval produced decision or mutation: %+v", call)
				return
			}
			_ = conn.WriteJSON(map[string]any{"method": "serverRequest/resolved", "params": map[string]string{"threadId": nativeTestThread, "requestId": "approval-1"}})
			_ = conn.WriteJSON(map[string]any{"id": call.ID, "result": map[string]any{}})
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, err := dialCodexRPC(ctx, socket, func(raw json.RawMessage) error {
		if err := session.observe(raw); err != nil {
			return err
		}
		select {
		case observed <- struct{}{}:
		default:
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	select {
	case <-observed:
	case <-ctx.Done():
		t.Fatal("request not persisted")
	}
	var record NativeCodexRecord
	if err := readResumeJSON(filepath.Join(session.dir, "native.json"), &record); err != nil {
		t.Fatal(err)
	}
	if record.State != "native_request_pending" || len(record.ServerRequests[`"approval-1"`].Request) == 0 {
		t.Fatalf("request missing: %+v", record)
	}
	for _, method := range []string{"turn/start", "thread/resume", "thread/unsubscribe", "config/mcpServer/reload"} {
		var out any
		if client.call(ctx, method, map[string]string{"threadId": nativeTestThread}, &out) == nil {
			t.Fatalf("pending approval admitted %s", method)
		}
	}
	var out any
	if err := client.call(ctx, "thread/read", map[string]string{"threadId": nativeTestThread}, &out); err != nil {
		t.Fatal(err)
	}
	client.mu.Lock()
	pending := len(client.serverRequests)
	client.mu.Unlock()
	if pending != 0 {
		t.Fatal("native resolution not consumed")
	}
	if err := readResumeJSON(filepath.Join(session.dir, "native.json"), &record); err != nil {
		t.Fatal(err)
	}
	if len(record.ServerRequests[`"approval-1"`].Resolved) == 0 {
		t.Fatal("resolution receipt missing")
	}
}

func TestCodexRPCRefusesUnselectedEndpoint(t *testing.T) {
	for _, socket := range []string{"", "relative.sock", "/tmp/a/../b", "ws://localhost:9999"} {
		if _, err := dialCodexRPC(context.Background(), socket, nil); err == nil {
			t.Fatalf("unselected endpoint accepted: %q", socket)
		}
	}
}
