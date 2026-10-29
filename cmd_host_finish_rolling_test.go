package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRollingPoolFailureRetainsFailedOriginalContext(t *testing.T) {
	h, c := newHostShutdownFixture(t)
	root := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(root, nil, 0600); err != nil {
		t.Fatal(err)
	}
	h.rolling = &hostRolling{root: root}
	h.applyHostFinish(c, nil, nil)
	if !c.failed || !h.blocked || h.contexts[c.receipt.ContextID] != c || c.receipt.ContextState != "failed" || c.receipt.TerminalCause != "" || h.rolling.failure == "" {
		t.Fatalf("pool failure lost original owner: %+v", c.receipt)
	}
	raw, err := os.ReadFile(filepath.Join(h.dir, "receipts.json"))
	if err != nil {
		t.Fatal(err)
	}
	var rows []hostReceipt
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].ContextState != "failed" || rows[0].TerminalCause != "" {
		t.Fatalf("durable success after pool failure: %s", raw)
	}
}
