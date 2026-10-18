package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/poteto/noodle/internal/loopview"
)

func TestHostViewsImportOfflineAndInspect(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("NOODLE_HOST_DIR", dir)
	source := filepath.Join(t.TempDir(), "view.json")
	data := `{"schema_version":1,"runs":[{"id":"original","repository":"repo","mode":"outer-only","status":"unknown","stages":[],"cards":[],"notes":[],"breakpoints":[],"next":null}]}`
	if err := os.WriteFile(source, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		t.Helper()
		cmd := NewRootCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}
	result := run("host", "views", "import", "--file", source)
	var receipt struct {
		Changed  bool `json:"changed"`
		RunCount int  `json:"run_count"`
	}
	if err := json.Unmarshal([]byte(result), &receipt); err != nil {
		t.Fatal(err)
	}
	if !receipt.Changed || receipt.RunCount != 1 {
		t.Fatalf("receipt: %s", result)
	}
	if got := run("host", "views", "inspect"); !strings.Contains(got, `"id":"original"`) {
		t.Fatalf("inspect: %s", got)
	}
	for _, name := range []string{"host.sock", "host.lock", "receipts.json"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("offline import touched Host lifecycle %s", name)
		}
	}
}

func TestHostReviewSelectionsNativeCommandHonorsOriginalHostDirectory(t *testing.T) {
	dir := t.TempDir()
	foreign := t.TempDir()
	t.Setenv("NOODLE_HOST_DIR", dir)
	source := filepath.Join(t.TempDir(), "original-request.json")
	raw := []byte(`{"goal":"source-bound ViewEnv inspection"}`)
	if err := os.WriteFile(source, raw, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	sha := hex.EncodeToString(sum[:])
	size := int64(len(raw))
	content := string(raw)
	owner := "source-repo"
	refs := []string{"original"}
	view := loopview.View{
		SchemaVersion: 2,
		Runs: []loopview.Run{{
			ID: "run", Repository: dir, Mode: "inner-only", Status: "unknown",
			Stages: []loopview.Stage{}, Cards: []loopview.Card{}, Notes: []loopview.Note{},
			Breakpoints: []loopview.Breakpoint{},
		}},
		Graph: &loopview.Graph{
			Objects: []loopview.Object{{ID: "original-owner", Namespace: "soodles.loop", Label: "Owner",
				Kind: "owner", Scope: "shared", SourceArtifactIDs: []string{}}},
			Ports: []loopview.Port{},
			Repositories: []loopview.Repository{{ID: owner, Label: "source", Root: dir,
				IndexStatus: "unknown", TrackedFiles: []loopview.IndexFile{}, SelectedFiles: []loopview.IndexFile{},
				Gaps: []string{}}},
			Artifacts: []loopview.Artifact{{ID: "original", Role: "loop_request", Format: "application/json",
				Authority: "source", KnowledgeRole: "source", Scope: "run", Status: "available",
				Source: &loopview.ArtifactSource{Path: source, SHA256: &sha, Bytes: &size},
				Content: &content, RepositoryID: &owner, ContentEncoding: "utf-8"}},
			Nodes: []loopview.Node{{ID: "original-node", RunID: "run", RepositoryID: owner,
				ObjectID: "original-owner", Kind: "cli", Label: "Source reference",
				Identity: loopview.NodeIdentity{Status: "unknown"},
				Status: loopview.NodeStatus{Execution: "unknown", Transfer: "unknown", Acceptance: "unknown"},
				InputArtifactIDs: []string{}, OutputArtifactIDs: refs, EvidenceArtifactIDs: []string{}}},
			Edges: []loopview.Edge{}, Findings: []loopview.Finding{}, Metrics: []loopview.Metric{},
		},
	}
	if _, _, err := loopview.Import(dir, view); err != nil {
		t.Fatal(err)
	}
	current, err := loopview.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	first, err := loopview.SaveReviewSelection(dir, loopview.ReviewVersion(current),
		"run", "node", "original-node")
	if err != nil {
		t.Fatal(err)
	}
	inspect := func() (string, error) {
		t.Helper()
		cmd := NewRootCmd()
		var output bytes.Buffer
		cmd.SetOut(&output)
		cmd.SetArgs([]string{"host", "views", "review-selections",
			"--request-file", source, "--id", first.Selection.ID})
		err := cmd.Execute()
		return output.String(), err
	}
	output, err := inspect()
	if err != nil {
		t.Fatal(err)
	}
	var result []loopview.ReviewReadback
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 || result[0].Status != "current" ||
		result[0].Selection.ID != first.Selection.ID || len(result[0].Effects) != 0 {
		t.Fatalf("original readonly Host CLI returned wrong source/status: %s", output)
	}
	t.Setenv("NOODLE_HOST_DIR", foreign)
	if _, err := inspect(); err == nil {
		t.Fatal("different Host registry returned original review selection")
	}
	t.Setenv("NOODLE_HOST_DIR", dir)
	if err := os.WriteFile(source, []byte("source changed"), 0600); err != nil {
		t.Fatal(err)
	}
	output, err = inspect()
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 || result[0].Status != "stale" {
		t.Fatalf("original changed source was accepted as current: %s", output)
	}
}
