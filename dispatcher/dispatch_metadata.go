package dispatcher

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/poteto/noodle/internal/filex"
)

type dispatchMetadata struct {
	SessionID       string    `json:"session_id"`
	Provider        string    `json:"provider"`
	Model           string    `json:"model"`
	Runtime         string    `json:"runtime,omitempty"`
	Skill           string    `json:"skill,omitempty"`
	WorktreePath    string    `json:"worktree_path,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	DispatchWarning string    `json:"dispatch_warning,omitempty"`
	DisplayName     string    `json:"display_name,omitempty"`
	Title           string    `json:"title,omitempty"`
	RetryCount      int       `json:"retry_count,omitempty"`
	ResumeSessionID string    `json:"resume_session_id,omitempty"`
	ResumeThreadID  string    `json:"resume_thread_id,omitempty"`
	ResumeSourceSHA string    `json:"resume_source_sha256,omitempty"`
	TaskKey         string    `json:"task_key,omitempty"`
	OrderID         string    `json:"order_id,omitempty"`
	StageIndex      string    `json:"stage_index,omitempty"`
}

func writeDispatchMetadata(
	runtimeDir string,
	sessionID string,
	req DispatchRequest,
	createdAt time.Time,
) error {
	runtimeDir = strings.TrimSpace(runtimeDir)
	sessionID = strings.TrimSpace(sessionID)
	if runtimeDir == "" {
		return fmt.Errorf("runtime directory is required")
	}
	if sessionID == "" {
		return fmt.Errorf("session ID is required")
	}

	path := filepath.Join(runtimeDir, "sessions", sessionID, "spawn.json")

	payload, err := json.Marshal(dispatchMetadata{
		SessionID:       sessionID,
		Provider:        strings.TrimSpace(req.Provider),
		Model:           strings.TrimSpace(req.Model),
		Runtime:         strings.TrimSpace(req.Runtime),
		Skill:           strings.TrimSpace(req.Skill),
		WorktreePath:    strings.TrimSpace(req.WorktreePath),
		CreatedAt:       createdAt.UTC(),
		DispatchWarning: strings.TrimSpace(req.DispatchWarning),
		DisplayName:     strings.TrimSpace(req.DisplayName),
		Title:           strings.TrimSpace(req.Title),
		RetryCount:      req.RetryCount,
		ResumeSessionID: req.ResumeSessionID,
		ResumeThreadID:  req.codexThreadID,
		ResumeSourceSHA: req.codexThreadSourceSHA,
		TaskKey:         req.TaskKey,
		OrderID:         req.EnvVars["NOODLE_ORDER_ID"],
		StageIndex:      req.EnvVars["NOODLE_STAGE_INDEX"],
	})
	if err != nil {
		return fmt.Errorf("encode spawn metadata: %w", err)
	}
	if err := filex.WriteFileAtomic(path, payload); err != nil {
		return fmt.Errorf("rename spawn metadata: %w", err)
	}
	return nil
}
