package loop

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/poteto/noodle/event"
	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/procx"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
)

type localTaskMergeRecoveryPacket struct {
	Schema    int                         `json:"schema"`
	Candidate localTaskFileRef            `json:"candidate"`
	Files     map[string]localTaskFileRef `json:"files"`
}

type localTaskMergeRecoveryBinding struct {
	Candidate     localTaskFileRef `json:"candidate"`
	Source        localTaskFileRef `json:"source"`
	SessionID     string           `json:"session_id"`
	AttemptID     string           `json:"attempt_id"`
	WorktreePath  string           `json:"worktree_path"`
	CandidateHead string           `json:"candidate_head"`
	OriginalError string           `json:"original_error"`
}

func PrepareLocalTaskMergeRecovery(candidate, digest, backup, output string) (map[string]any, error) {
	ref := localTaskFileRef{Path: candidate, SHA256: digest}
	c, err := readLocalTaskCandidate(ref)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(backup) || !filepath.IsAbs(output) || withinLocalTaskPath(c.Worktree, backup) || withinLocalTaskPath(c.Worktree, output) {
		return nil, fmt.Errorf("local task recovery paths are not absolute")
	}
	packet := localTaskMergeRecoveryPacket{Schema: 1, Candidate: ref, Files: map[string]localTaskFileRef{}}
	for name, expected := range c.Files {
		if expected == nil {
			continue
		}
		if !localTaskSafePath(name) {
			return nil, fmt.Errorf("local task backup path invalid")
		}
		path := filepath.Join(backup, name)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if recoveryDigest(data) != expected.SHA256 {
			return nil, fmt.Errorf("local task backup differs: %s", name)
		}
		packet.Files[name] = localTaskFileRef{Path: path, SHA256: expected.SHA256}
	}
	data, err := json.MarshalIndent(packet, "", "  ")
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	previous, err := os.ReadFile(output)
	if err == nil {
		if !bytes.Equal(previous, data) {
			return nil, fmt.Errorf("local task recovery output already differs")
		}
	} else if os.IsNotExist(err) {
		file, err := os.OpenFile(output, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return nil, err
		}
		_, writeErr := file.Write(data)
		syncErr := file.Sync()
		closeErr := file.Close()
		if writeErr != nil {
			return nil, writeErr
		}
		if syncErr != nil {
			return nil, syncErr
		}
		if closeErr != nil {
			return nil, closeErr
		}
	} else {
		return nil, err
	}
	return map[string]any{"schema": 1, "status": "prepared", "packet": localTaskFileRef{Path: output, SHA256: recoveryDigest(data)}, "effects": []string{}}, nil
}

func withinLocalTaskPath(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))))
}

func localTaskSafePath(name string) bool {
	return name != "" && name != ".git" && !strings.HasPrefix(name, ".git/") && name != "." && name != ".." && !filepath.IsAbs(name) && filepath.Clean(name) == name && !strings.HasPrefix(name, ".."+string(filepath.Separator))
}

func (l *Loop) originalLocalMergeFailure(order state.OrderNode, b requestChangesBinding, ackID string) error {
	data, err := os.ReadFile(filepath.Join(l.runtimeDir, "control-ack.ndjson"))
	if err != nil {
		return err
	}
	matches := 0
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var ack ControlAck
		if err := json.Unmarshal(line, &ack); err != nil {
			return err
		}
		if ack.ID == ackID {
			if ack.Action != "merge" || ack.Status != "ok" {
				return fmt.Errorf("original merge ACK differs")
			}
			matches++
		}
	}
	if matches != 1 {
		return fmt.Errorf("original merge ACK missing or ambiguous")
	}
	data, err = os.ReadFile(filepath.Join(l.runtimeDir, "loop-events.ndjson"))
	if err != nil {
		return err
	}
	stageFound, orderFound := false, false
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e event.LoopEvent
		if err := json.Unmarshal(line, &e); err != nil {
			return err
		}
		switch e.Type {
		case event.LoopEventStageFailed:
			var f StageFailedPayload
			if err := json.Unmarshal(e.Payload, &f); err != nil {
				return err
			}
			if f.OrderID == order.OrderID {
				stageFound = f.StageIndex == 0 && f.SessionID != nil && *f.SessionID == b.SessionID && f.Reason == b.Reason && f.AgentMistake == nil
				orderFound = false
			}
		case event.LoopEventOrderFailed:
			var f OrderFailedPayload
			if err := json.Unmarshal(e.Payload, &f); err != nil {
				return err
			}
			if f.OrderID == order.OrderID {
				orderFound = stageFound && f.Reason == b.Reason && f.AgentMistake == nil
			}
		case event.LoopEventOrderCompleted, event.LoopEventOrderDropped, event.LoopEventOrderRequeued:
			var f struct {
				OrderID string `json:"order_id"`
			}
			if err := json.Unmarshal(e.Payload, &f); err != nil {
				return err
			}
			if f.OrderID == order.OrderID {
				stageFound, orderFound = false, false
			}
		}
	}
	if !stageFound || !orderFound {
		return fmt.Errorf("original merge failure evidence missing")
	}
	return nil
}

func (l *Loop) controlRecoverLocalTaskMerge(cmd ControlCommand) error {
	if !filepath.IsAbs(cmd.Value) || cmd.Target == "" || cmd.Name == "" {
		return fmt.Errorf("local task merge recovery source missing")
	}
	data, err := os.ReadFile(cmd.Value)
	if err != nil {
		return err
	}
	if recoveryDigest(data) != cmd.Target {
		return fmt.Errorf("local task merge recovery source changed")
	}
	var packet localTaskMergeRecoveryPacket
	if err := decodeStoppedReview(data, &packet); err != nil {
		return err
	}
	if packet.Schema != 1 {
		return fmt.Errorf("local task merge recovery schema differs")
	}
	c, err := readLocalTaskCandidate(packet.Candidate)
	if err != nil {
		return err
	}
	order, ok := l.canonical.Orders[cmd.OrderID]
	if !ok || c.OrderID != cmd.OrderID || len(order.Stages) != 1 || !l.isAdmittedLocalTask(order, order.Stages[0]) {
		return fmt.Errorf("local task original order differs")
	}
	stage := order.Stages[0]
	if stage.Runtime == "openshell" {
		return fmt.Errorf("OpenShell merge restoration is unavailable; preserve the original candidate and inspect its owner")
	}
	if len(stage.Attempts) == 0 {
		return fmt.Errorf("local task original attempt missing")
	}
	attempt := stage.Attempts[len(stage.Attempts)-1]
	b := requestChangesBinding{Reason: attempt.Error, SessionID: attempt.SessionID, AttemptID: attempt.AttemptID, Attempt: len(stage.Attempts) - 1, WorktreeName: attempt.WorktreeName, WorktreePath: l.worktreePath(attempt.WorktreeName), Branch: attempt.WorktreeName, Head: c.Head, Files: map[string]string{}}
	if attempt.Status != state.AttemptFailed || !strings.HasPrefix(b.Reason, "merge failed:") || c.SessionID != b.SessionID || c.AttemptID != b.AttemptID || c.Worktree != b.WorktreePath || b.WorktreeName != cookBaseName(order.OrderID, 0, stage.TaskKey) {
		return fmt.Errorf("local task failed merge custody differs")
	}
	binding := localTaskMergeRecoveryBinding{Candidate: packet.Candidate, Source: localTaskFileRef{Path: cmd.Value, SHA256: cmd.Target}, SessionID: b.SessionID, AttemptID: b.AttemptID, WorktreePath: b.WorktreePath, CandidateHead: c.Head, OriginalError: b.Reason}
	if raw := stage.Extra[localTaskMergeRecoveryKey]; len(raw) > 0 {
		var previous localTaskMergeRecoveryBinding
		if json.Unmarshal(raw, &previous) != nil || !recoveryJSONEqual(previous, binding) {
			return fmt.Errorf("local task prior merge recovery differs")
		}
		if order.Status != state.OrderActive || stage.Status != state.StageReview {
			return fmt.Errorf("local task merge recovery already progressed")
		}
	} else if order.Status != state.OrderFailed || stage.Status != state.StageFailed {
		return fmt.Errorf("local task failed merge state differs")
	}
	if err := l.originalLocalMergeFailure(order, b, cmd.Name); err != nil {
		return err
	}
	if err := l.validateLocalTaskSession(order, stage, b); err != nil {
		return err
	}
	for _, name := range recoverySessionFiles {
		raw, err := os.ReadFile(filepath.Join(l.runtimeDir, "sessions", b.SessionID, name))
		if err != nil {
			return err
		}
		b.Files[name] = recoveryDigest(raw)
	}
	if b.Files["spawn.json"] != c.SpawnSHA256 {
		return fmt.Errorf("local task original spawn changed")
	}
	var process struct {
		SessionID string `json:"session_id"`
		PID       int    `json:"pid"`
	}
	raw, err := os.ReadFile(filepath.Join(l.runtimeDir, "sessions", b.SessionID, "process.json"))
	if err != nil {
		return err
	}
	if json.Unmarshal(raw, &process) != nil || process.SessionID != b.SessionID || process.PID <= 1 {
		return fmt.Errorf("local task original process differs")
	}
	absent, err := procx.ProcessGroupAbsent(process.PID)
	if err != nil {
		return err
	}
	if !absent {
		return fmt.Errorf("local task original process is live")
	}
	var prompt struct {
		WritePaths []string `json:"write_paths"`
	}
	if err := json.Unmarshal([]byte(stage.Prompt), &prompt); err != nil {
		return err
	}
	for name := range c.Files {
		if !localTaskPathAllowed(prompt.WritePaths, name) {
			return fmt.Errorf("local task restoration exceeds admitted paths")
		}
	}
	obsolete, err := l.localTaskObsoleteMerges(order.OrderID, b.WorktreeName, b.AttemptID)
	if err != nil {
		return err
	}
	backups := map[string][]byte{}
	for name, expected := range c.Files {
		if !localTaskSafePath(name) {
			return fmt.Errorf("local task backup path invalid")
		}
		source, exists := packet.Files[name]
		if expected == nil {
			if exists {
				return fmt.Errorf("deleted candidate has backup bytes")
			}
			continue
		}
		if !exists || !filepath.IsAbs(source.Path) || source.SHA256 != expected.SHA256 {
			return fmt.Errorf("local task backup reference differs")
		}
		raw, err := os.ReadFile(source.Path)
		if err != nil {
			return err
		}
		if recoveryDigest(raw) != expected.SHA256 {
			return fmt.Errorf("local task backup bytes differ")
		}
		backups[name] = raw
	}
	if len(packet.Files) != len(backups) {
		return fmt.Errorf("local task backup contains foreign files")
	}
	// The old candidate index must be reconstructible from its retained Git HEAD.
	tree, err := boundGitCommand(l.deps.Execution, l.projectDir, "-C", l.projectDir, "ls-tree", "-rz", c.Head).Output()
	if err != nil {
		return err
	}
	var index bytes.Buffer
	for _, line := range bytes.Split(tree, []byte{0}) {
		if len(line) == 0 {
			continue
		}
		left, path, ok := bytes.Cut(line, []byte{'\t'})
		fields := strings.Fields(string(left))
		if !ok || len(fields) != 3 {
			return fmt.Errorf("local task retained tree malformed")
		}
		fmt.Fprintf(&index, "%s %s 0\t%s%c", fields[0], fields[2], path, 0)
	}
	if recoveryDigest(index.Bytes()) != c.IndexSHA256 {
		return fmt.Errorf("local task original staged index is not recoverable from retained HEAD")
	}
	if l.cooks.activeCooksByOrder[order.OrderID] != nil || l.cooks.adoptedTargets[order.OrderID] != "" {
		return fmt.Errorf("local task original writer is active")
	}
	if err := localTaskRestorationPath(l.projectDir, filepath.Join(".worktrees", b.WorktreeName)); err != nil {
		return err
	}
	intentPath := filepath.Join(l.runtimeDir, "local-task-merge-recovery-"+recoveryDigest([]byte(order.OrderID))+".json")
	intent, _ := json.Marshal(binding)
	previous, readErr := os.ReadFile(intentPath)
	if readErr == nil && !bytes.Equal(previous, intent) {
		return fmt.Errorf("local task prior restoration intent differs")
	}
	if readErr != nil && !os.IsNotExist(readErr) {
		return readErr
	}
	if os.IsNotExist(readErr) {
		if err := filex.WriteFileAtomicDurable(intentPath, intent); err != nil {
			return err
		}
	}
	if _, err := os.Lstat(b.WorktreePath); os.IsNotExist(err) {
		refs, err := recoveryGitWithExecution(l.deps.Execution, l.projectDir, "for-each-ref", "--format=%(refname)", "refs/heads/"+b.Branch)
		if err != nil {
			return err
		}
		if refs != "" {
			return fmt.Errorf("local task restoration branch already exists")
		}
		if _, err := recoveryGitWithExecution(l.deps.Execution, l.projectDir, "worktree", "add", "-b", b.Branch, b.WorktreePath, c.Head); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if err := l.validateRecoveryWorktree(b); err != nil {
		return err
	}
	// Check every destination before restoring the first byte.
	for name := range c.Files {
		if err := localTaskRestorationPath(b.WorktreePath, name); err != nil {
			return err
		}
	}
	for name, expected := range c.Files {
		path := filepath.Join(b.WorktreePath, name)
		if expected == nil {
			raw, err := os.ReadFile(path)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return err
			}
			base, err := boundGitCommand(l.deps.Execution, l.projectDir, "-C", l.projectDir, "show", c.Head+":"+filepath.ToSlash(name)).Output()
			if err != nil || !bytes.Equal(raw, base) {
				return fmt.Errorf("local task deletion would remove changed bytes")
			}
			if err := os.Remove(path); err != nil {
				return err
			}
			continue
		}
		raw, err := os.ReadFile(path)
		if err == nil && recoveryDigest(raw) != expected.SHA256 {
			base, baseErr := boundGitCommand(l.deps.Execution, l.projectDir, "-C", l.projectDir, "show", c.Head+":"+filepath.ToSlash(name)).Output()
			if baseErr != nil || !bytes.Equal(raw, base) {
				return fmt.Errorf("local task restoration would overwrite changed bytes")
			}
		} else if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return err
		}
		if err := filex.WriteFileAtomicDurable(path, backups[name]); err != nil {
			return err
		}
		mode := os.FileMode(0644)
		if expected.Executable {
			mode = 0755
		}
		if err := os.Chmod(path, mode); err != nil {
			return err
		}
	}
	b.WorktreeSHA256, err = l.localTaskWorktreeDigest(stage, b.WorktreePath)
	if err != nil {
		return err
	}
	review := state.PendingReviewNode{OrderID: order.OrderID, StageIndex: 0, TaskKey: stage.TaskKey, Prompt: stage.Prompt, Provider: stage.Provider, Model: stage.Model, Runtime: stage.Runtime, Skill: stage.Skill, Plan: order.Plan, WorktreeName: b.WorktreeName, WorktreePath: b.WorktreePath, SessionID: b.SessionID, Reason: b.Reason}
	if err := l.validateRecovery(order, review, b); err != nil {
		return err
	}
	if err := l.validateLocalTaskCandidate(c, order, b); err != nil {
		return err
	}
	next := l.canonical.Clone()
	if stage.Extra == nil {
		stage.Extra = map[string]json.RawMessage{}
	}
	stage.Extra[localTaskMergeRecoveryKey] = intent
	stage.Status = state.StageReview
	stage.Merge = nil
	order.Status = state.OrderActive
	order.Stages[0] = stage
	next.Orders[order.OrderID] = order
	next.PendingReviews[order.OrderID] = review
	if err := next.Validate(); err != nil {
		return err
	}
	for _, id := range obsolete {
		if err := l.effectLedger.CancelPending(id, "retired by original local-task merge failure recovery", timeNowUTC(l.deps.Now)); err != nil {
			return err
		}
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

func localTaskRestorationPath(root, name string) error {
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		return err
	}
	current := canonical
	for _, part := range strings.Split(name, string(filepath.Separator)) {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("local task restoration path contains symlink")
		}
	}
	return nil
}

func (l *Loop) localTaskObsoleteMerges(orderID, worktree, attemptID string) ([]string, error) {
	ids := []string{}
	for _, record := range l.effectLedger.All() {
		if record.Status != reducer.EffectLedgerPending && record.Status != reducer.EffectLedgerRunning {
			continue
		}
		var p struct {
			OrderID      string `json:"order_id"`
			StageIndex   int    `json:"stage_index"`
			WorktreeName string `json:"worktree_name"`
			AttemptID    string `json:"attempt_id"`
		}
		if err := json.Unmarshal(record.Effect.Payload, &p); err != nil {
			return nil, err
		}
		if p.OrderID != orderID {
			continue
		}
		if record.Status != reducer.EffectLedgerPending || record.Result != nil || p.StageIndex != 0 {
			return nil, fmt.Errorf("local task has unresolved active effect")
		}
		switch record.Effect.Type {
		case reducer.EffectMerge, reducer.EffectCleanup:
			if p.WorktreeName != worktree {
				return nil, fmt.Errorf("local task obsolete worktree effect differs")
			}
		case reducer.EffectDispatch:
			if p.AttemptID != attemptID {
				return nil, fmt.Errorf("local task obsolete dispatch differs")
			}
		default:
			return nil, fmt.Errorf("local task has unknown pending effect")
		}
		ids = append(ids, record.EffectID)
	}
	return ids, nil
}
