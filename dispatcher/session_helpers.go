package dispatcher

import (
	"encoding/json"
	"time"

	"github.com/poteto/noodle/event"
	"github.com/poteto/noodle/parse"
)

func nowUTC() time.Time {
	return time.Now().UTC()
}

const sessionHeartbeatTTLSeconds = 30

// FormatEventLines projects one canonical event into log and broadcast records.
func FormatEventLines(sessionID string, canonical parse.CanonicalEvent) []event.Event {
	timestamp := canonical.Timestamp
	if timestamp.IsZero() {
		timestamp = nowUTC()
	}

	makePayload := func(values map[string]any) json.RawMessage {
		payload, err := json.Marshal(values)
		if err != nil {
			return nil
		}
		return payload
	}

	costEvent := func() event.Event {
		return event.Event{
			Type: event.EventCost,
			Payload: makePayload(map[string]any{
				"cost_usd":   canonical.CostUSD,
				"tokens_in":  canonical.TokensIn,
				"tokens_out": canonical.TokensOut,
			}),
			Timestamp: timestamp,
			SessionID: sessionID,
		}
	}

	switch canonical.Type {
	case parse.EventInit:
		return []event.Event{{
			Type:      event.EventSpawned,
			Payload:   makePayload(map[string]any{"message": canonical.Message, "provider": canonical.Provider}),
			Timestamp: timestamp,
			SessionID: sessionID,
		}}
	case parse.EventAction:
		action := parse.ParseActionMessage(canonical.Message)
		payload := map[string]any{"message": canonical.Message}
		if action.Tool != "" {
			payload["tool"] = action.Tool
		}
		if action.Summary != "" {
			payload["summary"] = action.Summary
		}
		actionEvent := event.Event{
			Type:      event.EventAction,
			Payload:   makePayload(payload),
			Timestamp: timestamp,
			SessionID: sessionID,
		}
		if canonical.CostUSD != 0 || canonical.TokensIn != 0 || canonical.TokensOut != 0 {
			// Snapshot consumers clear the current action on cost records. Write
			// usage first so a non-terminal action remains visible afterward.
			return []event.Event{costEvent(), actionEvent}
		}
		return []event.Event{actionEvent}
	case parse.EventResult:
		return []event.Event{costEvent()}
	case parse.EventError:
		return []event.Event{{
			Type:      event.EventAction,
			Payload:   makePayload(map[string]any{"message": canonical.Message, "tool": "Error", "summary": canonical.Message}),
			Timestamp: timestamp,
			SessionID: sessionID,
		}}
	case parse.EventComplete:
		return []event.Event{{
			Type:      event.EventExited,
			Payload:   makePayload(map[string]any{"message": canonical.Message, "outcome": "completed"}),
			Timestamp: timestamp,
			SessionID: sessionID,
		}}
	default:
		return nil
	}
}
