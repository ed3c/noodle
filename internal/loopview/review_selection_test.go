package loopview

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func TestReviewSelectionIdempotentBoundReadbackAndDrift(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "request.json")
	raw := []byte(`{"goal":"inspect the original route"}`)
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	view := graphFixture()
	sum := sha256.Sum256(raw)
	a := &view.Graph.Artifacts[0]
	a.Role = "loop_request"
	a.Source = &ArtifactSource{Path: path, SHA256: ptr(hex.EncodeToString(sum[:])), Bytes: ptr(int64(len(raw)))}
	a.Content = ptr(string(raw))
	if _, _, err = Import(dir, view); err != nil {
		t.Fatal(err)
	}
	current, _ := Load(dir)
	version := ReviewVersion(current)
	first, err := SaveReviewSelection(dir, version, "run", "node", "producer")
	if err != nil {
		t.Fatal(err)
	}
	second, err := SaveReviewSelection(dir, version, "run", "node", "producer")
	if err != nil {
		t.Fatal(err)
	}
	if first.Selection.ID != second.Selection.ID || first.Status != "current" {
		t.Fatal("duplicate selection changed or lost current source")
	}
	tampered := first.Selection
	tampered.ArtifactIDs = []string{}
	if read := readReview(current, tampered); read.Status != "unknown" || read.Problem == "" {
		t.Fatal("changed receipt evidence was accepted as current")
	}
	if _, err = SaveReviewSelection(dir, version, "other", "node", "producer"); err == nil {
		t.Fatal("cross-Run target accepted")
	}
	if _, err = SaveReviewSelection(dir, "old", "run", "node", "producer"); err == nil {
		t.Fatal("old graph accepted")
	}
	if _, err = InspectReviewSelections(dir, path+"other", first.Selection.ID); err == nil {
		t.Fatal("wrong request accepted")
	}
	if err = os.WriteFile(path, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	observed, err := InspectReviewSelections(dir, path, first.Selection.ID)
	if err != nil {
		t.Fatal(err)
	}
	if observed[0].Status != "stale" || len(observed[0].Effects) != 0 {
		t.Fatal("drift hidden or effects created")
	}
	if replay, err := SaveReviewSelection(dir, version, "run", "node", "producer"); err != nil || replay.Status != "stale" {
		t.Fatal("same selection did not read back original stale evidence")
	}
	if _, err = SaveReviewSelection(dir, version, "run", "node", "consumer"); err == nil {
		t.Fatal("new stale source selected")
	}
}
