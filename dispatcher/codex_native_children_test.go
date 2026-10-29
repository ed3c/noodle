//go:build !windows

package dispatcher

import (
	"context"
	"encoding/json"
	"github.com/gorilla/websocket"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const nativeChildThread = "22222222-2222-4222-8222-222222222222"

func TestNativeCodexUnknownChildTurnBlocksReleaseWithoutArchive(t *testing.T) {
	var worktree string
	d, req := nativeDispatcherFixture(t, func(conn *websocket.Conn) {
		for {
			var call struct {
				ID     json.RawMessage
				Method string
				Params map[string]json.RawMessage
			}
			if conn.ReadJSON(&call) != nil {
				return
			}
			var result any
			switch call.Method {
			case "thread/start":
				result = map[string]any{"thread": map[string]string{"id": nativeTestThread, "source": "appServer"}}
			case "turn/start":
				conn.WriteJSON(map[string]any{"method": "turn/started", "params": map[string]any{"threadId": nativeTestThread, "turn": map[string]string{"id": "root-turn"}}})
				conn.WriteJSON(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": nativeTestThread, "turn": map[string]string{"id": "root-turn", "status": "completed"}}})
				result = map[string]any{"turn": map[string]string{"id": "root-turn"}}
			case "thread/turns/list":
				var id string
				json.Unmarshal(call.Params["threadId"], &id)
				if id == nativeChildThread {
					result = map[string]any{"data": []any{}}
				} else {
					result = map[string]any{"data": []map[string]string{{"id": "root-turn", "status": "completed"}}}
				}
			case "thread/read":
				result = map[string]any{"thread": map[string]any{"id": nativeChildThread, "status": map[string]string{"type": "notLoaded"}}}
			case "thread/list":
				var kinds []string
				json.Unmarshal(call.Params["sourceKinds"], &kinds)
				if len(kinds) != 5 {
					t.Error("descendants hidden by interactive filter")
					return
				}
				result = map[string]any{"data": []map[string]string{{"id": nativeChildThread, "cwd": worktree}}}
			default:
				t.Errorf("child dependency triggered unsupported effect: %s", call.Method)
				return
			}
			if conn.WriteJSON(map[string]any{"id": call.ID, "result": result}) != nil {
				return
			}
		}
	})
	worktree = req.WorktreePath
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := d.Dispatch(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	native := session.(*nativeCodexSession)
	defer native.client.close()
	finished := make(chan struct{})
	go func() { native.completion.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-ctx.Done():
		t.Fatal("child refusal not surfaced")
	}
	select {
	case <-session.Done():
		t.Fatal("unverified child returned quota")
	default:
	}
	var record NativeCodexRecord
	if err := readResumeJSON(filepath.Join(native.dir, "native.json"), &record); err != nil {
		t.Fatal(err)
	}
	if record.State != "needs_owner_readback" || !strings.Contains(record.Problem, "native child") || record.UnsubscribeIntent || record.ArchiveIntent {
		t.Fatalf("unsafe child release: %+v", record)
	}
}
