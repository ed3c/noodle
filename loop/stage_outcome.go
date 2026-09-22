package loop

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/poteto/noodle/event"
	"github.com/poteto/noodle/internal/stringx"
)

func (l *Loop) requiresTypedOutcome(cook *cookHandle) bool {
	provider := stringx.Normalize(nonEmpty(cook.stage.Provider, l.config.Routing.Defaults.Provider))
	switch provider {
	case "claude":
		return l.config.Agents.Claude.RequireTypedOutcome
	case "codex":
		return l.config.Agents.Codex.RequireTypedOutcome
	default:
		return false
	}
}

func (l *Loop) readRequiredStageOutcome(cook *cookHandle) (*event.StageMessagePayload, error) {
	return readRequiredStageOutcomeForIdentity(
		l.runtimeDir, cook.session.ID(), cook.orderID, cook.stageIndex,
	)
}

func readRequiredStageOutcomeForIdentity(runtimeDir, sessionID, orderID string, stageIndex int) (*event.StageMessagePayload, error) {
	reader := event.NewEventReader(runtimeDir)
	events, err := reader.ReadSession(sessionID, event.EventFilter{
		Types: map[event.EventType]struct{}{event.EventStageMessage: {}},
	})
	if err != nil {
		return nil, fmt.Errorf("read stage messages: %w", err)
	}
	if len(events) == 0 {
		return nil, fmt.Errorf("missing stage_message")
	}

	typedIndex := -1
	var typed event.StageMessagePayload
	for index, recorded := range events {
		var payload event.StageMessagePayload
		if err := json.Unmarshal(recorded.Payload, &payload); err != nil {
			return nil, fmt.Errorf("stage_message %d is malformed: %w", index, err)
		}
		if payload.Outcome == "" {
			continue
		}
		if recorded.SessionID != sessionID {
			return nil, fmt.Errorf("typed stage outcome session %q does not match %q", recorded.SessionID, sessionID)
		}
		if typedIndex >= 0 {
			return nil, fmt.Errorf("duplicate typed stage outcomes")
		}
		typedIndex = index
		typed = payload
	}
	if typedIndex < 0 {
		return nil, fmt.Errorf("missing typed stage outcome")
	}
	if typedIndex != len(events)-1 {
		return nil, fmt.Errorf("typed stage outcome is not the final stage_message")
	}
	if !typed.Outcome.IsValid() {
		return nil, fmt.Errorf("unknown typed stage outcome %q", typed.Outcome)
	}
	if strings.TrimSpace(typed.Message) == "" {
		return nil, fmt.Errorf("typed stage outcome message is empty")
	}
	if typed.OrderID != orderID {
		return nil, fmt.Errorf("typed stage outcome order %q does not match %q", typed.OrderID, orderID)
	}
	if typed.StageIndex == nil || *typed.StageIndex != stageIndex {
		return nil, fmt.Errorf("typed stage outcome stage does not match %d", stageIndex)
	}
	if typed.Outcome == event.StageOutcomeCompleted && typed.IsBlocking() {
		return nil, fmt.Errorf("completed typed stage outcome is blocking")
	}
	if typed.Outcome != event.StageOutcomeCompleted && !typed.IsBlocking() {
		return nil, fmt.Errorf("%s typed stage outcome is non-blocking", typed.Outcome)
	}
	return &typed, nil
}

// readStageMessage reads the most recent stage_message event from a session's
// event log. Returns nil if no stage_message was emitted.
func (l *Loop) readStageMessage(sessionID string) *event.StageMessagePayload {
	reader := event.NewEventReader(l.runtimeDir)
	events, err := reader.ReadSession(sessionID, event.EventFilter{
		Types: map[event.EventType]struct{}{event.EventStageMessage: {}},
	})
	if err != nil || len(events) == 0 {
		return nil
	}
	last := events[len(events)-1]
	var payload event.StageMessagePayload
	if err := json.Unmarshal(last.Payload, &payload); err != nil {
		return nil
	}
	return &payload
}

// readStageYield reads the most recent stage_yield event from a session's
// event log. Returns nil if no stage_yield was emitted.
func (l *Loop) readStageYield(sessionID string) *event.StageYieldPayload {
	reader := event.NewEventReader(l.runtimeDir)
	events, err := reader.ReadSession(sessionID, event.EventFilter{
		Types: map[event.EventType]struct{}{event.EventStageYield: {}},
	})
	if err != nil || len(events) == 0 {
		return nil
	}
	last := events[len(events)-1]
	var payload event.StageYieldPayload
	if err := json.Unmarshal(last.Payload, &payload); err != nil {
		return nil
	}
	return &payload
}
