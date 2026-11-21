package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/poteto/noodle/loop"
	loopruntime "github.com/poteto/noodle/runtime"
)

func TestArchiveRecoveryPauseRequiresLatestNativeACK(t *testing.T) {
	at := time.Now()
	pause := loop.ControlAck{ID: "pause", Action: "pause", Status: "ok", DispatchState: loop.StatePaused, At: at}
	resume := loop.ControlAck{ID: "resume", Action: "resume", Status: "ok", DispatchState: loop.StateRunning, At: at}
	for _, test := range []struct {
		name string
		acks []loop.ControlAck
		want string
	}{
		{name: "confirmed", acks: []loop.ControlAck{pause}, want: "pause"},
		{name: "projection only"},
		{name: "resumed", acks: []loop.ControlAck{pause, resume}},
		{name: "duplicate cannot restore hold", acks: []loop.ControlAck{pause, resume, pause}},
		{name: "legacy pause without state", acks: []loop.ControlAck{{ID: "legacy", Action: "pause", Status: "ok", At: at}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var raw bytes.Buffer
			for _, ack := range test.acks {
				if err := json.NewEncoder(&raw).Encode(ack); err != nil {
					t.Fatal(err)
				}
			}
			got, err := archiveRecoveryPauseACK(raw.Bytes())
			if got != test.want || (err != nil) != (test.want == "") {
				t.Fatalf("pause decision = %q, %v (want %q)", got, err, test.want)
			}
		})
	}
}

func TestArchiveRecoveryPauseBindsOriginalPlanAndDoesNotReplay(t *testing.T) {
	runtimeDir := t.TempDir()
	session := filepath.Join(runtimeDir, "sessions", "original")
	if err := os.MkdirAll(session, 0700); err != nil {
		t.Fatal(err)
	}
	maintenanceTestCheckpoint(t, runtimeDir, "running", time.Now())
	p := loopruntime.ProgressMaintenance{SessionDirectory: session, ContextID: "context", HostInstance: "host"}
	review, capture := loopruntime.SessionReadbackFileRef{Path: "/original/review"}, loopruntime.SessionReadbackFileRef{Path: "/original/capture"}
	requestPause := func(ref loopruntime.SessionReadbackFileRef) (archiveRecoveryPauseReadback, error) {
		return prepareArchiveRecoveryPauseLocked(p, ref, capture, "original-attempt")
	}
	for range 2 {
		result, err := requestPause(review)
		if err != nil || result.Status != "archive_recovery_pause_ack_unknown" || result.DispatchHold != nil {
			t.Fatalf("unacknowledged pause claimed hold: %+v %v", result, err)
		}
	}
	queue := filepath.Join(runtimeDir, "control.ndjson")
	data, err := os.ReadFile(queue)
	if err != nil || bytes.Count(data, []byte{'\n'}) != 1 {
		t.Fatalf("native pause request was repeated: %s %v", data, err)
	}
	if _, err := requestPause(loopruntime.SessionReadbackFileRef{Path: "/foreign/review"}); err == nil {
		t.Fatal("pause plan accepted a foreign review")
	}
	var command loop.ControlCommand
	if err = json.Unmarshal(data, &command); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(queue, nil, 0600); err != nil {
		t.Fatal(err)
	}
	maintenanceTestCheckpoint(t, runtimeDir, "paused", time.Now())
	if _, err = archiveRecoveryPaused(runtimeDir); err == nil {
		t.Fatal("paused projection without original ACK was accepted")
	}
	ack, err := json.Marshal(loop.ControlAck{ID: command.ID, Action: "pause", Status: "ok", DispatchState: loop.StatePaused, At: command.At.Add(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(runtimeDir, "control-ack.ndjson"), append(ack, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	observed, err := requestPause(review)
	if err != nil || observed.Status != "archive_recovery_pause_observed" || observed.DispatchHold == nil || observed.DispatchHold.PauseCommandID != command.ID {
		t.Fatalf("original pause ACK was not adopted: %+v %v", observed, err)
	}
	if got, err := os.ReadFile(queue); err != nil || len(got) != 0 {
		t.Fatalf("readback re-emitted original pause: %s %v", got, err)
	}
}
