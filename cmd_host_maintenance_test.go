package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
	"github.com/poteto/noodle/loop"
)

func TestLegacyModeCheckpointKeepsFirstACKAndRejectsFalseSuccess(t *testing.T) {
	for _, dispatch := range []loop.State{loop.StatePaused, loop.StateRunning, loop.StateDraining} {
		t.Run(string(dispatch), func(t *testing.T) {
			runtimeDir := t.TempDir()
			path := filepath.Join(t.TempDir(), "hold.json")
			ctx := context.Background()
			if err := os.WriteFile(filepath.Join(runtimeDir, "status.json"), []byte(`{"mode":"auto","effective_mode":"supervised","mode_override":"supervised"}`), 0600); err != nil {
				t.Fatal(err)
			}
			if round, err := legacyModeCheckpoint(ctx, runtimeDir, path); err != nil || round != nil {
				t.Fatal(round, err)
			}
			queued, err := os.ReadFile(filepath.Join(runtimeDir, "control.ndjson"))
			if err != nil {
				t.Fatal(err)
			}
			var cmd loop.ControlCommand
			if err = json.Unmarshal(queued, &cmd); err != nil {
				t.Fatal(err)
			}
			// Re-entry before ACK reads the original intent without another append.
			if _, err = legacyModeCheckpoint(ctx, runtimeDir, path); err != nil {
				t.Fatal(err)
			}
			after, _ := os.ReadFile(filepath.Join(runtimeDir, "control.ndjson"))
			if string(after) != string(queued) {
				t.Fatal("repeated a pending effect")
			}
			ack := loop.ControlAck{ID: cmd.ID, Action: "mode", Status: "ok", At: time.Now(), DispatchState: dispatch}
			raw, _ := json.Marshal(ack)
			if err = os.WriteFile(filepath.Join(runtimeDir, "control-ack.ndjson"), append(raw, '\n'), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err = legacyModeCheckpoint(ctx, runtimeDir, path); err == nil {
				t.Fatal("ACK without persisted snapshot accepted")
			}
			at := time.Now()
			snapshot := reducer.BuildSnapshot(state.State{Mode: state.RunModeManual, ModeEpoch: 7, ModeTransitions: []state.ModeTransitionRecord{{FromMode: state.RunModeSupervised, ToMode: state.RunModeManual, Epoch: 7, AppliedAt: at}}}, nil, at)
			if err = reducer.WriteSnapshotAtomic(filepath.Join(runtimeDir, "state.snapshot.json"), snapshot); err != nil {
				t.Fatal(err)
			}
			round, err := legacyModeCheckpoint(ctx, runtimeDir, path)
			if err != nil || round == nil || round.Mode != "supervised" || round.DispatchState != dispatch {
				t.Fatal(round, err)
			}
			// A second control makes this boundary ambiguous, even if mode is manual.
			foreign, _ := json.Marshal(loop.ControlAck{ID: "foreign", Action: "mode", Status: "ok"})
			if err = os.WriteFile(filepath.Join(runtimeDir, "control-ack.ndjson"), append(append(raw, '\n'), append(foreign, '\n')...), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err = legacyModeCheckpoint(ctx, runtimeDir, path); err == nil {
				t.Fatal("foreign control accepted")
			}
		})
	}
}

func TestLegacyCheckpointDoesNotInferMissingEffectiveMode(t *testing.T) {
	dir := t.TempDir()
	intent := filepath.Join(t.TempDir(), "hold.json")
	if _, err := legacyModeCheckpoint(context.Background(), dir, intent); err != nil {
		t.Fatal(err)
	}
	var original legacyModeIntent
	raw, _ := os.ReadFile(intent)
	if err := json.Unmarshal(raw, &original); err != nil {
		t.Fatal(err)
	}
	ack, _ := json.Marshal(loop.ControlAck{ID: original.Command.ID, Action: "mode", Status: "ok", DispatchState: loop.StatePaused})
	if err := os.WriteFile(filepath.Join(dir, "control-ack.ndjson"), append(ack, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	snapshot := reducer.BuildSnapshot(state.State{Mode: state.RunModeManual, ModeEpoch: 1, ModeTransitions: []state.ModeTransitionRecord{{FromMode: state.RunModeAuto, ToMode: state.RunModeManual, Epoch: 1, AppliedAt: at}}}, nil, at)
	if err := reducer.WriteSnapshotAtomic(filepath.Join(dir, "state.snapshot.json"), snapshot); err != nil {
		t.Fatal(err)
	}
	round, err := legacyModeCheckpoint(context.Background(), dir, intent)
	if err != nil || round == nil || round.Mode != "" || round.DispatchState != loop.StatePaused {
		t.Fatal(round, err)
	}
}

func TestMaintenanceGateFencesAdmissionAndForwardsOriginalReadback(t *testing.T) {
	// macOS Unix sockets have a short pathname limit.
	root, err := os.MkdirTemp("/tmp", "noodle-gate-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	source := filepath.Join(root, "old")
	if err = os.MkdirAll(source, 0700); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", filepath.Join(source, "host.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	seen := make(chan string, 2)
	go func() {
		for {
			c, e := l.Accept()
			if e != nil {
				return
			}
			func() {
				defer c.Close()
				var r hostRequest
				if json.NewDecoder(c).Decode(&r) != nil {
					return
				}
				seen <- r.Operation
				_ = json.NewEncoder(c).Encode(map[string]any{"status": "ok", "host_instance_id": "original"})
			}()
		}
	}()
	g, err := openHostMaintenanceGate(root, source)
	if err != nil {
		t.Fatal(err)
	}
	defer g.Close()
	if err = publishMaintenanceSocket(root); err != nil {
		t.Fatal(err)
	}
	for _, operation := range []string{"attach", "once", "detach", "cleanup", "resident_acquire_bound", "resident_release_bound", "resident_reconcile", "resident_unknown"} {
		raw, e := nativeHostRPC(context.Background(), root, hostRequest{Operation: operation})
		if e != nil {
			t.Fatal(e)
		}
		var r hostResponse
		_ = json.Unmarshal(raw, &r)
		if r.Status != "busy" {
			t.Fatalf("%s: %s", operation, raw)
		}
	}
	raw, err := nativeHostRPC(context.Background(), root, hostRequest{Operation: "resources"})
	if err != nil {
		t.Fatal(err)
	}
	var r map[string]any
	_ = json.Unmarshal(raw, &r)
	if r["host_instance_id"] != "original" {
		t.Fatal(r)
	}
	if <-seen != "resources" {
		t.Fatal("forwarded an admission")
	}
	g.freeze()
	raw, err = nativeHostRPC(context.Background(), root, hostRequest{Operation: "resident_acquire_bound"})
	if err != nil {
		t.Fatal(err)
	}
	var response hostResponse
	_ = json.Unmarshal(raw, &response)
	if response.Status != "busy" {
		t.Fatal(string(raw))
	}
	select {
	case operation := <-seen:
		t.Fatalf("forwarded after freeze: %s", operation)
	default:
	}
}

func TestMaintenanceMarkerKeepsVersionPublicationClosed(t *testing.T) {
	root := t.TempDir()
	raw := []byte(`{"phase":"stopping"}`)
	if err := os.WriteFile(filepath.Join(root, "bootstrap.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if response := maintenanceWaitResponse(root); response == nil || response.Status != "busy" {
		t.Fatal(response)
	}
	if err := publishPoolSocket(root, "/unselected"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(root, "host.sock")); !os.IsNotExist(err) {
		t.Fatal("published during maintenance", err)
	}
	if err := os.WriteFile(filepath.Join(root, "bootstrap.json"), []byte(`{"phase":"active"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if response := maintenanceWaitResponse(root); response != nil {
		t.Fatal(response)
	}
}

func TestMaintenanceWaitingAdmissionPreservesOriginalRequestAcrossVersions(t *testing.T) {
	root := t.TempDir()
	oldID, newID := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	b := hostBootstrap{Schema: 1, Phase: "draining", Target: "/new", TargetSHA: "new", ProcessIdentity: "original", Source: hostGeneration{ID: oldID, Directory: filepath.Join(root, "g", oldID), Binary: "/old", SHA: "old"}}
	if err := saveHostBootstrap(root, b); err != nil {
		t.Fatal(err)
	}
	req := hostTestRequest(t, t.TempDir())
	req.RequestID = "cccccccccccccccccccccccccccccccc"
	req.BinarySHA256 = "old"
	req.Mode = "manual"
	req.Environment = []string{"TASK=original"}
	req.HostDir = root
	req.Operation = "attach"
	g := &hostMaintenanceGate{root: root}
	if err := g.rememberAdmission(req); err != nil {
		t.Fatal(err)
	}
	requestFile := filepath.Join(t.TempDir(), "host-request.json")
	raw, err := os.ReadFile(filepath.Join(root, "bootstrap", "waiting", req.RequestID+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(requestFile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err = updateHostPool(root, func(p *hostPool) error {
		p.Active = newID
		p.Generations[oldID] = b.Source
		p.Generations[newID] = hostGeneration{ID: newID, Directory: filepath.Join(root, "g", newID), Binary: "/new", SHA: "new"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	req.RequestFile = requestFile
	req.BinarySHA256 = "new"
	req.Environment = []string{"TASK=current"}
	binding, unlock, err := bindHostRequest(&req)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	if binding.RequestID != "cccccccccccccccccccccccccccccccc" || binding.BinarySHA256 != "old" || req.AdmissionSHA != "old" || req.AdmissionPath != "/old" {
		t.Fatal("replaced original waiting admission", binding, req)
	}
}
