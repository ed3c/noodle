package runtime

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenShellCustodyControlRootSurvivesReopen(t *testing.T) {
	g, b, security, open := custodyFixture(t)
	c := custodyOpen(t, b, security, nil, open)
	state := c.Snapshot()
	canonical, err := filepath.EvalSymlinks(filepath.Dir(b.StateRoot))
	if err != nil || state.Version != 4 || state.ControlRoot != canonical {
		t.Fatal(state.ControlRoot, err)
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(b.StateRoot, "owner.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	counts := []int{}
	a, stop, exec := g.counts()
	counts = append(counts, a, stop, exec)
	refused, err := OpenOpenShellCustody(t.Context(), b, security, nil, other)
	if err == nil || refused != nil {
		t.Fatal("different control root accepted", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("refusal changed original ledger", err)
	}
	a, stop, exec = g.counts()
	if a != counts[0] || stop != counts[1] || exec != counts[2] {
		t.Fatal("refusal sent a native mutation")
	}
	c = custodyOpen(t, b, security, nil, open)
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestOpenShellCustodyDoesNotUpgradeV3OnOpen(t *testing.T) {
	_, b, security, open := custodyFixture(t)
	c := custodyOpen(t, b, security, nil, open)
	state := c.Snapshot()
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	state.Version, state.ControlRoot, state.Handover = 3, "", nil
	before, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(b.StateRoot, "owner.json")
	if err = os.WriteFile(path, before, 0600); err != nil {
		t.Fatal(err)
	}
	owner, err := open(t.Context(), b, security, nil)
	if err == nil || owner != nil {
		t.Fatal("v3 silently upgraded", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("v3 bytes changed on refused open", err)
	}
}
