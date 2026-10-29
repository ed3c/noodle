//go:build !windows

package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestNativeCodexForegroundOfflineCompletesAndResumes(t *testing.T) {
	binary := os.Getenv("NOODLE_NATIVE_OFFLINE_BINARY")
	if binary == "" {
		t.Skip("exact native binary not selected")
	}
	dir := t.TempDir()
	if selected := os.Getenv("NOODLE_NATIVE_OFFLINE_EVIDENCE"); selected != "" {
		var err error
		dir, err = os.MkdirTemp(selected, "foreground-dispatch-")
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("native evidence: %s", dir)
	childMode := os.Getenv("NOODLE_NATIVE_OFFLINE_CHILD") == "1"
	approvalMode := os.Getenv("NOODLE_NATIVE_OFFLINE_APPROVAL") == "1"
	backgroundMode := os.Getenv("NOODLE_NATIVE_OFFLINE_BACKGROUND") == "1"
	stopChild := os.Getenv("NOODLE_NATIVE_OFFLINE_STOP_CHILD") == "1"
	childActive := make(chan struct{}, 1)
	fixtureDone := make(chan struct{})
	var parentID string
	var parentBodies []string
	var childBodies []string
	var bodies []string
	var bodyMu sync.Mutex
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodyMu.Lock()
		bodies = append(bodies, string(body))
		if parentID == "" {
			parentID = r.Header.Get("thread-id")
		}
		isParent := r.Header.Get("thread-id") == parentID
		if isParent {
			parentBodies = append(parentBodies, string(body))
		} else {
			childBodies = append(childBodies, string(body))
		}
		spawn := childMode && isParent && len(parentBodies) == 1
		followup := childMode && isParent && len(parentBodies) == 3
		background := backgroundMode && isParent && len(parentBodies) == 1
		approval := approvalMode && isParent && len(parentBodies) == 1
		bodyMu.Unlock()
		if stopChild && !isParent {
			childActive <- struct{}{}
			select {
			case <-r.Context().Done():
			case <-fixtureDone:
			}
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"offline-response\"}}\n\n")
		if approval {
			event := map[string]any{"type": "response.output_item.done", "item": map[string]string{"type": "function_call", "call_id": "offline-approval", "name": "exec_command", "arguments": `{"cmd":"touch approval-must-not-exist","sandbox_permissions":"require_escalated","justification":"offline approval lifecycle test"}`}}
			raw, _ := json.Marshal(event)
			fmt.Fprintf(w, "data: %s\n\n", raw)
		} else if background {
			event := map[string]any{"type": "response.output_item.done", "item": map[string]string{"type": "function_call", "call_id": "offline-background", "name": "exec_command", "arguments": `{"cmd":"/bin/sleep 5","yield_time_ms":1,"max_output_tokens":50}`}}
			raw, _ := json.Marshal(event)
			fmt.Fprintf(w, "data: %s\n\n", raw)
		} else if spawn {
			event := map[string]any{"type": "response.output_item.done", "item": map[string]string{"type": "function_call", "call_id": "spawn-offline-child", "namespace": "collaboration", "name": "spawn_agent", "arguments": `{"task_name":"worker","fork_turns":"none","message":"OFFLINE_CHILD_HISTORY_SENTINEL"}`}}
			raw, _ := json.Marshal(event)
			fmt.Fprintf(w, "data: %s\n\n", raw)
		} else if followup {
			event := map[string]any{"type": "response.output_item.done", "item": map[string]string{"type": "function_call", "call_id": "followup-offline-child", "namespace": "collaboration", "name": "followup_task", "arguments": `{"target":"worker","message":"OFFLINE_CHILD_FOLLOWUP_SENTINEL"}`}}
			raw, _ := json.Marshal(event)
			fmt.Fprintf(w, "data: %s\n\n", raw)
		} else {
			fmt.Fprint(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"offline-message\",\"content\":[{\"type\":\"output_text\",\"text\":\"OFFLINE_NATIVE_DISPATCH_REPLY\"}]}}\n\n")
		}
		fmt.Fprint(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"offline-response\",\"usage\":{\"input_tokens\":0,\"output_tokens\":0,\"total_tokens\":0}}}\n\n")
	}))
	defer provider.Close()
	defer close(fixtureDone)
	home := filepath.Join(dir, "home")
	bin := filepath.Join(dir, "bin")
	for _, path := range []string{home, bin} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	profile := "(version 1)(allow default)(deny network*)(allow network-outbound (remote ip \"localhost:" + strings.TrimPrefix(provider.URL, "http://127.0.0.1:") + "\"))"
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
	wrapper := "#!/bin/sh\nexec /usr/bin/sandbox-exec -p " + quote(profile) + " " + quote(binary) + " \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "codex"), []byte(wrapper), 0700); err != nil {
		t.Fatal(err)
	}

	// Zero delay is confined to this explicit-release fixture's private home.
	config := fmt.Sprintf("thread_unload_delay_secs = 0\nmodel_provider = \"offline\"\nmodel = \"offline\"\napproval_policy = \"never\"\n[model_providers.offline]\nname = \"offline\"\nbase_url = %q\nwire_api = \"responses\"\n", provider.URL+"/v1")
	// The outer seatbelt profile already confines this disposable fixture.
	// macOS refuses a second nested sandbox for the background shell.
	if backgroundMode {
		config = "sandbox_mode = \"danger-full-access\"\n" + config
	}
	if approvalMode {
		config = strings.Replace(config, `approval_policy = "never"`, `approval_policy = "on-request"`, 1)
	}
	identityPath := filepath.Join(dir, "mcp-generations.txt")
	config += fmt.Sprintf("[mcp_servers.fixture]\ncommand = %q\nargs = [\"-test.run=^TestNativeMCPFixture$\"]\n[mcp_servers.fixture.env]\nNOODLE_TEST_MCP_IDENTITY = %q\n", os.Args[0], identityPath)
	if childMode {
		config += "[features]\nmulti_agent_v2 = true\n"
	}
	if err := os.WriteFile(filepath.Join(home, "config.toml"), []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	d := NewCodexNativeDispatcher(ProcessDispatcherConfig{ProjectDir: dir, RuntimeDir: filepath.Join(dir, ".noodle"), ProviderConfigs: ProviderConfigs{Codex: ProviderConfig{Path: bin}}})
	req := DispatchRequest{Name: "offline-native", Prompt: "SOODLES_NATIVE_FOREGROUND_HISTORY_SENTINEL", SystemPrompt: "offline fixture", Provider: "codex", Model: "offline", WorktreePath: dir, AllowPrimaryCheckout: true, Runtime: CodexNativeRuntime, TaskRole: "offline-role", TaskKey: "verify", EnvVars: map[string]string{"CODEX_HOME": home, "HOME": home, "NOODLE_ORDER_ID": "offline-original-order"}}
	var original string
	var originalChild string
	previousGenerations := 0
	rounds := 2
	if stopChild || backgroundMode || approvalMode {
		rounds = 1
	}
	for round := 0; round < rounds; round++ {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		session, err := d.Dispatch(ctx, req)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		if approvalMode {
			native := session.(*nativeCodexSession)
			for {
				native.nativeMu.Lock()
				waiting := native.record.State == "native_request_pending" && len(native.record.ServerRequests) == 1
				native.nativeMu.Unlock()
				if waiting {
					break
				}
				select {
				case <-time.After(10 * time.Millisecond):
				case <-session.Done():
					t.Fatal("approval silently completed")
				case <-ctx.Done():
					t.Fatal("approval never observed")
				}
			}
			select {
			case <-session.Done():
				t.Fatal("approval returned quota")
			default:
			}
			if err := session.Terminate(); err != nil {
				t.Fatal(err)
			}
		}
		if backgroundMode {
			native := session.(*nativeCodexSession)
			for {
				native.nativeMu.Lock()
				draining := native.record.State == "draining_background"
				intent := native.record.UnsubscribeIntent
				native.nativeMu.Unlock()
				if intent {
					t.Fatal("background work released before drain observation")
				}
				if draining {
					break
				}
				select {
				case <-time.After(10 * time.Millisecond):
				case <-session.Done():
					t.Fatal("background work returned quota early")
				case <-ctx.Done():
					t.Fatal("background drain not observed")
				}
			}
		}
		if stopChild {
			select {
			case <-childActive:
			case <-ctx.Done():
				t.Fatal("native child never started")
			}
			native := session.(*nativeCodexSession)
			for {
				native.nativeMu.Lock()
				terminal := nativeTerminal(native.record.Terminal)
				native.nativeMu.Unlock()
				if terminal {
					break
				}
				select {
				case <-time.After(10 * time.Millisecond):
				case <-ctx.Done():
					t.Fatal("parent terminal not observed")
				}
			}
			if err := session.Terminate(); err != nil {
				t.Fatal(err)
			}
		}
		select {
		case <-session.Done():
		case <-ctx.Done():
			native := session.(*nativeCodexSession)
			native.nativeMu.Lock()
			snapshot := native.record
			native.nativeMu.Unlock()
			t.Fatalf("native release incomplete: %+v", snapshot)
		}
		cancel()
		var record NativeCodexRecord
		if err := readResumeJSON(filepath.Join(dir, ".noodle", "sessions", session.ID(), "native.json"), &record); err != nil {
			t.Fatal(err)
		}
		if record.State != "released" || record.ServerExit == nil || *record.ServerExit != 0 || len(record.UnsubscribeACK) == 0 || record.ArchiveIntent {
			t.Fatalf("native release evidence incomplete: %+v", record)
		}
		if approvalMode {
			if _, err := os.Stat(filepath.Join(dir, "approval-must-not-exist")); !os.IsNotExist(err) {
				t.Fatalf("unapproved command effect observed: %v", err)
			}
			if record.Terminal != "interrupted" || len(record.ServerRequests) != 1 || record.Problem != "" {
				t.Fatalf("approval interruption missing: %+v", record)
			}
			for _, request := range record.ServerRequests {
				if len(request.Resolved) == 0 {
					t.Fatal("approval native resolution missing")
				}
			}
		}
		if round == 0 {
			original = record.ThreadID
		} else if record.ThreadID != original {
			t.Fatal("resume replaced original native thread")
		}
		generations, err := os.ReadFile(identityPath)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Fields(string(generations))
		expected := 1
		if childMode {
			expected = 2
		}
		if len(lines)-previousGenerations != expected {
			t.Fatalf("MCP generations=%v round=%d", lines, round)
		}
		for _, line := range lines[previousGenerations:] {
			pid, err := strconv.Atoi(line)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, child := range record.ObservedChildren {
				if child.PID == pid {
					found = true
				}
			}
			if !found {
				t.Fatalf("MCP generation missing from exit observation: %d", pid)
			}
		}
		previousGenerations = len(lines)
		if childMode {
			if len(record.Children) != 1 {
				t.Fatalf("native child binding missing: %+v", record.Children)
			}
			for childID, child := range record.Children {
				if round == 0 {
					originalChild = childID
				} else if childID != originalChild {
					t.Fatal("native child thread replaced on followup")
				}
				if stopChild && (child.Terminal != "interrupted" || !child.StopIntent || len(child.StopACK) == 0) {
					t.Fatalf("active child interruption unverified: %+v", child)
				}
				if !child.Released || len(child.UnsubscribeACK) == 0 || child.ArchiveIntent {
					t.Fatalf("native child release unverified: %+v", child)
				}
			}
		}
		req.ResumeSessionID = session.ID()
		if raw, err := json.Marshal(record); err == nil {
			t.Log(string(raw))
		}
	}
	bodyMu.Lock()
	defer bodyMu.Unlock()
	if approvalMode {
		if len(parentBodies) != 1 {
			t.Fatal("approval operation continued or replayed")
		}
		return
	}
	if backgroundMode {
		return
	}
	if stopChild {
		if len(childBodies) != 1 {
			t.Fatal("interrupted child work was replayed")
		}
		return
	}
	if childMode && (len(childBodies) != 2 || !strings.Contains(childBodies[1], "OFFLINE_CHILD_HISTORY_SENTINEL") || !strings.Contains(childBodies[1], "OFFLINE_CHILD_FOLLOWUP_SENTINEL") || !strings.Contains(childBodies[1], "OFFLINE_NATIVE_DISPATCH_REPLY")) {
		t.Fatal("native child cold followup lost history or repeated work")
	}

	if len(parentBodies) < 2 || !strings.Contains(parentBodies[len(parentBodies)-1], "OFFLINE_NATIVE_DISPATCH_REPLY") || strings.Count(parentBodies[len(parentBodies)-1], "SOODLES_NATIVE_FOREGROUND_HISTORY_SENTINEL") < 2 {
		t.Fatal("same thread resume did not carry original input and reply to the native provider")
	}
}
