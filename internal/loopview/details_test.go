package loopview

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetailsRetainBodiesBeyondRegistryCapAndIsolateMissingSources(t *testing.T) {
	dir := t.TempDir()
	for _, id := range []string{"first", "second"} {
		run := testRun(id)
		run.Stages = []Stage{{ID: "stage", Kind: "prompt", Status: "observed", ConsumerStatus: "unknown", Prompt: strings.Repeat(id[:1], 17<<20)}}
		if _, _, err := Import(dir, View{SchemaVersion: 1, Runs: []Run{run}}); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) > 10000 {
		t.Fatalf("registry retained bodies: %d", len(raw))
	}
	summary, err := LoadSummary(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range summary.Runs {
		full, err := RunDetail(dir, r)
		if err != nil || len(full.Stages[0].Prompt) != 17<<20 {
			t.Fatalf("body lost: %v", err)
		}
	}
	if err := os.Remove(detailPath(dir, summary.Runs[0].Detail.SHA256)); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSummary(dir); err != nil {
		t.Fatal("unrelated summary blocked", err)
	}
	if _, err := RunDetail(dir, summary.Runs[0]); err == nil {
		t.Fatal("missing detail treated as empty")
	}
	if _, err := RunDetail(dir, summary.Runs[1]); err != nil {
		t.Fatal("unrelated detail blocked", err)
	}
}

func TestDetailsDriftAndFailedPublicationPreserveRegistry(t *testing.T) {
	dir := t.TempDir()
	v := graphFixture()
	if _, _, err := Import(dir, v); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(Path(dir))
	s, _ := LoadSummary(dir)
	ref := s.Graph.Repositories[0].Detail
	if err := os.WriteFile(detailPath(dir, ref.SHA256), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := RepositoryDetail(dir, s.Graph.Repositories[0]); err == nil {
		t.Fatal("digest drift accepted")
	}
	if _, _, err := Import(dir, v); err == nil {
		t.Fatal("damaged original detail overwritten")
	}
	after, _ := os.ReadFile(Path(dir))
	if !bytes.Equal(before, after) {
		t.Fatal("failure changed registry")
	}
}

func TestExplicitMigrationPreservesExactSourceAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	v := graphFixture()
	raw, _ := json.MarshalIndent(v, "", "  ")
	if err := os.WriteFile(Path(dir), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSummary(dir); err == nil {
		t.Fatal("read implicitly migrated old registry")
	}
	if changed, err := Migrate(dir); err != nil || !changed {
		t.Fatalf("migrate: %v %v", changed, err)
	}
	archived, err := os.ReadFile(fmt.Sprintf("%s/loop-view-history/%x.json", dir, sha256.Sum256(raw)))
	if err != nil || !bytes.Equal(raw, archived) {
		t.Fatal("exact old source lost", err)
	}
	full, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := json.Marshal(full)
	want, _ := json.Marshal(v)
	// Decode normalizes nullable collections; compare normalized original.
	normalized, err := Decode(want)
	if err != nil {
		t.Fatal(err)
	}
	want, _ = json.Marshal(referencedArtifacts(normalized))
	if !bytes.Equal(got, want) {
		t.Fatal("migration changed logical projection")
	}
	if changed, err := Migrate(dir); err != nil || changed {
		t.Fatalf("retry: %v %v", changed, err)
	}
}

func TestUnfinishedMigrationCannotBeRetriedOrImported(t *testing.T) {
	dir := t.TempDir()
	v := graphFixture()
	raw, _ := json.Marshal(v)
	if err := os.WriteFile(Path(dir), raw, 0600); err != nil {
		t.Fatal(err)
	}
	pending := fmt.Sprintf("%s/loop-view-migrations/%x.prepared.json", dir, sha256.Sum256(raw))
	if err := writeImmutable(pending, []byte(`{"version":1}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := Migrate(dir); err == nil {
		t.Fatal("unresolved migration retried")
	}
	if _, _, err := Import(dir, v); err == nil {
		t.Fatal("unresolved migration bypassed")
	}
	after, _ := os.ReadFile(Path(dir))
	if !bytes.Equal(raw, after) {
		t.Fatal("unresolved source changed")
	}
}

func TestRepositoryDetailsPreserveSnapshotIdentityAndIndexAssociations(t *testing.T) {
	dir := t.TempDir()
	v := graphFixture()
	r := &v.Graph.Repositories[0]
	r.Revision = ptr("original")
	r.IndexStatus = "complete"
	r.SelectedFiles = []IndexFile{{ID: "selected", Path: "src/a.go", ArtifactIDs: []string{"artifact"}, NodeIDs: []string{"producer"}}}
	other := *r
	other.ID = "other_snapshot"
	other.Revision = ptr("next")
	v.Graph.Repositories = append(v.Graph.Repositories, other)
	if _, _, err := Import(dir, v); err != nil {
		t.Fatal(err)
	}
	v.Graph.Nodes = []Node{}
	v.Graph.Edges = []Edge{}
	for i := range v.Graph.Repositories {
		v.Graph.Repositories[i].SelectedFiles = []IndexFile{}
	}
	if _, _, err := Import(dir, v); err != nil {
		t.Fatal(err)
	}
	summary, err := LoadSummary(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(summary.Graph.Repositories) != 2 {
		t.Fatal("repository revisions conflated")
	}
	for _, s := range summary.Graph.Repositories {
		if len(s.SelectedFiles) != 0 || s.Detail.Selected != 1 || len(s.Detail.ArtifactIDs) != 1 || s.Detail.ArtifactIDs[0] != "artifact" {
			t.Fatal("summary lost unloaded index associations")
		}
		detail, err := RepositoryDetail(dir, s)
		if err != nil {
			t.Fatal(err)
		}
		if len(detail.SelectedFiles) != 1 || detail.SelectedFiles[0].Path != "src/a.go" || len(detail.SelectedFiles[0].NodeIDs) != 0 {
			t.Fatal("index retention or removed-node pruning changed")
		}
	}
}

func TestDetailPublicationFailureLeavesPriorManifest(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := Import(dir, Empty()); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(Path(dir))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "loop-view-details"), []byte("blocked directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err = Import(dir, View{SchemaVersion: 1, Runs: []Run{testRun("next")}}); err == nil {
		t.Fatal("detail publication failure ignored")
	}
	after, err := os.ReadFile(Path(dir))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed publication replaced manifest", err)
	}
}
