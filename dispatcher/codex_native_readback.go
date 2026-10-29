package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"time"

	"github.com/poteto/noodle/internal/procx"
)

// ReadNativeSessionRecord reads the original native owner, not monitor metadata.
func ReadNativeSessionRecord(runtimeDir, session string) (NativeCodexRecord, error) {
	var record NativeCodexRecord
	if session == "" || session == "." || session == ".." || filepath.Base(session) != session {
		return record, fmt.Errorf("native session identity invalid")
	}
	if err := readResumeJSON(filepath.Join(runtimeDir, "sessions", session, "native.json"), &record); err != nil {
		return record, err
	}
	if record.Schema != 2 || record.SessionID != session || !filepath.IsAbs(record.NativeHome) || !filepath.IsAbs(record.Worktree) {
		return record, fmt.Errorf("original foreground native session binding differs")
	}
	return record, nil
}

func NativeSessionProcessesAlive(ctx context.Context, record NativeCodexRecord) (bool, error) {
	if record.Foreground.PID <= 0 {
		return false, nil
	}
	started, err := time.ParseInLocation(time.ANSIC, record.Foreground.Started, time.UTC)
	if err != nil {
		return false, fmt.Errorf("original native process start identity invalid: %w", err)
	}
	absent, err := (procx.ProcessRecord{PID: record.Foreground.PID, SessionID: record.SessionID, StartedAt: started}).GroupAbsent()
	if err != nil {
		return false, err
	}
	if !absent {
		return true, nil
	}
	possible := false
	for _, child := range record.ObservedChildren {
		if procx.IsPIDAlive(child.PID) {
			possible = true
			break
		}
	}
	if !possible {
		return false, nil
	}
	rows, err := nativeProcessSnapshot(ctx)
	if err != nil {
		return false, err
	}
	for _, original := range append(append([]nativeProcessIdentity{}, record.ObservedChildren...), record.Foreground) {
		if current, ok := rows[original.PID]; ok && current.Started == original.Started {
			return true, nil
		}
	}
	return false, nil
}

// Only the original runtime's terminal, unsubscribe, unload and exit evidence
// can establish release. Missing legacy process.json never supplies that proof.
func ReadReleasedNativeSession(ctx context.Context, runtimeDir, session string) (NativeCodexRecord, error) {
	record, err := ReadNativeSessionRecord(runtimeDir, session)
	if err != nil {
		return record, err
	}
	if record.State != "released" || record.Problem != "" || !nativeTerminal(record.Terminal) || !codexThreadUUID.MatchString(record.ThreadID) || record.ServerExit == nil || *record.ServerExit != 0 || record.Foreground.PID <= 0 || record.Foreground.Started == "" {
		return record, fmt.Errorf("original native session release is not confirmed")
	}
	if err := validateNativeUnsubscribeACK(record.UnsubscribeACK); err != nil {
		return record, err
	}
	var unloaded struct {
		Data       []string
		NextCursor *string
	}
	if json.Unmarshal(record.UnloadReadback, &unloaded) != nil || unloaded.Data == nil || len(unloaded.Data) != 0 || unloaded.NextCursor != nil {
		return record, fmt.Errorf("original native unload readback is incomplete")
	}
	alive, err := NativeSessionProcessesAlive(ctx, record)
	if err != nil {
		return record, err
	}
	if alive {
		return record, fmt.Errorf("original native process has not exited")
	}
	return record, nil
}
