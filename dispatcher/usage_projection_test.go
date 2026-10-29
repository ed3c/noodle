package dispatcher_test

import (
	"context"
	"testing"
	"time"

	"github.com/poteto/noodle/dispatcher"
	"github.com/poteto/noodle/event"
	"github.com/poteto/noodle/internal/snapshot"
	"github.com/poteto/noodle/loop"
	"github.com/poteto/noodle/stamp"
)

func TestUsageProjectionPreservesSnapshotAction(t *testing.T) {
	const assistant = `{"role":"assistant","content":[{"type":"text","text":"answer"}],"stopReason":"stop","usage":{"input":10,"output":6,"cost":{"total":0.5}}}`
	type projectionStep struct {
		line, action string
		tokens       int
		costDisplay  string
	}
	tests := []struct {
		name  string
		steps []projectionStep
	}{
		{name: "pi", steps: []projectionStep{
			{`{"type":"message_end","message":` + assistant + `}`, "Think answer", 10, "$0.50 | 10 in / 6 out"},
			{`{"type":"message_end","message":{"role":"toolResult","usage":{"input":3,"output":2}}}`, "Think tool result received", 13, "$0.00 | 3 in / 2 out"},
			{`{"type":"compaction_end","result":{"usage":{"input":8,"output":3,"cost":{"total":0.125}}}}`, "Think context compacted", 21, "$0.12 | 8 in / 3 out"},
			{`{"type":"turn_end","message":` + assistant + `}`, "Think context compacted", 21, ""},
			{`{"type":"agent_end","willRetry":false,"messages":[` + assistant + `]}`, "Think context compacted", 21, ""},
			{`{"type":"agent_settled","aborted":false}`, "", 21, "$0.00"},
		}},
		{name: "claude", steps: []projectionStep{
			{`{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash","input":{"command":"go test ./..."}}]}}`, "Bash go test ./...", 0, ""},
			{`{"type":"result","total_cost_usd":0.5,"usage":{"input_tokens":10,"output_tokens":6}}`, "", 10, "$0.50 | 10 in / 6 out"},
		}},
		{name: "codex", steps: []projectionStep{
			{`{"type":"item.started","item":{"type":"command_execution","command":"go test ./..."}}`, "Bash go test ./...", 0, ""},
			{`{"type":"turn.completed"}`, "", 0, "$0.00"},
			{`{"type":"event_msg","payload":{"type":"task_complete","message":"done","cost":0.8,"tokens_in":210,"tokens_out":55}}`, "", 0, ""},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runtimeDir := t.TempDir()
			writer, err := event.NewEventWriter(runtimeDir, "projection")
			if err != nil {
				t.Fatal(err)
			}
			processor := stamp.NewProcessor()
			now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
			processor.Now = func() time.Time { return now }
			for _, step := range tt.steps {
				_, canonical, err := processor.ProcessLine([]byte(step.line))
				if err != nil {
					t.Fatal(err)
				}
				var latestCostDisplay string
				for _, ce := range canonical {
					for _, record := range dispatcher.FormatEventLines("projection", ce) {
						if err := writer.Append(context.Background(), record); err != nil {
							t.Fatal(err)
						}
						if record.Type == event.EventCost {
							line, ok := snapshot.FormatSingleEvent(record)
							if !ok || line.Label != "Cost" {
								t.Fatalf("cost display=%+v ok=%v", line, ok)
							}
							latestCostDisplay = line.Body
						}
					}
				}
				if latestCostDisplay != step.costDisplay {
					t.Fatalf("cost display=%q want %q", latestCostDisplay, step.costDisplay)
				}
				state := loop.LoopState{ActiveCooks: []loop.CookSummary{{SessionID: "projection", Status: "running"}}}
				view, err := snapshot.LoadSnapshot(runtimeDir, now, state)
				if err != nil {
					t.Fatal(err)
				}
				if len(view.Active) != 1 || view.Active[0].CurrentAction != step.action || view.Active[0].ContextWindowUsagePct != float64(step.tokens)/200_000*100 {
					t.Fatalf("snapshot=%+v want action=%q input tokens=%d", view.Active, step.action, step.tokens)
				}
			}
		})
	}
}
