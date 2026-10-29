package loop

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/event"
	"github.com/poteto/noodle/internal/execution"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
)

const localTaskCompletionRecoveryKey = "local_task_completion_recovery"

type localTaskCompletionTerminal struct {
	SessionID string `json:"session_id"`
	Status    string `json:"status"`
	Alive     bool   `json:"alive"`
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	Runtime   string `json:"runtime"`
}

type localTaskCompletionPredecessor struct {
	Packet    localTaskFileRef `json:"packet"`
	ControlID string           `json:"control_id"`
}

type localTaskCompletionPacket struct {
	Schema       int                             `json:"schema"`
	Project      string                          `json:"project"`
	Order        state.OrderNode                 `json:"order"`
	Binding      requestChangesBinding           `json:"binding"`
	BaseHead     string                          `json:"base_head"`
	BaseBranch   string                          `json:"base_branch"`
	EventsSHA256 string                          `json:"events_sha256"`
	Effects      []reducer.EffectLedgerRecord    `json:"effects"`
	Terminal     *localTaskCompletionTerminal    `json:"terminal,omitempty"`
	Predecessor  *localTaskCompletionPredecessor `json:"predecessor,omitempty"`
}

// Preparation only reads native custody and writes one external immutable packet.
func PrepareLocalTaskCompletionRecovery(project string, cfg config.Config, id, output string, predecessor ...string) (map[string]any, error) {
	project, err := filepath.Abs(project)
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(output) || withinLocalTaskPath(project, output) {
		return nil, fmt.Errorf("completion recovery output is not external")
	}
	l := &Loop{projectDir: project, runtimeDir: filepath.Join(project, ".noodle"), config: cfg, deps: Dependencies{Execution: execution.Capture(project)}}
	loaded, err := l.loadCanonicalSnapshot()
	if err != nil {
		return nil, err
	}
	if !loaded {
		return nil, fmt.Errorf("completion recovery snapshot missing")
	}
	packet, err := l.captureLocalTaskCompletion(l.canonical.Orders[id])
	if err != nil {
		return nil, err
	}
	// A concurrent checkpoint change cannot silently enter the prepared custody.
	before := l.canonical.Orders[id]
	if _, err := l.loadCanonicalSnapshot(); err != nil {
		return nil, err
	}
	if !recoveryJSONEqual(before, l.canonical.Orders[id]) {
		return nil, fmt.Errorf("completion recovery order changed during preparation")
	}
	again, err := l.captureLocalTaskCompletion(l.canonical.Orders[id])
	if err != nil {
		return nil, err
	}
	if !recoveryJSONEqual(packet, again) {
		return nil, fmt.Errorf("completion recovery custody changed during preparation")
	}
	if len(predecessor) != 0 {
		if len(predecessor) != 3 {
			return nil, fmt.Errorf("completion recovery predecessor arguments differ")
		}
		packet.Predecessor = &localTaskCompletionPredecessor{Packet: localTaskFileRef{Path: predecessor[0], SHA256: predecessor[1]}, ControlID: predecessor[2]}
		if err := l.validateCompletionPredecessor(packet); err != nil {
			return nil, err
		}
	}
	data, err := json.MarshalIndent(packet, "", "  ")
	if err != nil {
		return nil, err
	}
	data = append(data, '\n')
	previous, err := os.ReadFile(output)
	if err == nil {
		if !bytes.Equal(previous, data) {
			return nil, fmt.Errorf("completion recovery output already differs")
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

func (l *Loop) captureLocalTaskCompletion(order state.OrderNode) (localTaskCompletionPacket, error) {
	p := localTaskCompletionPacket{Schema: 2, Project: l.projectDir, Order: order, Effects: []reducer.EffectLedgerRecord{}}
	if order.Status != state.OrderCompleted || len(order.Stages) != 1 || !l.isAdmittedLocalTask(order, order.Stages[0]) {
		return p, fmt.Errorf("local task completed admission missing")
	}
	stage := order.Stages[0]
	if stage.Status != state.StageCompleted || stage.Merge != nil || len(stage.Attempts) == 0 || len(stage.Extra[localTaskMergeKey]) != 0 {
		return p, fmt.Errorf("local task completion was merged or lacks terminal custody")
	}
	attempt := stage.Attempts[len(stage.Attempts)-1]
	if attempt.Status != state.AttemptCompleted || attempt.Error != "" {
		return p, fmt.Errorf("local task terminal attempt is not completed")
	}
	pending := &pendingReviewCook{cookIdentity: cookIdentity{orderID: order.OrderID, stageIndex: 0}, sessionID: attempt.SessionID, worktreeName: attempt.WorktreeName, worktreePath: l.worktreePath(attempt.WorktreeName)}
	b, err := l.captureRequestChanges(pending)
	if err != nil {
		return p, err
	}
	meta, err := os.ReadFile(filepath.Join(l.runtimeDir, "sessions", b.SessionID, "meta.json"))
	if err != nil {
		return p, err
	}
	var terminal localTaskCompletionTerminal
	if err := json.Unmarshal(meta, &terminal); err != nil {
		return p, err
	}
	p.Terminal = &terminal
	p.Binding = b
	review := completionPendingReview(order, b)
	if err := l.validateRecovery(order, review, b); err != nil {
		return p, err
	}
	dirty, err := recoveryGitWithExecution(l.deps.Execution, b.WorktreePath, "status", "--porcelain")
	if err != nil {
		return p, err
	}
	if dirty == "" {
		return p, fmt.Errorf("local task completion has no retained dirty candidate")
	}
	// The defect occurred without a commit; recovery never adopts a different base.
	p.BaseHead, err = recoveryGitWithExecution(l.deps.Execution, l.projectDir, "rev-parse", "HEAD")
	if err != nil {
		return p, err
	}
	if p.BaseHead != b.Head {
		return p, fmt.Errorf("local task completion base changed")
	}
	p.BaseBranch, err = recoveryGitWithExecution(l.deps.Execution, l.projectDir, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return p, err
	}
	raw, err := os.ReadFile(filepath.Join(l.runtimeDir, "integration-branch.json"))
	if err != nil {
		return p, err
	}
	var integration struct {
		Branch string `json:"branch"`
		Root   string `json:"root"`
	}
	ownerRoot, rootErr := canonicalDirectory(l.projectDir)
	var boundRoot string
	var boundErr error
	if json.Unmarshal(raw, &integration) != nil {
		return p, fmt.Errorf("local task completion integration binding invalid")
	}
	boundRoot, boundErr = canonicalDirectory(integration.Root)
	if rootErr != nil || boundErr != nil || boundRoot != ownerRoot || integration.Branch != p.BaseBranch {
		return p, fmt.Errorf("local task completion integration binding differs")
	}
	var prompt struct {
		WritePaths []string `json:"write_paths"`
	}
	if err := json.Unmarshal([]byte(stage.Prompt), &prompt); err != nil {
		return p, err
	}
	for _, args := range [][]string{{"diff", "--name-only", "--no-renames", "-z", "HEAD", "--"}, {"ls-files", "--others", "--exclude-standard", "-z"}} {
		raw, err := boundGitCommand(l.deps.Execution, b.WorktreePath, append([]string{"-C", b.WorktreePath}, args...)...).Output()
		if err != nil {
			return p, err
		}
		for _, name := range bytes.Split(raw, []byte{0}) {
			if len(name) > 0 && !localTaskPathAllowed(prompt.WritePaths, string(name)) {
				return p, fmt.Errorf("local task completion exceeds admitted paths: %s", name)
			}
		}
	}
	data, err := os.ReadFile(filepath.Join(l.runtimeDir, "loop-events.ndjson"))
	if err != nil {
		return p, err
	}
	p.EventsSHA256 = recoveryDigest(data)
	stageFound, orderFound := false, false
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e event.LoopEvent
		if err := json.Unmarshal(line, &e); err != nil {
			return p, err
		}
		var identity struct {
			OrderID string `json:"order_id"`
		}
		if err := json.Unmarshal(e.Payload, &identity); err != nil {
			return p, err
		}
		if identity.OrderID != order.OrderID {
			continue
		}
		switch e.Type {
		case event.LoopEventStageCompleted:
			var completed StageCompletedPayload
			if err := json.Unmarshal(e.Payload, &completed); err != nil {
				return p, err
			}
			stageFound = completed.StageIndex == 0 && completed.TaskKey == stage.TaskKey && completed.SessionID != nil && *completed.SessionID == b.SessionID
			orderFound = false
		case event.LoopEventOrderCompleted:
			orderFound = stageFound
		case event.LoopEventWorktreeMerged:
			return p, fmt.Errorf("local task completion already has merge evidence")
		case event.LoopEventOrderFailed, event.LoopEventOrderDropped, event.LoopEventOrderRequeued:
			stageFound, orderFound = false, false
		}
	}
	if !stageFound || !orderFound {
		return p, fmt.Errorf("local task original completion evidence missing")
	}
	for _, record := range l.effectLedger.All() {
		if record.Status != reducer.EffectLedgerPending && record.Status != reducer.EffectLedgerRunning {
			continue
		}
		var identity struct {
			OrderID    string `json:"order_id"`
			AttemptID  string `json:"attempt_id"`
			StageIndex int    `json:"stage_index"`
		}
		if err := json.Unmarshal(record.Effect.Payload, &identity); err != nil {
			return p, err
		}
		if identity.OrderID != order.OrderID {
			continue
		}
		if record.Status != reducer.EffectLedgerPending || record.Result != nil {
			return p, fmt.Errorf("local task completion has unresolved running effect")
		}
		switch record.Effect.Type {
		case reducer.EffectDispatch:
			found := false
			for _, a := range stage.Attempts {
				if a.AttemptID == identity.AttemptID && (a.Status == state.AttemptCompleted || a.Status == state.AttemptFailed) {
					found = true
				}
			}
			if !found || identity.StageIndex != 0 {
				return p, fmt.Errorf("local task completion dispatch custody differs")
			}
		case reducer.EffectWriteProjection, reducer.EffectAck:
		default:
			return p, fmt.Errorf("local task completion has unresolved effect %s", record.Effect.Type)
		}
		p.Effects = append(p.Effects, record)
	}
	return p, nil
}

func completionPendingReview(order state.OrderNode, b requestChangesBinding) state.PendingReviewNode {
	stage := order.Stages[0]
	return state.PendingReviewNode{OrderID: order.OrderID, StageIndex: 0, TaskKey: stage.TaskKey, Prompt: stage.Prompt, Provider: stage.Provider, Model: stage.Model, Runtime: stage.Runtime, Skill: stage.Skill, Plan: order.Plan, WorktreeName: b.WorktreeName, WorktreePath: b.WorktreePath, SessionID: b.SessionID, Reason: "retained local task completion requires candidate review"}
}

func (l *Loop) controlRecoverLocalTaskCompletion(cmd ControlCommand) error {
	if !filepath.IsAbs(cmd.Value) || cmd.Target == "" {
		return fmt.Errorf("local task completion recovery source missing")
	}
	data, err := os.ReadFile(cmd.Value)
	if err != nil {
		return err
	}
	if recoveryDigest(data) != cmd.Target {
		return fmt.Errorf("local task completion recovery source changed")
	}
	var packet localTaskCompletionPacket
	if err := decodeStoppedReview(data, &packet); err != nil {
		return err
	}
	if packet.Schema != 2 || packet.Terminal == nil || packet.Project != l.projectDir || packet.Order.OrderID != cmd.OrderID {
		return fmt.Errorf("local task completion recovery identity differs")
	}
	order, ok := l.canonical.Orders[cmd.OrderID]
	if !ok || len(order.Stages) != 1 {
		return fmt.Errorf("local task completion recovery order missing")
	}
	ref := localTaskFileRef{Path: cmd.Value, SHA256: cmd.Target}
	raw, _ := json.Marshal(ref)
	if previous := order.Stages[0].Extra[localTaskCompletionRecoveryKey]; len(previous) > 0 {
		if !recoveryJSONEqual(json.RawMessage(previous), json.RawMessage(raw)) || order.Status != state.OrderActive || order.Stages[0].Status != state.StageReview {
			return fmt.Errorf("local task completion recovery already progressed")
		}
		if !recoveryJSONEqual(l.canonical.PendingReviews[cmd.OrderID], completionPendingReview(packet.Order, packet.Binding)) {
			return fmt.Errorf("local task recovered review differs")
		}
		if err := l.validateRecovery(order, l.canonical.PendingReviews[cmd.OrderID], packet.Binding); err != nil {
			return err
		}
		if err := l.projectRecoveredOrder(cmd.OrderID); err != nil {
			return err
		}
		return l.syncPendingReviewProjection()
	}
	if _, exists := l.canonical.PendingReviews[cmd.OrderID]; exists {
		return fmt.Errorf("local task completion already has review")
	}
	if err := l.validateCompletionPredecessor(packet); err != nil {
		return err
	}
	current, err := l.captureLocalTaskCompletion(order)
	if err != nil {
		return err
	}
	current.Predecessor = packet.Predecessor
	if !recoveryJSONEqual(current, packet) {
		return fmt.Errorf("local task completion recovery custody changed")
	}
	next := l.canonical.Clone()
	order = next.Orders[cmd.OrderID]
	stage := &order.Stages[0]
	if stage.Extra == nil {
		stage.Extra = map[string]json.RawMessage{}
	}
	stage.Extra[localTaskCompletionRecoveryKey] = raw
	stage.Status = state.StageReview
	order.Status = state.OrderActive
	next.Orders[cmd.OrderID] = order
	next.PendingReviews[cmd.OrderID] = completionPendingReview(packet.Order, packet.Binding)
	if err := next.Validate(); err != nil {
		return err
	}
	for _, record := range packet.Effects {
		if err := l.effectLedger.CancelPending(record.EffectID, "retired by original local-task completion review recovery", timeNowUTC(l.deps.Now)); err != nil {
			return err
		}
	}
	l.canonical = next
	if err := l.persistCanonicalCheckpoint(); err != nil {
		return err
	}
	if err := l.projectRecoveredOrder(cmd.OrderID); err != nil {
		return err
	}
	return l.syncPendingReviewProjection()
}

// Schema 1 retained a monitor projection digest. Only its known error ACK can
// lead to a successor, and every immutable custody field still has to match.
func (l *Loop) validateCompletionPredecessor(packet localTaskCompletionPacket) error {
	prior := packet.Predecessor
	if prior == nil {
		return nil
	}
	if !filepath.IsAbs(prior.Packet.Path) || prior.Packet.SHA256 == "" || prior.ControlID == "" {
		return fmt.Errorf("completion recovery predecessor identity missing")
	}
	data, err := os.ReadFile(prior.Packet.Path)
	if err != nil {
		return err
	}
	if recoveryDigest(data) != prior.Packet.SHA256 {
		return fmt.Errorf("completion recovery predecessor source changed")
	}
	var original localTaskCompletionPacket
	if err := decodeStoppedReview(data, &original); err != nil {
		return err
	}
	if original.Schema != 1 || original.Terminal != nil || original.Predecessor != nil || len(original.Binding.Files["meta.json"]) != 64 || len(original.Order.Stages) != 1 {
		return fmt.Errorf("completion recovery predecessor is not the original projection-bound packet")
	}
	data, err = os.ReadFile(filepath.Join(l.runtimeDir, "control-ack.ndjson"))
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
		if ack.ID != prior.ControlID {
			continue
		}
		if ack.Action != "recover-local-task-completion" || ack.Status != "error" || ack.Message != "local task completion recovery custody changed" {
			return fmt.Errorf("completion recovery predecessor ACK is not the known projection refusal")
		}
		matches++
	}
	if matches != 1 {
		return fmt.Errorf("completion recovery predecessor ACK missing or ambiguous")
	}
	original.Schema = 2
	delete(original.Binding.Files, "meta.json")
	stage := original.Order.Stages[0]
	original.Terminal = &localTaskCompletionTerminal{SessionID: original.Binding.SessionID, Status: "exited", Alive: false, Provider: stage.Provider, Model: stage.Model, Runtime: stage.Runtime}
	original.Predecessor = prior
	if !recoveryJSONEqual(original, packet) {
		return fmt.Errorf("completion recovery immutable predecessor custody changed")
	}
	return nil
}
