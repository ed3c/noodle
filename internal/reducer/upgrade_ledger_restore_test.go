package reducer

import (
	"reflect"
	"testing"
	"time"
)

func TestRestoreEffectLedgerPreservesHistory(t *testing.T) {
	at := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
	ledger := NewEffectLedger()
	for _, id := range []string{"pending", "running", "done", "failed", "cancelled", "missing-result"} {
		ledger.Record(Effect{EffectID: id, Type: EffectDispatch, Payload: []byte(`{"order_id":"original"}`), CreatedAt: at})
	}
	for _, id := range []string{"running", "done", "failed"} {
		if err := ledger.MarkRunning(id); err != nil {
			t.Fatal(err)
		}
	}
	if err := ledger.MarkDone("done", EffectResult{EffectID: "done", Status: EffectResultCompleted, Timestamp: at}); err != nil {
		t.Fatal(err)
	}
	if err := ledger.MarkFailed("failed", EffectResult{EffectID: "failed", Status: EffectResultFailed, Timestamp: at, Error: "original failure"}); err != nil {
		t.Fatal(err)
	}
	if err := ledger.CancelPending("cancelled", "original cancellation", at); err != nil {
		t.Fatal(err)
	}
	records := ledger.All()
	// Preserve historical uncertainty as stored; never synthesize a terminal result.
	records[len(records)-1].Status = EffectLedgerDone
	restored, err := RestoreEffectLedger(records)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(restored.All(), records) {
		t.Fatal("restore changed original effect history")
	}
	records[0].Effect.Payload[0] = '!'
	if restored.All()[0].Effect.Payload[0] == '!' {
		t.Fatal("restore shared mutable input payload")
	}
	records[2].Result.Error = "mutated"
	if restored.All()[2].Result.Error == "mutated" {
		t.Fatal("restore shared mutable result")
	}
}

func TestRestoreEffectLedgerRejectsForeignAndPartial(t *testing.T) {
	base := EffectLedgerRecord{EffectID: "original", Effect: Effect{EffectID: "original"}, Status: EffectLedgerRunning}
	cases := [][]EffectLedgerRecord{
		{base, base},
		{{EffectID: "original", Effect: Effect{EffectID: "foreign"}, Status: EffectLedgerRunning}},
		{{EffectID: "original", Effect: Effect{EffectID: "original"}, Status: "foreign"}},
		{{EffectID: "original", Effect: Effect{EffectID: "original"}, Status: EffectLedgerDone, Result: &EffectResult{EffectID: "foreign"}}},
	}
	for _, records := range cases {
		if _, err := RestoreEffectLedger(records); err == nil {
			t.Fatalf("foreign/partial accepted: %+v", records)
		}
	}
}
