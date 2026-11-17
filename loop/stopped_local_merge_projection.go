package loop

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"time"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/ingest"
	"github.com/poteto/noodle/internal/orderx"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
	loopruntime "github.com/poteto/noodle/runtime"
)

// ValidateStoppedLocalMergeCompanion qualifies canonical data, not dispatch.
// The caller supplies only the original Host-bound, digest-checked config.
func ValidateStoppedLocalMergeCompanion(before []byte, orderID string, cfg config.Config) error {
	var s reducer.DurableSnapshot
	if err := decodeStoppedReview(before, &s); err != nil {
		return err
	}
	if orderID == scheduleOrderID || len(s.State.Orders) != 2 || len(cfg.Runtime.AdmittedOrders) != 1 || cfg.Runtime.AdmittedOrders[0] != orderID {
		return fmt.Errorf("stopped merge has foreign order/admission custody")
	}
	if _, ok := s.State.Orders[orderID]; !ok {
		return fmt.Errorf("original stopped merge order missing")
	}
	companion, ok := s.State.Orders[scheduleOrderID]
	if !ok || len(companion.Stages) != 1 || companion.CreatedAt.IsZero() || companion.UpdatedAt.IsZero() || companion.Sequence < 0 {
		return fmt.Errorf("stopped merge dormant schedule missing or incomplete")
	}
	// Use the actual producer and canonical conversion, including routing and
	// the implicit process runtime. Timestamps and metadata are retained data.
	want := synthesizeCanonicalState(bootstrapScheduleOrder(cfg), nil, state.RunMode(cfg.Mode), companion.CreatedAt).Orders[scheduleOrderID]
	if companion.OrderID != want.OrderID || companion.Title != want.Title || !reflect.DeepEqual(companion.Plan, want.Plan) || companion.Rationale != want.Rationale || companion.Status != want.Status {
		return fmt.Errorf("stopped merge companion differs from built-in schedule producer")
	}
	stage := companion.Stages[0]
	if len(stage.Attempts) != 0 || stage.Merge != nil {
		return fmt.Errorf("stopped merge schedule has execution custody")
	}
	for _, key := range []string{localTaskMergeKey, localTaskMergeRecoveryKey, localTaskCompletionRecoveryKey, requestChangesKey, requestChangesRequeuedKey, savedProgressAttemptsKey, interruptionKey, interruptionHistoryKey, nativeDegradationKey, "merge_worktree", "merge_mode", "merge_branch", "stopped_local_merge_continuation"} {
		if _, exists := stage.Extra[key]; exists {
			return fmt.Errorf("stopped merge schedule has execution custody: %s", key)
		}
	}
	stage.Attempts, stage.Extra = nil, nil
	if !reflect.DeepEqual(stage, want.Stages[0]) {
		return fmt.Errorf("stopped merge companion stage differs from built-in schedule producer")
	}
	// A missing attempts field cannot prove the producer's explicit null/empty
	// history. Require its ordinary emitted fields, even when their value is zero.
	var raw struct {
		State struct {
			Orders map[string]struct {
				Stages []map[string]json.RawMessage `json:"stages"`
			} `json:"orders"`
		} `json:"state"`
	}
	if err := json.Unmarshal(before, &raw); err != nil {
		return err
	}
	encoded, err := json.Marshal(want.Stages[0])
	if err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		return err
	}
	for key := range fields {
		if _, present := raw.State.Orders[scheduleOrderID].Stages[0][key]; !present {
			return fmt.Errorf("stopped merge schedule stage evidence missing: %s", key)
		}
	}
	if len(s.State.PendingReviews) != 1 {
		return fmt.Errorf("stopped merge has foreign review custody")
	}
	for key, review := range s.State.PendingReviews {
		if key != orderID || review.OrderID != orderID {
			return fmt.Errorf("stopped merge has foreign review custody")
		}
	}
	if s.EffectLedger == nil {
		return fmt.Errorf("stopped merge schedule effect evidence missing")
	}
	for _, row := range s.EffectLedger {
		var subject struct {
			OrderID string `json:"order_id"`
		}
		if json.Unmarshal(row.Effect.Payload, &subject) != nil || subject.OrderID == "" || subject.OrderID == scheduleOrderID || row.EffectID == initialAdmissionEffectID(scheduleOrderID) || row.Effect.EffectID == initialAdmissionEffectID(scheduleOrderID) {
			return fmt.Errorf("stopped merge schedule has effect custody or unknown subject")
		}
	}
	return nil
}

// CompleteStoppedLocalMerge computes only the original effect's completion.
// The stopped caller must already prove the bound Git HEAD/tree landed.
func CompleteStoppedLocalMerge(before []byte, orderID, effectID string, plan loopruntime.SessionReadbackFileRef, at time.Time) ([]byte, error) {
	var s reducer.DurableSnapshot
	if err := decodeStoppedReview(before, &s); err != nil {
		return nil, err
	}
	o, ok := s.State.Orders[orderID]
	if _, pending := s.State.PendingReviews[orderID]; !pending {
		return nil, fmt.Errorf("original failed merge review missing")
	}
	if !ok || o.Status != state.OrderActive || len(o.Stages) != 1 || o.Stages[0].Status != state.StageReview {
		return nil, fmt.Errorf("original stopped merge stage differs")
	}
	count := 0
	for _, row := range s.EffectLedger {
		if row.EffectID == effectID && row.Effect.Type == reducer.EffectMerge && row.Status == reducer.EffectLedgerPending && row.Result == nil {
			var p struct {
				Order    string `json:"order_id"`
				Stage    int    `json:"stage_index"`
				Worktree string `json:"worktree_name"`
				Mode     string `json:"merge_mode"`
				Branch   string `json:"merge_branch"`
			}
			if json.Unmarshal(row.Effect.Payload, &p) != nil || p.Order != orderID || p.Stage != 0 || p.Mode != "local" || p.Branch != "" {
				return nil, fmt.Errorf("original merge effect differs")
			}
			o.Stages[0].Status = state.StageMerging
			o.Stages[0].Merge = &state.MergeRecoveryNode{WorktreeName: p.Worktree, Mode: p.Mode, Branch: p.Branch}
			count++
		}
	}
	if count != 1 {
		return nil, fmt.Errorf("original merge effect is not unique")
	}
	s.State.Orders[orderID] = o
	id, err := strconv.ParseUint(s.State.LastEventID, 10, 64)
	if err != nil || id == ^uint64(0) {
		return nil, fmt.Errorf("original event sequence is unknown")
	}
	payload, _ := json.Marshal(map[string]any{"order_id": orderID, "stage_index": 0})
	next, effects, err := reducer.Reduce(s.State, ingest.StateEvent{ID: ingest.EventID(id + 1), Source: string(ingest.SourceInternal), Type: string(ingest.EventMergeCompleted), Timestamp: at, Payload: payload})
	if err != nil || len(effects) != 0 {
		return nil, fmt.Errorf("original merge completion failed: %v", err)
	}
	// Keep opaque values and integer spelling; only this effect's facts change.
	var document map[string]any
	decoder := json.NewDecoder(bytes.NewReader(before))
	decoder.UseNumber()
	if err = decoder.Decode(&document); err != nil {
		return nil, err
	}
	rawState := document["state"].(map[string]any)
	orders := rawState["orders"].(map[string]any)
	rawOrder := orders[orderID].(map[string]any)
	stage := rawOrder["stages"].([]any)[0].(map[string]any)
	stage["status"] = next.Orders[orderID].Stages[0].Status
	delete(stage, "merge")
	extra, _ := stage["extra"].(map[string]any)
	if extra == nil {
		extra = map[string]any{}
		stage["extra"] = extra
	}
	extra["stopped_local_merge_continuation"] = map[string]any{"plan": plan, "effect_id": effectID, "completed_at": at}
	rawOrder["status"], rawOrder["updated_at"] = next.Orders[orderID].Status, at
	delete(rawState["pending_reviews"].(map[string]any), orderID)
	rawState["last_event_id"] = next.LastEventID
	for _, entry := range document["effect_ledger"].([]any) {
		row := entry.(map[string]any)
		if row["effect_id"] == effectID {
			row["status"], row["attempts"], row["last_attempt_at"] = reducer.EffectLedgerDone, 1, at
			row["result"] = reducer.EffectResult{EffectID: effectID, Status: reducer.EffectResultCompleted, Timestamp: at}
		}
	}
	document["generated_at"] = at
	raw, err := json.MarshalIndent(document, "", "  ")
	return append(raw, '\n'), err
}

// ProjectStoppedLocalMerge reuses the stopped owner's normal projections.
func ProjectStoppedLocalMerge(project string, after []byte) error {
	var s reducer.DurableSnapshot
	if err := decodeStoppedReview(after, &s); err != nil {
		return err
	}
	if err := validateAdmissionSnapshot(s); err != nil {
		return err
	}
	l := &Loop{runtimeDir: filepath.Join(project, ".noodle"), canonical: s.State, canonicalLoaded: true}
	if err := l.writeProjectionState(); err != nil {
		return err
	}
	if err := l.writePendingReview(); err != nil {
		return err
	}
	return CheckStoppedLocalMergeProjection(project, after)
}

// The caller owns the separate OpenShell completion/detach proof. This only
// checks the existing projections and never invokes the process-only oracle.
func CheckStoppedLocalMergeProjection(project string, after []byte) error {
	var s reducer.DurableSnapshot
	if err := decodeStoppedReview(after, &s); err != nil {
		return err
	}
	if err := validateAdmissionSnapshot(s); err != nil {
		return err
	}
	dir := filepath.Join(project, ".noodle")
	current, err := os.ReadFile(filepath.Join(dir, "state.snapshot.json"))
	if err != nil || string(current) != string(after) {
		return fmt.Errorf("original completed canonical projection changed")
	}
	raw, err := readAdmissionFile(filepath.Join(dir, "orders.json"))
	if err != nil {
		return err
	}
	orders, err := orderx.ParseOrdersStrict(raw)
	if err != nil {
		return err
	}
	if err = validateAdmissionProjection(s.State, orders); err != nil {
		return err
	}
	reviews, err := ReadPendingReview(dir)
	if err != nil {
		return err
	}
	if len(reviews) != 0 {
		return fmt.Errorf("original completed pending review projection remains")
	}
	return nil
}
