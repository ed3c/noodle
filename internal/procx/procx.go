// Package procx provides cross-platform process utilities.
package procx

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// ReadPIDFile reads a JSON file containing a {"pid": N} field and returns the PID.
// Returns an error if the file cannot be read, parsed, or contains an invalid PID.
func ReadPIDFile(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var meta struct {
		PID int `json:"pid"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return 0, fmt.Errorf("parse process.json: %w", err)
	}
	if meta.PID <= 0 {
		return 0, fmt.Errorf("invalid PID %d", meta.PID)
	}
	return meta.PID, nil
}

// RecordedProcessGroupAbsent keeps the dispatch identity when a PID is reused.
// Missing start evidence retains the existing process/group refusal.
func RecordedProcessGroupAbsent(path string) (bool, error) {
	pid, err := ReadPIDFile(path)
	if err != nil {
		return false, err
	}
	absent, err := ProcessGroupAbsent(pid)
	if err != nil || absent {
		return absent, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	var record struct {
		StartedAt time.Time `json:"started_at"`
	}
	if err := json.Unmarshal(data, &record); err != nil {
		return false, fmt.Errorf("parse process start identity: %w", err)
	}
	if record.StartedAt.IsZero() {
		return false, nil
	}
	return processStartedAfter(pid, record.StartedAt)
}
