package loop

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/poteto/noodle/internal/orderx"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
	"github.com/poteto/noodle/internal/statever"
	loopruntime "github.com/poteto/noodle/runtime"
)

type candidateFile struct {
	Mode    uint32 `json:"mode"`
	SHA256  string `json:"sha256"`
	Missing bool   `json:"missing,omitempty"`
}

type candidateManifest struct {
	IndexSHA256 string                   `json:"index_sha256"`
	Files       map[string]candidateFile `json:"files"`
}

func candidateGit(path string, args ...string) ([]byte, error) {
	cmd := exec.Command("git", append([]string{"-C", path}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_OPTIONAL_LOCKS=0")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("candidate git %v: %w", args, err)
	}
	return out, nil
}

func captureCandidate(project, name string) (candidateManifest, string, string, error) {
	m := candidateManifest{Files: map[string]candidateFile{}}
	if name == "" || name == "." || filepath.Base(name) != name {
		return m, "", "", fmt.Errorf("invalid worktree name")
	}
	path := filepath.Join(project, ".worktrees", name)
	root, err := canonicalDirectory(path)
	if err != nil || root != path {
		return m, "", "", fmt.Errorf("candidate worktree path differs")
	}
	top, err := publicationGit(path, "rev-parse", "--show-toplevel")
	if err != nil || top != path {
		return m, "", "", fmt.Errorf("candidate is not a linked checkout")
	}
	owner, err := publicationGit(project, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return m, "", "", err
	}
	common, err := publicationGit(path, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || common != owner {
		return m, "", "", fmt.Errorf("candidate repository differs")
	}
	branch, err := publicationGit(path, "symbolic-ref", "--short", "HEAD")
	if err != nil || (branch != name && branch != "noodle/"+name) {
		return m, "", "", fmt.Errorf("candidate branch differs")
	}
	head, err := exactPublicationObject(path, "HEAD")
	if err != nil {
		return m, "", "", err
	}
	registration, err := publicationGit(project, "worktree", "list", "--porcelain")
	if err != nil {
		return m, "", "", err
	}
	found := false
	for _, block := range strings.Split(registration, "\n\n") {
		if strings.Contains("\n"+block+"\n", "\nbranch refs/heads/"+branch+"\n") {
			if !strings.HasPrefix(block, "worktree "+path+"\n") || !strings.Contains(block, "\nHEAD "+head+"\n") {
				return m, "", "", fmt.Errorf("candidate registration differs")
			}
			found = true
		}
	}
	if !found {
		return m, "", "", fmt.Errorf("candidate registration missing")
	}
	index, err := candidateGit(path, "ls-files", "--stage", "-z")
	if err != nil {
		return m, "", "", err
	}
	m.IndexSHA256 = publicationDigest(index)
	names, err := candidateGit(path, "ls-files", "--cached", "--others", "--exclude-standard", "-z")
	if err != nil {
		return m, "", "", err
	}
	for _, name := range strings.Split(string(names), "\x00") {
		if name == "" {
			continue
		}
		if !filepath.IsLocal(name) {
			return m, "", "", fmt.Errorf("candidate file escapes worktree")
		}
		full := filepath.Join(path, name)
		parent, err := filepath.EvalSymlinks(filepath.Dir(full))
		if err != nil && !os.IsNotExist(err) {
			return m, "", "", err
		}
		if err == nil && parent != path && !strings.HasPrefix(parent, path+string(filepath.Separator)) {
			return m, "", "", fmt.Errorf("candidate parent escapes worktree")
		}
		info, err := os.Lstat(full)
		if os.IsNotExist(err) {
			m.Files[name] = candidateFile{Missing: true}
			continue
		}
		if err != nil {
			return m, "", "", err
		}
		var raw []byte
		switch {
		case info.Mode().IsRegular():
			raw, err = os.ReadFile(full)
		case info.Mode()&os.ModeSymlink != 0:
			var target string
			target, err = os.Readlink(full)
			raw = []byte(target)
		default:
			return m, "", "", fmt.Errorf("unsupported candidate file type: %s", name)
		}
		if err != nil {
			return m, "", "", err
		}
		m.Files[name] = candidateFile{Mode: uint32(info.Mode()), SHA256: publicationDigest(raw)}
	}
	return m, branch, head, nil
}

func interruptionSubject(prompt, subject string) (string, error) {
	var p struct {
		Repository string `json:"repository"`
		Issue      int    `json:"issue"`
		Envelope   string `json:"envelope_sha256"`
	}
	if json.Unmarshal([]byte(prompt), &p) != nil || p.Issue <= 0 || p.Repository+"#"+fmt.Sprint(p.Issue) != subject || len(p.Envelope) != 64 || !publicationSubjectPattern.MatchString(subject) {
		return "", fmt.Errorf("subject does not match admitted stage prompt")
	}
	return p.Envelope, nil
}

func captureInterruption(project, orderID, subject string) (interruptionIntent, error) {
	var i interruptionIntent
	dir := filepath.Join(project, ".noodle")
	raw, err := readAdmissionFile(filepath.Join(dir, "state.snapshot.json"))
	if err != nil {
		return i, err
	}
	var s reducer.DurableSnapshot
	if err := decodeStoppedReview(raw, &s); err != nil {
		return i, err
	}
	if !orderx.ValidOrderRevision(s.OrderRevision) || s.GeneratedAt.IsZero() || s.EffectLedger == nil || s.State.SchemaVersion != statever.Current {
		return i, fmt.Errorf("incomplete interruption snapshot")
	}
	order, ok := s.State.Orders[orderID]
	if !ok || order.OrderID != orderID || order.Status != state.OrderActive || len(order.Stages) != 1 || len(s.State.PendingReviews) != 0 {
		return i, fmt.Errorf("one original active execution stage is required")
	}
	stage := order.Stages[0]
	if stage.StageIndex != 0 || stage.Status != state.StageRunning || stage.Runtime != "process" || stage.Provider != "codex" || stage.TaskKey != "execute" || len(stage.Attempts) == 0 {
		return i, fmt.Errorf("original interrupted process stage is required")
	}
	if _, exists := stage.Extra[interruptionKey]; exists {
		return i, fmt.Errorf("interruption binding already exists")
	}
	ordinal := len(stage.Attempts) - 1
	attempt := stage.Attempts[ordinal]
	if attempt.Status != state.AttemptRunning || attempt.ExitCode != nil || !attempt.CompletedAt.IsZero() || attempt.AttemptID != dispatchAttemptID(orderID, 0, ordinal) || attempt.SessionID == "" || attempt.SessionID == "." || filepath.Base(attempt.SessionID) != attempt.SessionID || attempt.WorktreeName != cookBaseName(orderID, 0, stage.TaskKey) {
		return i, fmt.Errorf("original running attempt identity differs")
	}
	envelope, err := interruptionSubject(stage.Prompt, subject)
	if err != nil {
		return i, err
	}
	remote, err := publicationGit(project, "remote", "get-url", "origin")
	if err != nil {
		return i, err
	}
	repo, err := githubRepository(remote)
	if err != nil || repo+"#"+strings.Split(subject, "#")[1] != subject {
		return i, fmt.Errorf("subject repository differs from project")
	}
	// Validate other owners without treating this observed interruption as a writer outcome.
	checked := s
	checked.State = s.State.Clone()
	normalized := checked.State.Orders[orderID]
	normalized.Stages[0].Status = state.StagePending
	normalized.Stages[0].Attempts[ordinal].Status = state.AttemptCancelled
	checked.State.Orders[orderID] = normalized
	if err := validateAdmissionSnapshot(checked); err != nil {
		return i, err
	}
	if err := observeAdmissionSessions(dir, s.State); err != nil {
		return i, err
	}
	for id, other := range s.State.Orders {
		if id != orderID && id != scheduleOrderID && !other.Status.IsTerminal() {
			return i, fmt.Errorf("foreign nonterminal order %s", id)
		}
	}
	if err := validateInterruptionLedger(s, orderID); err != nil {
		return i, err
	}
	orders, err := readOrders(filepath.Join(dir, "orders.json"))
	if err != nil {
		return i, err
	}
	// The existing projection validator accepts active mirrors only for reviews.
	projected := s.State.Clone()
	node := projected.Orders[orderID]
	node.Stages[0].Status = state.StageReview
	projected.Orders[orderID] = node
	if err := validateStoppedProjection(projected, orders, true); err != nil {
		return i, err
	}
	c := InterruptionCustody{OrderID: orderID, StageIndex: 0, AttemptID: attempt.AttemptID, SessionID: attempt.SessionID, Subject: subject, EnvelopeSHA256: envelope, WorktreeName: attempt.WorktreeName, WorktreePath: filepath.Join(project, ".worktrees", attempt.WorktreeName), SessionSHA256: map[string]string{}}
	entries, err := os.ReadDir(filepath.Join(dir, "sessions"))
	if err != nil {
		return i, err
	}
	known := map[string]bool{}
	for _, o := range s.State.Orders {
		for _, st := range o.Stages {
			for _, a := range st.Attempts {
				known[a.SessionID] = true
			}
		}
	}
	for _, entry := range entries {
		if !known[entry.Name()] {
			if err := validateHistoricalScheduleSession(project, filepath.Join(dir, "sessions", entry.Name())); err != nil {
				return i, fmt.Errorf("foreign session %s: %w", entry.Name(), err)
			}
		}
	}
	for _, name := range []string{"spawn.json", "prompt.txt", "events.ndjson", "process.json", "raw.ndjson"} {
		data, err := readAdmissionFile(filepath.Join(dir, "sessions", c.SessionID, name))
		if err != nil {
			return i, err
		}
		c.SessionSHA256[name] = publicationDigest(data)
		if name == "events.ndjson" || name == "raw.ndjson" {
			if err := noInterruptionTerminal(data); err != nil {
				return i, err
			}
		}
	}
	data, err := readAdmissionFile(filepath.Join(dir, "sessions", c.SessionID, "spawn.json"))
	if err != nil {
		return i, err
	}
	var spawn struct {
		SessionID    string `json:"session_id"`
		WorktreePath string `json:"worktree_path"`
		Provider     string `json:"provider"`
		Model        string `json:"model"`
		Runtime      string `json:"runtime"`
		RetryCount   int    `json:"retry_count"`
	}
	if json.Unmarshal(data, &spawn) != nil || spawn.SessionID != c.SessionID || spawn.WorktreePath != c.WorktreePath || spawn.Provider != stage.Provider || spawn.Model != stage.Model || spawn.Runtime != stage.Runtime || spawn.RetryCount != ordinal {
		return i, fmt.Errorf("original spawn identity differs")
	}
	promptPath := filepath.Join(dir, "sessions", c.SessionID, "prompt.txt")
	originalPrompt, err := readAdmissionFile(promptPath)
	if err != nil {
		return i, err
	}
	if loopruntime.ReadSessionTarget(promptPath) != orderID || !strings.Contains(string(originalPrompt), stage.Prompt) {
		return i, fmt.Errorf("original session prompt does not contain admitted order and stage")
	}
	c.CandidateManifest, c.Branch, c.Head, err = captureCandidate(project, c.WorktreeName)
	if err != nil {
		return i, err
	}
	again, err := readAdmissionFile(filepath.Join(dir, "state.snapshot.json"))
	if err != nil || !bytes.Equal(raw, again) {
		return i, fmt.Errorf("snapshot changed during inspection")
	}
	i.Before, i.Custody = raw, c
	i.Digest = interruptionDigest(i)
	return i, nil
}

func noInterruptionTerminal(data []byte) error {
	pending := map[string]bool{}
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event struct {
			Item struct {
				ID   string `json:"id"`
				Type string `json:"type"`
			} `json:"item"`
			Type    string `json:"type"`
			Payload struct {
				Outcome string `json:"outcome"`
			} `json:"payload"`
		}
		if json.Unmarshal(line, &event) != nil {
			return fmt.Errorf("session event readback is incomplete")
		}
		if event.Type == "item.started" && (event.Item.Type == "command_execution" || event.Item.Type == "mcp_tool_call" || event.Item.Type == "file_change") {
			if event.Item.ID == "" {
				return fmt.Errorf("started effect has no item identity")
			}
			pending[event.Item.ID] = true
		}
		if event.Type == "item.completed" {
			delete(pending, event.Item.ID)
		}
		if event.Type == "turn.completed" || event.Type == "turn.failed" || event.Type == "complete" || event.Type == "result" || event.Type == "stage_yield" || (event.Type == "stage_message" && event.Payload.Outcome != "") {
			return fmt.Errorf("terminal session evidence requires its completion owner")
		}
	}
	if len(pending) != 0 {
		return fmt.Errorf("unfinished session effects require original owner readback")
	}
	return nil
}

func validateInterruptionLedger(s reducer.DurableSnapshot, orderID string) error {
	for _, r := range s.EffectLedger {
		if r.Status != reducer.EffectLedgerPending {
			continue
		}
		if r.Attempts != 0 || !r.LastAttemptAt.IsZero() {
			return fmt.Errorf("pending effect has execution history: %s", r.EffectID)
		}
		var p struct {
			OrderID    string `json:"order_id"`
			StageIndex int    `json:"stage_index"`
			AttemptID  string `json:"attempt_id"`
		}
		if json.Unmarshal(r.Effect.Payload, &p) != nil {
			return fmt.Errorf("pending effect payload is unknown")
		}
		if r.Effect.Type == reducer.EffectWriteProjection {
			continue
		}
		if r.Effect.Type != reducer.EffectDispatch {
			return fmt.Errorf("pending effect requires owner readback: %s", r.EffectID)
		}
		o, st, ok := s.State.LookupStage(p.OrderID, p.StageIndex)
		if !ok {
			return fmt.Errorf("pending dispatch identity is unknown")
		}
		matched := false
		for _, a := range st.Attempts {
			if a.AttemptID == p.AttemptID && a.SessionID != "" {
				matched = true
			}
		}
		// Schedule dispatch effects are declarative. Its completed sessions are retained outside the renewable schedule row.
		if o.OrderID == scheduleOrderID && st.Status == state.StagePending && len(st.Attempts) == 0 {
			matched = true
		}
		if !matched || (p.OrderID != orderID && p.OrderID != scheduleOrderID && !o.Status.IsTerminal()) {
			return fmt.Errorf("pending dispatch requires original session readback")
		}
	}
	return nil
}

func validateHistoricalScheduleSession(project, sessionDir string) error {
	promptPath := filepath.Join(sessionDir, "prompt.txt")
	if loopruntime.ReadSessionTarget(promptPath) != scheduleOrderID {
		return fmt.Errorf("prompt does not identify scheduling owner")
	}
	data, err := readAdmissionFile(filepath.Join(sessionDir, "spawn.json"))
	if err != nil {
		return err
	}
	var spawn struct {
		SessionID    string `json:"session_id"`
		Skill        string `json:"skill"`
		Runtime      string `json:"runtime"`
		WorktreePath string `json:"worktree_path"`
	}
	if err := json.Unmarshal(data, &spawn); err != nil {
		return err
	}
	if spawn.SessionID != filepath.Base(sessionDir) || spawn.Runtime != "process" || spawn.WorktreePath != project || strings.TrimSpace(spawn.Skill) == "" {
		return fmt.Errorf("schedule spawn does not identify this control root")
	}
	prompt, err := readAdmissionFile(promptPath)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(string(prompt), "Use Skill("+spawn.Skill+")") {
		return fmt.Errorf("schedule prompt differs from spawn skill")
	}
	return nil
}
