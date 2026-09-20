package loop

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/poteto/noodle/event"
	"github.com/poteto/noodle/internal/orderx"
	"github.com/poteto/noodle/internal/procx"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
)

const requestChangesKey = "request_changes_recovery"

// Stored in existing stage Extra, so the checkpoint and order projection carry
// the same custody binding. It authorizes only explicit edit-item and requeue.
type requestChangesBinding struct {
	SessionID    string            `json:"session_id"`
	AttemptID    string            `json:"attempt_id"`
	Attempt      int               `json:"attempt"`
	WorktreeName string            `json:"worktree_name"`
	WorktreePath string            `json:"worktree_path"`
	Branch       string            `json:"branch"`
	Head         string            `json:"candidate_head"`
	Files        map[string]string `json:"session_sha256"`
}

// The supervisor supplies the full definition and exact evidence digests; no
// order definition is inferred from a branch name or a session prompt.
type requestChangesPacket struct {
	Order            state.OrderNode            `json:"order"`
	Review           state.PendingReviewNode    `json:"review"`
	Binding          requestChangesBinding      `json:"binding"`
	LoopEventsSHA256 string                     `json:"loop_events_sha256"`
	Admission        reducer.EffectLedgerRecord `json:"initial_admission"`
}

var recoverySessionFiles = []string{"spawn.json", "prompt.txt", "events.ndjson", "process.json"}

func recoveryDigest(data []byte) string { return fmt.Sprintf("%x", sha256.Sum256(data)) }

func recoveryGit(path string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", path}, args...)...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("recovery git %v: %w: %s", args, err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

func (l *Loop) captureRequestChanges(pending *pendingReviewCook) (requestChangesBinding, error) {
	b := requestChangesBinding{SessionID: pending.sessionID, WorktreeName: pending.worktreeName, WorktreePath: pending.worktreePath}
	node := l.canonical.Orders[pending.orderID]
	if pending.stageIndex < 0 || pending.stageIndex >= len(node.Stages) {
		return b, fmt.Errorf("recovery stage missing")
	}
	attempts := node.Stages[pending.stageIndex].Attempts
	if len(attempts) == 0 {
		return b, fmt.Errorf("recovery attempt missing")
	}
	b.Attempt = len(attempts) - 1
	b.AttemptID = attempts[b.Attempt].AttemptID
	var err error
	b.Branch, err = recoveryGit(b.WorktreePath, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return b, err
	}
	b.Head, err = recoveryGit(b.WorktreePath, "rev-parse", "HEAD")
	if err != nil {
		return b, err
	}
	b.Files = make(map[string]string)
	for _, name := range recoverySessionFiles {
		data, err := os.ReadFile(filepath.Join(l.runtimeDir, "sessions", b.SessionID, name))
		if err != nil {
			return b, err
		}
		b.Files[name] = recoveryDigest(data)
	}
	return b, nil
}

func recoveryBinding(stage state.StageNode) (requestChangesBinding, error) {
	var b requestChangesBinding
	raw, ok := stage.Extra[requestChangesKey]
	if !ok || json.Unmarshal(raw, &b) != nil || b.SessionID == "" {
		return b, fmt.Errorf("request-changes recovery binding missing")
	}
	return b, nil
}

func (l *Loop) validateRecovery(order state.OrderNode, review state.PendingReviewNode, b requestChangesBinding) error {
	si := review.StageIndex
	if order.OrderID == "" || review.OrderID != order.OrderID || si < 0 || si >= len(order.Stages) {
		return fmt.Errorf("recovery order/stage identity mismatch")
	}
	stage := order.Stages[si]
	if b.SessionID == "" || b.SessionID == "." || filepath.Base(b.SessionID) != b.SessionID || b.Attempt < 0 || b.Attempt != len(stage.Attempts)-1 {
		return fmt.Errorf("recovery session/attempt identity mismatch")
	}
	attempt := stage.Attempts[b.Attempt]
	if attempt.SessionID != b.SessionID || attempt.AttemptID != b.AttemptID || b.AttemptID != dispatchAttemptID(order.OrderID, si, b.Attempt) || attempt.WorktreeName != b.WorktreeName || (attempt.Status != state.AttemptCompleted && attempt.Status != state.AttemptFailed) {
		return fmt.Errorf("recovery terminal attempt mismatch")
	}
	if stage.StageIndex != si || review.SessionID != b.SessionID || review.WorktreeName != b.WorktreeName || review.WorktreePath != b.WorktreePath || b.WorktreeName != cookBaseName(order.OrderID, si, stage.TaskKey) || b.WorktreePath != l.worktreePath(b.WorktreeName) {
		return fmt.Errorf("recovery worktree identity mismatch")
	}
	if review.TaskKey != stage.TaskKey || review.Skill != stage.Skill || review.Provider != stage.Provider || review.Model != stage.Model || nonEmpty(review.Runtime, "process") != nonEmpty(stage.Runtime, "process") || review.Prompt != stage.Prompt || !slices.Equal(review.Plan, order.Plan) {
		return fmt.Errorf("recovery review definition mismatch")
	}
	if l.cooks.activeCooksByOrder[order.OrderID] != nil || l.cooks.adoptedTargets[order.OrderID] != "" {
		return fmt.Errorf("recovery prior session is active")
	}
	for _, active := range l.cooks.activeCooksByOrder {
		if active.worktreePath == b.WorktreePath {
			return fmt.Errorf("recovery worktree is in use")
		}
	}
	dir := filepath.Join(l.runtimeDir, "sessions", b.SessionID)
	contents := map[string][]byte{}
	for _, name := range recoverySessionFiles {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return fmt.Errorf("recovery %s: %w", name, err)
		}
		if b.Files[name] == "" || recoveryDigest(data) != b.Files[name] {
			return fmt.Errorf("recovery %s digest changed", name)
		}
		contents[name] = data
	}
	var spawn struct {
		SessionID    string `json:"session_id"`
		WorktreePath string `json:"worktree_path"`
		RetryCount   int    `json:"retry_count"`
	}
	if json.Unmarshal(contents["spawn.json"], &spawn) != nil || spawn.SessionID != b.SessionID || spawn.WorktreePath != b.WorktreePath || spawn.RetryCount != b.Attempt {
		return fmt.Errorf("recovery spawn identity mismatch")
	}
	var process struct {
		SessionID string `json:"session_id"`
		PID       int    `json:"pid"`
	}
	if json.Unmarshal(contents["process.json"], &process) != nil || process.SessionID != b.SessionID || process.PID <= 0 {
		return fmt.Errorf("recovery process identity mismatch")
	}
	absent, err := procx.ProcessGroupAbsent(process.PID)
	if err != nil {
		return err
	}
	if !absent {
		return fmt.Errorf("recovery prior process/group is live")
	}
	cook := &cookHandle{cookIdentity: cookIdentity{orderID: order.OrderID, stageIndex: si}, session: &adoptedSession{id: b.SessionID, status: "completed"}}
	outcome, err := l.readRequiredStageOutcome(cook)
	if err != nil {
		return err
	}
	if outcome.Outcome != event.StageOutcomeBlocked {
		return fmt.Errorf("recovery requires exact typed blocked outcome")
	}
	top, err := recoveryGit(b.WorktreePath, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	actual, err := filepath.EvalSymlinks(b.WorktreePath)
	if err != nil {
		return err
	}
	if top != actual {
		return fmt.Errorf("recovery worktree root mismatch")
	}
	common, err := recoveryGit(b.WorktreePath, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	owner, err := recoveryGit(l.projectDir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	if common != owner {
		return fmt.Errorf("recovery worktree owner mismatch")
	}
	branch, err := recoveryGit(b.WorktreePath, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		return err
	}
	if b.Branch == "" || branch != b.Branch {
		return fmt.Errorf("recovery branch moved")
	}
	head, err := recoveryGit(b.WorktreePath, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if b.Head == "" || head != b.Head {
		return fmt.Errorf("recovery HEAD moved")
	}
	dirty, err := recoveryGit(b.WorktreePath, "status", "--porcelain")
	if err != nil {
		return err
	}
	if dirty != "" {
		return fmt.Errorf("recovery worktree is dirty")
	}
	return nil
}

func (l *Loop) requestChangesReview(orderID string) (state.PendingReviewNode, error) {
	order, ok := l.canonical.Orders[orderID]
	review, hasReview := l.canonical.PendingReviews[orderID]
	if !ok || !hasReview || order.Status != state.OrderFailed || review.StageIndex < 0 || review.StageIndex >= len(order.Stages) || order.Stages[review.StageIndex].Status != state.StageFailed {
		return review, fmt.Errorf("request-changes bound failed review missing")
	}
	pending := l.cooks.pendingReview[orderID]
	if pending == nil || pending.sessionID != review.SessionID || pending.stageIndex != review.StageIndex || pending.worktreeName != review.WorktreeName || pending.worktreePath != review.WorktreePath {
		return review, fmt.Errorf("request-changes pending review projection mismatch")
	}
	b, err := recoveryBinding(order.Stages[review.StageIndex])
	if err != nil {
		return review, err
	}
	if err := l.validateRecovery(order, review, b); err != nil {
		return review, err
	}
	data, err := os.ReadFile(filepath.Join(l.runtimeDir, "loop-events.ndjson"))
	if err != nil {
		return review, err
	}
	return review, validateRequestChangesFailure(data, requestChangesPacket{Order: order, Review: review, Binding: b})
}

// A custody change prevents continuation without discarding diagnostic state.
// Only request-changes created this binding; generic terminal failures lack it.
func (l *Loop) preserveRequestChanges(orderID string) bool {
	order, ok := l.canonical.Orders[orderID]
	review, exists := l.canonical.PendingReviews[orderID]
	if !ok || !exists || order.Status != state.OrderFailed || review.StageIndex < 0 || review.StageIndex >= len(order.Stages) {
		return false
	}
	stage := order.Stages[review.StageIndex]
	b, err := recoveryBinding(stage)
	if err != nil || stage.Status != state.StageFailed || review.SessionID != b.SessionID {
		return false
	}
	outcome, err := l.readRequiredStageOutcome(&cookHandle{cookIdentity: cookIdentity{orderID: orderID, stageIndex: review.StageIndex}, session: &adoptedSession{id: b.SessionID}})
	if err != nil || outcome.Outcome != event.StageOutcomeBlocked || b.Attempt < 0 || b.Attempt >= len(stage.Attempts) {
		return false
	}
	data, err := os.ReadFile(filepath.Join(l.runtimeDir, "loop-events.ndjson"))
	return err == nil && validateRequestChangesFailure(data, requestChangesPacket{Order: order, Review: review, Binding: b}) == nil
}

func (l *Loop) controlRecoverRequestChanges(cmd ControlCommand) error {
	if !filepath.IsAbs(cmd.Value) {
		return fmt.Errorf("recovery packet path is not absolute")
	}
	data, err := os.ReadFile(cmd.Value)
	if err != nil {
		return err
	}
	if cmd.Target == "" || recoveryDigest(data) != cmd.Target {
		return fmt.Errorf("recovery packet digest changed")
	}
	var packet requestChangesPacket
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&packet); err != nil {
		return fmt.Errorf("recovery packet: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("recovery packet has trailing data")
	}
	if !l.canonicalLoaded {
		return fmt.Errorf("recovery canonical state not loaded")
	}
	id := packet.Order.OrderID
	if id == scheduleOrderID || id == "" || (cmd.OrderID != "" && cmd.OrderID != id) {
		return fmt.Errorf("recovery order identity mismatch")
	}
	orders, err := l.currentOrders()
	if err != nil {
		return err
	}
	for _, o := range orders.Orders {
		if o.ID == id {
			return fmt.Errorf("recovery conflicts with current order")
		}
	}
	if _, ok := l.canonical.Orders[id]; ok {
		return fmt.Errorf("recovery conflicts with canonical order")
	}
	if _, ok := l.canonical.PendingReviews[id]; ok {
		return fmt.Errorf("recovery conflicts with pending review")
	}
	if _, ok := l.cooks.pendingReview[id]; ok {
		return fmt.Errorf("recovery conflicts with pending review projection")
	}
	for _, r := range l.canonical.PendingReviews {
		if r.SessionID == packet.Binding.SessionID || r.WorktreePath == packet.Binding.WorktreePath {
			return fmt.Errorf("recovery conflicts with another pending review")
		}
	}
	if packet.Order.Status != state.OrderFailed || packet.Review.StageIndex < 0 || packet.Review.StageIndex >= len(packet.Order.Stages) || packet.Order.Stages[packet.Review.StageIndex].Status != state.StageFailed {
		return fmt.Errorf("recovery packet is not a failed request-changes stage")
	}
	for _, order := range l.canonical.Orders {
		for _, stage := range order.Stages {
			for _, attempt := range stage.Attempts {
				if attempt.SessionID == packet.Binding.SessionID || attempt.WorktreeName == packet.Binding.WorktreeName {
					return fmt.Errorf("recovery conflicts with current attempt custody")
				}
			}
		}
	}
	for _, stage := range packet.Order.Stages {
		if stage.Status.IsBusy() {
			return fmt.Errorf("recovery packet contains a busy stage")
		}
		for _, attempt := range stage.Attempts {
			if attempt.Status == state.AttemptRunning || attempt.Status == state.AttemptLaunching {
				return fmt.Errorf("recovery packet contains a live attempt")
			}
		}
	}
	if err := l.validateRecovery(packet.Order, packet.Review, packet.Binding); err != nil {
		return err
	}
	attempt := packet.Order.Stages[packet.Review.StageIndex].Attempts[packet.Binding.Attempt]
	if attempt.Status != state.AttemptFailed || strings.TrimSpace(attempt.Error) == "" {
		return fmt.Errorf("recovery request-changes failed attempt missing")
	}
	if err := l.validateRecoveryAdmission(packet); err != nil {
		return err
	}
	loopBytes, err := os.ReadFile(filepath.Join(l.runtimeDir, "loop-events.ndjson"))
	if err != nil {
		return err
	}
	if recoveryDigest(loopBytes) != packet.LoopEventsSHA256 {
		return fmt.Errorf("recovery loop events digest changed")
	}
	if err := validateRequestChangesFailure(loopBytes, packet); err != nil {
		return err
	}
	// All refusal checks precede mutation. Checkpoint remains the owner; the
	// projection is rebuilt from it, and migration never dispatches or requeues.
	next := l.canonical.Clone()
	raw, err := json.Marshal(packet.Binding)
	if err != nil {
		return err
	}
	stage := &packet.Order.Stages[packet.Review.StageIndex]
	if stage.Extra == nil {
		stage.Extra = map[string]json.RawMessage{}
	}
	stage.Extra[requestChangesKey] = raw
	next.Orders[id] = packet.Order
	next.PendingReviews[id] = packet.Review
	if err := next.Validate(); err != nil {
		return err
	}
	l.canonical = next
	if err := l.persistCanonicalCheckpoint(); err != nil {
		return err
	}
	if err := l.projectRecoveredOrder(id); err != nil {
		return err
	}
	return l.syncPendingReviewProjection()
}

func (l *Loop) validateRecoveryAdmission(p requestChangesPacket) error {
	r := p.Admission
	var payload struct {
		OrderID  string `json:"order_id"`
		Revision string `json:"initial_revision"`
	}
	if json.Unmarshal(r.Effect.Payload, &payload) != nil || payload.OrderID != p.Order.OrderID || !orderx.ValidOrderRevision(payload.Revision) || r.EffectID != initialAdmissionEffectID(p.Order.OrderID) || r.Effect.EffectID != r.EffectID || r.Effect.Type != reducer.EffectInitialAdmission || r.Status != reducer.EffectLedgerDone || r.Result == nil || r.Result.EffectID != r.EffectID || r.Result.Status != reducer.EffectResultCompleted {
		return fmt.Errorf("recovery done initial-admission binding missing")
	}
	for _, record := range l.effectLedger.All() {
		if record.EffectID == r.EffectID && recoveryJSONEqual(record, r) {
			return nil
		}
	}
	return fmt.Errorf("recovery initial-admission effect mismatch")
}

func validateRequestChangesFailure(data []byte, p requestChangesPacket) error {
	stageFound, orderFound := false, false
	mistake := newCookMistakeEnvelope(CookMistakeReasonRequestChanges, p.Order.OrderID, p.Review.StageIndex)
	failure := eventFailureMetadataForLoop(CycleFailureClassOrderHard, OrderFailureClassStageTerminal, &mistake)
	reason := p.Order.Stages[p.Review.StageIndex].Attempts[p.Binding.Attempt].Error
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var e event.LoopEvent
		if err := json.Unmarshal(line, &e); err != nil {
			return fmt.Errorf("recovery loop event: %w", err)
		}
		switch e.Type {
		case event.LoopEventStageFailed:
			var f StageFailedPayload
			if err := json.Unmarshal(e.Payload, &f); err != nil {
				return err
			}
			if f.OrderID == p.Order.OrderID {
				stageFound = f.StageIndex == p.Review.StageIndex && f.SessionID != nil && *f.SessionID == p.Binding.SessionID && reflect.DeepEqual(f.AgentMistake, &mistake) && reflect.DeepEqual(f.Failure, &failure) && f.Reason == reason
				orderFound = false
			}
		case event.LoopEventOrderFailed:
			var f OrderFailedPayload
			if err := json.Unmarshal(e.Payload, &f); err != nil {
				return err
			}
			if f.OrderID == p.Order.OrderID {
				orderFound = stageFound && reflect.DeepEqual(f.AgentMistake, &mistake) && reflect.DeepEqual(f.Failure, &failure) && f.Reason == reason
			}
		case event.LoopEventOrderCompleted, event.LoopEventOrderDropped, event.LoopEventOrderRequeued:
			var f struct {
				OrderID string `json:"order_id"`
			}
			if err := json.Unmarshal(e.Payload, &f); err != nil {
				return err
			}
			if f.OrderID == p.Order.OrderID {
				stageFound, orderFound = false, false
			}
		}
	}
	if !stageFound || !orderFound {
		return fmt.Errorf("recovery matching request_changes failure missing")
	}
	return nil
}

func (l *Loop) editRequestChanges(cmd ControlCommand) error {
	id := strings.TrimSpace(cmd.OrderID)
	review, err := l.requestChangesReview(id)
	if err != nil {
		return err
	}
	if cmd.TaskKey != "" || cmd.Skill != "" || cmd.Provider != "" || cmd.Model != "" {
		return fmt.Errorf("recovered request-changes stage permits prompt editing only")
	}
	prompt := strings.TrimSpace(cmd.Prompt)
	if prompt == "" {
		return nil
	}
	node := l.canonical.Orders[id]
	node.Title = titleFromPrompt(prompt, 8)
	node.Stages[review.StageIndex].Prompt = prompt
	review.Prompt = prompt
	l.canonical.Orders[id] = node
	l.canonical.PendingReviews[id] = review
	if err := l.persistCanonicalCheckpoint(); err != nil {
		return err
	}
	if err := l.projectRecoveredOrder(id); err != nil {
		return err
	}
	return l.syncPendingReviewProjection()
}

// Replace the full legacy definition, including an edited prompt. The general
// review mirror only copies lifecycle statuses and cannot restore absent orders.
func (l *Loop) projectRecoveredOrder(id string) error {
	node := l.canonical.Orders[id]
	order := Order{ID: node.OrderID, Title: node.Title, Plan: node.Plan, Rationale: node.Rationale, Status: OrderStatusFailed}
	for _, s := range node.Stages {
		order.Stages = append(order.Stages, Stage{TaskKey: s.TaskKey, Prompt: s.Prompt, Skill: s.Skill, Provider: s.Provider, Model: s.Model, Runtime: s.Runtime, Group: s.Group, Status: canonicalStageStatusToLegacy(s.Status), Extra: cloneLegacyExtra(s.Extra), ExtraPrompt: s.ExtraPrompt})
	}
	orders, err := l.currentOrders()
	if err != nil {
		return err
	}
	for i, o := range orders.Orders {
		if o.ID == id {
			orders.Orders[i] = order
			return l.writeProjectedMirrorState(orders)
		}
	}
	orders.Orders = append(orders.Orders, order)
	return l.writeProjectedMirrorState(orders)
}

func recoveryJSONEqual(a, b any) bool {
	left, leftErr := json.Marshal(a)
	right, rightErr := json.Marshal(b)
	return leftErr == nil && rightErr == nil && bytes.Equal(left, right)
}
