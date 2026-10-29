package loopview

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	current, _ := LoadSummary(dir)
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
	// A later projection may change the same artifact metadata; the saved selection
	// must resolve its original artifact role/source rather than today's row.
	view.Graph.Nodes[0].Label = "updated node"
	view.Graph.Artifacts[0].Role = "source"
	if _, _, err = Import(dir, view); err != nil {
		t.Fatal(err)
	}
	historical, err := InspectReviewSelections(dir, path, first.Selection.ID)
	if err != nil || historical[0].Status != "stale" || len(historical[0].Sources) == 0 || historical[0].Sources[0].Artifact.Role != "loop_request" {
		t.Fatal("original selection source replaced", err)
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

func TestLegacySelectionKeepsOriginalSourcesAcrossExplicitMigration(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "request.json")
	body := []byte(`{"request":"original"}`)
	if err = os.WriteFile(source, body, 0600); err != nil {
		t.Fatal(err)
	}
	v := graphFixture()
	a := &v.Graph.Artifacts[0]
	a.Role = "loop_request"
	a.Content = nil
	sum := sha256.Sum256(body)
	a.Source = &ArtifactSource{Path: source, SHA256: ptr(hex.EncodeToString(sum[:])), Bytes: ptr(int64(len(body)))}
	raw, _ := json.Marshal(v)
	v, err = Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	request, err := reviewRequest(v, "run")
	if err != nil {
		t.Fatal(err)
	}
	refs, err := reviewTarget(v, "run", "node", "producer")
	if err != nil {
		t.Fatal(err)
	}
	s := ReviewSelection{Version: 1, GraphVersion: ReviewVersion(v), RunID: "run", Kind: "node", TargetID: "producer", Request: request, ArtifactIDs: unionRefs(refs, []string{request.ID})}
	s.ID = reviewID(s)
	receipt, _ := json.Marshal(s)
	if err = os.MkdirAll(filepath.Join(dir, "loop-review-selections"), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "loop-review-selections", s.ID+".json")
	if err = os.WriteFile(path, receipt, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(Path(dir), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = Migrate(dir); err != nil {
		t.Fatal(err)
	}
	result, err := InspectReviewSelections(dir, source, s.ID)
	if err != nil || len(result) != 1 || result[0].Status != "stale" || len(result[0].Sources) != 1 || result[0].Sources[0].Content == nil || *result[0].Sources[0].Content != string(body) {
		t.Fatalf("legacy evidence lost: %+v %v", result, err)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(receipt, after) {
		t.Fatal("migration rewrote selection")
	}
}
