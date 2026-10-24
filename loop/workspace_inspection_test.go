package loop

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWorkspaceInspectionRetainsSessionSourcesWithoutCanonicalSnapshot(t *testing.T) {
	root := t.TempDir()
	session := filepath.Join(root, ".noodle", "sessions", "retained-session")
	if err := os.MkdirAll(session, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(session, "events.ndjson")
	if err := os.WriteFile(path, []byte("retained evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	got := ReadWorkspaceInspection(root)
	if got.Status != "unavailable" || len(got.Sources) != 1 || got.Sources[0].Path != path || got.Sources[0].SessionID != "retained-session" || len(got.Gaps) != 1 {
		t.Fatalf("missing canonical hid retained evidence: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(root, ".noodle", "noodle.lock")); !os.IsNotExist(err) {
		t.Fatal("read acquired a mutation lock")
	}
}
