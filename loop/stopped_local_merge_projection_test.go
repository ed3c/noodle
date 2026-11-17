package loop

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/orderx"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
	loopruntime "github.com/poteto/noodle/runtime"
)

func dormantStoppedMergeSnapshot(t *testing.T) (reducer.DurableSnapshot, config.Config) {
	t.Helper()
	cfg, err := config.Parse([]byte("mode='manual'\n[runtime]\nadmitted_orders=['original']\n[routing.defaults]\nprovider='codex'\nmodel='fixture-model'\n"))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 14, 8, 0, 0, 0, time.UTC)
	orders := bootstrapScheduleOrder(cfg)
	orders.Orders = append(orders.Orders, Order{ID: "original", Title: "reviewed task", Status: OrderStatusActive, Stages: []Stage{{TaskKey: "local-task", Skill: "local-task", Runtime: "openshell", Status: StageStatusActive}}})
	reviews := []PendingReviewItem{{OrderID: "original", StageIndex: 0, SessionID: "session", WorktreeName: "original-worker", WorktreePath: "/fixture/original-worker", Reason: "original failed merge"}}
	s := reducer.DurableSnapshot{OrderRevision: strings.Repeat("b", 32), State: synthesizeCanonicalState(orders, reviews, state.RunModeManual, at), GeneratedAt: at}
	// The existing-node sync path preserves the canonical producer's metadata.
	l := &Loop{canonical: s.State, deps: Dependencies{Now: func() time.Time { return at }}}
	l.syncCanonicalOrderFromLegacy(orders.Orders[0])
	s.State = l.canonical
	s.State.LastEventID = "10"
	for _, effect := range []struct {
		id      string
		kind    reducer.EffectType
		payload string
		status  reducer.EffectLedgerStatus
	}{
		{initialAdmissionEffectID("original"), reducer.EffectInitialAdmission, `{"order_id":"original","initial_revision":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}`, reducer.EffectLedgerDone},
		{"event-4-effect-0", reducer.EffectDispatch, `{"order_id":"original","stage_index":0,"attempt_id":"legacy-review-original"}`, reducer.EffectLedgerPending},
		{"event-8-effect-0", reducer.EffectMerge, `{"order_id":"original","stage_index":0,"worktree_name":"original-worker","merge_mode":"local","merge_branch":""}`, reducer.EffectLedgerPending},
		{"event-9-effect-0", reducer.EffectAck, `{"order_id":"original","stage_index":0}`, reducer.EffectLedgerPending},
	} {
		row := reducer.EffectLedgerRecord{EffectID: effect.id, Effect: reducer.Effect{EffectID: effect.id, Type: effect.kind, Payload: json.RawMessage(effect.payload)}, Status: effect.status}
		if effect.status == reducer.EffectLedgerDone {
			row.Result = &reducer.EffectResult{EffectID: effect.id, Status: reducer.EffectResultCompleted, Timestamp: at}
		}
		s.EffectLedger = append(s.EffectLedger, row)
	}
	return s, cfg
}

func stoppedMergeSnapshotBytes(t *testing.T, s reducer.DurableSnapshot) []byte {
	t.Helper()
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(raw, '\n')
}

func stoppedMergeSameJSON(t *testing.T, a, b any) bool {
	t.Helper()
	values := make([]any, 2)
	for i, value := range []any{a, b} {
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if err := decoder.Decode(&values[i]); err != nil {
			t.Fatal(err)
		}
	}
	return reflect.DeepEqual(values[0], values[1])
}

func TestStoppedLocalMergeProducerCompanionAndSameEffectProjection(t *testing.T) {
	s, cfg := dormantStoppedMergeSnapshot(t)
	schedule := s.State.Orders[scheduleOrderID]
	schedule.Metadata = map[string]string{"title": schedule.Title, "opaque": "retain original metadata"}
	schedule.Stages[0].Extra = map[string]json.RawMessage{"opaque": json.RawMessage(`{"huge":900719925474099312345,"lexical":1e+20,"flag":false,"empty":null,"list":[1,"2"]}`)}
	s.State.Orders[scheduleOrderID] = schedule
	original := s.State.Orders["original"]
	original.Stages[0].Extra = map[string]json.RawMessage{"opaque_history": json.RawMessage(`{"huge":900719925474099312345,"lexical":1e+20,"status":"unknown"}`)}
	s.State.Orders["original"] = original
	before := stoppedMergeSnapshotBytes(t, s)
	if len(s.State.Orders) == 1 {
		t.Fatal("regression fixture no longer fails the old singleton gate")
	}
	if err := ValidateStoppedLocalMergeCompanion(before, "original", cfg); err != nil {
		t.Fatal(err)
	}
	l := &Loop{config: cfg}
	if err := l.requireAdmittedOrder("original"); err != nil {
		t.Fatal(err)
	}
	if err := l.requireAdmittedOrder(scheduleOrderID); err == nil {
		t.Fatal("companion qualification admitted schedule dispatch")
	}
	project := t.TempDir()
	dir := filepath.Join(project, ".noodle")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(dir, "before.json")
	if err := os.WriteFile(archive, before, 0600); err != nil {
		t.Fatal(err)
	}
	plan := loopruntime.SessionReadbackFileRef{Path: filepath.Join(project, "plan.json"), SHA256: strings.Repeat("a", 64)}
	at := s.GeneratedAt.Add(time.Minute)
	after, err := CompleteStoppedLocalMerge(before, "original", "event-8-effect-0", plan, at)
	if err != nil {
		t.Fatal(err)
	}
	again, err := CompleteStoppedLocalMerge(before, "original", "event-8-effect-0", plan, at)
	if err != nil || !bytes.Equal(after, again) {
		t.Fatal("same original completion is not deterministic", err)
	}
	var completed reducer.DurableSnapshot
	if err := json.Unmarshal(after, &completed); err != nil {
		t.Fatal(err)
	}
	if !stoppedMergeSameJSON(t, completed.State.Orders[scheduleOrderID], schedule) || !reflect.DeepEqual(completed.State.Orders["original"].Stages[0].Attempts, original.Stages[0].Attempts) {
		t.Fatal("completion changed schedule or original attempt custody")
	}
	if completed.OrderRevision != s.OrderRevision || completed.State.Orders["original"].Status != state.OrderCompleted || len(completed.State.PendingReviews) != 0 || completed.State.LastEventID != "11" || len(completed.EffectLedger) != len(s.EffectLedger) {
		t.Fatal("original same-effect completion facts differ")
	}
	for i, row := range completed.EffectLedger {
		if row.EffectID == "event-8-effect-0" {
			if row.Status != reducer.EffectLedgerDone || row.Result == nil || row.Attempts != 1 || row.Result.Status != reducer.EffectResultCompleted || !stoppedMergeSameJSON(t, row.Effect, s.EffectLedger[i].Effect) {
				t.Fatal("original merge effect was replaced or not completed")
			}
		} else if !stoppedMergeSameJSON(t, row, s.EffectLedger[i]) {
			t.Fatal("non-target ledger history changed")
		}
	}
	for _, spelling := range [][]byte{[]byte("900719925474099312345"), []byte("1e+20"), []byte(`"unknown"`)} {
		if !bytes.Contains(after, spelling) {
			t.Fatal("opaque value or number spelling lost")
		}
	}
	unchanged := make([]any, 2)
	for i, raw := range [][]byte{before, after} {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var document map[string]any
		if err := decoder.Decode(&document); err != nil {
			t.Fatal(err)
		}
		delete(document, "generated_at")
		canonical := document["state"].(map[string]any)
		delete(canonical, "last_event_id")
		delete(canonical["pending_reviews"].(map[string]any), "original")
		target := canonical["orders"].(map[string]any)["original"].(map[string]any)
		delete(target, "status")
		delete(target, "updated_at")
		stage := target["stages"].([]any)[0].(map[string]any)
		delete(stage, "status")
		delete(stage, "merge")
		delete(stage["extra"].(map[string]any), "stopped_local_merge_continuation")
		for _, entry := range document["effect_ledger"].([]any) {
			row := entry.(map[string]any)
			if row["effect_id"] == "event-8-effect-0" {
				for _, key := range []string{"status", "attempts", "last_attempt_at", "result"} {
					delete(row, key)
				}
			}
		}
		unchanged[i] = document
	}
	if !reflect.DeepEqual(unchanged[0], unchanged[1]) {
		t.Fatal("completion changed facts outside the original task/effect completion")
	}
	if _, err := CompleteStoppedLocalMerge(after, "original", "event-8-effect-0", plan, at); err == nil {
		t.Fatal("already completed effect accepted as pending")
	}
	if err := os.WriteFile(filepath.Join(dir, "state.snapshot.json"), after, 0600); err != nil {
		t.Fatal(err)
	}
	if err := ProjectStoppedLocalMerge(project, after); err != nil {
		t.Fatal(err)
	}
	if err := CheckStoppedLocalMergeProjection(project, after); err != nil {
		t.Fatal(err)
	}
	projection, err := os.ReadFile(filepath.Join(dir, "orders.json"))
	if err != nil {
		t.Fatal(err)
	}
	orders, err := orderx.ParseOrdersStrict(projection)
	if err != nil || len(orders.Orders) != 1 || orders.Orders[0].ID != scheduleOrderID || orders.Orders[0].Stages[0].Status != StageStatusPending || !bytes.Contains(projection, []byte("900719925474099312345")) {
		t.Fatal("normal projection lost the pending schedule", err)
	}
	kept, err := os.ReadFile(archive)
	if err != nil || !bytes.Equal(kept, before) {
		t.Fatal("raw before-image changed", err)
	}
	kept, err = os.ReadFile(filepath.Join(dir, "state.snapshot.json"))
	if err != nil || !bytes.Equal(kept, after) {
		t.Fatal("projection rewrote canonical values", err)
	}
}

func TestStoppedLocalMergeCompanionPreservesTerminalForeignHistory(t *testing.T) {
	s, cfg := dormantStoppedMergeSnapshot(t)
	s.EffectLedger = append(s.EffectLedger, reducer.EffectLedgerRecord{EffectID: "past-foreign", Effect: reducer.Effect{EffectID: "past-foreign", Type: reducer.EffectAck, Payload: json.RawMessage(`{"order_id":"past-foreign"}`)}, Status: reducer.EffectLedgerDone, Result: &reducer.EffectResult{EffectID: "past-foreign", Status: reducer.EffectResultCompleted}})
	if err := ValidateStoppedLocalMergeCompanion(stoppedMergeSnapshotBytes(t, s), "original", cfg); err != nil {
		t.Fatal("previously allowed completed history was rejected", err)
	}
	s.EffectLedger[len(s.EffectLedger)-1].EffectID = initialAdmissionEffectID(scheduleOrderID)
	if err := ValidateStoppedLocalMergeCompanion(stoppedMergeSnapshotBytes(t, s), "original", cfg); err == nil {
		t.Fatal("schedule initial-admission identity accepted with a foreign payload")
	}
}

func TestStoppedLocalMergeProjectionKeepsPendingAndUnknownRefusals(t *testing.T) {
	for _, fault := range []string{"pending-merge", "running-effect", "unknown-effect", "running-attempt"} {
		t.Run(fault, func(t *testing.T) {
			s, _ := dormantStoppedMergeSnapshot(t)
			before := stoppedMergeSnapshotBytes(t, s)
			if _, err := CompleteStoppedLocalMerge(before, "original", "foreign-effect", loopruntime.SessionReadbackFileRef{}, s.GeneratedAt); err == nil {
				t.Fatal("completion accepted a different effect")
			}
			if fault != "pending-merge" {
				after, err := CompleteStoppedLocalMerge(before, "original", "event-8-effect-0", loopruntime.SessionReadbackFileRef{}, s.GeneratedAt.Add(time.Minute))
				if err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(after, &s); err != nil {
					t.Fatal(err)
				}
				switch fault {
				case "running-effect":
					s.EffectLedger[2].Status, s.EffectLedger[2].Result = reducer.EffectLedgerRunning, nil
				case "unknown-effect":
					s.EffectLedger[2].Status = reducer.EffectLedgerStatus("unknown")
				case "running-attempt":
					o := s.State.Orders["original"]
					o.Stages[0].Attempts[0].Status = state.AttemptRunning
					s.State.Orders["original"] = o
				}
			}
			project := t.TempDir()
			dir := filepath.Join(project, ".noodle")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"state.snapshot.json", "orders.json", "pending-review.json"} {
				if err := os.WriteFile(filepath.Join(dir, name), before, 0600); err != nil {
					t.Fatal(err)
				}
			}
			if err := ProjectStoppedLocalMerge(project, stoppedMergeSnapshotBytes(t, s)); err == nil {
				t.Fatal("projection weakened the pending/unknown oracle", fault)
			}
			for _, name := range []string{"state.snapshot.json", "orders.json", "pending-review.json"} {
				got, err := os.ReadFile(filepath.Join(dir, name))
				if err != nil || !bytes.Equal(got, before) {
					t.Fatal("refused projection changed original file", name, err)
				}
			}
		})
	}
}

func TestStoppedLocalMergeCompanionRefusesForeignCustody(t *testing.T) {
	for _, fault := range []string{"third-order", "foreign-name", "title", "plan", "rationale", "status", "stage-count", "task-key", "prompt", "skill", "provider", "model", "runtime", "group", "stage-index", "stage-status", "extra-prompt", "merge", "attempt-completed", "attempt-failed", "attempt-running", "review", "review-subject", "scope-empty", "scope-schedule", "scope-foreign", "scope-extra", "effects-missing", "effect-subject-missing", "missing-attempts"} {
		t.Run(fault, func(t *testing.T) {
			s, cfg := dormantStoppedMergeSnapshot(t)
			o := s.State.Orders[scheduleOrderID]
			stage := &o.Stages[0]
			switch fault {
			case "third-order":
				s.State.Orders["third"] = s.State.Orders["original"]
			case "foreign-name":
				delete(s.State.Orders, scheduleOrderID)
				o.OrderID = "foreign"
				s.State.Orders["foreign"] = o
			case "title":
				o.Title = "foreign"
			case "plan":
				o.Plan = []string{"foreign"}
			case "rationale":
				o.Rationale = "foreign"
			case "status":
				o.Status = state.OrderCompleted
			case "stage-count":
				o.Stages = append(o.Stages, *stage)
			case "task-key":
				stage.TaskKey = "local-task"
			case "prompt":
				stage.Prompt = "foreign"
			case "skill":
				stage.Skill = "local-task"
			case "provider":
				stage.Provider = "foreign"
			case "model":
				stage.Model = "foreign"
			case "runtime":
				stage.Runtime = "openshell"
			case "group":
				stage.Group = 1
			case "stage-index":
				stage.StageIndex = 1
			case "stage-status":
				stage.Status = state.StageCompleted
			case "extra-prompt":
				stage.ExtraPrompt = "foreign"
			case "merge":
				stage.Merge = &state.MergeRecoveryNode{WorktreeName: "foreign", Mode: "local"}
			case "attempt-completed", "attempt-failed", "attempt-running":
				stage.Attempts = []state.AttemptNode{{AttemptID: "foreign", SessionID: "foreign", Status: state.AttemptStatus(strings.TrimPrefix(fault, "attempt-"))}}
			case "review":
				s.State.PendingReviews[scheduleOrderID] = state.PendingReviewNode{OrderID: scheduleOrderID}
			case "review-subject":
				s.State.PendingReviews["original"] = state.PendingReviewNode{OrderID: scheduleOrderID}
			case "scope-empty":
				cfg.Runtime.AdmittedOrders = nil
			case "scope-schedule":
				cfg.Runtime.AdmittedOrders = []string{"original", scheduleOrderID}
			case "scope-foreign":
				cfg.Runtime.AdmittedOrders = []string{"foreign"}
			case "scope-extra":
				cfg.Runtime.AdmittedOrders = []string{"original", "foreign"}
			case "effects-missing":
				s.EffectLedger = nil
			case "effect-subject-missing":
				s.EffectLedger[0].Effect.Payload = json.RawMessage(`{}`)
			}
			if fault != "foreign-name" {
				s.State.Orders[scheduleOrderID] = o
			}
			raw := stoppedMergeSnapshotBytes(t, s)
			if fault == "missing-attempts" {
				raw = bytes.Replace(raw, []byte(`"attempts": null`), []byte(`"extra": {}`), 1)
			}
			before := append([]byte(nil), raw...)
			if err := ValidateStoppedLocalMergeCompanion(raw, "original", cfg); err == nil {
				t.Fatal("unqualified companion accepted", fault)
			}
			if !bytes.Equal(raw, before) {
				t.Fatal("qualification changed before-image")
			}
		})
	}
	for _, kind := range []reducer.EffectType{reducer.EffectInitialAdmission, reducer.EffectDispatch, reducer.EffectMerge, reducer.EffectAck, reducer.EffectCleanup, reducer.EffectWriteProjection} {
		for _, status := range []reducer.EffectLedgerStatus{reducer.EffectLedgerPending, reducer.EffectLedgerRunning, reducer.EffectLedgerDone, reducer.EffectLedgerFailed} {
			t.Run(string(kind)+"/"+string(status), func(t *testing.T) {
				s, cfg := dormantStoppedMergeSnapshot(t)
				row := reducer.EffectLedgerRecord{EffectID: "schedule-effect", Effect: reducer.Effect{EffectID: "schedule-effect", Type: kind, Payload: json.RawMessage(`{"order_id":"schedule","initial_revision":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}`)}, Status: status}
				if kind == reducer.EffectInitialAdmission {
					row.EffectID = initialAdmissionEffectID(scheduleOrderID)
					row.Effect.EffectID = row.EffectID
				}
				if status == reducer.EffectLedgerDone || status == reducer.EffectLedgerFailed {
					row.Result = &reducer.EffectResult{EffectID: row.EffectID, Status: reducer.EffectResultCompleted}
					if status == reducer.EffectLedgerFailed {
						row.Result.Status = reducer.EffectResultFailed
					}
				}
				s.EffectLedger = append(s.EffectLedger, row)
				if err := ValidateStoppedLocalMergeCompanion(stoppedMergeSnapshotBytes(t, s), "original", cfg); err == nil {
					t.Fatal("historical schedule effect accepted", kind, status)
				}
			})
		}
	}
	for _, key := range []string{localTaskMergeKey, localTaskMergeRecoveryKey, localTaskCompletionRecoveryKey, requestChangesKey, requestChangesRequeuedKey, savedProgressAttemptsKey, interruptionKey, interruptionHistoryKey, nativeDegradationKey, "merge_worktree", "merge_mode", "merge_branch", "stopped_local_merge_continuation"} {
		t.Run(key, func(t *testing.T) {
			s, cfg := dormantStoppedMergeSnapshot(t)
			o := s.State.Orders[scheduleOrderID]
			o.Stages[0].Extra = map[string]json.RawMessage{key: json.RawMessage(`null`)}
			s.State.Orders[scheduleOrderID] = o
			if err := ValidateStoppedLocalMergeCompanion(stoppedMergeSnapshotBytes(t, s), "original", cfg); err == nil {
				t.Fatal("schedule execution history accepted", key)
			}
		})
	}
}
