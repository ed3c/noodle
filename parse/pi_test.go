package parse

import (
	"fmt"
	"strings"
	"testing"
)

// Wire shapes follow Pi v1.3.0 docs/json.md and docs/message-types.md.
const piAssistantStop = `{"role":"assistant","content":[{"type":"text","text":"Done"}],"api":"openai-responses","provider":"openai","model":"test","usage":{"input":100,"output":20,"cacheRead":10,"cacheWrite":5,"reasoning":7,"totalTokens":135,"cost":{"input":0.1,"output":0.2,"cacheRead":0.1,"cacheWrite":0.1,"total":0.5}},"stopReason":"stop","timestamp":1733234401000}`
const piAssistantTool = `{"role":"assistant","content":[{"type":"toolCall","id":"call-1","name":"bash","arguments":{"command":"pwd"}}],"api":"openai-responses","provider":"openai","model":"test","usage":{"input":40,"output":10,"cost":{"total":0.25}},"stopReason":"toolUse","timestamp":1733234400000}`
const piToolResult = `{"role":"toolResult","toolCallId":"call-1","toolName":"bash","content":[{"type":"text","text":"/work"}],"usage":{"input":30,"output":8,"reasoning":3,"cost":{"total":0.125}},"isError":false,"timestamp":1733234400500}`
const piSettled = `{"type":"agent_settled","aborted":false}`
const piEndStop = `{"type":"agent_end","willRetry":false,"messages":[` + piAssistantStop + `]}`

func piParse(t *testing.T, registry *Registry, line string) []CanonicalEvent {
	t.Helper()
	provider, events, err := registry.ParseLine([]byte(line))
	if err != nil || provider != "pi" {
		t.Fatalf("Pi routing for %s: provider=%q err=%v", line, provider, err)
	}
	return events
}

func piAssertNoSuccess(t *testing.T, events []CanonicalEvent) {
	t.Helper()
	for _, event := range events {
		if event.Type == EventResult || event.Type == EventComplete {
			t.Fatalf("premature success: %+v", events)
		}
	}
}

func TestPiToolLoopUsageAndSettled(t *testing.T) {
	registry := NewRegistry()
	lines := []string{
		`{"type":"session","version":3,"id":"session-1","timestamp":"2024-12-03T14:00:00.000Z","cwd":"/work"}`,
		`{"type":"agent_start"}`, `{"type":"turn_start"}`,
		`{"type":"message_start","message":{"role":"user","content":"Review","timestamp":1733234400000}}`,
		`{"type":"message_end","message":{"role":"user","content":"Review","timestamp":1733234400000}}`,
		`{"type":"message_end","message":` + piAssistantTool + `}`,
		`{"type":"tool_execution_start","toolCallId":"call-1","toolName":"bash","args":{"command":"pwd"}}`,
		`{"type":"tool_execution_update","toolCallId":"call-1","toolName":"bash","partialResult":{"usage":{"input":999,"output":999,"cost":{"total":999}}}}`,
		`{"type":"tool_execution_end","toolCallId":"call-1","toolName":"bash","result":{"usage":{"input":30,"output":8,"cost":{"total":0.125}}},"isError":false}`,
		`{"type":"message_end","message":` + piToolResult + `}`,
		`{"type":"turn_end","message":` + piAssistantTool + `,"toolResults":[` + piToolResult + `]}`,
		`{"type":"turn_start"}`,
		`{"type":"message_start","message":{"role":"assistant","content":[],"stopReason":"pending"}}`,
		`{"type":"message_update","usage":{"input":100,"output":20,"cost":{"total":0.5}},"assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"Done"}}`,
		`{"type":"message_end","message":` + piAssistantStop + `}`,
		`{"type":"turn_end","message":` + piAssistantStop + `,"toolResults":[]}`,
		`{"type":"agent_end","willRetry":false,"messages":[` + piAssistantTool + `,` + piToolResult + `,` + piAssistantStop + `]}`,
		`{"type":"compaction_start","reason":"threshold"}`,
		`{"type":"compaction_end","reason":"threshold","result":{"summary":"Summary","firstKeptEntryId":"entry-1","tokensBefore":1000,"estimatedTokensAfter":100,"usage":{"input":200,"output":50,"reasoning":12,"cost":{"total":0.25}}},"aborted":false,"willRetry":false}`,
	}
	var input, output int
	var cost float64
	for _, line := range lines {
		events := piParse(t, registry, line)
		piAssertNoSuccess(t, events)
		for _, e := range events {
			input += e.TokensIn
			output += e.TokensOut
			cost += e.CostUSD
		}
	}
	if input != 370 || output != 88 || cost != 1.125 {
		t.Fatalf("usage got in=%d out=%d cost=%v; want 370,88,1.125", input, output, cost)
	}
	events := piParse(t, registry, `{"type":"agent_settled","aborted":false,"_ts":"2026-10-25T06:00:00Z","timestamp":"2024-12-03T14:00:00Z"}`)
	if len(events) != 1 || events[0].Type != EventResult || events[0].Message != "turn complete" {
		t.Fatalf("settled=%+v", events)
	}
	if events[0].Timestamp.Format("2006-01-02T15:04:05Z07:00") != "2026-10-25T06:00:00Z" {
		t.Fatalf("timestamp=%v", events[0].Timestamp)
	}
	if events[0].TokensIn != 0 || events[0].TokensOut != 0 || events[0].CostUSD != 0 {
		t.Fatalf("settled repeats usage: %+v", events)
	}
	if _, _, err := registry.ParseLine([]byte(piSettled)); err == nil {
		t.Fatal("duplicate settled reused completion")
	}
}

func TestPiTerminalStopReasons(t *testing.T) {
	for _, reason := range []string{"error", "length", "aborted", "pending", "toolUse", "deferred", "unknown"} {
		t.Run(reason, func(t *testing.T) {
			r := NewRegistry()
			end := fmt.Sprintf(`{"type":"agent_end","willRetry":false,"messages":[{"role":"assistant","stopReason":%q,"errorMessage":"provider detail"}]}`, reason)
			piAssertNoSuccess(t, piParse(t, r, end))
			events := piParse(t, r, piSettled)
			if len(events) != 1 || events[0].Type != EventError || !strings.Contains(events[0].Message, reason) || !strings.Contains(events[0].Message, "provider detail") {
				t.Fatalf("terminal failure=%+v", events)
			}
		})
	}
}

func TestPiMissingTerminalState(t *testing.T) {
	tests := []struct {
		name       string
		before     []string
		settled    string
		diagnostic string
	}{
		{"no-end", nil, piSettled, "no confirmed final"},
		{"message-only", []string{`{"type":"message_end","message":` + piAssistantStop + `}`}, piSettled, "no confirmed final"},
		{"turn-only", []string{`{"type":"turn_end","message":` + piAssistantStop + `,"toolResults":[]}`}, piSettled, "no confirmed final"},
		{"no-assistant", []string{`{"type":"agent_end","willRetry":false,"messages":[` + piToolResult + `]}`}, piSettled, "no confirmed final"},
		{"missing-messages", []string{`{"type":"agent_end","willRetry":false}`}, piSettled, "no confirmed final"},
		{"missing-stop", []string{`{"type":"agent_end","willRetry":false,"messages":[{"role":"assistant"}]}`}, piSettled, "stopReason missing"},
		{"missing-abort", []string{piEndStop}, `{"type":"agent_settled"}`, "abort state missing"},
		{"null-abort", []string{piEndStop}, `{"type":"agent_settled","aborted":null}`, "abort state missing"},
		{"retrying", []string{strings.Replace(piEndStop, `"willRetry":false`, `"willRetry":true`, 1)}, piSettled, "no confirmed final"},
		{"new-run", []string{piEndStop, `{"type":"agent_start"}`}, piSettled, "no confirmed final"},
		{"new-session", []string{piEndStop, `{"type":"session","version":3,"id":"next"}`}, piSettled, "no confirmed final"},
		{"new-message", []string{piEndStop, `{"type":"message_start","message":{"role":"assistant","content":[]}}`}, piSettled, "no confirmed final"},
		{"retry-start", []string{piEndStop, `{"type":"auto_retry_start","attempt":1}`}, piSettled, "no confirmed final"},
		{"retry-end-alone", []string{piEndStop, `{"type":"auto_retry_start","attempt":1}`, `{"type":"auto_retry_end","success":true,"attempt":2}`}, piSettled, "no confirmed final"},
		{"compaction-retry", []string{piEndStop, `{"type":"compaction_end","aborted":false,"willRetry":true,"result":{"summary":"retry"}}`}, piSettled, "no confirmed final"},
		{"summary-retry", []string{piEndStop, `{"type":"summarization_retry_scheduled","attempt":1}`}, piSettled, "no confirmed final"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRegistry()
			for _, line := range tc.before {
				piAssertNoSuccess(t, piParse(t, r, line))
			}
			_, events, err := r.ParseLine([]byte(tc.settled))
			piAssertNoSuccess(t, events)
			if err == nil || !strings.Contains(err.Error(), tc.diagnostic) {
				t.Fatalf("diagnostic=%v; want %s", err, tc.diagnostic)
			}
		})
	}
}

func TestPiAbortAndRetryFailure(t *testing.T) {
	for _, line := range []string{`{"type":"agent_settled","aborted":true}`, `{"type":"auto_retry_end","success":false,"attempt":3,"finalError":"overloaded"}`} {
		r := NewRegistry()
		piParse(t, r, piEndStop)
		events := piParse(t, r, line)
		if len(events) != 1 || events[0].Type != EventError {
			t.Fatalf("failure=%+v", events)
		}
		if _, _, err := r.ParseLine([]byte(piSettled)); err == nil {
			t.Fatal("failure retained earlier success")
		}
	}
}

func TestPiRetryRecoversAtSettled(t *testing.T) {
	for _, retry := range []string{"auto", "overflow"} {
		t.Run(retry, func(t *testing.T) {
			r := NewRegistry()
			lines := []string{`{"type":"agent_start"}`, `{"type":"agent_end","willRetry":true,"messages":[{"role":"assistant","stopReason":"error","errorMessage":"overloaded"}]}`}
			if retry == "auto" {
				lines = append(lines, `{"type":"auto_retry_start","attempt":1,"maxAttempts":3,"delayMs":2000,"errorMessage":"overloaded"}`)
			} else {
				lines = append(lines, `{"type":"compaction_start","reason":"overflow"}`, `{"type":"compaction_end","reason":"overflow","result":{"summary":"recovered","usage":{"input":10,"output":5,"cost":{"total":0.25}}},"aborted":false,"willRetry":true}`)
			}
			lines = append(lines, `{"type":"agent_start"}`, `{"type":"message_end","message":`+piAssistantStop+`}`, piEndStop)
			if retry == "auto" {
				lines = append(lines, `{"type":"auto_retry_end","success":true,"attempt":2}`)
			}
			for _, line := range lines {
				piAssertNoSuccess(t, piParse(t, r, line))
			}
			events := piParse(t, r, piSettled)
			if len(events) != 1 || events[0].Type != EventResult {
				t.Fatalf("retry result=%+v", events)
			}
		})
	}
}

func TestPiMalformedEventsInvalidateTerminal(t *testing.T) {
	for _, line := range []string{
		`{"type":"agent_end"}`, `{"type":"agent_end","willRetry":"false"}`,
		`{"type":"agent_settled","aborted":"false"}`, `{"type":"auto_retry_end"}`,
		`{"type":"message_end","message":{}}`,
		`{"type":"message_end","message":{"role":"assistant","content":null}}`,
		`{"type":"tool_execution_start","toolName":""}`,
		`{"type":"compaction_end","aborted":false,"willRetry":false}`,
		`{"type":"message_end","message":{"role":"assistant","content":{}}}`,
		`{"type":"message_end","message":{"role":"assistant","content":[],"usage":{"input":"bad"}}}`,
		`{"type":"compaction_end","result":{"usage":{"cost":{"total":"bad"}}}}`,
	} {
		r := NewRegistry()
		piParse(t, r, piEndStop)
		if _, _, err := r.ParseLine([]byte(line)); err == nil {
			t.Fatalf("missing parse diagnostic for %s", line)
		}
		if _, events, err := r.ParseLine([]byte(piSettled)); err == nil {
			t.Fatalf("malformed input retained completion: events=%+v", events)
		}
	}
	adapter := &PiAdapter{}
	if _, err := adapter.Parse([]byte(`{"type":"agent_end"`)); err == nil || !strings.Contains(err.Error(), "parse Pi event") {
		t.Fatalf("truncated JSON diagnostic: %v", err)
	}
}

func TestPiRegistryStreamsAreIndependent(t *testing.T) {
	for i := 0; i < 16; i++ {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			t.Parallel()
			a, b := NewRegistry(), NewRegistry()
			piParse(t, a, piEndStop)
			piParse(t, b, `{"type":"agent_end","willRetry":true,"messages":[{"role":"assistant","stopReason":"error"}]}`)
			events := piParse(t, a, piSettled)
			if len(events) != 1 || events[0].Type != EventResult {
				t.Fatalf("independent success=%+v", events)
			}
			if _, _, err := b.ParseLine([]byte(piSettled)); err == nil {
				t.Fatal("retry borrowed another stream's completion")
			}
		})
	}
}

func TestPiProviderDetectionAndNoise(t *testing.T) {
	for _, kind := range []string{
		"session", "agent_start", "agent_end", "agent_settled", "turn_start", "turn_end",
		"message_start", "message_update", "message_end", "tool_execution_start", "tool_execution_update", "tool_execution_end",
		"queue_update", "entry_appended", "session_info_changed", "thinking_level_changed", "compaction_start", "compaction_end",
		"auto_retry_start", "auto_retry_end", "summarization_retry_scheduled", "summarization_retry_attempt_start", "summarization_retry_finished",
		"bash_execution_update", "extension_error",
	} {
		provider, err := DetectProvider([]byte(fmt.Sprintf(`{"type":%q}`, kind)))
		if err != nil || provider != "pi" {
			t.Fatalf("detect %s: provider=%q err=%v", kind, provider, err)
		}
	}
	r := NewRegistry()
	for _, line := range []string{
		`{"type":"queue_update","steering":[],"followUp":[]}`, `{"type":"entry_appended","entry":{"type":"custom"}}`,
		`{"type":"session_info_changed"}`, `{"type":"thinking_level_changed","level":"high"}`,
		`{"type":"summarization_retry_attempt_start","source":"branchSummary"}`, `{"type":"summarization_retry_finished"}`,
		`{"type":"message_end","message":{"role":"custom","content":"note"}}`,
		`{"type":"tool_execution_update","toolCallId":"call-1"}`, `{"type":"tool_execution_end","toolCallId":"call-1"}`,
	} {
		if events := piParse(t, r, line); len(events) != 0 {
			t.Fatalf("noise produced events: %+v", events)
		}
	}
}

func TestPiCompactionFailuresInvalidateEarlierTerminal(t *testing.T) {
	for _, line := range []string{
		`{"type":"compaction_end","reason":"threshold","aborted":true,"willRetry":false}`,
		`{"type":"compaction_end","reason":"threshold","aborted":false,"willRetry":false,"errorMessage":"summary failed"}`,
	} {
		r := NewRegistry()
		piParse(t, r, piEndStop)
		events := piParse(t, r, line)
		if len(events) != 1 || events[0].Type != EventError {
			t.Fatalf("compaction failure=%+v", events)
		}
		if _, _, err := r.ParseLine([]byte(piSettled)); err == nil {
			t.Fatal("compaction failure retained earlier success")
		}
	}
}

func TestPiActionsAndTimestampFallback(t *testing.T) {
	r := NewRegistry()
	for _, tc := range []struct {
		line, message string
		kind          EventType
	}{
		{`{"type":"tool_execution_start","toolName":"bash","args":{"command":"go test ./..."}}`, "$ go test ./...", EventAction},
		{`{"type":"tool_execution_start","toolName":"read","args":{"path":"main.go"}}`, "read main.go", EventAction},
		{`{"type":"tool_execution_start","toolName":"nested","args":{}}`, "nested", EventAction},
		{`{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"hello"}}`, "hello", EventDelta},
	} {
		events := piParse(t, r, tc.line)
		if len(events) != 1 || events[0].Type != tc.kind || events[0].Message != tc.message {
			t.Fatalf("action=%+v; want %s", events, tc.message)
		}
	}
	events := piParse(t, r, `{"type":"session","timestamp":"2024-12-03T14:00:00.000Z","version":3,"id":"test"}`)
	if len(events) != 1 || events[0].Type != EventInit || events[0].Timestamp.Format("2006-01-02T15:04:05Z07:00") != "2024-12-03T14:00:00Z" {
		t.Fatalf("session timestamp=%+v", events)
	}
}

func TestPiCompactionRetryCanRecoverWithoutNewAssistant(t *testing.T) {
	r := NewRegistry()
	for _, line := range []string{
		piEndStop,
		`{"type":"compaction_start","reason":"threshold"}`,
		`{"type":"compaction_end","reason":"threshold","aborted":false,"willRetry":false,"errorMessage":"terminated"}`,
		`{"type":"summarization_retry_scheduled","attempt":1,"maxAttempts":3,"delayMs":2000,"errorMessage":"terminated"}`,
		`{"type":"summarization_retry_attempt_start","source":"compaction","reason":"threshold"}`,
		`{"type":"compaction_end","reason":"threshold","result":{"summary":"recovered","usage":{"input":10,"output":5,"cost":{"total":0.25}}},"aborted":false,"willRetry":false}`,
		`{"type":"summarization_retry_finished"}`,
	} {
		piAssertNoSuccess(t, piParse(t, r, line))
	}
	events := piParse(t, r, piSettled)
	if len(events) != 1 || events[0].Type != EventResult {
		t.Fatalf("recovered compaction result=%+v", events)
	}
}

func TestPiRetriedAssistantUsageCountsEachAttempt(t *testing.T) {
	r := NewRegistry()
	const failed = `{"role":"assistant","content":[],"stopReason":"error","errorMessage":"overloaded","usage":{"input":30,"output":4,"reasoning":2,"cost":{"total":0.125}},"timestamp":1733234400000}`
	var input, output int
	var cost float64
	for _, line := range []string{
		`{"type":"agent_start"}`,
		`{"type":"message_end","message":` + failed + `}`,
		`{"type":"turn_end","message":` + failed + `,"toolResults":[]}`,
		`{"type":"agent_end","willRetry":true,"messages":[` + failed + `]}`,
		`{"type":"auto_retry_start","attempt":1,"maxAttempts":3,"delayMs":2000,"errorMessage":"overloaded"}`,
		`{"type":"agent_start"}`,
		`{"type":"message_end","message":` + piAssistantStop + `}`,
		piEndStop,
		`{"type":"auto_retry_end","success":true,"attempt":2}`,
	} {
		events := piParse(t, r, line)
		piAssertNoSuccess(t, events)
		for _, e := range events {
			input += e.TokensIn
			output += e.TokensOut
			cost += e.CostUSD
		}
	}
	if input != 130 || output != 24 || cost != 0.625 {
		t.Fatalf("retry usage in=%d out=%d cost=%v; want 130,24,0.625", input, output, cost)
	}
	events := piParse(t, r, piSettled)
	if len(events) != 1 || events[0].Type != EventResult {
		t.Fatalf("retry settled=%+v", events)
	}
}
