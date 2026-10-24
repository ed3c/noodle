package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
)

func inspectCommand(t *testing.T, project string, args ...string) []byte {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := os.Chdir(cwd); err != nil {
			t.Error(err)
		}
	}()
	root := NewRootCmd()
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs(append([]string{"--project-dir", project}, args...))
	if err := root.Execute(); err != nil {
		t.Fatalf("%v: %v\n%s", args, err, output.String())
	}
	return output.Bytes()
}

func inspectionSnapshot(t *testing.T, project, order string) {
	t.Helper()
	dir := filepath.Join(project, ".noodle")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	snapshot := reducer.DurableSnapshot{State: state.State{SchemaVersion: 1, Orders: map[string]state.OrderNode{order: {OrderID: order, Status: state.OrderActive}}}}
	if err := reducer.WriteSnapshotAtomic(filepath.Join(dir, "state.snapshot.json"), snapshot); err != nil {
		t.Fatal(err)
	}
}

func TestHostWorkspaceDiscoveryListsAllAndDoesNotSelectOrWrite(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	git("init", "-q")
	git("-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-qm", "fixture")
	linked := filepath.Join(root, "linked")
	git("worktree", "add", "--detach", linked, "HEAD")
	inspectionSnapshot(t, root, "root-order")
	inspectionSnapshot(t, linked, "linked-order")
	if err := os.WriteFile(filepath.Join(root, ".noodle.toml"), []byte("broken [ config"), 0600); err != nil {
		t.Fatal(err)
	}
	host := t.TempDir()
	t.Setenv("NOODLE_HOST_DIR", host)
	receipts := []hostReceipt{
		{Schema: 1, Protocol: hostProtocol, RequestID: "old-request", ConfigSHA256: "config", BinarySHA256: "binary", Mode: "manual", ProjectDir: root, ContextID: "old", HostInstanceID: "host", ContextState: "detached", HostDir: host},
		{Schema: 1, Protocol: hostProtocol, RequestID: "linked-request", ConfigSHA256: "config", BinarySHA256: "binary", Mode: "manual", ProjectDir: linked, ContextID: "linked", HostInstanceID: "host", ContextState: "failed", HostDir: host},
		{Schema: 1, Protocol: hostProtocol, RequestID: "new-request", ConfigSHA256: "config", BinarySHA256: "binary", Mode: "manual", ProjectDir: root, ContextID: "new", HostInstanceID: "host", ContextState: "active", HostDir: host},
	}
	before := writeViewReceipts(t, host, receipts)
	var got hostWorkspaceInspection
	if err := json.Unmarshal(inspectCommand(t, root, "host", "status"), &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != "observed" || len(got.Contexts) != 3 || len(got.Workspaces) != 2 || len(got.Selections) != 3 {
		t.Fatalf("lost candidates: %+v", got)
	}
	if len(got.Selections[1].ReadbackArgv) < 3 || got.Selections[1].ReadbackArgv[2] != linked {
		t.Fatal("cross-workspace selector omitted selected project")
	}
	selected, _, err := readSelectedHostReceipt(hostRequest{ProjectDir: linked, ContextID: "linked", InstanceID: "host"})
	if err != nil || selected.ProjectDir != linked {
		t.Fatalf("selected workspace cannot resume: %+v %v", selected, err)
	}
	if _, _, err := readSelectedHostReceipt(hostRequest{ProjectDir: root}); err == nil {
		t.Fatal("auto-selected a context")
	}
	// Reenter through the linked workspace: the primary checkout is still discoverable.
	fromLinked, err := inspectHostWorkspace(hostRequest{ProjectDir: linked})
	if err != nil || len(fromLinked.Workspaces) != 2 {
		t.Fatalf("lost primary checkout: %+v %v", fromLinked, err)
	}
	after, _ := os.ReadFile(filepath.Join(host, "receipts.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("inspection changed receipts")
	}
	for _, name := range []string{"host.sock", "host.lock"} {
		if _, err := os.Stat(filepath.Join(host, name)); !os.IsNotExist(err) {
			t.Fatal("inspection touched Host lifecycle")
		}
	}
}

func TestWorkspaceReadbackSurvivesMissingHostAndActiveLock(t *testing.T) {
	root := t.TempDir()
	inspectionSnapshot(t, root, "visible-order")
	host := filepath.Join(t.TempDir(), "absent-host")
	t.Setenv("NOODLE_HOST_DIR", host)
	lock, err := lockfile.TryLock(filepath.Join(root, ".noodle", "noodle.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := os.WriteFile(filepath.Join(root, ".noodle.toml"), []byte("broken [ config"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"host", "status"}, {"host", "views", "inspect", "--include-workspace"}, {"review", "inspect"}, {"review", "inspect", "visible-order", "example/repo#1"}} {
		out := inspectCommand(t, root, args...)
		if !bytes.Contains(out, []byte("visible-order")) {
			t.Fatalf("facts blocked: %v %s", args, out)
		}
	}
	if _, err := os.Stat(host); !os.IsNotExist(err) {
		t.Fatal("read created Host directory")
	}
}

func TestHistoricalRequestSelectorIsReadOnlyAcrossBinaryAndSession(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	host := t.TempDir()
	t.Setenv("NOODLE_HOST_DIR", host)
	request := hostRequestBinding{Schema: 1, RequestID: "saved-request", Operation: "attach", ProjectDir: root, HostDir: host, BinarySHA256: "unavailable-old-binary", ConfigSHA256: "config", Mode: "manual"}
	path := filepath.Join(t.TempDir(), "request.json")
	if err := saveHostRequest(path, request); err != nil {
		t.Fatal(err)
	}
	receipt := hostReceipt{ProjectDir: root, HostDir: host, RequestID: request.RequestID, ContextID: "native-context", HostInstanceID: "native-host", BinarySHA256: request.BinarySHA256, ConfigSHA256: request.ConfigSHA256, Mode: request.Mode, ContextState: "detached"}
	writeViewReceipts(t, host, []hostReceipt{receipt})
	before, _ := os.ReadFile(path)
	got := callHost(context.Background(), hostRequest{Operation: "status", ProjectDir: root, RequestFile: path})
	if got.Status != "observed" || !reflect.DeepEqual(got.hostReceipt, receipt) {
		t.Fatalf("old source became actor gate: %+v", got)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("selector changed request")
	}
	if _, err := os.Stat(path + ".lock"); !os.IsNotExist(err) {
		t.Fatal("read locked original request")
	}
	if _, _, err := readSelectedHostReceipt(hostRequest{ProjectDir: root, ContextID: "native-context", InstanceID: "other-host"}); err == nil {
		t.Fatal("foreign target accepted")
	}
}

func TestWorkspaceDiscoveryReportsCorruptSourcesWithoutHidingCandidates(t *testing.T) {
	root := t.TempDir()
	inspectionSnapshot(t, root, "known-order")
	host := t.TempDir()
	t.Setenv("NOODLE_HOST_DIR", host)
	if err := os.WriteFile(filepath.Join(host, "receipts.json"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := inspectHostWorkspace(hostRequest{ProjectDir: root})
	if err != nil || len(got.Workspaces) != 1 || got.Workspaces[0].Snapshot == nil || len(got.Gaps) == 0 {
		t.Fatalf("corrupt receipts hid workspace: %+v %v", got, err)
	}
	if got.LiveVerified || got.ObservationScope != "persisted_only" {
		t.Fatal("persisted data asserted liveness")
	}
	if err := os.WriteFile(filepath.Join(root, ".noodle", "state.snapshot.json"), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err = inspectHostWorkspace(hostRequest{ProjectDir: root})
	if err != nil || got.Workspaces[0].Status != "unavailable" || got.Workspaces[0].Gaps[0].Code != "invalid" {
		t.Fatalf("corrupt snapshot was absent evidence: %+v %v", got, err)
	}
}

func TestOfflineResourceReadbackDoesNotNeedHostOrOriginalActor(t *testing.T) {
	root := t.TempDir()
	host := t.TempDir()
	t.Setenv("NOODLE_HOST_DIR", host)
	data := []byte(`{"retained":{"id":"retained","state":"running","pid":12345}}`)
	path := filepath.Join(host, "residents.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"host", "resources"}, {"host", "resources", "residents", "inspect"}} {
		out := inspectCommand(t, root, args...)
		if !bytes.Contains(out, []byte(`"id":"retained"`)) || !bytes.Contains(out, []byte(`"live_verified":false`)) {
			t.Fatalf("retained facts unavailable: %s", out)
		}
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(data, after) {
		t.Fatal("offline readback rewrote resident state")
	}
	for _, name := range []string{"host.lock", "host.sock"} {
		if _, err := os.Stat(filepath.Join(host, name)); !os.IsNotExist(err) {
			t.Fatal("inspection touched Host lifecycle")
		}
	}
}

func TestSelectedStatusUsesRetainedFactsWithoutContactingOldHost(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	host, err := os.MkdirTemp("", "n-status-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(host)
	host, err = filepath.EvalSymlinks(host)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("NOODLE_HOST_DIR", host)
	inspectionSnapshot(t, root, "retained-order")
	receipt := hostReceipt{Schema: 1, Protocol: hostProtocol, ProjectDir: root, HostDir: host, RequestID: "request", ContextID: "context", HostInstanceID: "old-host", BinarySHA256: "old-binary", ConfigSHA256: "config", Mode: "manual", ContextState: "active"}
	writeViewReceipts(t, host, []hostReceipt{receipt})
	// This socket represents a running incompatible Host. Pure status must not send bytes.
	listener, err := net.Listen("unix", filepath.Join(host, "host.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := listener.(*net.UnixListener).SetDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	out := inspectCommand(t, root, "host", "status", "--context-id", "context", "--instance-id", "old-host", "--persisted-only")
	var got hostSelectedStatus
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if got.ContextID != "context" || got.ContextState != "active" || got.Status != "observed" || got.LiveVerified || got.ObservationScope != "persisted_only" {
		t.Fatalf("old Host erased retained target: %s", out)
	}
	if len(got.WorkspaceObservation.Gaps) == 0 || !bytes.Contains(out, []byte("live_unverified")) {
		t.Fatal("unknown liveness was omitted")
	}
	conn, err := listener.Accept()
	if err == nil {
		conn.Close()
		t.Fatal("read-only status contacted a running Host")
	}
	if timeout, ok := err.(net.Error); !ok || !timeout.Timeout() {
		t.Fatalf("socket probe: %v", err)
	}
}

func TestSelectedStatusLiveObservationNeverHidesRetainedTarget(t *testing.T) {
	for _, scenario := range []string{"live", "binary_mismatch", "wrong_target"} {
		t.Run(scenario, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			host, err := os.MkdirTemp("", "n-live-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(host)
			host, err = filepath.EvalSymlinks(host)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("NOODLE_HOST_DIR", host)
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(binary)
			if err != nil {
				t.Fatal(err)
			}
			receipt := hostReceipt{Schema: 1, Protocol: hostProtocol, ProjectDir: root, HostDir: host, RequestID: "request", ContextID: "selected", HostInstanceID: "host", ConfigSHA256: "config", BinarySHA256: hostDigest(raw), Mode: "manual", ContextState: "active"}
			writeViewReceipts(t, host, []hostReceipt{receipt})
			listener, err := net.Listen("unix", filepath.Join(host, "host.sock"))
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			done := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					done <- err
					return
				}
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				var req hostRequest
				if err := json.NewDecoder(conn).Decode(&req); err != nil {
					done <- err
					return
				}
				reply := hostResponse{hostReceipt: receipt, Status: "ok"}
				switch scenario {
				case "binary_mismatch":
					reply = hostError("binary_mismatch", fmt.Errorf("old Host binary differs"))
				case "wrong_target":
					reply.ContextID = "foreign"
				}
				done <- json.NewEncoder(conn).Encode(reply)
			}()
			req := hostRequest{ProjectDir: root, ContextID: "selected", InstanceID: "host"}
			inventory, err := inspectHostWorkspace(req)
			if err != nil {
				t.Fatal(err)
			}
			got := observeSelectedHostStatus(context.Background(), req, inventory, false)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if got.ContextID != "selected" || got.ContextState != "active" {
				t.Fatalf("optional live read erased retained facts: %+v", got)
			}
			if scenario == "live" {
				if got.Status != "ok" || !got.LiveVerified || got.ObservationScope != "live_host_reply" {
					t.Fatalf("legal live preflight lost: %+v", got)
				}
			} else if got.Status != "observed" || got.LiveVerified || !bytes.Contains(mustJSON(t, got), []byte("live_unavailable")) {
				t.Fatalf("failed live read became eligibility: %+v", got)
			}
		})
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestResidentInspectionRetainsFactsAfterSelectorGapOrHostRefusal(t *testing.T) {
	for _, scenario := range []string{"missing_receipt", "missing_receipt_other_endpoint", "host_refused", "resource_refused"} {
		t.Run(scenario, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			host, err := os.MkdirTemp("", "n-resread-")
			if err != nil {
				t.Fatal(err)
			}
			defer os.RemoveAll(host)
			host, err = filepath.EvalSymlinks(host)
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("NOODLE_HOST_DIR", host)
			data := []byte(`{"retained":{"id":"retained","state":"running","pid":12345}}`)
			path := filepath.Join(host, "residents.json")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			var done chan error
			if scenario == "host_refused" || scenario == "resource_refused" {
				receipt := hostReceipt{Schema: 1, Protocol: hostProtocol, ProjectDir: root, HostDir: host, RequestID: "request", ContextID: "context", HostInstanceID: "old-host", ConfigSHA256: "config", BinarySHA256: "old-binary", Mode: "manual", ContextState: "active"}
				writeViewReceipts(t, host, []hostReceipt{receipt})
				listener, err := net.Listen("unix", filepath.Join(host, "host.sock"))
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				done = make(chan error, 1)
				go func() {
					conn, err := listener.Accept()
					if err != nil {
						done <- err
						return
					}
					defer conn.Close()
					_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
					var req hostRequest
					if err := json.NewDecoder(conn).Decode(&req); err != nil {
						done <- err
						return
					}
					done <- json.NewEncoder(conn).Encode(map[string]any{"status": "refused", "error": "prior Host instance unavailable"})
				}()
			}
			args := []string{"host", "resources", "residents", "inspect", "--context-id", "context", "--instance-id", "old-host"}
			if scenario == "resource_refused" {
				args = []string{"host", "resources"}
			}
			if scenario == "missing_receipt_other_endpoint" {
				t.Setenv("NOODLE_HOST_DIR", filepath.Join(t.TempDir(), "different-host"))
				requestPath := filepath.Join(root, "selector.json")
				if err := saveHostRequest(requestPath, hostRequestBinding{Schema: 1, RequestID: "retained", Operation: "attach", ProjectDir: root, HostDir: host}); err != nil {
					t.Fatal(err)
				}
				args = []string{"host", "resources", "residents", "inspect", "--request-file", requestPath}
			}
			out := inspectCommand(t, root, args...)
			if done != nil {
				if err := <-done; err != nil {
					t.Fatal(err)
				}
			}
			if !bytes.Contains(out, []byte(`"id":"retained"`)) || !bytes.Contains(out, []byte(`"live_verified":false`)) || !bytes.Contains(out, []byte("live_unavailable")) {
				t.Fatalf("read qualification erased retained facts: %s", out)
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(data, after) {
				t.Fatal("readback changed native records")
			}
		})
	}
}
