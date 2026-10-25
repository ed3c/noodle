package parse

import (
	"encoding/json"
	"fmt"
	"strings"
)

// LogAdapter parses one NDJSON line into canonical events.
type LogAdapter interface {
	Parse(line []byte) ([]CanonicalEvent, error)
}

// Registry resolves provider adapters by name.
type Registry struct {
	adapters map[string]LogAdapter
}

var codexLineTypes = map[string]struct{}{
	"event_msg":      {},
	"response_item":  {},
	"session_meta":   {},
	"turn_context":   {},
	"thread.started": {},
	"turn.started":   {},
	"turn.completed": {},
	"item.started":   {},
	"item.completed": {},
	"compacted":      {},
}

var claudeLineTypes = map[string]struct{}{
	"system":           {},
	"assistant":        {},
	"result":           {},
	"stream_event":     {},
	"user":             {},
	"rate_limit_event": {},
	"controlresponse":  {},
}

// Pi v1.3.0 JSON/RPC session events. Keep provider detection explicit so
// unrecognized records still produce the existing routing diagnostic.
var piLineTypes = map[string]struct{}{
	"session": {}, "agent_start": {}, "agent_end": {}, "agent_settled": {},
	"turn_start": {}, "turn_end": {},
	"message_start": {}, "message_update": {}, "message_end": {},
	"tool_execution_start": {}, "tool_execution_update": {}, "tool_execution_end": {},
	"queue_update": {}, "entry_appended": {}, "session_info_changed": {}, "thinking_level_changed": {},
	"compaction_start": {}, "compaction_end": {},
	"auto_retry_start": {}, "auto_retry_end": {},
	"summarization_retry_scheduled": {}, "summarization_retry_attempt_start": {}, "summarization_retry_finished": {},
	"bash_execution_update": {}, "extension_error": {},
}

func NewRegistry() *Registry {
	return &Registry{
		adapters: map[string]LogAdapter{
			"claude": ClaudeAdapter{},
			"codex":  CodexAdapter{},
			"pi":     &PiAdapter{},
		},
	}
}

func (r *Registry) Register(provider string, adapter LogAdapter) {
	if r.adapters == nil {
		r.adapters = map[string]LogAdapter{}
	}
	r.adapters[provider] = adapter
}

func (r *Registry) AdapterForProvider(provider string) (LogAdapter, bool) {
	adapter, ok := r.adapters[provider]
	return adapter, ok
}

// ParseLine detects the provider and parses one line with that adapter.
func (r *Registry) ParseLine(line []byte) (string, []CanonicalEvent, error) {
	provider, err := DetectProvider(line)
	if err != nil {
		return "", nil, err
	}

	adapter, ok := r.AdapterForProvider(provider)
	if !ok {
		return "", nil, fmt.Errorf("adapter not found for provider %q", provider)
	}

	events, err := adapter.Parse(line)
	if err != nil {
		return "", nil, err
	}
	return provider, events, nil
}

// DetectProvider picks the best adapter from a line's top-level "type".
func DetectProvider(line []byte) (string, error) {
	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(line, &probe); err != nil {
		return "", fmt.Errorf("parse NDJSON line: %w", err)
	}

	lineType := strings.TrimSpace(probe.Type)
	if lineType == "" {
		return "", fmt.Errorf("provider unresolved for line type: missing type")
	}

	if _, ok := codexLineTypes[lineType]; ok {
		return "codex", nil
	}
	if _, ok := claudeLineTypes[lineType]; ok {
		return "claude", nil
	}
	if _, ok := piLineTypes[lineType]; ok {
		return "pi", nil
	}

	// Claude transport control responses can appear with type variants across
	// provider/runtime versions; route these variants to Claude and drop them
	// in the adapter as non-canonical noise.
	switch strings.ToLower(lineType) {
	case "controlresponse", "control_response":
		return "claude", nil
	}

	return "", fmt.Errorf("provider unresolved for line type %q", lineType)
}
