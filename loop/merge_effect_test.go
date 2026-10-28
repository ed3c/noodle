package loop

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/poteto/noodle/internal/ingest"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
)

func TestCompletedMergeEffectUsesExactTransitionAndPreservesOthers(t *testing.T) {
	for _, condition := range []string{"exact", "duplicate", "different-worktree", "already-completed-label", "invalid-effect"} {
		t.Run(condition, func(t *testing.T) {
			l := completedCleanupFixture(t)
			before := l.canonical.Clone()
			order := before.Orders["done"]
			order.Status = state.OrderActive
			order.Stages[0].Status = state.StageMerging
			order.Stages[0].Merge = &state.MergeRecoveryNode{WorktreeName: "owned", Mode: "local"}
			before.Orders["done"] = order
			after := l.canonical.Clone()
			add := func(id, order string, stage int, worktree string) {
				payload, err := json.Marshal(map[string]any{"order_id": order, "stage_index": stage, "worktree_name": worktree, "merge_mode": "local", "merge_branch": ""})
				if err != nil {
					t.Fatal(err)
				}
				l.effectLedger.Record(reducer.Effect{EffectID: id, Type: reducer.EffectMerge, Payload: payload})
			}
			add("current", "done", 0, "owned")
			add("other-order", "other", 0, "owned")
			add("other-stage", "done", 1, "owned")
			add("old-failure", "done", 0, "owned")
			if err := l.effectLedger.MarkRunning("old-failure"); err != nil {
				t.Fatal(err)
			}
			if err := l.effectLedger.MarkFailed("old-failure", reducer.EffectResult{EffectID: "old-failure", Status: reducer.EffectResultFailed, Error: "retained original failure"}); err != nil {
				t.Fatal(err)
			}
			switch condition {
			case "duplicate":
				add("duplicate", "done", 0, "owned")
			case "different-worktree":
				order.Stages[0].Merge.WorktreeName = "foreign"
				before.Orders["done"] = order
			case "already-completed-label":
				before = after.Clone()
			case "invalid-effect":
				l.effectLedger.Record(reducer.Effect{EffectID: "invalid", Type: reducer.EffectMerge, Payload: []byte("{")})
			}
			original := l.effectLedger.All()
			event := ingest.StateEvent{Type: string(ingest.EventMergeCompleted), Timestamp: time.Now(), Payload: []byte(`{"order_id":"done","stage_index":0}`)}
			err := l.recordMergeEffectResult(before, after, event)
			if condition == "exact" {
				if err != nil {
					t.Fatal(err)
				}
				for i, record := range l.effectLedger.All() {
					if record.EffectID == "current" {
						if record.Status != reducer.EffectLedgerDone || record.Result == nil || record.Result.Status != reducer.EffectResultCompleted {
							t.Fatalf("result absent: %+v", record)
						}
					} else if !reflect.DeepEqual(original[i], record) {
						t.Fatalf("unrelated history changed: %s", record.EffectID)
					}
				}
			} else {
				if (err == nil) != (condition == "already-completed-label") {
					t.Fatalf("unexpected reconcile result: %v", err)
				}
				if !reflect.DeepEqual(original, l.effectLedger.All()) {
					t.Fatal("unknown or repeated label rewrote effect history")
				}
			}
		})
	}
}

func TestMergeFailureRequiresObservedConflict(t *testing.T) {
	for _, observed := range []bool{false, true} {
		t.Run(fmt.Sprint(observed), func(t *testing.T) {
			l := completedCleanupFixture(t)
			before := l.canonical.Clone()
			order := before.Orders["done"]
			order.Status = state.OrderActive
			order.Stages[0].Status = state.StageMerging
			order.Stages[0].Merge = &state.MergeRecoveryNode{WorktreeName: "owned", Mode: "local"}
			before.Orders["done"] = order
			l.effectLedger.Record(reducer.Effect{EffectID: "original", Type: reducer.EffectMerge,
				Payload: []byte(`{"order_id":"done","stage_index":0,"worktree_name":"owned","merge_mode":"local"}`)})
			original := l.effectLedger.All()
			payload, err := json.Marshal(map[string]any{"order_id": "done", "stage_index": 0, "error": "merge interrupted", "conflict_observed": observed})
			if err != nil {
				t.Fatal(err)
			}
			event := ingest.StateEvent{Type: string(ingest.EventMergeFailed), Timestamp: time.Now(), Payload: payload}
			after, _, err := reducer.Reduce(before, event)
			if err != nil {
				t.Fatal(err)
			}
			if err := l.recordMergeEffectResult(before, after, event); err != nil {
				t.Fatal(err)
			}
			if !observed {
				if !reflect.DeepEqual(original, l.effectLedger.All()) {
					t.Fatal("unconfirmed command error changed original effect custody")
				}
				return
			}
			result := l.effectLedger.All()[0]
			if result.Status != reducer.EffectLedgerFailed || result.Result == nil || result.Result.Error != "merge interrupted" {
				t.Fatalf("observed failure was not retained: %+v", result)
			}
		})
	}
}

func TestUnresolvedMergeRefusesAnotherControlBeforeChangingCanonicalState(t *testing.T) {
	wt := &fakeWorktree{mergeErr: errors.New("git exited 1 without confirmed conflict")}
	l := newControlTestLoop(t, wt, newMockRuntime())
	if err := l.controlMerge("42"); err != nil {
		t.Fatal(err)
	}
	before := l.canonical.Clone()
	effects := l.effectLedger.All()
	merges := len(wt.merged)
	wt.mergeErr = nil
	if err := l.controlMerge("42"); err == nil || !strings.Contains(err.Error(), "original merge readback required") {
		t.Fatalf("unresolved original merge allowed another owner: %v", err)
	}
	if !reflect.DeepEqual(before, l.canonical) || !reflect.DeepEqual(effects, l.effectLedger.All()) || len(wt.merged) != merges {
		t.Fatal("refused merge changed original canonical/effect/Git custody")
	}
}

func TestNewMergeEffectGuardPreservesOriginalUnknownAndAllowsSettledHistory(t *testing.T) {
	for _, condition := range []string{"pending", "running", "different-order", "different-stage", "failed", "done"} {
		t.Run(condition, func(t *testing.T) {
			l := completedCleanupFixture(t)
			order, stage := "done", 0
			if condition == "different-order" {
				order = "other"
			}
			if condition == "different-stage" {
				stage = 1
			}
			payload, err := json.Marshal(map[string]any{"order_id": order, "stage_index": stage})
			if err != nil {
				t.Fatal(err)
			}
			l.effectLedger.Record(reducer.Effect{EffectID: "original", Type: reducer.EffectMerge, Payload: payload})
			if condition == "running" || condition == "failed" || condition == "done" {
				if err := l.effectLedger.MarkRunning("original"); err != nil {
					t.Fatal(err)
				}
			}
			if condition == "failed" {
				if err := l.effectLedger.MarkFailed("original", reducer.EffectResult{EffectID: "original", Status: reducer.EffectResultFailed, Error: "retained conflict"}); err != nil {
					t.Fatal(err)
				}
			}
			if condition == "done" {
				if err := l.effectLedger.MarkDone("original", reducer.EffectResult{EffectID: "original", Status: reducer.EffectResultCompleted}); err != nil {
					t.Fatal(err)
				}
			}
			before := l.effectLedger.All()
			err = l.validateNewMergeEffects([]reducer.Effect{{EffectID: "new", Type: reducer.EffectMerge, Payload: []byte(`{"order_id":"done","stage_index":0}`)}})
			blocked := condition == "pending" || condition == "running"
			if (err != nil) != blocked {
				t.Fatalf("blocked=%v error=%v", blocked, err)
			}
			if !reflect.DeepEqual(before, l.effectLedger.All()) {
				t.Fatal("guard rewrote historical effects")
			}
		})
	}
}
