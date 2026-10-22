package dispatcher

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/poteto/noodle/internal/procx"
)

type CodexResumeRefusal struct {
	SessionID       string            `json:"session_id"`
	ResumeSessionID string            `json:"resume_session_id"`
	ThreadID        string            `json:"thread_id"`
	Reason          string            `json:"reason"`
	WorktreePath    string            `json:"worktree_path"`
	TaskKey         string            `json:"task_key"`
	OrderID         string            `json:"order_id"`
	StageIndex      string            `json:"stage_index"`
	Files           map[string]string `json:"files"`
}

// Only the original resume storage-missing refusal precedes thread.started and
// TurnStart. notLoaded, transport errors and empty stdout alone prove nothing.
func ReadCodexResumeRefusal(runtimeDir, sessionID string) (*CodexResumeRefusal, error) {
	if sessionID == "" || filepath.Base(sessionID) != sessionID || sessionID == "." || sessionID == ".." {
		return nil, fmt.Errorf("native resume failure session identity invalid")
	}
	dir := filepath.Join(runtimeDir, "sessions", sessionID)
	files := map[string]string{}
	read := func(name string) ([]byte, error) {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		hash := sha256.Sum256(data)
		files[name] = hex.EncodeToString(hash[:])
		return data, nil
	}
	data, err := read("spawn.json")
	if err != nil {
		return nil, err
	}
	var spawn dispatchMetadata
	if json.Unmarshal(data, &spawn) != nil || spawn.SessionID != sessionID {
		return nil, fmt.Errorf("native resume failure spawn differs")
	}
	if spawn.ResumeSessionID == "" || spawn.Provider != "codex" || spawn.DegradedFromSessionID != "" {
		return nil, nil
	}
	if !codexThreadUUID.MatchString(spawn.ResumeThreadID) {
		return nil, fmt.Errorf("native resume failure thread identity invalid")
	}
	data, err = read("codex-startup.json")
	if err != nil {
		return nil, err
	}
	var startup codexStartupReceipt
	if json.Unmarshal(data, &startup) != nil || startup.SessionID != sessionID || startup.PersistenceError != "" {
		return nil, fmt.Errorf("native resume failure startup observation invalid")
	}
	if startup.FirstInitAt != nil || startup.FirstStdout != nil || startup.NativeThreadID != "" {
		return nil, nil
	}
	// These observations may refresh after exit; revalidate their facts on read.
	delete(files, "codex-startup.json")
	data, err = read("raw.ndjson")
	if err != nil {
		return nil, err
	}
	if len(bytes.TrimSpace(data)) != 0 {
		return nil, nil
	}
	data, err = read("stderr.log")
	if err != nil {
		return nil, err
	}
	want := "Error: thread/resume: thread/resume failed: no rollout found for thread id " + spawn.ResumeThreadID + " (code -32600)"
	matches := 0
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == want {
			matches++
		} else if strings.HasPrefix(line, "Error:") {
			return nil, nil
		}
	}
	if matches != 1 {
		return nil, nil
	}
	data, err = read("meta.json")
	if err != nil {
		return nil, err
	}
	var meta struct {
		SessionID string `json:"session_id"`
		Status    string `json:"status"`
		Alive     *bool  `json:"alive"`
	}
	if json.Unmarshal(data, &meta) != nil || meta.SessionID != sessionID || meta.Status != "exited" || meta.Alive == nil || *meta.Alive {
		return nil, fmt.Errorf("native resume failure exit unconfirmed")
	}
	delete(files, "meta.json")
	data, err = read("process.json")
	if err != nil {
		return nil, err
	}
	var process struct {
		SessionID string `json:"session_id"`
	}
	if json.Unmarshal(data, &process) != nil || process.SessionID != sessionID {
		return nil, fmt.Errorf("native resume failure process identity differs")
	}
	absent, err := procx.RecordedProcessGroupAbsent(filepath.Join(dir, "process.json"))
	if err != nil {
		return nil, err
	}
	if !absent {
		return nil, fmt.Errorf("native resume failure process group exit unconfirmed")
	}
	return &CodexResumeRefusal{SessionID: sessionID, ResumeSessionID: spawn.ResumeSessionID, ThreadID: spawn.ResumeThreadID, Reason: want, WorktreePath: spawn.WorktreePath, TaskKey: spawn.TaskKey, OrderID: spawn.OrderID, StageIndex: spawn.StageIndex, Files: files}, nil
}
