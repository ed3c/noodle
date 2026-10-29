// Package procx provides cross-platform process utilities.
package procx

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// ProcessRecord is the original dispatch observation, not a signal capability.
type ProcessRecord struct {
	PID       int       `json:"pid"`
	SessionID string    `json:"session_id"`
	StartedAt time.Time `json:"started_at"`
}

func ReadProcessRecord(path string) (ProcessRecord, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return ProcessRecord{}, err
	}
	var record ProcessRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return record, fmt.Errorf("parse process.json: %w", err)
	}
	if record.PID <= 0 {
		return record, fmt.Errorf("invalid PID %d", record.PID)
	}
	return record, nil
}

// ReadPIDFile reads the original process record's PID. It grants no ownership.
func ReadPIDFile(path string) (int, error) {
	record, err := ReadProcessRecord(path)
	return record.PID, err
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

// RecordedProcessGroupAbsent keeps dispatch identity when a PID is reused.
func RecordedProcessGroupAbsent(path string) (bool, error) {
	record, err := ReadProcessRecord(path)
	if err != nil {
		return false, err
	}
	return record.GroupAbsent()
}
