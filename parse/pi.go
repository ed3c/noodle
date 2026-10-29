package parse

import (
	"encoding/json"
	"fmt"
	"strings"
)

// PiAdapter belongs to one JSON event stream, just like its owning Registry.
// agent_end closes a low-level run; only agent_settled confirms completion.
// Protocol: https://github.com/earendil-works/pi/blob/v1.3.0/packages/coding-agent/docs/json.md
// Like the stream reader, an adapter is used sequentially, never across workers.
type PiAdapter struct {
	ended             bool
	retrying          bool
	compactionPending bool
	final             *piMessage
}

type piUsage struct {
	Input  int `json:"input"`
	Output int `json:"output"`
	Cost   struct {
		Total float64 `json:"total"`
	} `json:"cost"`
}

type piMessage struct {
	Role         string          `json:"role"`
	Content      json.RawMessage `json:"content"`
	StopReason   string          `json:"stopReason"`
	ErrorMessage string          `json:"errorMessage"`
	Usage        piUsage         `json:"usage"`
}

type piEvent struct {
	Type         string          `json:"type"`
	Timestamp    json.RawMessage `json:"timestamp"`
	Injected     json.RawMessage `json:"_ts"`
	Message      piMessage       `json:"message"`
	Messages     []piMessage     `json:"messages"`
	WillRetry    *bool           `json:"willRetry"`
	Aborted      *bool           `json:"aborted"`
	Success      *bool           `json:"success"`
	FinalError   string          `json:"finalError"`
	ErrorMessage string          `json:"errorMessage"`
	Error        string          `json:"error"`
	ToolName     string          `json:"toolName"`
	Args         json.RawMessage `json:"args"`
	Result       *struct {
		Usage piUsage `json:"usage"`
	} `json:"result"`
	Delta struct {
		Type  string `json:"type"`
		Delta string `json:"delta"`
	} `json:"assistantMessageEvent"`
}

func (p *PiAdapter) Parse(line []byte) ([]CanonicalEvent, error) {
	var event piEvent
	if err := json.Unmarshal(line, &event); err != nil {
		*p = PiAdapter{}
		return nil, fmt.Errorf("parse Pi event: %w", err)
	}
	ts := parseTimestamp(parseTimestampField(event.Injected), parseTimestampField(event.Timestamp))
	one := func(kind EventType, message string) ([]CanonicalEvent, error) {
		return []CanonicalEvent{{Type: kind, Message: message, Timestamp: ts}}, nil
	}
	usage := func(message string, u piUsage) ([]CanonicalEvent, error) {
		// Only authoritative message_end/compaction_end carry usage. Progress and
		// turn/agent projections repeat it; reasoning is already included in output.
		return []CanonicalEvent{{Type: EventAction, Message: message, Timestamp: ts,
			TokensIn: u.Input, TokensOut: u.Output, CostUSD: u.Cost.Total}}, nil
	}
	switch event.Type {
	case "session":
		*p = PiAdapter{}
		return one(EventInit, "pi session started")
	case "agent_start":
		*p = PiAdapter{}
		return one(EventAction, "text:agent started")
	case "turn_start", "message_start":
		p.ended, p.final = false, nil
	case "message_update":
		p.ended, p.final = false, nil
		if event.Delta.Type == "text_delta" && event.Delta.Delta != "" {
			return one(EventDelta, event.Delta.Delta)
		}
	case "message_end":
		p.ended, p.final = false, nil
		switch event.Message.Role {
		case "assistant":
			var content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if err := json.Unmarshal(event.Message.Content, &content); err != nil {
				return nil, fmt.Errorf("parse Pi assistant content: %w", err)
			}
			if content == nil {
				return nil, fmt.Errorf("Pi assistant content array missing")
			}
			texts := make([]string, 0, len(content))
			for _, block := range content {
				if block.Type == "text" && strings.TrimSpace(block.Text) != "" {
					texts = append(texts, block.Text)
				}
			}
			message := "text:assistant response ended"
			if len(texts) != 0 {
				message = "text:" + strings.Join(texts, "\n")
			}
			return usage(message, event.Message.Usage)
		case "toolResult":
			return usage("text:tool result received", event.Message.Usage)
		case "":
			return nil, fmt.Errorf("Pi message_end role missing")
		}
	case "tool_execution_start":
		p.ended, p.final = false, nil
		if strings.TrimSpace(event.ToolName) == "" {
			return nil, fmt.Errorf("Pi tool execution name missing")
		}
		if command := extractStringField(event.Args, "command"); event.ToolName == "bash" && command != "" {
			return one(EventAction, "$ "+command)
		}
		message := event.ToolName
		if path := extractStringField(event.Args, "path"); path != "" {
			message += " " + path
		}
		return one(EventAction, message)
	case "compaction_start", "summarization_retry_scheduled":
		p.compactionPending = true
	case "compaction_end":
		p.compactionPending = true
		if event.WillRetry != nil && *event.WillRetry {
			p.ended, p.retrying, p.final = false, true, nil
		}
		if event.Aborted != nil && *event.Aborted {
			return one(EventError, "pi compaction aborted")
		}
		if event.ErrorMessage != "" {
			return one(EventError, "pi compaction failed: "+event.ErrorMessage)
		}
		if event.Result == nil {
			p.ended, p.final = false, nil
			return nil, fmt.Errorf("Pi compaction_end result missing without abort or error")
		}
		p.compactionPending = false
		return usage("text:context compacted", event.Result.Usage)
	case "agent_end":
		p.ended, p.final = false, nil
		if event.WillRetry == nil {
			return nil, fmt.Errorf("Pi agent_end retry state missing")
		}
		p.ended, p.retrying = true, *event.WillRetry
		for i := len(event.Messages) - 1; i >= 0; i-- {
			if event.Messages[i].Role == "assistant" {
				p.final = &event.Messages[i]
				break
			}
		}
	case "auto_retry_start":
		p.ended, p.retrying, p.final = false, true, nil
	case "auto_retry_end":
		if event.Success == nil {
			*p = PiAdapter{}
			return nil, fmt.Errorf("Pi auto_retry_end success state missing")
		}
		if !*event.Success {
			*p = PiAdapter{}
			return one(EventError, "pi retry failed: "+event.FinalError)
		}
	case "extension_error":
		return one(EventError, "pi extension failed: "+event.Error)
	case "agent_settled":
		ended, retrying, final := p.ended, p.retrying, p.final
		compactionPending := p.compactionPending
		*p = PiAdapter{}
		if event.Aborted == nil {
			return nil, fmt.Errorf("Pi agent_settled abort state missing")
		}
		if *event.Aborted {
			return one(EventError, "pi run aborted")
		}
		if !ended || retrying || compactionPending || final == nil {
			return nil, fmt.Errorf("Pi agent_settled has no confirmed final assistant response")
		}
		if final.StopReason == "" {
			return nil, fmt.Errorf("Pi final assistant stopReason missing")
		}
		if final.StopReason != "stop" || final.ErrorMessage != "" {
			message := "pi run ended with stop reason " + final.StopReason
			if final.ErrorMessage != "" {
				message += ": " + final.ErrorMessage
			}
			return one(EventError, message)
		}
		return one(EventResult, "turn complete")
	}
	return nil, nil
}
