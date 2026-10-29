package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
	"github.com/poteto/noodle/loop"
)

func maintenanceTestCheckpoint(t *testing.T, dir, dispatch string, at time.Time) {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"effective_mode": "supervised", "loop_state": dispatch})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "status.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	snapshot := reducer.BuildSnapshot(state.State{Mode: state.RunModeSupervised}, nil, at)
	if err = reducer.WriteSnapshotAtomic(filepath.Join(dir, "state.snapshot.json"), snapshot); err != nil {
		t.Fatal(err)
	}
}

func TestMaintenancePauseRequiresEvidenceBeforeSending(t *testing.T) {
	for _, missing := range []string{"status.json", "state.snapshot.json"} {
		t.Run(missing, func(t *testing.T) {
			dir, custody := t.TempDir(), t.TempDir()
			maintenanceTestCheckpoint(t, dir, "running", time.Now())
			if err := os.Remove(filepath.Join(dir, missing)); err != nil {
				t.Fatal(err)
			}
			if held, err := legacyPauseCheckpoint(context.Background(), dir, filepath.Join(custody, "pause.json")); err == nil || held {
				t.Fatal("missing original evidence allowed pause", held, err)
			}
			for _, path := range []string{filepath.Join(custody, "pause.json"), filepath.Join(dir, "control.ndjson")} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatal("effect or intent created before evidence", path, err)
				}
			}
		})
	}
}

func TestMaintenancePauseConcurrentCallsAppendOnce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(t.TempDir(), "pause.json")
	maintenanceTestCheckpoint(t, dir, "running", time.Now())
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			held, err := legacyPauseCheckpoint(context.Background(), dir, path)
			if err != nil || held {
				t.Error(held, err)
			}
		}()
	}
	wg.Wait()
	queued, err := os.ReadFile(filepath.Join(dir, "control.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Count(queued, []byte{'\n'}) != 1 {
		t.Fatalf("duplicate pause commands: %s", queued)
	}
	var cmd loop.ControlCommand
	if err = json.Unmarshal(queued, &cmd); err != nil {
		t.Fatal(err)
	}
	if cmd.Action != "pause" || cmd.Value != "" {
		t.Fatal("mode change substituted for pause", cmd)
	}
	// Saving intent but losing the mailbox response never authorizes a re-append.
	if err = os.WriteFile(filepath.Join(dir, "control.ndjson"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if held, err := legacyPauseCheckpoint(context.Background(), dir, path); err != nil || held {
		t.Fatal(held, err)
	}
	if after, _ := os.ReadFile(filepath.Join(dir, "control.ndjson")); len(after) != 0 {
		t.Fatal("unknown pause was resent")
	}
	ack := loop.ControlAck{ID: cmd.ID, Action: "pause", Status: "ok", At: time.Now(), DispatchState: loop.StatePaused}
	raw, _ := json.Marshal(ack)
	if err = os.WriteFile(filepath.Join(dir, "control-ack.ndjson"), append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	// A paused ACK without current paused state does not pass.
	if held, err := legacyPauseCheckpoint(context.Background(), dir, path); err == nil || held {
		t.Fatal("ACK alone accepted", held, err)
	}
	// Pause has its own ACK/status boundary and does not generate a fresh
	// canonical snapshot. Canonical export is verified by the next mode event.
	maintenanceTestCheckpoint(t, dir, "paused", time.Now().Add(-time.Hour))
	if held, err := legacyPauseCheckpoint(context.Background(), dir, path); err != nil || !held {
		t.Fatal("complete original hold rejected", held, err)
	}
	// A later foreign control invalidates this boundary, even if paused remains.
	foreign, _ := json.Marshal(loop.ControlAck{ID: "foreign", Action: "pause", Status: "ok"})
	if err = os.WriteFile(filepath.Join(dir, "control-ack.ndjson"), append(append(raw, '\n'), append(foreign, '\n')...), 0600); err != nil {
		t.Fatal(err)
	}
	if held, err := legacyPauseCheckpoint(context.Background(), dir, path); err == nil || held {
		t.Fatal("foreign control accepted", held, err)
	}
}

func TestMaintenanceDetachUnknownOnlyReadsOriginalStatus(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "nh-detach-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	l, err := net.Listen("unix", filepath.Join(dir, "host.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	req := hostRequest{Operation: "detach", ContextID: "context", InstanceID: "host", RequestID: "request", ProjectDir: "/original", ConfigSHA256: "config", Mode: "manual", BinarySHA256: "binary"}
	seen := make(chan string, 3)
	go func() {
		for {
			conn, acceptErr := l.Accept()
			if acceptErr != nil {
				return
			}
			func() {
				defer conn.Close()
				var got hostRequest
				if json.NewDecoder(conn).Decode(&got) != nil {
					return
				}
				seen <- got.Operation
				if got.Operation == "detach" {
					return // Simulate an unknown response after owner accepted control.
				}
				_ = json.NewEncoder(conn).Encode(hostResponse{hostReceipt: hostReceipt{
					ContextID: got.ContextID, HostInstanceID: got.InstanceID, RequestID: got.RequestID,
					ProjectDir: got.ProjectDir, ConfigSHA256: got.ConfigSHA256, Mode: got.Mode,
					BinarySHA256: got.BinarySHA256, ContextState: "detached", TerminalCause: loop.TerminalMigrationDetach}, Status: "ok"})
			}()
		}
	}()
	intent := filepath.Join(t.TempDir(), "detach.json")
	if _, err = legacyDetachOnce(context.Background(), dir, intent, req); err == nil {
		t.Fatal("missing reply was not unknown")
	}
	result, err := legacyDetachOnce(context.Background(), dir, intent, req)
	if err != nil || result.ContextState != "detached" {
		t.Fatal(result, err)
	}
	if <-seen != "detach" || <-seen != "status" {
		t.Fatal("unknown detach was repeated")
	}
	req.ContextID = "other-generation"
	if _, err = legacyDetachOnce(context.Background(), dir, intent, req); err == nil {
		t.Fatal("changed context accepted")
	}
	select {
	case operation := <-seen:
		t.Fatal("sent a control or read for changed binding", operation)
	default:
	}
}

func TestMaintenanceContextPlanPreservesMissingEvidence(t *testing.T) {
	dir := t.TempDir()
	r := hostReceipt{ProjectDir: dir, ContextID: "original", RequestID: "retained", Mode: "manual", ConfigSHA256: "unknown"}
	p := planHostMaintenanceContext(t.TempDir(), r, nil)
	joined := strings.Join(p.Blockers, "\n")
	for _, want := range []string{"original accepted request envelope unavailable", "status.json", "state.snapshot.json", "original config bytes unavailable"} {
		if !strings.Contains(joined, want) {
			t.Fatal("missing evidence silently filled", want, joined)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatal("read-only plan wrote source files", entries, err)
	}
}

func TestMaintenanceStatusDoesNotTreatManualAsPaused(t *testing.T) {
	mode, dispatch, err := readMaintenanceStatus([]byte(`{"effective_mode":"manual","loop_state":"running"}`))
	if err != nil || mode != "manual" || dispatch != "running" {
		t.Fatal(mode, dispatch, err)
	}
	for _, raw := range []string{`{"mode":"manual","loop_state":"paused"}`, `{"effective_mode":"auto","mode_override":"manual","loop_state":"paused"}`, `{"effective_mode":"manual"}`} {
		if _, _, err := readMaintenanceStatus([]byte(raw)); err == nil {
			t.Fatal("missing/conflicting original status accepted", raw)
		}
	}
}

func TestMaintenancePlanDirectoryReadbackNeverCreatesOrChmods(t *testing.T) {
	root := filepath.Join(t.TempDir(), "missing")
	t.Setenv("NOODLE_HOST_DIR", root)
	if _, err := existingHostDirectory(); !os.IsNotExist(err) {
		t.Fatal("missing Host root was accepted", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("plan created a Host root", err)
	}
	if err := os.Mkdir(root, 0755); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := existingHostDirectory(); err != nil || got != root {
		t.Fatal(got, err)
	}
	after, err := os.Stat(root)
	if err != nil || before.Mode() != after.Mode() {
		t.Fatal("readback changed original permissions", err)
	}
	cmd := newHostRollingCmd()
	cmd.SetArgs([]string{"--maintenance-plan", "--initial-maintenance"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("readback flag combined with a control operation")
	}
}

func TestMaintenanceRetainedRunningRoundCannotReplayControls(t *testing.T) {
	root := t.TempDir()
	b := hostBootstrap{Tasks: []hostBootstrapTask{{
		Original: hostReceipt{ProjectDir: filepath.Join(root, "original")},
		Round:    &loop.HostContinuation{Mode: "manual", DispatchState: loop.StateRunning},
	}}}
	if done, err := bootstrapCheckpoint(context.Background(), root, &b); err == nil || done {
		t.Fatal("unproved retained hold advanced", done, err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatal("retained unknown checkpoint created new control state", entries, err)
	}
}
