//go:build !windows

package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestNativeCodexUnknownRetainsSubscriptionAndQuota(t *testing.T) {
	socket := nativeRPCTestServer(t, func(conn *websocket.Conn) {
		for {
			var request struct {
				ID     json.RawMessage
				Method string
			}
			if conn.ReadJSON(&request) != nil {
				return
			}
			if request.Method != "thread/read" {
				t.Errorf("unknown caused effect: %s", request.Method)
				return
			}
			if conn.WriteJSON(map[string]any{"id": request.ID, "result": map[string]string{"status": "active"}}) != nil {
				return
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, err := dialCodexRPC(ctx, socket, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer client.close()
	s := &nativeCodexSession{sessionBase: newSessionBase(sessionBaseConfig{id: "original"}),
		dir: t.TempDir(), client: client, record: NativeCodexRecord{Schema: 1, SessionID: "original", State: "running", ThreadID: nativeTestThread}}
	s.unknown(fmt.Errorf("original mutation response missing"))
	var observed struct{ Status string }
	if err := client.call(ctx, "thread/read", map[string]string{"threadId": nativeTestThread}, &observed); err != nil {
		t.Fatalf("unknown implicitly disconnected original subscription: %v", err)
	}
	if observed.Status != "active" {
		t.Fatalf("owner readback unavailable: %+v", observed)
	}
	select {
	case <-s.Done():
		t.Fatal("unknown returned quota")
	default:
	}
	var record NativeCodexRecord
	if err := readResumeJSON(filepath.Join(s.dir, "native.json"), &record); err != nil {
		t.Fatal(err)
	}
	if record.State != "needs_owner_readback" || record.Problem == "" {
		t.Fatalf("unknown evidence missing: %+v", record)
	}
}

func TestCodexRPCEventPersistenceFailureDoesNotUnsubscribe(t *testing.T) {
	disconnected := make(chan struct{})
	socket := nativeRPCTestServer(t, func(conn *websocket.Conn) {
		_ = conn.WriteJSON(map[string]any{"method": "turn/completed", "params": map[string]string{"threadId": nativeTestThread}})
		_, _, _ = conn.ReadMessage()
		close(disconnected)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, err := dialCodexRPC(ctx, socket, func(json.RawMessage) error { return fmt.Errorf("evidence storage unavailable") })
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-client.done:
	case <-ctx.Done():
		t.Fatal("persistence failure not surfaced")
	}
	var result any
	if err := client.call(ctx, "thread/read", map[string]string{"threadId": nativeTestThread}, &result); err == nil {
		t.Fatal("failed evidence consumer allowed more calls")
	}
	select {
	case <-disconnected:
		t.Fatal("local persistence error dropped native subscription")
	case <-time.After(50 * time.Millisecond):
	}
	client.close()
	select {
	case <-disconnected:
	case <-ctx.Done():
		t.Fatal("explicit owner close did not reach transport")
	}
}

func TestNativeRequestResolutionCannotClearAnotherRequest(t *testing.T) {
	s := &nativeCodexSession{dir: t.TempDir(), record: NativeCodexRecord{SessionID: "requests", ThreadID: nativeTestThread, State: "running"}}
	for _, id := range []int{0, 1} {
		raw, _ := json.Marshal(map[string]any{"id": id, "method": "item/fileChange/requestApproval", "params": map[string]string{"threadId": nativeTestThread}})
		if err := s.observe(raw); err != nil {
			t.Fatal(err)
		}
	}
	for _, params := range []map[string]any{
		{"requestId": 0, "threadId": "another-thread"},
		{"requestId": "0", "threadId": nativeTestThread},
		{"requestId": 9, "threadId": nativeTestThread},
	} {
		raw, _ := json.Marshal(map[string]any{"method": "serverRequest/resolved", "params": params})
		if s.observe(raw) == nil {
			t.Fatalf("conflicting resolution accepted: %v", params)
		}
	}
	raw, _ := json.Marshal(map[string]any{"method": "serverRequest/resolved", "params": map[string]any{"requestId": 0, "threadId": nativeTestThread}})
	if err := s.observe(raw); err != nil {
		t.Fatal(err)
	}
	if s.record.State != "native_request_pending" || len(s.record.ServerRequests["1"].Resolved) != 0 {
		t.Fatal("one resolution cleared another pending request")
	}
	raw, _ = json.Marshal(map[string]any{"method": "serverRequest/resolved", "params": map[string]any{"requestId": 1, "threadId": nativeTestThread}})
	if err := s.observe(raw); err != nil {
		t.Fatal(err)
	}
	if s.record.State != "running" || s.record.Problem != "" || len(s.record.ServerRequests) != 2 {
		t.Fatal("resolved state lost history or retained stale blocker")
	}
}
