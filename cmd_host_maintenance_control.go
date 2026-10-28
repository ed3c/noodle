package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/loop"
)

// A mode change cannot stand in for a dispatch hold. The original mailbox
// accepts pause without stopping an active worker; subsequent calls only read.
func legacyPauseCheckpoint(ctx context.Context, runtimeDir, intentPath string) (bool, error) {
	return legacyPauseCapture(ctx, runtimeDir, intentPath, false)
}

// Recovery is restricted to the verified legacy owner in a confirmed plan.
// Pause does not checkpoint canonical state; the following native mode event
// persists the old actor's own state, never a reconstruction from legacy files.
func legacyPauseCapture(ctx context.Context, runtimeDir, intentPath string, recoverMissing bool) (bool, error) {
	lock, err := lockfile.Acquire(ctx, filepath.Join(runtimeDir, "control.lock"))
	if err != nil {
		return false, err
	}
	defer lock.Close()
	ackRaw, err := os.ReadFile(filepath.Join(runtimeDir, "control-ack.ndjson"))
	if os.IsNotExist(err) {
		ackRaw = nil
	} else if err != nil {
		return false, err
	}
	raw, err := os.ReadFile(intentPath)
	var intent legacyModeIntent
	if os.IsNotExist(err) {
		status, readErr := os.ReadFile(filepath.Join(runtimeDir, "status.json"))
		if readErr != nil && !(recoverMissing && os.IsNotExist(readErr)) {
			return false, readErr
		}
		if len(status) != 0 {
			if _, _, readErr = readMaintenanceStatus(status); readErr != nil {
				return false, readErr
			}
		}
		snapshot, readErr := reducer.ReadSnapshot(filepath.Join(runtimeDir, "state.snapshot.json"))
		if readErr != nil && !(recoverMissing && errors.Is(readErr, os.ErrNotExist)) {
			return false, readErr
		}
		if readErr == nil && (snapshot.EffectLedger == nil || snapshot.GeneratedAt.IsZero()) {
			return false, errors.New("original canonical checkpoint is incomplete; no pause was sent")
		}
		pending, readErr := os.ReadFile(filepath.Join(runtimeDir, "control.ndjson"))
		if readErr != nil && !os.IsNotExist(readErr) {
			return false, readErr
		}
		if len(bytes.TrimSpace(pending)) != 0 {
			return false, errors.New("original control input is pending; no pause was sent")
		}
		id, idErr := hostID()
		if idErr != nil {
			return false, idErr
		}
		intent = legacyModeIntent{Command: loop.ControlCommand{ID: id, Action: "pause", At: time.Now()}, ACKPrefix: ackRaw, Status: status}
		raw, err = json.Marshal(intent)
		if err != nil {
			return false, err
		}
		if err = filex.WriteFileAtomicDurable(intentPath, raw); err != nil {
			return false, err
		}
		line, marshalErr := json.Marshal(intent.Command)
		if marshalErr != nil {
			return false, marshalErr
		}
		f, openErr := os.OpenFile(filepath.Join(runtimeDir, "control.ndjson"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if openErr != nil {
			return false, openErr
		}
		_, writeErr := f.Write(append(line, '\n'))
		return false, errors.Join(writeErr, f.Sync(), f.Close())
	}
	if err != nil {
		return false, err
	}
	if err = json.Unmarshal(raw, &intent); err != nil {
		return false, err
	}
	if intent.Command.Action != "pause" || intent.Command.ID == "" || intent.Command.At.IsZero() || !bytes.HasPrefix(ackRaw, intent.ACKPrefix) {
		return false, errors.New("original pause intent or ACK history changed")
	}
	var matched *loop.ControlAck
	for _, line := range bytes.Split(ackRaw[len(intent.ACKPrefix):], []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var ack loop.ControlAck
		if err = json.Unmarshal(line, &ack); err != nil {
			return false, err
		}
		if matched != nil || ack.ID != intent.Command.ID || ack.Action != "pause" || ack.Status != "ok" || ack.DispatchState != loop.StatePaused || ack.At.Before(intent.Command.At) {
			return false, errors.New("original pause ACK is not a unique confirmed dispatch hold; readback only")
		}
		matched = &ack
	}
	if matched == nil {
		return false, nil
	}
	status, err := os.ReadFile(filepath.Join(runtimeDir, "status.json"))
	if err != nil {
		return false, err
	}
	_, dispatch, err := readMaintenanceStatus(status)
	if err != nil {
		return false, err
	}
	if dispatch != string(loop.StatePaused) {
		return false, errors.New("original status does not confirm paused dispatch")
	}
	if recoverMissing {
		return true, nil
	}
	snapshot, err := reducer.ReadSnapshot(filepath.Join(runtimeDir, "state.snapshot.json"))
	if err != nil {
		return false, err
	}
	// Pause changes dispatch only. The following mode event checkpoints the
	// actor's canonical state; waiting for a new snapshot here would deadlock
	// an otherwise idle, correctly paused actor.
	if snapshot.EffectLedger == nil || snapshot.GeneratedAt.IsZero() {
		return false, nil
	}
	return true, nil
}

type legacyDetachIntent struct {
	Request hostRequest `json:"request"`
}

// A timeout, busy response or crash cannot authorize repeating a detach. Once
// the intent exists, query the exact original context with status instead.
func legacyDetachOnce(ctx context.Context, dir, intentPath string, req hostRequest) (hostResponse, error) {
	lock, err := lockfile.Acquire(ctx, intentPath+".lock")
	if err != nil {
		return hostResponse{}, err
	}
	defer lock.Close()
	if req.Operation != "detach" || req.ContextID == "" || req.InstanceID == "" || req.RequestID == "" || req.BinarySHA256 == "" {
		return hostResponse{}, errors.New("original detach requires exact context, Host, request and binary identities")
	}
	want, err := json.Marshal(legacyDetachIntent{Request: req})
	if err != nil {
		return hostResponse{}, err
	}
	prior, readErr := os.ReadFile(intentPath)
	if readErr == nil {
		if !bytes.Equal(prior, want) {
			return hostResponse{}, errors.New("original detach intent changed; no control was sent")
		}
		req.Operation = "status"
	} else if os.IsNotExist(readErr) {
		if err = filex.WriteFileAtomicDurable(intentPath, want); err != nil {
			return hostResponse{}, err
		}
	} else {
		return hostResponse{}, readErr
	}
	raw, err := nativeHostRPC(ctx, dir, req)
	if err != nil {
		return hostResponse{}, err
	}
	var result hostResponse
	if err = json.Unmarshal(raw, &result); err != nil {
		return result, err
	}
	if result.ContextID != req.ContextID || result.HostInstanceID != req.InstanceID || result.BinarySHA256 != req.BinarySHA256 || !hostReceiptMatches(req, result.hostReceipt) {
		return result, errors.New("original detach readback identity changed")
	}
	return result, nil
}
