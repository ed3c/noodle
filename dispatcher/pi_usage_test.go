package dispatcher

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/poteto/noodle/event"
)

type usageTestSink struct {
	records []event.Event
	deltas  []string
}

func (s *usageTestSink) PublishSessionEvent(_ string, ev event.Event) {
	s.records = append(s.records, ev)
}

func (s *usageTestSink) PublishSessionDelta(_ string, text string, _ time.Time) {
	s.deltas = append(s.deltas, text)
}

func TestPiUsageStreamConsumers(t *testing.T) {
	const assistant = `{"role":"assistant","content":[{"type":"text","text":"answer"}],"stopReason":"stop","usage":{"input":10,"output":6,"cost":{"total":0.5}}}`
	const failed = `{"role":"assistant","content":[],"stopReason":"error","errorMessage":"provider failed","usage":{"input":10,"output":6,"cost":{"total":0.5}}}`
	const limited = `{"role":"assistant","content":[],"stopReason":"length","usage":{"input":10,"output":6,"cost":{"total":0.5}}}`
	const retryAnswer = `{"role":"assistant","content":[{"type":"text","text":"retried"}],"stopReason":"stop","usage":{"input":4,"output":2,"cost":{"total":0.25}}}`
	const settled = `{"type":"agent_settled","aborted":false}`
	const compaction = `{"type":"compaction_end","aborted":false,"willRetry":false,"result":{"usage":{"input":8,"output":3,"cost":{"total":0.125}}}}`
	start := []string{`{"type":"session","version":3}`, `{"type":"agent_start"}`}
	// These projections repeat usage, but only message_end owns it.
	assistantLines := []string{
		`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","delta":"answer"},"message":` + assistant + `}`,
		`{"type":"message_end","message":` + assistant + `}`,
		`{"type":"turn_end","message":` + assistant + `}`,
		`{"type":"agent_end","willRetry":false,"messages":[` + assistant + `]}`,
		`{"type":"agent_end","willRetry":false,"messages":[` + assistant + `]}`,
	}
	toolLines := []string{
		`{"type":"tool_execution_start","toolName":"bash","args":{"command":"true"}}`,
		`{"type":"message_end","message":{"role":"toolResult","content":[],"usage":{"input":4,"output":2,"cost":{"total":0.25}}}}`,
		`{"type":"tool_execution_end","toolName":"bash","result":{"usage":{"input":4,"output":2,"cost":{"total":0.25}}}}`,
	}
	join := func(parts ...[]string) string {
		var lines []string
		lines = append(lines, start...)
		for _, part := range parts {
			lines = append(lines, part...)
		}
		return strings.Join(lines, "\n") + "\n"
	}
	failedLines := []string{`{"type":"message_end","message":` + failed + `}`, `{"type":"agent_end","willRetry":true,"messages":[` + failed + `]}`, `{"type":"auto_retry_start"}`}
	tests := []struct {
		name        string
		input       string
		cost        float64
		in, out     int
		costRecords int
		lastAction  string
		completed   bool
		diagnostic  string
	}{
		{name: "action without usage", input: join([]string{`{"type":"tool_execution_start","toolName":"bash","args":{"command":"true"}}`}), lastAction: "$ true"},
		{name: "assistant and repeated projections", input: join(assistantLines), cost: 0.5, in: 10, out: 6, costRecords: 1, lastAction: "text:answer"},
		{name: "tool result", input: join(assistantLines, toolLines), cost: 0.75, in: 14, out: 8, costRecords: 2, lastAction: "text:tool result received"},
		{name: "compaction before completion", input: join(assistantLines, toolLines, []string{compaction}), cost: 0.875, in: 22, out: 11, costRecords: 3, lastAction: "text:context compacted"},
		{name: "settled success", input: join(assistantLines, toolLines, []string{compaction, `{"type":"agent_end","willRetry":false,"messages":[` + assistant + `]}`, settled}), cost: 0.875, in: 22, out: 11, costRecords: 4, lastAction: "text:context compacted", completed: true},
		{name: "zero cost input and output tokens", input: join([]string{
			`{"type":"message_end","message":{"role":"assistant","content":[],"usage":{"output":7}}}`,
			`{"type":"message_end","message":{"role":"toolResult","usage":{"input":3}}}`,
			`{"type":"compaction_end","result":{"usage":{"input":5,"output":2}}}`,
		}), in: 8, out: 9, costRecords: 3, lastAction: "text:context compacted"},
		{name: "cost without tokens", input: join([]string{`{"type":"compaction_end","result":{"usage":{"cost":{"total":0.125}}}}`}), cost: 0.125, costRecords: 1, lastAction: "text:context compacted"},
		{name: "failed final response", input: join([]string{`{"type":"message_end","message":` + failed + `}`, `{"type":"agent_end","willRetry":false,"messages":[` + failed + `]}`, settled}), cost: 0.5, in: 10, out: 6, costRecords: 1, lastAction: "text:assistant response ended", diagnostic: "pi run ended with stop reason error: provider failed"},
		{name: "length limited response", input: join([]string{`{"type":"message_end","message":` + limited + `}`, `{"type":"agent_end","willRetry":false,"messages":[` + limited + `]}`, settled}), cost: 0.5, in: 10, out: 6, costRecords: 1, lastAction: "text:assistant response ended", diagnostic: "pi run ended with stop reason length"},
		{name: "aborted", input: join(assistantLines, []string{`{"type":"agent_settled","aborted":true}`}), cost: 0.5, in: 10, out: 6, costRecords: 1, lastAction: "text:answer", diagnostic: "pi run aborted"},
		{name: "EOF before settled", input: strings.TrimSuffix(join(assistantLines), "\n"), cost: 0.5, in: 10, out: 6, costRecords: 1, lastAction: "text:answer"},
		{name: "truncated JSON", input: join(assistantLines) + `{"type":"agent_settled"`, cost: 0.5, in: 10, out: 6, costRecords: 1, lastAction: "text:answer", diagnostic: "parse JSON object"},
		{name: "retry pending", input: join(failedLines), cost: 0.5, in: 10, out: 6, costRecords: 1, lastAction: "text:assistant response ended"},
		{name: "retry failed", input: join(failedLines, []string{`{"type":"auto_retry_end","success":false,"finalError":"exhausted"}`}), cost: 0.5, in: 10, out: 6, costRecords: 1, lastAction: "text:assistant response ended", diagnostic: "pi retry failed: exhausted"},
		{name: "retry succeeded", input: join(failedLines, []string{
			`{"type":"message_end","message":` + retryAnswer + `}`,
			`{"type":"turn_end","message":` + retryAnswer + `}`,
			`{"type":"agent_end","willRetry":false,"messages":[` + retryAnswer + `]}`,
			`{"type":"auto_retry_end","success":true}`, settled,
		}), cost: 0.75, in: 14, out: 8, costRecords: 3, lastAction: "text:retried", completed: true},
		{name: "pending compaction", input: join(assistantLines, []string{`{"type":"compaction_start"}`, settled}), cost: 0.5, in: 10, out: 6, costRecords: 1, lastAction: "text:answer", diagnostic: "no confirmed final assistant response"},
		{name: "failed compaction", input: join(assistantLines, []string{`{"type":"compaction_end","errorMessage":"summary failed"}`, settled}), cost: 0.5, in: 10, out: 6, costRecords: 1, lastAction: "text:answer", diagnostic: "pi compaction failed: summary failed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runtimeDir := t.TempDir()
			writer, err := event.NewEventWriter(runtimeDir, "usage")
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(runtimeDir, "sessions", "usage")
			sink := &usageTestSink{}
			session := newProcessSession(processSessionConfig{id: "usage", eventWriter: writer, sink: sink,
				canonicalPath: filepath.Join(path, "canonical.ndjson"), stampedPath: filepath.Join(path, "raw.ndjson")})
			session.processStream(context.Background(), strings.NewReader(tt.input), &canonicalLineInterceptor{onLine: func(line []byte) {
				session.consumeCanonicalLine(line, session.processHook)
			}})

			if session.TotalCost() != tt.cost {
				t.Fatalf("session cost=%v want %v", session.TotalCost(), tt.cost)
			}
			outcome := session.resolveOutcome(0, false)
			if (outcome.Status == StatusCompleted) != tt.completed || outcome.HasDeliverable != tt.completed {
				t.Fatalf("outcome=%+v want completion=%v", outcome, tt.completed)
			}
			if session.Status() != "running" {
				t.Fatalf("stream marked session done before process exit: %s", session.Status())
			}
			select {
			case <-session.Done():
				t.Fatal("stream closed Done before process exit")
			default:
			}

			var liveIn, liveOut int
			var liveCost float64
			var lastAction string
			var diagnostics []string
			for len(session.events) > 0 {
				ev := <-session.Events()
				liveCost += ev.CostUSD
				liveIn += ev.TokensIn
				liveOut += ev.TokensOut
				if ev.Type == "action" {
					lastAction = ev.Message
				}
				if ev.Type == "warning" || ev.Type == "error" {
					diagnostics = append(diagnostics, ev.Message)
				}
			}
			if liveCost != tt.cost || liveIn != tt.in || liveOut != tt.out {
				t.Fatalf("session events cost=%v in=%d out=%d", liveCost, liveIn, liveOut)
			}
			if lastAction != tt.lastAction {
				t.Fatalf("last action=%q want %q", lastAction, tt.lastAction)
			}
			if tt.diagnostic == "" && len(diagnostics) != 0 || tt.diagnostic != "" && !strings.Contains(strings.Join(diagnostics, "\n"), tt.diagnostic) {
				t.Fatalf("diagnostics=%q want %q", diagnostics, tt.diagnostic)
			}
			records, err := event.NewEventReader(runtimeDir).ReadSession("usage", event.EventFilter{})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(records, sink.records) {
				t.Fatalf("event log and broadcast differ: log=%+v sink=%+v", records, sink.records)
			}
			var costRecords, tokensIn, tokensOut int
			var cost float64
			for i, record := range records {
				if record.Type != event.EventCost {
					continue
				}
				var usage struct {
					Cost float64 `json:"cost_usd"`
					In   int     `json:"tokens_in"`
					Out  int     `json:"tokens_out"`
				}
				if err := json.Unmarshal(record.Payload, &usage); err != nil {
					t.Fatal(err)
				}
				costRecords++
				cost += usage.Cost
				tokensIn += usage.In
				tokensOut += usage.Out
				if usage.Cost != 0 || usage.In != 0 || usage.Out != 0 {
					if i+1 >= len(records) || records[i+1].Type != event.EventAction || !records[i+1].Timestamp.Equal(record.Timestamp) {
						t.Fatalf("non-terminal usage lost its action: %+v", records)
					}
				}
			}
			if costRecords != tt.costRecords || cost != tt.cost || tokensIn != tt.in || tokensOut != tt.out {
				t.Fatalf("cost records=%d cost=%v in=%d out=%d", costRecords, cost, tokensIn, tokensOut)
			}
			t.Logf("parser→stamp→consumer: cost=%g tokens=%d/%d cost_records=%d completed=%v diagnostics=%q", cost, tokensIn, tokensOut, costRecords, outcome.HasDeliverable, diagnostics)
		})
	}
}
