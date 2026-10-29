package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
	"time"
)

// Keep subprocess output bounded without including stderr or private owner
// reports in browser diagnostics. A cap breach is an unconfirmed operation.
type contextOwnerOutput struct {
	buffer   bytes.Buffer
	exceeded bool
	limit    int
}

func (b *contextOwnerOutput) Len() int       { return b.buffer.Len() }
func (b *contextOwnerOutput) String() string { return b.buffer.String() }
func (b *contextOwnerOutput) Bytes() []byte  { return b.buffer.Bytes() }
func (b *contextOwnerOutput) Write(p []byte) (int, error) {
	n := len(p)
	limit := b.limit
	if limit == 0 {
		limit = 256 << 10
	}
	available := limit - b.Len()
	if len(p) > available {
		b.exceeded = true
		p = p[:available]
	}
	_, _ = b.buffer.Write(p)
	return n, nil
}
func runContextOwner(ctx context.Context, entry contextRegistration, action, expected string) (result contextObservation, err error) {
	identity, err := readContextHost(ctx, entry.OriginalHost.HostDir)
	if err != nil || identity != entry.OriginalHost {
		return result, fmt.Errorf("original Host unavailable or changed")
	}
	if _, err = validateContextRegistration(entry, identity); err != nil {
		return result, err
	}
	operation := "loop_ui_inspect"
	input := map[string]any{"request": entry.Request, "original_host": identity}
	if action != "inspect" {
		operation = "loop_ui_action"
		input["action"] = action
		input["expected_version"] = expected
	}
	data, err := json.Marshal(input)
	if err != nil {
		return result, err
	}
	// A browser disconnect is not a request to interrupt the original owner.
	// The bounded process lifetime still has an explicit unknown outcome path;
	// the UI never retries an action automatically.
	processCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(processCtx, entry.Launcher.Python.Path, "-B", "-E", "-s", entry.Launcher.Entry.Path, "manager-request", operation)
	cmd.Dir = filepath.Dir(entry.Launcher.Entry.Path)
	cmd.WaitDelay = 5 * time.Second
	keys := make([]string, 0, len(entry.Launcher.Environment))
	for key := range entry.Launcher.Environment {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		cmd.Env = append(cmd.Env, key+"="+entry.Launcher.Environment[key])
	}
	cmd.Stdin = bytes.NewReader(data)
	var output contextOwnerOutput
	stderr := contextOwnerOutput{limit: 64 << 10}
	cmd.Stdout = &output
	cmd.Stderr = &stderr
	started := time.Now().UTC()
	var runErr error
	defer func() {
		if err == nil && runErr == nil {
			return
		}
		exitCode := -1
		if cmd.ProcessState != nil {
			exitCode = cmd.ProcessState.ExitCode()
		}
		evidence := contextOwnerDiagnostic{Schema: 1, Started: started, Finished: time.Now().UTC(), Action: action, Request: entry.Request, Launcher: entry.Launcher.Entry, Manifest: entry.Launcher.Manifest, OriginalHost: entry.OriginalHost, ExitCode: exitCode, TimedOut: errors.Is(processCtx.Err(), context.DeadlineExceeded), Stderr: stderr.String(), StderrTruncated: stderr.exceeded, Stdout: output.String(), StdoutTruncated: output.exceeded}
		if err != nil {
			evidence.Failure = err.Error()
		} else {
			evidence.Failure = runErr.Error()
		}
		id, saveErr := saveContextOwnerDiagnostic(entry.OriginalHost.HostDir, evidence)
		if saveErr != nil {
			err = &contextOwnerOperationError{Cause: errors.Join(err, runErr, saveErr), DiagnosticFailed: true}
			return
		}
		if err != nil {
			err = &contextOwnerOperationError{Cause: err, DiagnosticID: id}
		}
	}()
	// Private process diagnostics never cross the browser boundary.
	runErr = cmd.Run()
	if output.exceeded {
		return result, fmt.Errorf("owner projection exceeds limit")
	}
	if err = json.Unmarshal(output.Bytes(), &result); err != nil {
		return result, fmt.Errorf("owner projection unavailable")
	}
	if err = validateContextObservation(result, entry); err != nil {
		return contextObservation{}, err
	}
	if runErr != nil && result.Status != "refused" {
		return contextObservation{}, fmt.Errorf("owner process ended without confirmed result")
	}
	after, err := readContextHost(processCtx, identity.HostDir)
	if err != nil || after != identity {
		return contextObservation{}, fmt.Errorf("original Host changed after operation")
	}
	if _, err = validateContextRegistration(entry, identity); err != nil {
		return contextObservation{}, err
	}
	return result, nil
}
