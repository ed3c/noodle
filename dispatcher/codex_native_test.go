//go:build !windows

package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/poteto/noodle/internal/execution"
)

const nativeTestThread = "11111111-1111-4111-8111-111111111111"

func nativeDispatcherFixture(t *testing.T, server func(*websocket.Conn)) (*CodexNativeDispatcher, DispatchRequest) {
	t.Helper()
	socket := nativeRPCTestServer(t, server)
	project := t.TempDir()
	binary := filepath.Join(project, "codex")
	script := "#!/bin/sh\nexec '" + strings.ReplaceAll(os.Args[0], "'", "'\"'\"'") + "' -test.run=^TestNativeStdioBridge$\n"
	if err := os.WriteFile(binary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	bound, err := execution.New(project, append(os.Environ(), "NOODLE_TEST_BRIDGE_SOCKET="+socket, "CODEX_HOME="+filepath.Join(project, "home")))
	if err != nil {
		t.Fatal(err)
	}
	d := NewCodexNativeDispatcher(ProcessDispatcherConfig{Execution: bound, ProjectDir: project, RuntimeDir: filepath.Join(project, ".noodle"),
		ProviderConfigs: ProviderConfigs{Codex: ProviderConfig{Path: project}}})
	req := DispatchRequest{Name: "native-test", Prompt: "read only", SystemPrompt: "selected test instruction", Provider: "codex", Model: "test-model",
		WorktreePath: project, AllowPrimaryCheckout: true, Runtime: CodexNativeRuntime, TaskRole: "read-role", TaskKey: "verify", EnvVars: map[string]string{"NOODLE_ORDER_ID": "original-order"}}
	return d, req
}

func TestNativeCodexCompletesAndResumesSameThreadWithoutMCP(t *testing.T) {
	var starts, resumes, unsubscribes, rounds atomic.Int32
	d, req := nativeDispatcherFixture(t, func(conn *websocket.Conn) {
		loaded := false
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
			case "thread/start", "thread/resume":
				loaded = true
				if call.Method == "thread/start" {
					starts.Add(1)
				} else {
					resumes.Add(1)
				}
				result = map[string]any{"thread": map[string]string{"id": nativeTestThread, "source": "appServer"}}
			case "turn/start":
				round := fmt.Sprintf("round-%d", rounds.Add(1))
				_ = conn.WriteJSON(map[string]any{"method": "turn/started", "params": map[string]any{"threadId": nativeTestThread, "turn": map[string]string{"id": round}}})
				_ = conn.WriteJSON(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": nativeTestThread, "turn": map[string]string{"id": round, "status": "completed"}}})
				result = map[string]any{"turn": map[string]string{"id": round}}
			case "thread/turns/list":
				result = map[string]any{"data": []map[string]string{{"id": fmt.Sprintf("round-%d", rounds.Load()), "status": "completed"}}}
			case "thread/list", "thread/backgroundTerminals/list", "mcpServerStatus/list":
				result = map[string]any{"data": []any{}}
			case "thread/read":
				result = map[string]any{"thread": map[string]any{"id": nativeTestThread, "status": map[string]string{"type": "notLoaded"}}}
			case "thread/unsubscribe":
				loaded = false
				unsubscribes.Add(1)
				result = map[string]any{"status": "unsubscribed"}
			case "thread/loaded/list":
				rows := []string{}
				if loaded {
					rows = append(rows, nativeTestThread)
				}
				result = map[string]any{"data": rows}
			default:
				_ = conn.WriteJSON(map[string]any{"id": call.ID, "error": map[string]any{"code": -32601, "message": "unexpected method"}})
				continue
			}
			if conn.WriteJSON(map[string]any{"id": call.ID, "result": result}) != nil {
				return
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	first, err := d.Dispatch(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-first.Done():
	case <-ctx.Done():
		t.Fatal("normal native release did not finish")
	}
	if first.Outcome().Status != StatusCompleted {
		t.Fatalf("native outcome: %+v", first.Outcome())
	}
	req.ResumeSessionID = first.ID()
	second, err := d.Dispatch(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-second.Done():
	case <-ctx.Done():
		t.Fatal("resumed native release did not finish")
	}
	if starts.Load() != 1 || resumes.Load() != 1 || unsubscribes.Load() != 2 || rounds.Load() != 2 {
		t.Fatalf("native work repeated or replaced: start=%d resume=%d archive=%d round=%d", starts.Load(), resumes.Load(), unsubscribes.Load(), rounds.Load())
	}
	for _, session := range []Session{first, second} {
		path := filepath.Join(d.prepare.runtimeDir, "sessions", session.ID())
		var record NativeCodexRecord
		if err := readResumeJSON(filepath.Join(path, "native.json"), &record); err != nil {
			t.Fatal(err)
		}
		if record.State != "released" || record.ThreadID != nativeTestThread || len(record.UnsubscribeACK) == 0 {
			t.Fatalf("native effect readback lost: %+v", record)
		}
		var unloaded struct {
			Data       []string
			NextCursor *string
		}
		if err := json.Unmarshal(record.UnloadReadback, &unloaded); err != nil || unloaded.Data == nil || len(unloaded.Data) != 0 || unloaded.NextCursor != nil {
			t.Fatalf("native unload readback missing or incomplete: %s (%v)", record.UnloadReadback, err)
		}
		if record.ServerExit == nil || *record.ServerExit != 0 {
			t.Fatal("released without original foreground exit")
		}
		raw, err := os.ReadFile(filepath.Join(path, "raw.ndjson"))
		if err != nil || !strings.Contains(string(raw), `"method":"turn/completed"`) {
			t.Fatalf("original native events missing: %s %v", raw, err)
		}
	}
}

func TestNativeCodexUnsubscribeACKDoesNotReleaseLoadedThread(t *testing.T) {
	read := make(chan struct{}, 1)
	d, req := nativeDispatcherFixture(t, func(conn *websocket.Conn) {
		for {
			var call struct {
				ID     json.RawMessage
				Method string
			}
			if conn.ReadJSON(&call) != nil {
				return
			}
			var result any
			switch call.Method {
			case "thread/start":
				result = map[string]any{"thread": map[string]string{"id": nativeTestThread, "source": "appServer"}}
			case "turn/start":
				_ = conn.WriteJSON(map[string]any{"method": "turn/started", "params": map[string]any{"threadId": nativeTestThread, "turn": map[string]string{"id": "round-1"}}})
				_ = conn.WriteJSON(map[string]any{"method": "turn/completed", "params": map[string]any{"threadId": nativeTestThread, "turn": map[string]string{"id": "round-1", "status": "completed"}}})
				result = map[string]any{"turn": map[string]string{"id": "round-1"}}
			case "thread/turns/list":
				result = map[string]any{"data": []map[string]string{{"id": "round-1", "status": "completed"}}}
			case "thread/list", "thread/backgroundTerminals/list", "mcpServerStatus/list":
				result = map[string]any{"data": []any{}}
			case "thread/unsubscribe":
				result = map[string]any{"status": "unsubscribed"}
			case "thread/loaded/list":
				result = map[string]any{"data": []string{nativeTestThread}}
				select {
				case read <- struct{}{}:
				default:
				}
			default:
				return
			}
			if conn.WriteJSON(map[string]any{"id": call.ID, "result": result}) != nil {
				return
			}
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	session, err := d.Dispatch(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		native := session.(*nativeCodexSession)
		native.client.close()
		native.completion.Wait()
	})
	select {
	case <-read:
	case <-ctx.Done():
		t.Fatal("no native post-ACK readback")
	}
	select {
	case <-session.Done():
		t.Fatal("unsubscribe ACK falsely released a loaded native thread")
	default:
	}
	native := session.(*nativeCodexSession)
	native.nativeMu.Lock()
	claimedUnload := len(native.record.UnloadReadback) != 0
	native.nativeMu.Unlock()
	if claimedUnload {
		t.Fatal("loaded thread acquired an unload receipt")
	}
	if err := session.ForceKill(); err == nil {
		t.Fatal("shared daemon acquired force-stop authority")
	}
}

func TestNativeCodexPreservesCarrierPolicyAndRefusesUnknownFlags(t *testing.T) {
	config, err := nativeCodexConfig([]string{"--sandbox", "workspace-write", "-c", `model_reasoning_effort="high"`, "-a", "on-request"})
	if err != nil || config["sandbox_mode"] != "workspace-write" || config["approval_policy"] != "on-request" || config["model_reasoning_effort"] != "high" {
		t.Fatalf("original carrier policy lost: %v %v", config, err)
	}
	if _, err := nativeCodexConfig([]string{"--dangerously-bypass-approvals-and-sandbox"}); err == nil {
		t.Fatal("unsupported policy silently widened")
	}
}

func TestNativeCodexUnknownInterruptIsNotReplayed(t *testing.T) {
	var stops atomic.Int32
	d, req := nativeDispatcherFixture(t, func(conn *websocket.Conn) {
		for {
			var call struct {
				ID     json.RawMessage
				Method string
			}
			if conn.ReadJSON(&call) != nil {
				return
			}
			var result any
			switch call.Method {
			case "thread/start":
				result = map[string]any{"thread": map[string]string{"id": nativeTestThread, "source": "appServer"}}
			case "turn/start":
				result = map[string]any{"turn": map[string]string{"id": "original-turn"}}
			case "turn/interrupt":
				stops.Add(1)
				_ = conn.Close()
				return
			default:
				t.Errorf("unknown interrupt replayed through %s", call.Method)
				return
			}
			if conn.WriteJSON(map[string]any{"id": call.ID, "result": result}) != nil {
				return
			}
		}
	})
	session, err := d.Dispatch(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	native := session.(*nativeCodexSession)
	t.Cleanup(func() { native.client.close(); native.completion.Wait() })
	if err := session.Terminate(); err == nil {
		t.Fatal("lost stop ACK treated as success")
	}
	if err := session.Terminate(); err == nil {
		t.Fatal("unknown stop effect did not block replay")
	}
	if stops.Load() != 1 {
		t.Fatalf("stop calls=%d", stops.Load())
	}
	select {
	case <-session.Done():
		t.Fatal("unknown effect returned quota")
	default:
	}
}

func TestNativeUnsubscribeRejectsUnknownACK(t *testing.T) {
	for _, raw := range []string{`{}`, `null`, `{"status":"released"}`, `{"status":true}`, `{`} {
		if err := validateNativeUnsubscribeACK(json.RawMessage(raw)); err == nil {
			t.Errorf("accepted unknown ACK %s", raw)
		}
	}
	for _, raw := range []string{`{"status":"unsubscribed"}`, `{"status":"notSubscribed"}`, `{"status":"notLoaded"}`} {
		if err := validateNativeUnsubscribeACK(json.RawMessage(raw)); err != nil {
			t.Errorf("rejected native ACK %s: %v", raw, err)
		}
	}
}

func TestNativeCodexStopRefusalRequiresOriginalTerminal(t *testing.T) {
	for _, status := range []string{"completed", "inProgress"} {
		t.Run(status, func(t *testing.T) {
			var stops atomic.Int32
			d, req := nativeDispatcherFixture(t, func(conn *websocket.Conn) {
				loaded := true
				for {
					var call struct {
						ID     json.RawMessage
						Method string
					}
					if conn.ReadJSON(&call) != nil {
						return
					}
					var result any
					switch call.Method {
					case "thread/start":
						result = map[string]any{"thread": map[string]string{"id": nativeTestThread, "source": "appServer"}}
					case "turn/start":
						result = map[string]any{"turn": map[string]string{"id": "original-turn"}}
					case "turn/interrupt":
						stops.Add(1)
						_ = conn.WriteJSON(map[string]any{"id": call.ID, "error": map[string]any{"code": -32600, "message": "no active turn to interrupt"}})
						continue
					case "thread/turns/list":
						result = map[string]any{"data": []map[string]string{{"id": "original-turn", "status": status}}}
					case "thread/list", "thread/backgroundTerminals/list":
						result = map[string]any{"data": []any{}}
					case "thread/read":
						result = map[string]any{"thread": map[string]any{"id": nativeTestThread, "status": map[string]string{"type": "notLoaded"}}}
					case "thread/loaded/list":
						rows := []string{}
						if loaded {
							rows = append(rows, nativeTestThread)
						}
						result = map[string]any{"data": rows}
					case "thread/unsubscribe":
						loaded = false
						result = map[string]string{"status": "unsubscribed"}
					default:
						t.Errorf("unexpected %s", call.Method)
						return
					}
					if conn.WriteJSON(map[string]any{"id": call.ID, "result": result}) != nil {
						return
					}
				}
			})
			session, err := d.Dispatch(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			native := session.(*nativeCodexSession)
			t.Cleanup(func() { native.client.close(); native.completion.Wait() })
			err = session.Terminate()
			if status == "completed" {
				if err != nil {
					t.Fatal(err)
				}
				select {
				case <-session.Done():
				case <-time.After(5 * time.Second):
					t.Fatal("terminal readback did not complete release")
				}
			} else {
				if err == nil {
					t.Fatal("nonterminal refusal accepted")
				}
				if session.Terminate() == nil {
					t.Fatal("refusal replay permitted")
				}
			}
			native.nativeMu.Lock()
			refusal, ack := len(native.record.StopRefusal), len(native.record.StopACK)
			native.nativeMu.Unlock()
			if refusal == 0 || ack != 0 || stops.Load() != 1 {
				t.Fatalf("refusal lost or converted to ACK: refusal=%d ack=%d stops=%d", refusal, ack, stops.Load())
			}
		})
	}
}
