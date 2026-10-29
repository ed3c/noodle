package runtime

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

type maintenanceToolCall struct {
	ID, Input, Thread string
	Started, Returned time.Time
	Result            map[string]json.RawMessage
}

// The orchestrator preserves the actual launch/wait call-output pairs. A bare
// exit_code document cannot identify which original execution handle returned.
func maintenanceTool(ref SessionReadbackFileRef) (maintenanceToolCall, error) {
	var transcript struct {
		Schema  int    `json:"schema"`
		Thread  string `json:"source_thread"`
		Records []struct {
			Record struct {
				Timestamp time.Time `json:"timestamp"`
				Payload   struct {
					Type, Name, Input string
					CallID            string `json:"call_id"`
					Output            []struct {
						Text string `json:"text"`
					} `json:"output"`
				} `json:"payload"`
			} `json:"record"`
		} `json:"records"`
	}
	var call maintenanceToolCall
	if err := maintenanceVerify(ref, &transcript); err != nil {
		return call, err
	}
	if transcript.Schema != 1 || transcript.Thread == "" || len(transcript.Records) != 2 {
		return call, fmt.Errorf("original launcher tool transcript is incomplete")
	}
	first, second := transcript.Records[0].Record, transcript.Records[1].Record
	if first.Payload.Type != "custom_tool_call" || first.Payload.Name != "exec" || second.Payload.Type != "custom_tool_call_output" || first.Payload.CallID == "" || first.Payload.CallID != second.Payload.CallID || first.Timestamp.IsZero() || second.Timestamp.Before(first.Timestamp) {
		return call, fmt.Errorf("original launcher tool call and return differ")
	}
	count := 0
	for _, block := range second.Payload.Output {
		var result map[string]json.RawMessage
		if json.Unmarshal([]byte(block.Text), &result) == nil && result != nil {
			call.Result = result
			count++
		}
	}
	if count != 1 {
		return call, fmt.Errorf("original launcher tool result is missing or ambiguous")
	}
	call.ID, call.Input, call.Started, call.Returned = first.Payload.CallID, first.Payload.Input, first.Timestamp, second.Timestamp
	call.Thread = transcript.Thread
	return call, nil
}

func ValidateProgressMaintenanceLauncher(p ProgressMaintenance) error {
	return validateOriginalHostLauncher(p.Launcher, p.LauncherCall, p.LauncherSession)
}

type OriginalHostLifetime struct {
	Launcher        SessionReadbackFileRef `json:"launcher"`
	LauncherCall    SessionReadbackFileRef `json:"launcher_call"`
	WaitCall        SessionReadbackFileRef `json:"wait_call"`
	WaitResult      SessionReadbackFileRef `json:"wait_result"`
	LauncherSession int                    `json:"launcher_session"`
	ObservedAt      time.Time              `json:"observed_at"`
}

func validateOriginalHostLauncher(intent, launchCall SessionReadbackFileRef, executionSession int) error {
	call, err := maintenanceTool(launchCall)
	if err != nil {
		return err
	}
	var session int
	var output string
	if json.Unmarshal(call.Result["session_id"], &session) != nil || session != executionSession || session <= 0 || len(call.Result["exit_code"]) != 0 || json.Unmarshal(call.Result["output"], &output) != nil || strings.Count(call.Input, "tools.exec_command(") != 1 {
		return fmt.Errorf("original launcher did not return this execution session")
	}
	var identity struct{ Intent, Launcher string }
	if json.Unmarshal([]byte(output), &identity) != nil || identity.Intent != intent.Path || identity.Launcher != filepath.Dir(intent.Path) {
		return fmt.Errorf("original launcher return does not identify this intent")
	}
	return maintenanceVerify(intent, nil)
}

func maintenanceWait(p ProgressMaintenance, exit ProgressMaintenanceExit, released time.Time) error {
	return ValidateOriginalHostLifetime(OriginalHostLifetime{Launcher: p.Launcher, LauncherCall: p.LauncherCall, WaitCall: exit.WaitCall, WaitResult: exit.WaitResult, LauncherSession: p.LauncherSession, ObservedAt: exit.ObservedAt}, released)
}

// ValidateOriginalHostLifetime binds the authentic launch/wait call pairs. Each
// consumer separately decides which exit codes and resource states are valid.
func ValidateOriginalHostLifetime(lifetime OriginalHostLifetime, released time.Time) error {
	if released.IsZero() || lifetime.ObservedAt.IsZero() {
		return fmt.Errorf("original Host lifetime timestamps are missing")
	}
	if err := validateOriginalHostLauncher(lifetime.Launcher, lifetime.LauncherCall, lifetime.LauncherSession); err != nil {
		return err
	}
	call, err := maintenanceTool(lifetime.WaitCall)
	if err != nil {
		return err
	}
	launch, err := maintenanceTool(lifetime.LauncherCall)
	if err != nil {
		return err
	}
	if call.Thread != launch.Thread {
		return fmt.Errorf("original launcher wait belongs to a different execution thread")
	}
	input := strings.TrimSpace(call.Input)
	const prefix = "text(await tools.write_stdin("
	const suffix = "));"
	var args struct {
		SessionID int `json:"session_id"`
	}
	if !strings.HasPrefix(input, prefix) || !strings.HasSuffix(input, suffix) || json.Unmarshal([]byte(strings.TrimSuffix(strings.TrimPrefix(input, prefix), suffix)), &args) != nil || args.SessionID != lifetime.LauncherSession || call.Started.Before(released) || lifetime.ObservedAt.Before(call.Returned) {
		return fmt.Errorf("original launcher wait call is not bound to this released execution")
	}
	var result map[string]json.RawMessage
	if err := maintenanceVerify(lifetime.WaitResult, &result); err != nil {
		return err
	}
	if !reflect.DeepEqual(result, call.Result) {
		return fmt.Errorf("original launcher terminal result differs from its wait return")
	}
	return nil
}
