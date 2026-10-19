package loop

import (
	"encoding/json"
	"reflect"
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
			err := l.recordCompletedMergeEffect(before, after, event)
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
