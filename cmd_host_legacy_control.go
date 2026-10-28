package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
	"github.com/poteto/noodle/loop"
)

type legacyModeIntent struct {
	Command   loop.ControlCommand `json:"command"`
	ACKPrefix []byte              `json:"ack_prefix"`
	Status    []byte              `json:"status_before"`
}

// Use the original control mailbox and first ACK. An unknown append is never
// repeated; an ACK alone cannot prove the old actor checkpointed its state.
func legacyModeCheckpoint(ctx context.Context, runtimeDir, intentPath string) (*loop.HostContinuation, error) {
	lock, err := lockfile.Acquire(ctx, filepath.Join(runtimeDir, "control.lock"))
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	ackRaw, err := os.ReadFile(filepath.Join(runtimeDir, "control-ack.ndjson"))
	if os.IsNotExist(err) {
		ackRaw = nil
	} else if err != nil {
		return nil, err
	}
	var intent legacyModeIntent
	raw, err := os.ReadFile(intentPath)
	if os.IsNotExist(err) {
		pending, readErr := os.ReadFile(filepath.Join(runtimeDir, "control.ndjson"))
		if readErr != nil && !os.IsNotExist(readErr) {
			return nil, readErr
		}
		if len(bytes.TrimSpace(pending)) != 0 {
			return nil, errors.New("original control input is pending; wait for its owner")
		}
		id, idErr := hostID()
		if idErr != nil {
			return nil, idErr
		}
		intent = legacyModeIntent{Command: loop.ControlCommand{ID: id, Action: "mode", Value: "manual", At: time.Now()}, ACKPrefix: ackRaw}
		intent.Status, err = os.ReadFile(filepath.Join(runtimeDir, "status.json"))
		if os.IsNotExist(err) {
			intent.Status = nil
		} else if err != nil {
			return nil, err
		}
		raw, err = json.Marshal(intent)
		if err != nil {
			return nil, err
		}
		if err = filex.WriteFileAtomicDurable(intentPath, raw); err != nil {
			return nil, err
		}
		line, marshalErr := json.Marshal(intent.Command)
		if marshalErr != nil {
			return nil, marshalErr
		}
		f, openErr := os.OpenFile(filepath.Join(runtimeDir, "control.ndjson"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
		if openErr != nil {
			return nil, openErr
		}
		_, writeErr := f.Write(append(line, '\n'))
		err = errors.Join(writeErr, f.Sync(), f.Close())
		if err != nil {
			return nil, err
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err = json.Unmarshal(raw, &intent); err != nil {
		return nil, err
	}
	if intent.Command.ID == "" || intent.Command.Action != "mode" || intent.Command.Value != "manual" || intent.Command.At.IsZero() {
		return nil, errors.New("invalid original mode intent")
	}
	if !bytes.HasPrefix(ackRaw, intent.ACKPrefix) {
		return nil, errors.New("original control ACK history changed")
	}
	var matched *loop.ControlAck
	for _, line := range bytes.Split(ackRaw[len(intent.ACKPrefix):], []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var ack loop.ControlAck
		if err = json.Unmarshal(line, &ack); err != nil {
			return nil, err
		}
		if ack.ID != intent.Command.ID {
			return nil, errors.New("another control changed the checkpoint boundary; readback required")
		}
		if matched != nil {
			return nil, errors.New("mode command has more than one ACK; retain the first owner readback")
		}
		matched = &ack
	}
	if matched == nil {
		return nil, nil
	}
	if matched.Status != "ok" || matched.Action != "mode" {
		return nil, fmt.Errorf("original mode control failed: %s", matched.Message)
	}
	if matched.DispatchState != loop.StateRunning && matched.DispatchState != loop.StatePaused && matched.DispatchState != loop.StateDraining && matched.DispatchState != loop.StateIdle {
		return nil, errors.New("first mode ACK lacks dispatch state")
	}
	snapshot, err := reducer.ReadSnapshot(filepath.Join(runtimeDir, "state.snapshot.json"))
	if err != nil {
		return nil, err
	}
	if snapshot.EffectLedger == nil || snapshot.State.Mode != state.RunModeManual || len(snapshot.State.ModeTransitions) == 0 {
		return nil, errors.New("mode ACK has no complete canonical checkpoint")
	}
	transition := snapshot.State.ModeTransitions[len(snapshot.State.ModeTransitions)-1]
	if transition.ToMode != state.RunModeManual || transition.Epoch != snapshot.State.ModeEpoch || transition.AppliedAt.Before(intent.Command.At) || transition.AppliedAt.After(snapshot.GeneratedAt) {
		return nil, errors.New("mode checkpoint does not match the original control boundary")
	}
	var status struct {
		EffectiveMode string `json:"effective_mode"`
		ModeOverride  string `json:"mode_override"`
	}
	if len(intent.Status) > 0 {
		if err = json.Unmarshal(intent.Status, &status); err != nil {
			return nil, err
		}
	}
	originalMode := status.EffectiveMode
	if originalMode != "" && originalMode != "manual" && originalMode != "supervised" && originalMode != "auto" {
		return nil, errors.New("original effective mode is invalid")
	}
	if status.ModeOverride != "" && status.ModeOverride != originalMode {
		return nil, errors.New("original effective mode conflicts with its override")
	}
	// Missing status cannot be replaced by canonical FromMode: startup overrides
	// can hold manual while the old checkpoint still says auto.
	return &loop.HostContinuation{Mode: originalMode, DispatchState: matched.DispatchState}, nil
}
