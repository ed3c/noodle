package loop

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
)

// An explicit path lets the original owner/consumer fixture hand the same current state to Go.
func TestUpgradeCurrentStateConsumer(t *testing.T) {
	runtimeDir := os.Getenv("UPGRADE_FIXTURE_RUNTIME")
	if runtimeDir == "" {
		runtimeDir = t.TempDir()
		at := time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)
		ledger := reducer.NewEffectLedger()
		ledger.Record(reducer.Effect{EffectID: "original-unknown-effect", Type: reducer.EffectDispatch, Payload: []byte(`{"order_id":"fixture"}`), CreatedAt: at})
		if err := ledger.MarkRunning("original-unknown-effect"); err != nil {
			t.Fatal(err)
		}
		snapshot := reducer.BuildSnapshot(state.State{Orders: map[string]state.OrderNode{}, LastEventID: "42"}, ledger, at)
		snapshot.OrderRevision = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		if err := reducer.WriteSnapshotAtomic(filepath.Join(runtimeDir, "state.snapshot.json"), snapshot); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(runtimeDir, "state.snapshot.json")
	before, err := reducer.ReadSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	l := &Loop{runtimeDir: runtimeDir, deps: Dependencies{Now: time.Now}}
	if loaded, err := l.loadCanonicalSnapshot(); err != nil || !loaded {
		t.Fatal("original canonical load", loaded, err)
	}
	if err := l.canonical.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := l.persistCanonicalCheckpoint(); err != nil {
		t.Fatal(err)
	}
	after, err := reducer.ReadSnapshot(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.State, after.State) || before.OrderRevision != after.OrderRevision {
		t.Fatal("cold consume rewrote current state or original ownership revision")
	}
	beforeLedger, err := json.Marshal(before.EffectLedger)
	if err != nil {
		t.Fatal(err)
	}
	afterLedger, err := json.Marshal(after.EffectLedger)
	if err != nil {
		t.Fatal(err)
	}
	if string(beforeLedger) != string(afterLedger) {
		t.Fatalf("cold consume rewrote effect history: before=%+v after=%+v", before.EffectLedger, after.EffectLedger)
	}
}
