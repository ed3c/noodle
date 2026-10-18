package loopview

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestViewEnvNativeBridgeFixture is an opt-in original-Noodle fixture Producer.
// It is NOT a worker invocation: no synthetic successful native occurrences,
// tool events or green causal edges are generated. The original SaveReviewSelection
// and Import owners compute every ID, source digest and immutable receipt.
// Only isolated CI activates this via explicit VIEWENV_BRIDGE_* paths.
func TestViewEnvNativeBridgeFixture(t *testing.T) {
	host := os.Getenv("VIEWENV_BRIDGE_HOST_DIR")
	requestPath := os.Getenv("VIEWENV_BRIDGE_REQUEST_FILE")
	project := os.Getenv("VIEWENV_BRIDGE_PROJECT_DIR")
	receiptPath := os.Getenv("VIEWENV_BRIDGE_RECEIPT_FILE")
	if host == "" && requestPath == "" && project == "" && receiptPath == "" {
		t.Skip("No explicitly selected offline integration fixture")
	}
	for _, item := range []string{host, requestPath, project, receiptPath} {
		if !filepath.IsAbs(item) || item == "" {
			t.Fatal("all original fixture paths must be absolute")
		}
	}
	if err := os.MkdirAll(host, 0700); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(requestPath)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	requestSHA := hex.EncodeToString(sum[:])
	size := int64(len(raw))
	content := string(raw)
	view := graphFixture()
	view.Runs[0].ID = requestSHA
	view.Runs[0].Repository = project
	view.Runs[0].Status = "unknown"
	view.Graph.Repositories[0].Root = project
	view.Graph.Artifacts[0].Role = "loop_request"
	view.Graph.Artifacts[0].Scope = requestSHA
	view.Graph.Artifacts[0].Source = &ArtifactSource{Path: requestPath, SHA256: &requestSHA, Bytes: &size}
	view.Graph.Artifacts[0].Content = &content
	view.Graph.Artifacts[0].Format = "application/json"
	view.Graph.Nodes = view.Graph.Nodes[:1]
	view.Graph.Nodes[0].RunID = requestSHA
	view.Graph.Nodes[0].Label = "Original request source (fixture; no invocation)"
	view.Graph.Nodes[0].Identity = NodeIdentity{Status: "unknown"}
	view.Graph.Nodes[0].Status = NodeStatus{Execution: "unknown", Transfer: "unknown", Acceptance: "unknown"}
	view.Graph.Objects = view.Graph.Objects[:1]
	view.Graph.Ports = []Port{}
	view.Graph.Edges = []Edge{}
	view.Graph.Findings = []Finding{}
	view.Graph.Metrics = []Metric{}
	if _, _, err := Import(host, view); err != nil {
		t.Fatal(err)
	}
	stored, err := Load(host)
	if err != nil {
		t.Fatal(err)
	}
	version := ReviewVersion(stored)
	selection, err := SaveReviewSelection(host, version, requestSHA, "node", "producer")
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := SaveReviewSelection(host, version, requestSHA, "node", "producer")
	if err != nil {
		t.Fatal(err)
	}
	if selection.Status != "current" || repeated.Selection.ID != selection.Selection.ID ||
		len(selection.Effects) != 0 || len(stored.Graph.Edges) != 0 ||
		stored.Graph.Nodes[0].Identity.Status != "unknown" {
		t.Fatal("offline fixture falsely asserted native execution or changed selection")
	}
	manifest := map[string]string{
		"request_file": requestPath, "request_sha256": requestSHA,
		"run_id": requestSHA, "review_id": selection.Selection.ID,
		"evidence_id": view.Graph.Artifacts[0].ID, "host_dir": host,
		"status": selection.Status,
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(receiptPath, data, 0600); err != nil {
		t.Fatal(err)
	}
}
