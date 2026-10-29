package runtime

import (
	"encoding/json"
	"path/filepath"
	"testing"
	"time"
)

// Protocol fixture only: this is not a physical Host launch/wait transcript.
func maintenanceToolFixture(t *testing.T, input string, result any, started, returned time.Time) SessionReadbackFileRef {
	t.Helper()
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	transcript := map[string]any{"schema": 1, "source_thread": "disposable-fixture", "records": []any{
		map[string]any{"record": map[string]any{"timestamp": started, "payload": map[string]any{"type": "custom_tool_call", "name": "exec", "call_id": "fixture-call", "input": input}}},
		map[string]any{"record": map[string]any{"timestamp": returned, "payload": map[string]any{"type": "custom_tool_call_output", "call_id": "fixture-call", "output": []any{map[string]any{"text": string(data)}}}}},
	}}
	name := filepath.Join(t.TempDir(), "tool.json")
	if err := maintenanceWrite(name, transcript); err != nil {
		t.Fatal(err)
	}
	ref, err := ReadSessionReadbackFile(name, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}
