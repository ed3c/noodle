package procx

import "time"

// ProcessRecord is the original dispatch observation, not a signal capability.
type ProcessRecord struct {
	PID       int       `json:"pid"`
	SessionID string    `json:"session_id"`
	StartedAt time.Time `json:"started_at"`
}

// GroupAbsent reads the original process and group; it never sends a stop signal.
// Missing start evidence retains the process/group refusal.
func (record ProcessRecord) GroupAbsent() (bool, error) {
	absent, err := ProcessGroupAbsent(record.PID)
	if err != nil || absent {
		return absent, err
	}
	if record.StartedAt.IsZero() {
		return false, nil
	}
	return processStartedAfter(record.PID, record.StartedAt)
}
