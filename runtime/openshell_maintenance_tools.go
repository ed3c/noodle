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
	call, err := maintenanceTool(p.LauncherCall)
	if err != nil {
		return err
	}
	var session int
	var output string
	if json.Unmarshal(call.Result["session_id"], &session) != nil || session != p.LauncherSession || session <= 0 || len(call.Result["exit_code"]) != 0 || json.Unmarshal(call.Result["output"], &output) != nil || strings.Count(call.Input, "tools.exec_command(") != 1 {
		return fmt.Errorf("original launcher did not return this execution session")
	}
	var identity struct{ Intent, Launcher string }
	if json.Unmarshal([]byte(output), &identity) != nil || identity.Intent != p.Launcher.Path || identity.Launcher != filepath.Dir(p.Launcher.Path) {
		return fmt.Errorf("original launcher return does not identify this intent")
	}
	return maintenanceVerify(p.Launcher, nil)
}

func maintenanceWait(p ProgressMaintenance, exit ProgressMaintenanceExit, released time.Time) error {
	if err := ValidateProgressMaintenanceLauncher(p); err != nil {
		return err
	}
	call, err := maintenanceTool(exit.WaitCall)
	if err != nil {
		return err
	}
	launch, err := maintenanceTool(p.LauncherCall)
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
	if !strings.HasPrefix(input, prefix) || !strings.HasSuffix(input, suffix) || json.Unmarshal([]byte(strings.TrimSuffix(strings.TrimPrefix(input, prefix), suffix)), &args) != nil || args.SessionID != p.LauncherSession || call.Started.Before(released) || exit.ObservedAt.Before(call.Returned) {
		return fmt.Errorf("original launcher wait call is not bound to this released execution")
	}
	var result map[string]json.RawMessage
	if err := maintenanceVerify(exit.WaitResult, &result); err != nil {
		return err
	}
	if !reflect.DeepEqual(result, call.Result) {
		return fmt.Errorf("original launcher terminal result differs from its wait return")
	}
	return nil
}
