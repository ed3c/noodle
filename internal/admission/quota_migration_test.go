package admission

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestQuotaMigrationPreservesWindowAndConcurrentOutcome(t *testing.T) {
	dir, custody := t.TempDir(), t.TempDir()
	original := []byte(`{"schema":1,"maximum":4,"swap_used_limit_bytes":15000000000,"last_start_window":358629289,"leases":{},"retained_field":"original"}`)
	path := filepath.Join(dir, "quota.json")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := MigrateEmptySharedQuota(context.Background(), dir, custody, original); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var q map[string]json.RawMessage
	if err = json.Unmarshal(raw, &q); err != nil {
		t.Fatal(err)
	}
	if string(q["schema"]) != "2" || string(q["last_start_window"]) != "358629289" || string(q["retained_field"]) != `"original"` {
		t.Fatal(string(raw))
	}
	backup, err := os.ReadFile(filepath.Join(custody, "quota-schema1.json"))
	if err != nil || !bytes.Equal(original, backup) {
		t.Fatal("old bytes not preserved", err)
	}
	// New leases are never erased by re-entering maintenance with an old plan.
	q["leases"] = json.RawMessage(`{"new":{"owner":"new-owner","context":"active"}}`)
	live, _ := json.Marshal(q)
	if err = os.WriteFile(path, live, 0600); err != nil {
		t.Fatal(err)
	}
	if err = MigrateEmptySharedQuota(context.Background(), dir, custody, original); err == nil {
		t.Fatal("new allocation overwritten")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(after, live) {
		t.Fatal("live ledger changed")
	}
}

func TestQuotaMigrationRefusesDriftAndNonemptySources(t *testing.T) {
	for _, raw := range []string{`{"schema":1,"maximum":4,"swap_used_limit_bytes":15000000000,"leases":{"live":{}}}`, `{"schema":1,"maximum":4,"swap_used_limit_bytes":15000000000}`, `{"schema":2,"maximum":4,"swap_used_limit_bytes":15000000000,"leases":{}}`} {
		dir := t.TempDir()
		path := filepath.Join(dir, "quota.json")
		if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		if err := MigrateEmptySharedQuota(context.Background(), dir, t.TempDir(), []byte(raw)); err == nil {
			t.Fatal("invalid migration source accepted", raw)
		}
		after, _ := os.ReadFile(path)
		if string(after) != raw {
			t.Fatal("refused source was changed")
		}
	}
}
