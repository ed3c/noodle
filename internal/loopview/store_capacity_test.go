package loopview

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func capacityView(t *testing.T, id string, size int) View {
	t.Helper()
	v := graphFixture()
	v.Runs = []Run{testRun(id)}
	for i := range v.Graph.Nodes {
		v.Graph.Nodes[i].RunID = id
		v.Graph.Nodes[i].ID += id
	}
	v.Graph.Edges = []Edge{}
	v.Graph.Artifacts = []Artifact{}
	for i := range v.Graph.Nodes {
		v.Graph.Nodes[i].InputArtifactIDs = []string{}
		v.Graph.Nodes[i].OutputArtifactIDs = []string{}
		v.Graph.Nodes[i].EvidenceArtifactIDs = []string{}
	}
	body := strings.Repeat(id[:1], size)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, id)
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(body))
	v.Graph.Artifacts = []Artifact{{ID: id, Role: "source", Format: "text/plain", Authority: "knowledge", KnowledgeRole: "source", Scope: "run", Status: "available", Source: &ArtifactSource{Path: path, SHA256: ptr(hex.EncodeToString(sum[:])), Bytes: ptr(int64(len(body)))}, Content: &body, ContentEncoding: "utf-8"}}
	return v
}

func TestImportCapacityUsesBoundArtifactsAndRetainsExactHistory(t *testing.T) {
	dir := t.TempDir()
	a := capacityView(t, "first", 17<<20)
	// Reproduce an existing registry with retained inline bodies.
	prior, err := json.MarshalIndent(a, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(Path(dir), prior, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(dir); err != nil {
		t.Fatal(err)
	}
	b := capacityView(t, "second", 17<<20)
	changed, count, err := Import(dir, b)
	if err != nil || !changed || count != 2 {
		t.Fatalf("import: %v %d %v", changed, count, err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Graph.Artifacts) != 2 || len(got.Graph.Nodes) != 4 {
		t.Fatal("history identities lost")
	}
	for _, a := range got.Graph.Artifacts {
		content := ArtifactContent(a)
		if content.Content == nil || len(*content.Content) != 17<<20 {
			t.Fatal("original content cannot be read")
		}
	}
	history, err := os.ReadDir(filepath.Join(dir, "loop-view-history"))
	if err != nil {
		t.Fatal(err)
	}
	exact := false
	for _, f := range history {
		raw, err := os.ReadFile(filepath.Join(dir, "loop-view-history", f.Name()))
		if err != nil {
			t.Fatal(err)
		}
		exact = exact || bytes.Equal(raw, prior)
	}
	if !exact {
		t.Fatal("exact prior registry missing")
	}
	if b.Graph.Artifacts[0].Content == nil {
		t.Fatal("caller input mutated")
	}
	before, _ := os.ReadFile(Path(dir))
	changed, _, err = Import(dir, b)
	after, _ := os.ReadFile(Path(dir))
	if err != nil || changed || !bytes.Equal(before, after) {
		t.Fatal("retry changed accepted state", err)
	}
}

func TestImportArchiveFailureAndInvalidContentPreserveRegistry(t *testing.T) {
	dir := t.TempDir()
	a := capacityView(t, "first", 10)
	raw, _ := json.Marshal(a)
	if err := os.WriteFile(Path(dir), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "loop-view-history"), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	b := capacityView(t, "second", 10)
	if _, err := Migrate(dir); err == nil {
		t.Fatal("archive failure ignored")
	}
	after, _ := os.ReadFile(Path(dir))
	if !bytes.Equal(raw, after) {
		t.Fatal("failed archive changed registry")
	}
	b.Graph.Artifacts[0].Content = ptr("forged")
	if _, _, err := Import(t.TempDir(), b); err == nil {
		t.Fatal("normalization laundered invalid content")
	}
}

func TestImportFirstInlineOnMetadataNoopRetainsHistory(t *testing.T) {
	dir := t.TempDir()
	incoming := capacityView(t, "first", 32)
	if _, _, err := Import(dir, referencedArtifacts(incoming)); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	changed, _, err := Import(dir, incoming)
	if err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(Path(dir))
	if err != nil || changed || !bytes.Equal(before, after) {
		t.Fatal("metadata NOOP changed registry", err)
	}
	raw, err := json.Marshal(incoming)
	if err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(dir, "loop-view-history", fmt.Sprintf("%x.json", sha256.Sum256(raw)))
	got, err := os.ReadFile(archive)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatal("accepted incoming body missing from history", err)
	}
}
