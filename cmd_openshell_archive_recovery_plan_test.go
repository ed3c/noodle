package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/poteto/noodle/loop"
	loopruntime "github.com/poteto/noodle/runtime"
)

func TestArchiveRecoveryPlanOnlyOffersUnreservedOriginalActions(t *testing.T) {
	runtimeDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	session := filepath.Join(runtimeDir, "sessions", "original")
	if err := os.MkdirAll(filepath.Join(session, "archive-recovery-stop"), 0700); err != nil {
		t.Fatal(err)
	}
	review := loopruntime.SessionReadbackFileRef{Path: filepath.Join(session, "review.json"), SHA256: "original digest"}
	capture := loopruntime.SessionReadbackFileRef{Path: filepath.Join(session, "capture.json")}
	p := loopruntime.ProgressMaintenance{SessionDirectory: session, ContextID: "context", HostInstance: "host"}
	expected := archiveRecoveryPausePlan{Schema: 1, Review: review, Capture: capture, Host: p.HostInstance, ContextID: p.ContextID, AttemptID: "original-attempt"}
	check := func(want, condition string) *loopruntime.ArchiveRecoveryDispatchHold {
		t.Helper()
		operation, wait, hold, err := archiveRecoveryPlanStage(session, runtimeDir, expected)
		if err != nil || operation != want || wait != condition {
			t.Fatalf("operation=%q wait=%q hold=%+v error=%v", operation, wait, hold, err)
		}
		return hold
	}
	write := func(path string, value []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(runtimeDir, path), value, 0600); err != nil {
			t.Fatal(err)
		}
	}

	maintenanceTestCheckpoint(t, runtimeDir, "running", time.Now())
	for range 2 {
		check(archivePauseOperation, "")
	}
	for _, path := range []string{filepath.Join(runtimeDir, "control.ndjson"), filepath.Join(session, "archive-recovery-pause-plan.json")} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("read-only plan wrote %s: %v", path, err)
		}
	}
	write("orders-next.json", []byte("unresolved foreign order"))
	check("", "original_archive_recovery_pause_readback")
	write("orders-next.json", nil)
	pending, err := prepareArchiveRecoveryPauseLocked(p, review, capture, expected.AttemptID)
	if err != nil || pending.DispatchHold != nil {
		t.Fatalf("unacknowledged pause: %+v %v", pending, err)
	}
	queue, err := os.ReadFile(filepath.Join(runtimeDir, "control.ndjson"))
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		check("", "original_archive_recovery_pause_readback")
	}
	actualQueue, err := os.ReadFile(filepath.Join(runtimeDir, "control.ndjson"))
	if err != nil || !bytes.Equal(actualQueue, queue) || bytes.Count(queue, []byte{'\n'}) != 1 {
		t.Fatalf("pause was reoffered or changed: %s %v", actualQueue, err)
	}
	var original loop.ControlCommand
	if err := json.Unmarshal(queue, &original); err != nil {
		t.Fatal(err)
	}
	write("control.ndjson", nil)
	maintenanceTestCheckpoint(t, runtimeDir, "paused", time.Now())
	check("", "original_archive_recovery_pause_readback")
	ack, err := json.Marshal(loop.ControlAck{ID: original.ID, Action: "pause", Status: "ok", DispatchState: loop.StatePaused, At: original.At.Add(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	write("control-ack.ndjson", append(ack, '\n'))
	if hold := check(archiveStopOperation, ""); hold == nil || hold.PauseCommandID != original.ID {
		t.Fatalf("original pause ACK was not selected: %+v", hold)
	}

	observe := func(operation, phase string) *archiveRecoveryNext {
		t.Helper()
		next, err := archiveRecoveryOfferAfter(session, review, phase, operation)
		if err != nil || next.Next == nil || next.WaitFor != "" {
			t.Fatalf("expected typed continuation: %+v %v", next, err)
		}
		want := []string{next.Next.Executable.Path, "openshell", "progress-maintenance", operation, "--review-file", review.Path, "--expected-sha256", review.SHA256}
		if next.Next.Owner != "Noodle" || next.Next.Kind != "executable" || next.Next.Operation != operation || !reflect.DeepEqual(next.Next.Argv, want) || len(next.Next.Executable.SHA256) != 64 {
			t.Fatalf("fixture continuation changed identity: %+v", next.Next)
		}
		return next.Next
	}
	observe(archiveStopOperation, "stop")
	write(filepath.Join("sessions", "original", "archive-recovery-stop-intent.json"), []byte("{}"))
	check("", "original_archive_recovery_stop_readback")
	held, err := archiveRecoveryOfferAfter(session, review, "stop", archiveStopOperation)
	if err != nil || held.Next != nil || held.WaitFor != "original_archive_recovery_stop_readback" {
		t.Fatalf("reserved stop was reoffered: %+v %v", held, err)
	}
	write(filepath.Join("sessions", "original", "archive-recovery-stop", "archive-recovery-stop.json"), []byte("{}"))
	// The pure stage reader selects the status to verify. The original VM and
	// recovery evidence must still pass the read-only runtime verification.
	check(archiveApplyOperation, "")
	observe(archiveApplyOperation, "apply")
	write(filepath.Join("sessions", "original", "archive-recovery-apply-intent.json"), []byte("{}"))
	check("", "original_archive_recovery_apply_readback")
	write(filepath.Join("sessions", "original", "archive-recovery-candidate.json"), []byte("{}"))
	check("", "original_archive_recovery_candidate_readback")
}
