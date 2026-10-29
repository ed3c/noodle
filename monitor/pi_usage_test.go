package monitor

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/poteto/noodle/stamp"
)

func TestPiUsageClaimsFromStream(t *testing.T) {
	const assistant = `{"role":"assistant","content":[],"stopReason":"stop","usage":{"input":10,"output":6,"cost":{"total":0.5}}}`
	const retry = `{"role":"assistant","content":[],"stopReason":"stop","usage":{"input":4,"output":2,"cost":{"total":0.25}}}`
	const failed = `{"role":"assistant","content":[],"stopReason":"error","usage":{"input":10,"output":6,"cost":{"total":0.5}}}`
	const limited = `{"role":"assistant","content":[],"stopReason":"length","usage":{"input":10,"output":6,"cost":{"total":0.5}}}`
	const settled = `{"type":"agent_settled","aborted":false}`
	const end = `{"type":"agent_end","willRetry":false,"messages":[` + assistant + `]}`
	const base = `{"type":"agent_start"}` + "\n" + `{"type":"message_end","message":` + assistant + `}` + "\n"
	tests := []struct {
		name       string
		input      string
		cost       float64
		in, out    int
		completed  bool
		processErr bool
	}{
		{name: "usage before settled", input: base + end, cost: 0.5, in: 10, out: 6},
		{name: "duplicate projections", input: base + `{"type":"message_update","message":` + assistant + `}` + "\n" + `{"type":"turn_end","message":` + assistant + `}` + "\n" + end + "\n" + end + "\n" + settled, cost: 0.5, in: 10, out: 6, completed: true},
		{name: "tool and compaction", input: base + `{"type":"message_end","message":{"role":"toolResult","usage":{"input":4,"output":2,"cost":{"total":0.25}}}}` + "\n" + `{"type":"compaction_end","result":{"usage":{"input":8,"output":3,"cost":{"total":0.125}}}}` + "\n" + end + "\n" + settled, cost: 0.875, in: 22, out: 11, completed: true},
		{name: "free tokens", input: `{"type":"message_end","message":{"role":"assistant","content":[],"usage":{"output":7}}}` + "\n" + `{"type":"message_end","message":{"role":"toolResult","usage":{"input":3}}}` + "\n" + `{"type":"compaction_end","result":{"usage":{"input":5,"output":2}}}`, in: 8, out: 9},
		{name: "no usage", input: `{"type":"tool_execution_start","toolName":"bash","args":{"command":"true"}}`},
		{name: "error", input: `{"type":"message_end","message":` + failed + `}` + "\n" + `{"type":"agent_end","willRetry":false,"messages":[` + failed + `]}` + "\n" + settled, cost: 0.5, in: 10, out: 6},
		{name: "length limited response", input: `{"type":"message_end","message":` + limited + `}` + "\n" + `{"type":"agent_end","willRetry":false,"messages":[` + limited + `]}` + "\n" + settled, cost: 0.5, in: 10, out: 6},
		{name: "truncated", input: base + `{"type":"agent_end"`, cost: 0.5, in: 10, out: 6, processErr: true},
		{name: "retry pending", input: base + `{"type":"auto_retry_start"}`, cost: 0.5, in: 10, out: 6},
		{name: "retry failed", input: base + `{"type":"auto_retry_start"}` + "\n" + `{"type":"auto_retry_end","success":false,"finalError":"exhausted"}`, cost: 0.5, in: 10, out: 6},
		{name: "retry success", input: base + `{"type":"auto_retry_start"}` + "\n" + `{"type":"message_end","message":` + retry + `}` + "\n" + `{"type":"agent_end","willRetry":false,"messages":[` + retry + `]}` + "\n" + `{"type":"auto_retry_end","success":true}` + "\n" + settled, cost: 0.75, in: 14, out: 8, completed: true},
		{name: "compaction pending", input: base + end + "\n" + `{"type":"compaction_start"}` + "\n" + settled, cost: 0.5, in: 10, out: 6},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var raw, canonical bytes.Buffer
			err := stamp.NewProcessor().Process(context.Background(), strings.NewReader(tt.input), &raw, &canonical)
			if (err != nil) != tt.processErr {
				t.Fatalf("stream error=%v want error=%v", err, tt.processErr)
			}
			runtimeDir := t.TempDir()
			dir := filepath.Join(runtimeDir, "sessions", "pi")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "canonical.ndjson"), canonical.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
			reader := NewCanonicalClaimsReader(runtimeDir)
			// Re-reading the append-only stream must report the same totals.
			for range 2 {
				claims, err := reader.ReadSession("pi")
				if err != nil {
					t.Fatal(err)
				}
				if claims.TotalCostUSD != tt.cost || claims.TokensIn != tt.in || claims.TokensOut != tt.out || claims.Completed != tt.completed {
					t.Fatalf("claims=%+v want cost=%g tokens=%d/%d completed=%v", claims, tt.cost, tt.in, tt.out, tt.completed)
				}
			}
			t.Logf("claims: cost=%g tokens=%d/%d completed=%v stream_error=%v", tt.cost, tt.in, tt.out, tt.completed, err)
		})
	}
}
