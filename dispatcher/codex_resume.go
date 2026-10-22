package dispatcher

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/poteto/noodle/internal/procx"
)

var codexThreadUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Read the original stream without rewriting it or substituting a recent thread.
func readCodexThread(path string) (string, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	hash := sha256.New()
	decoder := json.NewDecoder(io.TeeReader(f, hash))
	id := ""
	for {
		var row struct {
			Type     string `json:"type"`
			ThreadID string `json:"thread_id"`
		}
		if err := decoder.Decode(&row); err != nil {
			if err == io.EOF {
				break
			}
			return "", "", fmt.Errorf("Codex native stream incomplete: %w", err)
		}
		if row.Type != "thread.started" {
			continue
		}
		if !codexThreadUUID.MatchString(row.ThreadID) {
			return "", "", fmt.Errorf("Codex native thread identity invalid")
		}
		if id != "" && id != row.ThreadID {
			return "", "", fmt.Errorf("Codex native thread identity ambiguous")
		}
		id = row.ThreadID
	}
	if id == "" {
		return "", "", fmt.Errorf("Codex native thread identity not observed")
	}
	return id, hex.EncodeToString(hash.Sum(nil)), nil
}

func readResumeJSON(path string, target any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("read resume evidence %s: %w", filepath.Base(path), err)
	}
	return nil
}

func (d *ProcessDispatcher) resolveCodexResume(req DispatchRequest) (DispatchRequest, error) {
	return d.resolveCodexResumeChain(req, map[string]bool{})
}

func (d *ProcessDispatcher) resolveCodexResumeChain(req DispatchRequest, seen map[string]bool) (DispatchRequest, error) {
	if req.ResumeSessionID == "" {
		return req, nil
	}
	id := req.ResumeSessionID
	if seen[id] {
		return req, fmt.Errorf("resume predecessor cycle")
	}
	seen[id] = true
	if id == "." || id == ".." || filepath.Base(id) != id || strings.ContainsAny(id, `/\`) || strings.TrimSpace(id) != id {
		return req, fmt.Errorf("resume predecessor identity invalid")
	}
	if req.Provider != "codex" || d.runtimeDefault != "" {
		return req, fmt.Errorf("native Codex resume unavailable for selected runtime")
	}
	dir := filepath.Join(d.runtimeDir, "sessions", id)
	var spawn dispatchMetadata
	if err := readResumeJSON(filepath.Join(dir, "spawn.json"), &spawn); err != nil {
		return req, err
	}
	if spawn.SessionID != id || spawn.Provider != req.Provider || spawn.Skill != req.Skill || spawn.WorktreePath != req.WorktreePath {
		return req, fmt.Errorf("resume predecessor provider, role or worktree differs")
	}
	if (spawn.TaskKey != "" && spawn.TaskKey != req.TaskKey) ||
		(spawn.OrderID != "" && spawn.OrderID != req.EnvVars["NOODLE_ORDER_ID"]) ||
		(spawn.StageIndex != "" && spawn.StageIndex != req.EnvVars["NOODLE_STAGE_INDEX"]) {
		return req, fmt.Errorf("resume predecessor task type, order or stage differs")
	}
	var meta struct {
		SessionID string `json:"session_id"`
		Status    string `json:"status"`
		Alive     *bool  `json:"alive"`
	}
	if err := readResumeJSON(filepath.Join(dir, "meta.json"), &meta); err != nil {
		return req, err
	}
	if meta.SessionID != id || (meta.Status != "exited" && meta.Status != "failed") || meta.Alive == nil || *meta.Alive {
		return req, fmt.Errorf("resume predecessor exit unconfirmed")
	}
	var process struct {
		SessionID string `json:"session_id"`
	}
	path := filepath.Join(dir, "process.json")
	if err := readResumeJSON(path, &process); err != nil {
		return req, err
	}
	if process.SessionID != id {
		return req, fmt.Errorf("resume predecessor process identity differs")
	}
	absent, err := procx.RecordedProcessGroupAbsent(path)
	if err != nil {
		return req, err
	}
	if !absent {
		return req, fmt.Errorf("resume predecessor process group exit unconfirmed")
	}
	thread, digest, err := readCodexThread(filepath.Join(dir, "raw.ndjson"))
	if err != nil {
		if !codexResumeParserRefusal(dir, id) || spawn.ResumeSessionID == "" {
			return req, err
		}
		parent := req
		parent.ResumeSessionID = spawn.ResumeSessionID
		bound, parentErr := d.resolveCodexResumeChain(parent, seen)
		if parentErr != nil {
			return req, parentErr
		}
		if bound.codexThreadID != spawn.ResumeThreadID || bound.codexThreadSourceSHA != spawn.ResumeSourceSHA {
			return req, fmt.Errorf("resume refused launch predecessor source changed")
		}
		req.codexThreadID, req.codexThreadSourceSHA = bound.codexThreadID, bound.codexThreadSourceSHA
		return req, nil
	}
	data, err := os.ReadFile(filepath.Join(dir, "codex-startup.json"))
	if err == nil {
		var observed codexStartupReceipt
		if json.Unmarshal(data, &observed) != nil || observed.SessionID != id || observed.PersistenceError != "" {
			return req, fmt.Errorf("resume predecessor startup observation invalid")
		}
		if observed.NativeThreadID != "" && (observed.NativeThreadID != thread || observed.NativeThreadSourceSHA256 != digest || observed.NativeThreadError != "") {
			return req, fmt.Errorf("resume predecessor native source changed")
		}
	} else if !os.IsNotExist(err) {
		return req, err
	}
	req.codexThreadID = thread
	req.codexThreadSourceSHA = digest
	return req, nil
}

// A CLI parser refusal never entered a native turn. An explicit retry may follow
// its saved predecessor; transport failures and missing output remain unknown.
func codexResumeParserRefusal(dir, id string) bool {
	raw, err := os.ReadFile(filepath.Join(dir, "raw.ndjson"))
	if err != nil || len(raw) != 0 {
		return false
	}
	var observed codexStartupReceipt
	if readResumeJSON(filepath.Join(dir, "codex-startup.json"), &observed) != nil || observed.SessionID != id || observed.PersistenceError != "" || observed.FirstStdout != nil || observed.FirstInitAt != nil || observed.FirstStderr == nil {
		return false
	}
	stderr, err := os.ReadFile(filepath.Join(dir, "stderr.log"))
	if err != nil {
		return false
	}
	text := string(stderr)
	if !strings.HasPrefix(text, "error: unexpected argument '") || !strings.Contains(text, "' found\n") || !strings.Contains(text, "\nUsage: codex exec resume ") || !strings.HasSuffix(text, "For more information, try '--help'.\n") {
		return false
	}
	return strings.Count(text, "error:") == 1 && !strings.Contains(text, "Error:")
}
