package loop

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/poteto/noodle/event"
	"github.com/poteto/noodle/internal/state"
)

// Local tasks use native process completion, not an Issue stage_message.
func (l *Loop) isAdmittedLocalTask(order state.OrderNode, stage state.StageNode) bool {
	var prompt struct {
		Kind       string   `json:"kind"`
		Goal       string   `json:"goal"`
		WritePaths []string `json:"write_paths"`
	}
	return slices.Contains(l.config.Runtime.Process.AdmittedOrders, order.OrderID) &&
		len(order.Stages) == 1 && stage.StageIndex == 0 && stage.TaskKey == "local-task" && stage.Skill == "local-task" &&
		stage.Provider == "codex" && stage.Runtime == "process" && !l.config.Agents.Codex.RequireTypedOutcome &&
		json.Unmarshal([]byte(stage.Prompt), &prompt) == nil && prompt.Kind == "local_task" && prompt.Goal != "" && len(prompt.WritePaths) > 0
}

func (l *Loop) validateLocalTaskSession(order state.OrderNode, stage state.StageNode, b requestChangesBinding) error {
	if !l.isAdmittedLocalTask(order, stage) {
		return fmt.Errorf("local task admission differs")
	}
	found := false
	for _, record := range l.effectLedger.All() {
		if record.EffectID == initialAdmissionEffectID(order.OrderID) {
			if err := l.validateRecoveryAdmission(requestChangesPacket{Order: order, Admission: record}); err != nil {
				return err
			}
			found = true
		}
	}
	if !found {
		return fmt.Errorf("local task initial admission missing")
	}
	events, err := event.NewEventReader(l.runtimeDir).ReadSession(b.SessionID, event.EventFilter{Types: map[event.EventType]struct{}{event.EventStageMessage: {}}})
	if err != nil {
		return err
	}
	if len(events) != 0 {
		return fmt.Errorf("local task has unexpected stage_message")
	}
	dir := filepath.Join(l.runtimeDir, "sessions", b.SessionID)
	var meta struct {
		SessionID string `json:"session_id"`
		Status    string `json:"status"`
		Alive     *bool  `json:"alive"`
		Provider  string `json:"provider"`
		Model     string `json:"model"`
		Runtime   string `json:"runtime"`
	}
	data, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return err
	}
	if json.Unmarshal(data, &meta) != nil || meta.SessionID != b.SessionID || meta.Status != "exited" || meta.Alive == nil || *meta.Alive || meta.Provider != stage.Provider || meta.Model != stage.Model || meta.Runtime != stage.Runtime {
		return fmt.Errorf("local task terminal metadata differs")
	}
	var spawn struct {
		SessionID    string          `json:"session_id"`
		WorktreePath string          `json:"worktree_path"`
		RetryCount   int             `json:"retry_count"`
		Sync         json.RawMessage `json:"sync"`
		Provider     string          `json:"provider"`
		Model        string          `json:"model"`
		Runtime      string          `json:"runtime"`
		Skill        string          `json:"skill"`
	}
	data, err = os.ReadFile(filepath.Join(dir, "spawn.json"))
	if err != nil {
		return err
	}
	if json.Unmarshal(data, &spawn) != nil || spawn.SessionID != b.SessionID || spawn.WorktreePath != b.WorktreePath || spawn.RetryCount != b.Attempt || len(spawn.Sync) != 0 || spawn.Provider != stage.Provider || spawn.Model != stage.Model || spawn.Runtime != stage.Runtime || spawn.Skill != stage.Skill {
		return fmt.Errorf("local task spawn definition differs")
	}
	return nil
}

type localTaskCandidate struct {
	Schema      int    `json:"schema"`
	OrderID     string `json:"order_id"`
	SessionID   string `json:"session_id"`
	AttemptID   string `json:"attempt_id"`
	Worktree    string `json:"worktree"`
	Head        string `json:"head"`
	SpawnSHA256 string `json:"spawn_sha256"`
	IndexSHA256 string `json:"index_sha256"`
	Files       map[string]*struct {
		SHA256     string `json:"sha256"`
		Executable bool   `json:"executable"`
	} `json:"files"`
}

func (l *Loop) validateLocalTaskCandidate(c localTaskCandidate, order state.OrderNode, b requestChangesBinding) error {
	if c.Schema != 1 || c.OrderID != order.OrderID || c.SessionID != b.SessionID || c.AttemptID != b.AttemptID || c.Worktree != b.WorktreePath || c.Head != b.Head || c.SpawnSHA256 != b.Files["spawn.json"] {
		return fmt.Errorf("local task candidate custody differs")
	}
	index, err := boundGitCommand(l.deps.Execution, b.WorktreePath, "-C", b.WorktreePath, "ls-files", "--stage", "-z").Output()
	if err != nil {
		return err
	}
	if recoveryDigest(index) != c.IndexSHA256 {
		return fmt.Errorf("local task candidate index changed")
	}
	var prompt struct {
		WritePaths []string `json:"write_paths"`
	}
	if err := json.Unmarshal([]byte(order.Stages[0].Prompt), &prompt); err != nil {
		return err
	}
	if len(c.Files) < len(prompt.WritePaths) {
		return fmt.Errorf("local task candidate files missing")
	}
	for _, name := range prompt.WritePaths {
		if _, ok := c.Files[name]; !ok {
			return fmt.Errorf("local task candidate admitted file missing")
		}
	}
	return l.validateLocalTaskFiles(c, b.WorktreePath)
}

func (l *Loop) validateLocalTaskFiles(c localTaskCandidate, root string) error {
	for _, args := range [][]string{{"diff", "--name-only", "--no-renames", "-z", "HEAD", "--"}, {"ls-files", "--others", "--exclude-standard", "-z"}} {
		data, err := boundGitCommand(l.deps.Execution, root, append([]string{"-C", root}, args...)...).Output()
		if err != nil {
			return err
		}
		for _, name := range strings.Split(string(data), "\x00") {
			if name != "" {
				if _, ok := c.Files[name]; !ok {
					return fmt.Errorf("local task unreviewed change: %s", name)
				}
			}
		}
	}
	for name, expected := range c.Files {
		if name == "." || name == ".." || filepath.IsAbs(name) || filepath.Clean(name) != name || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("local task candidate path invalid")
		}
		path := filepath.Join(root, name)
		info, err := os.Lstat(path)
		if os.IsNotExist(err) && expected == nil {
			continue
		}
		if err != nil {
			return err
		}
		resolved, err := filepath.EvalSymlinks(path)
		canonicalRoot, rootErr := filepath.EvalSymlinks(root)
		if err != nil || rootErr != nil || resolved != filepath.Join(canonicalRoot, name) || expected == nil || !info.Mode().IsRegular() {
			return fmt.Errorf("local task candidate file identity changed")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if recoveryDigest(data) != expected.SHA256 || (info.Mode()&0111 != 0) != expected.Executable {
			return fmt.Errorf("local task candidate file changed: %s", name)
		}
	}
	return nil
}

// Restore only a retained failed order whose exact request-changes ACK and
// failure events survived. This repairs custody; edit-item/requeue remain explicit.
func (l *Loop) controlRecoverLocalTaskReview(cmd ControlCommand) error {
	if !filepath.IsAbs(cmd.Value) || cmd.Target == "" || cmd.Name == "" {
		return fmt.Errorf("local task recovery source missing")
	}
	data, err := os.ReadFile(cmd.Value)
	if err != nil {
		return err
	}
	if recoveryDigest(data) != cmd.Target {
		return fmt.Errorf("local task recovery source changed")
	}
	var c localTaskCandidate
	if err := json.Unmarshal(data, &c); err != nil {
		return err
	}
	order, ok := l.canonical.Orders[cmd.OrderID]
	if !ok || c.OrderID != cmd.OrderID || len(order.Stages) != 1 || order.Status != state.OrderFailed || order.Stages[0].Status != state.StageFailed {
		return fmt.Errorf("local task failed order missing")
	}
	stage := order.Stages[0]
	if len(stage.Attempts) == 0 || !l.isAdmittedLocalTask(order, stage) {
		return fmt.Errorf("local task failed attempt missing")
	}
	attempt := stage.Attempts[len(stage.Attempts)-1]
	review := state.PendingReviewNode{OrderID: order.OrderID, StageIndex: 0, TaskKey: stage.TaskKey, Prompt: stage.Prompt, Provider: stage.Provider, Model: stage.Model, Runtime: stage.Runtime, Skill: stage.Skill, Plan: order.Plan, WorktreeName: attempt.WorktreeName, WorktreePath: l.worktreePath(attempt.WorktreeName), SessionID: attempt.SessionID, Reason: "request-changes custody restored"}
	pending := &pendingReviewCook{cookIdentity: cookIdentity{orderID: order.OrderID, stageIndex: 0}, sessionID: attempt.SessionID, worktreeName: attempt.WorktreeName, worktreePath: review.WorktreePath}
	b, err := l.captureRequestChanges(pending)
	if err != nil {
		return err
	}
	b.Reason = attempt.Error
	if !strings.HasPrefix(b.Reason, "changes requested") || attempt.Status != state.AttemptFailed {
		return fmt.Errorf("local task request-changes disposition missing")
	}
	if err := l.validateRecovery(order, review, b); err != nil {
		return err
	}
	if err := l.validateLocalTaskCandidate(c, order, b); err != nil {
		return err
	}
	loopBytes, err := os.ReadFile(filepath.Join(l.runtimeDir, "loop-events.ndjson"))
	if err != nil {
		return err
	}
	if err := validateRequestChangesFailure(loopBytes, requestChangesPacket{Order: order, Review: review, Binding: b}); err != nil {
		return err
	}
	ackBytes, err := os.ReadFile(filepath.Join(l.runtimeDir, "control-ack.ndjson"))
	if err != nil {
		return err
	}
	matches := 0
	for _, line := range bytes.Split(ackBytes, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var ack ControlAck
		if err := json.Unmarshal(line, &ack); err != nil {
			return err
		}
		if ack.ID == cmd.Name {
			if ack.Action != "request-changes" || ack.Status != "ok" {
				return fmt.Errorf("local task original control refused")
			}
			matches++
		}
	}
	if matches != 1 {
		return fmt.Errorf("local task original control ACK missing or ambiguous")
	}
	if old, exists := l.canonical.PendingReviews[order.OrderID]; exists {
		existing, err := recoveryBinding(stage)
		if err != nil || !recoveryJSONEqual(existing, b) || !recoveryJSONEqual(old, review) {
			return fmt.Errorf("local task existing custody differs")
		}
		if err := l.projectRecoveredOrder(order.OrderID); err != nil {
			return err
		}
		return l.syncPendingReviewProjection()
	}
	if l.cooks.pendingReview[order.OrderID] != nil {
		return fmt.Errorf("local task pending review projection conflicts")
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return err
	}
	next := l.canonical.Clone()
	if stage.Extra == nil {
		stage.Extra = map[string]json.RawMessage{}
	}
	stage.Extra[requestChangesKey] = raw
	order.Stages[0] = stage
	next.Orders[order.OrderID] = order
	next.PendingReviews[order.OrderID] = review
	if err := next.Validate(); err != nil {
		return err
	}
	l.canonical = next
	if err := l.persistCanonicalCheckpoint(); err != nil {
		return err
	}
	if err := l.projectRecoveredOrder(order.OrderID); err != nil {
		return err
	}
	return l.syncPendingReviewProjection()
}

func (l *Loop) localTaskWorktreeDigest(stage state.StageNode, root string) (string, error) {
	var prompt struct {
		WritePaths []string `json:"write_paths"`
	}
	if err := json.Unmarshal([]byte(stage.Prompt), &prompt); err != nil {
		return "", err
	}
	names := map[string]bool{}
	for _, name := range prompt.WritePaths {
		names[name] = true
	}
	for _, args := range [][]string{{"diff", "--name-only", "--no-renames", "-z", "HEAD", "--"}, {"ls-files", "--others", "--exclude-standard", "-z"}} {
		data, err := boundGitCommand(l.deps.Execution, root, append([]string{"-C", root}, args...)...).Output()
		if err != nil {
			return "", err
		}
		for _, name := range strings.Split(string(data), "\x00") {
			if name != "" {
				names[name] = true
			}
		}
	}
	files := map[string]any{}
	for name := range names {
		if name == "." || name == ".." || filepath.IsAbs(name) || filepath.Clean(name) != name || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("local task worktree path invalid")
		}
		path := filepath.Join(root, name)
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			files[name] = nil
			continue
		}
		if err != nil {
			return "", err
		}
		resolved, err := filepath.EvalSymlinks(path)
		canonicalRoot, rootErr := filepath.EvalSymlinks(root)
		if err != nil || rootErr != nil || resolved != filepath.Join(canonicalRoot, name) || !info.Mode().IsRegular() {
			return "", fmt.Errorf("local task worktree file identity invalid")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		files[name] = map[string]any{"sha256": recoveryDigest(data), "executable": info.Mode()&0111 != 0}
	}
	index, err := boundGitCommand(l.deps.Execution, root, "-C", root, "ls-files", "--stage", "-z").Output()
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(map[string]any{"files": files, "index_sha256": recoveryDigest(index)})
	if err != nil {
		return "", err
	}
	return recoveryDigest(raw), nil
}

func (l *Loop) preserveLocalTaskFailure(id string) bool {
	order, ok := l.canonical.Orders[id]
	return ok && len(order.Stages) == 1 && order.Status == state.OrderFailed && l.isAdmittedLocalTask(order, order.Stages[0])
}

func (l *Loop) localTaskRevisionForDispatch(cand dispatchCandidate, attempt int) (bool, error) {
	order, ok := l.canonical.Orders[cand.OrderID]
	if !ok || len(order.Stages) != 1 || !l.isAdmittedLocalTask(order, order.Stages[0]) {
		return false, nil
	}
	stage := order.Stages[0]
	raw, exists := stage.Extra[requestChangesRequeuedKey]
	if !exists {
		return false, nil
	}
	var receipt requestChangesRequeued
	if err := decodeStoppedReview(raw, &receipt); err != nil {
		return false, err
	}
	if stage.Status != state.StagePending || attempt != len(stage.Attempts) || attempt != receipt.Binding.Attempt+1 || cand.Stage.Prompt != stage.Prompt || cand.StageIndex != 0 {
		return false, fmt.Errorf("local task revision dispatch custody differs")
	}
	if _, exists := l.canonical.PendingReviews[order.OrderID]; exists {
		return false, fmt.Errorf("local task revision still has pending review")
	}
	if err := l.validateRecovery(order, receipt.Review, receipt.Binding); err != nil {
		return false, err
	}
	return true, nil
}
