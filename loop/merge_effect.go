package loop

import (
	"encoding/json"
	"fmt"

	"github.com/poteto/noodle/internal/ingest"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
)

// The merge owner has just observed success. Bind its result to the effect
// that produced this exact merging stage before checkpointing both together.
func (l *Loop) recordCompletedMergeEffect(before, after state.State, event ingest.StateEvent) error {
	if event.Type != string(ingest.EventMergeCompleted) || l.effectLedger == nil {
		return nil
	}
	var subject struct {
		OrderID    string `json:"order_id"`
		StageIndex int    `json:"stage_index"`
	}
	if err := json.Unmarshal(event.Payload, &subject); err != nil {
		return err
	}
	_, stage, exists := before.LookupStage(subject.OrderID, subject.StageIndex)
	_, completed, existsAfter := after.LookupStage(subject.OrderID, subject.StageIndex)
	if !exists || !existsAfter || stage.Status != state.StageMerging || stage.Merge == nil || completed.Status != state.StageCompleted || completed.Merge != nil {
		return nil
	}
	var selected *reducer.EffectLedgerRecord
	for _, record := range l.effectLedger.All() {
		if record.Effect.Type != reducer.EffectMerge || record.Status == reducer.EffectLedgerDone || record.Status == reducer.EffectLedgerFailed {
			continue
		}
		var identity struct {
			OrderID      string `json:"order_id"`
			StageIndex   int    `json:"stage_index"`
			WorktreeName string `json:"worktree_name"`
			Mode         string `json:"merge_mode"`
			Branch       string `json:"merge_branch"`
		}
		if err := json.Unmarshal(record.Effect.Payload, &identity); err != nil {
			return fmt.Errorf("merge effect identity is unresolved: %w", err)
		}
		if identity.OrderID != subject.OrderID || identity.StageIndex != subject.StageIndex {
			continue
		}
		if record.Result != nil || (record.Status != reducer.EffectLedgerPending && record.Status != reducer.EffectLedgerRunning) || identity.WorktreeName != stage.Merge.WorktreeName || identity.Mode != stage.Merge.Mode || identity.Branch != stage.Merge.Branch || selected != nil {
			return fmt.Errorf("merge effect custody is ambiguous for %s/%d", subject.OrderID, subject.StageIndex)
		}
		copy := record
		selected = &copy
	}
	if selected == nil {
		return nil
	}
	if selected.Status == reducer.EffectLedgerPending {
		if err := l.effectLedger.MarkRunning(selected.EffectID); err != nil {
			return err
		}
	}
	return l.effectLedger.MarkDone(selected.EffectID, reducer.EffectResult{EffectID: selected.EffectID, Status: reducer.EffectResultCompleted, Timestamp: event.Timestamp})
}
