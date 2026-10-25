package stamp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/poteto/noodle/parse"
)

func TestPiProcessPreservesFramingTimestampAndUsage(t *testing.T) {
	processor := NewProcessor()
	now := time.Date(2026, 10, 25, 6, 0, 0, 123, time.UTC)
	processor.Now = func() time.Time { return now }
	// Literal Unicode separators belong inside a JSON string, not at record boundaries.
	message := `{"role":"assistant","content":[{"type":"text","text":"one` + "\u2028" + `two` + "\u2029" + `three"}],"stopReason":"stop","timestamp":1733234401000,"usage":{"input":10,"output":6,"reasoning":4,"cost":{"total":0.5}}}`
	lines := []string{
		`{"type":"session","version":3,"id":"test","timestamp":"2024-12-03T14:00:00.000Z","cwd":"/work"}`,
		`{"type":"agent_start"}`,
		`{"type":"message_update","usage":{"input":10,"output":6,"cost":{"total":0.5}},"assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"one"}}`,
		`{"type":"message_end","message":` + message + `}`,
		`{"type":"message_end","message":{"role":"toolResult","toolCallId":"call-1","toolName":"nested","content":[],"isError":false,"timestamp":1733234401000,"usage":{"input":4,"output":2,"cost":{"total":0.25}}}}`,
		`{"type":"compaction_end","result":{"summary":"summary","usage":{"input":8,"output":3,"cost":{"total":0.125}}},"aborted":false,"willRetry":false}`,
		`{"type":"turn_end","message":` + message + `,"toolResults":[]}`,
		`{"type":"agent_end","willRetry":false,"messages":[` + message + `]}`,
		`{"type":"agent_settled","aborted":false}`,
	}
	var stamped, sidecar bytes.Buffer
	if err := processor.Process(context.Background(), strings.NewReader(strings.Join(lines, "\r\n")+"\n"), &stamped, &sidecar); err != nil {
		t.Fatal(err)
	}
	stampedLines := strings.Split(strings.TrimSuffix(stamped.String(), "\n"), "\n")
	if len(stampedLines) != len(lines) {
		t.Fatalf("framing: got %d lines want %d", len(stampedLines), len(lines))
	}
	for _, line := range stampedLines {
		var record struct {
			Injected string `json:"_ts"`
		}
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		if record.Injected != now.Format(time.RFC3339Nano) {
			t.Fatalf("timestamp=%s", record.Injected)
		}
	}
	var input, output, successes int
	var cost float64
	var text string
	for _, line := range strings.Split(strings.TrimSpace(sidecar.String()), "\n") {
		var event parse.CanonicalEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatal(err)
		}
		if event.Provider != "pi" || !event.Timestamp.Equal(now) {
			t.Fatalf("sidecar metadata=%+v", event)
		}
		input += event.TokensIn
		output += event.TokensOut
		cost += event.CostUSD
		if event.Type == parse.EventResult || event.Type == parse.EventComplete {
			successes++
		}
		if strings.Contains(event.Message, "three") {
			text = event.Message
		}
	}
	if input != 22 || output != 11 || cost != 0.875 || successes != 1 {
		t.Fatalf("usage/outcome: in=%d out=%d cost=%v successes=%d", input, output, cost, successes)
	}
	if text != "text:one\u2028two\u2029three" {
		t.Fatalf("Unicode content=%q", text)
	}
}

func TestPiProcessMissingSettledNeverSucceeds(t *testing.T) {
	for _, suffix := range []string{"", "\n"} {
		t.Run("eof"+strings.ReplaceAll(suffix, "\n", "-lf"), func(t *testing.T) {
			processor := NewProcessor()
			input := `{"type":"agent_end","willRetry":false,"messages":[{"role":"assistant","stopReason":"stop"}]}` + suffix
			var stamped, events bytes.Buffer
			if err := processor.Process(context.Background(), strings.NewReader(input), &stamped, &events); err != nil {
				t.Fatal(err)
			}
			if events.Len() != 0 {
				t.Fatalf("EOF inferred success: %s", events.String())
			}
		})
	}
}

func TestPiProcessDiagnosticsAndIndependentStreams(t *testing.T) {
	a, b := NewProcessor(), NewProcessor()
	if _, _, err := a.ProcessLine([]byte(`{"type":"agent_end","willRetry":false,"messages":[{"role":"assistant","stopReason":"stop"}]}`)); err != nil {
		t.Fatal(err)
	}
	_, events, err := b.ProcessLine([]byte(`{"type":"agent_settled","aborted":false}`))
	if err != nil || len(events) != 1 || events[0].Type != parse.EventError || !strings.Contains(events[0].Message, "Pi agent_settled has no confirmed final") {
		t.Fatalf("routing diagnostic: events=%+v err=%v", events, err)
	}
	_, events, err = a.ProcessLine([]byte(`{"type":"agent_settled","aborted":false}`))
	if err != nil || len(events) != 1 || events[0].Type != parse.EventResult || events[0].Provider != "pi" {
		t.Fatalf("independent success: events=%+v err=%v", events, err)
	}
	var stamped bytes.Buffer
	err = NewProcessor().Process(context.Background(), strings.NewReader("{\"type\":\"agent_start\"}\n{\"type\":\"agent_end\""), &stamped, nil)
	if err == nil || !strings.Contains(err.Error(), "process line 2: parse JSON object") {
		t.Fatalf("truncated JSON diagnostic=%v", err)
	}
}
