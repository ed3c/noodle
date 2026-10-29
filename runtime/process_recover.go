package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/poteto/noodle/dispatcher"
	"github.com/poteto/noodle/internal/jsonx"
	"github.com/poteto/noodle/internal/procx"
)

// processRuntime discovers original process records without adopting signal authority.
type processRuntime struct {
	*DispatcherRuntime
}

// NewProcessRuntime adapts a process dispatcher to the Runtime interface with
// original process/group readback.
func NewProcessRuntime(d dispatcher.Dispatcher, runtimeDir string, maxConcurrent int) Runtime {
	r := NewDispatcherRuntime("process", d, runtimeDir)
	r.SetMaxConcurrent(maxConcurrent)
	return &processRuntime{DispatcherRuntime: r}
}

func (p *processRuntime) Recover(_ context.Context) ([]RecoveredSession, error) {
	sessionsDir := filepath.Join(p.runtimeDir, "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var recovered []RecoveredSession
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		sessionID := entry.Name()
		sessionDir := filepath.Join(sessionsDir, sessionID)

		metaPath := filepath.Join(sessionDir, "meta.json")
		data, err := os.ReadFile(metaPath)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read session %s metadata: %w", sessionID, err)
		}
		var meta map[string]json.RawMessage
		if err := json.Unmarshal(data, &meta); err != nil {
			return nil, fmt.Errorf("parse session %s metadata: %w", sessionID, err)
		}
		var status string
		var kind string
		if raw, exists := meta["runtime"]; exists {
			if err := json.Unmarshal(raw, &kind); err != nil {
				return nil, fmt.Errorf("parse session %s runtime: %w", sessionID, err)
			}
			if kind == dispatcher.CodexNativeRuntime {
				continue // Native thread custody cannot be inferred from an OS group.
			}
		}
		if err := json.Unmarshal(meta["status"], &status); err != nil {
			return nil, fmt.Errorf("parse session %s status: %w", sessionID, err)
		}
		if raw, exists := meta["runtime"]; exists {
			var runtimeName string
			if err := json.Unmarshal(raw, &runtimeName); err != nil {
				return nil, fmt.Errorf("parse session %s runtime: %w", sessionID, err)
			}
			if runtimeName == "openshell" {
				binding, err := maintainedRecoveryBinding(sessionDir)
				if err != nil {
					return nil, err
				}
				if binding != nil {
					if _, err := os.Lstat(filepath.Join(sessionDir, "process.json")); err == nil {
						return nil, fmt.Errorf("session %s has conflicting process and maintained native ownership", sessionID)
					} else if !os.IsNotExist(err) {
						return nil, fmt.Errorf("session %s process ownership inspection failed: %w", sessionID, err)
					}
					continue
				}
			}
		}
		record, err := procx.ReadProcessRecord(filepath.Join(sessionDir, "process.json"))
		if err != nil {
			return nil, fmt.Errorf("session %s process identity requires original owner readback: %w", sessionID, err)
		}
		if record.SessionID != sessionID || record.StartedAt.IsZero() {
			return nil, fmt.Errorf("session %s process identity missing or conflicts with original Session", sessionID)
		}
		absent, err := record.GroupAbsent()
		if err != nil {
			return nil, fmt.Errorf("session %s process/group exit requires original owner readback: %w", sessionID, err)
		}
		if absent {
			if status == "running" {
				meta["status"] = json.RawMessage(`"exited"`)
			}
			meta["alive"] = json.RawMessage(`false`)
			delete(meta, "exit_pending")
			if err := jsonx.WriteJSON(metaPath, meta); err != nil {
				return nil, err
			}
			continue
		}

		target := ReadSessionTarget(filepath.Join(sessionDir, "prompt.txt"))
		recovered = append(recovered, RecoveredSession{
			OrderID:       target,
			SessionHandle: &recoveredSessionHandle{id: sessionID, status: "running"},
			RuntimeName:   p.name,
			Reason:        "original process/group exit requires owner readback",
		})
	}

	return recovered, nil
}

// recoveredSessionHandle represents a session discovered during crash recovery.
type recoveredSessionHandle struct {
	id     string
	status string
}

func (s *recoveredSessionHandle) ID() string              { return s.id }
func (s *recoveredSessionHandle) Status() string          { return s.status }
func (s *recoveredSessionHandle) Outcome() SessionOutcome { return SessionOutcome{} }
func (s *recoveredSessionHandle) TotalCost() float64      { return 0 }
func (s *recoveredSessionHandle) Terminate() error {
	return fmt.Errorf("session %s stop unavailable without original child owner", s.id)
}
func (s *recoveredSessionHandle) ForceKill() error {
	return fmt.Errorf("session %s force stop unavailable without original child owner", s.id)
}
func (s *recoveredSessionHandle) VerdictPath() string { return "" }
func (s *recoveredSessionHandle) Controller() AgentController {
	return dispatcher.NoopController()
}

// Discovery cannot acknowledge exit. The original owner must read it back.
func (s *recoveredSessionHandle) Done() <-chan struct{} { return nil }

// scheduleOrderID is the well-known order ID for the scheduler session.
const scheduleOrderID = "schedule"

// Prompt parsing patterns for extracting order IDs from session prompts.
var (
	promptOrderRegexp           = regexp.MustCompile(`(?im)^\[order:([^\]]+)\]`)
	schedulePromptRegexp        = regexp.MustCompile(`(?im)^\s*use skill\([^)]+\)\s+to refresh .+from \.noodle/mise\.json\.`)
	currentSchedulePromptRegexp = regexp.MustCompile(`\AUse Skill\(([^)\r\n]+)\)\. The selected skill is the single owner of project scheduling and output policy\.\n\nRuntime interface:\n- checkout_mode: primary-checkout\n- selected_skill: ([^\r\n]+)\n`)
)

// ReadSessionTarget extracts the order ID from a session's prompt file.
func ReadSessionTarget(promptPath string) string {
	data, err := os.ReadFile(promptPath)
	if err != nil {
		return ""
	}

	orderMatches := promptOrderRegexp.FindStringSubmatch(string(data))
	if len(orderMatches) == 2 {
		return strings.TrimSpace(orderMatches[1])
	}

	if matches := currentSchedulePromptRegexp.FindStringSubmatch(string(data)); len(matches) == 3 && matches[1] == matches[2] {
		return scheduleOrderID
	}

	if schedulePromptRegexp.Match(data) {
		return scheduleOrderID
	}
	return ""
}
