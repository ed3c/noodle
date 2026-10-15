package loopview

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func ptr[T any](v T) *T { return &v }
func graphFixture() View {
	refs := []string{"artifact"}
	empty := []string{}
	sum := sha256.Sum256([]byte("input"))
	a := Artifact{ID: "artifact", RepositoryID: ptr("repo"), Role: "prompt", Format: "text/plain", Authority: "knowledge", KnowledgeRole: "source", Scope: "run", Source: &ArtifactSource{Path: "/source/input", SHA256: ptr(hex.EncodeToString(sum[:])), Bytes: ptr(int64(5))}, Status: "available", Content: ptr("input"), ContentEncoding: "utf-8"}
	objects := []Object{{ID: "manager", Namespace: "soodles.loop", Kind: "manager", Scope: "shared", SourceArtifactIDs: empty}, {ID: "inner", Namespace: "noodle", Kind: "inner", Scope: "repository", SourceArtifactIDs: empty}}
	ports := []Port{{ID: "out", ObjectID: "manager", Direction: "output", SourceArtifactIDs: empty}, {ID: "in", ObjectID: "inner", Direction: "input", SourceArtifactIDs: empty}}
	nodes := []Node{}
	for i, object := range []string{"manager", "inner"} {
		id := []string{"producer", "consumer"}[i]
		nodes = append(nodes, Node{ID: id, RunID: "run", RepositoryID: "repo", ObjectID: object, Kind: "cli", Identity: NodeIdentity{Status: "observed", InvocationID: ptr(id), Locator: ptr("receipt#" + id)}, Status: NodeStatus{Execution: "success", Transfer: "observed", Acceptance: "unknown"}, InputArtifactIDs: refs, OutputArtifactIDs: refs, EvidenceArtifactIDs: refs})
	}
	g := &Graph{Objects: objects, Ports: ports, Repositories: []Repository{{ID: "repo", Root: "/repo", IndexStatus: "unknown", TrackedFiles: []IndexFile{}, SelectedFiles: []IndexFile{}, Gaps: empty}}, Nodes: nodes, Artifacts: []Artifact{a}, Edges: []Edge{{ID: "handoff", RunID: "run", FromNodeID: "producer", ToNodeID: "consumer", FromPortID: "out", ToPortID: "in", InputArtifactIDs: refs, OutputArtifactIDs: refs, EvidenceArtifactIDs: refs, Kind: "handoff", Status: "observed", Scope: "within_repository", CurrentValidity: "current"}}, Findings: []Finding{}, Metrics: []Metric{}}
	return View{SchemaVersion: 2, Runs: []Run{testRun("run")}, Graph: g}
}
func TestGraphRejectsUnsupportedCausalClaims(t *testing.T) {
	for name, change := range map[string]func(*View){
		"duplicate object":   func(v *View) { v.Graph.Objects = append(v.Graph.Objects, v.Graph.Objects[0]) },
		"unknown endpoint":   func(v *View) { v.Graph.Edges[0].ToNodeID = "missing" },
		"wrong port owner":   func(v *View) { v.Graph.Ports[1].ObjectID = "manager" },
		"unknown invocation": func(v *View) { v.Graph.Nodes[1].Identity.Status = "unknown" },
		"absent receipt":     func(v *View) { v.Graph.Edges[0].EvidenceArtifactIDs = []string{} },
		"unbound producer": func(v *View) {
			v.Graph.Nodes[0].InputArtifactIDs = []string{}
			v.Graph.Nodes[0].OutputArtifactIDs = []string{}
		},
		"false cross scope": func(v *View) { v.Graph.Edges[0].Scope = "cross_repository" },
		"cycle": func(v *View) {
			g := v.Graph
			g.Ports = append(g.Ports, Port{ID: "out2", ObjectID: "inner", Direction: "output", SourceArtifactIDs: []string{}}, Port{ID: "in2", ObjectID: "manager", Direction: "input", SourceArtifactIDs: []string{}})
			e := g.Edges[0]
			e.ID = "back"
			e.FromNodeID = "consumer"
			e.ToNodeID = "producer"
			e.FromPortID = "out2"
			e.ToPortID = "in2"
			g.Edges = append(g.Edges, e)
		},
		"false measured value": func(v *View) {
			v.Graph.Metrics = []Metric{{ID: "cost", RunID: "run", Status: "unknown", Value: ptr(0.0), SourceArtifactIDs: []string{}}}
		},
		"zero denominator": func(v *View) {
			v.Graph.Metrics = []Metric{{ID: "cost", RunID: "run", Status: "observed", Value: ptr(100.0), Denominator: ptr(0.0), SourceArtifactIDs: []string{}}}
		},
		"index traversal": func(v *View) {
			v.Graph.Repositories[0].TrackedFiles = []IndexFile{{ID: "file", Path: "../private", ArtifactIDs: []string{}, NodeIDs: []string{}}}
		},
		"content drift": func(v *View) { v.Graph.Artifacts[0].Content = ptr("changed") },
	} {
		t.Run(name, func(t *testing.T) {
			v := graphFixture()
			if err := validateImport(v); err != nil {
				t.Fatal(err)
			}
			change(&v)
			if err := validateImport(v); err == nil {
				t.Fatal("invalid graph accepted")
			}
		})
	}
}
func TestGraphMergePreservesOtherRunsAndVersionIdentity(t *testing.T) {
	dir := t.TempDir()
	v := graphFixture()
	if _, _, err := Import(dir, v); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Import(dir, View{SchemaVersion: 1, Runs: []Run{testRun("legacy")}}); err != nil {
		t.Fatal(err)
	}
	current, err := Load(dir)
	if err != nil || current.Graph == nil || len(current.Runs) != 2 {
		t.Fatalf("v1 lost graph: %v", err)
	}
	current.Graph.Artifacts[0].Source.Path = "/changed"
	before, _ := os.ReadFile(Path(dir))
	if _, _, err := Import(dir, current); err == nil {
		t.Fatal("same ID rebound")
	}
	after, _ := os.ReadFile(Path(dir))
	if string(before) != string(after) {
		t.Fatal("refusal modified registry")
	}
	metadata := Metadata(v)
	if metadata.Graph.Artifacts[0].Content != nil || v.Graph.Artifacts[0].Content == nil {
		t.Fatal("metadata body leak or mutation")
	}
}
func TestArtifactVersionAndPathRefusals(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "prompt.txt")
	if err := os.WriteFile(path, []byte("input"), 0600); err != nil {
		t.Fatal(err)
	}
	a := graphFixture().Graph.Artifacts[0]
	a.Source.Path = path
	got := ArtifactContent(a)
	if got.Content == nil || *got.Content != "input" {
		t.Fatalf("content: %+v error: %v", got, *got.Error)
	}
	if err := os.WriteFile(path, []byte("other"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := ArtifactContent(a); got.Status != "stale" || got.Content != nil {
		t.Fatalf("drift accepted: %+v", got)
	}
	for name, pathValue := range map[string]string{"relative": "../prompt.txt", "traversal": dir + "/../" + filepath.Base(dir) + "/prompt.txt", "directory": dir} {
		t.Run(name, func(t *testing.T) {
			b := a
			b.Source = &ArtifactSource{Path: pathValue, SHA256: a.Source.SHA256, Bytes: a.Source.Bytes}
			if ArtifactContent(b).Content != nil {
				t.Fatal("invalid path read")
			}
		})
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	a.Source.Path = link
	if got := ArtifactContent(a); got.Content != nil || got.Error == nil || !strings.Contains(*got.Error, "symlink") {
		t.Fatalf("symlink accepted: %+v", got)
	}
}
func TestIndexReadsCapturedGitBlobNotCurrentWorkingFile(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	git("config", "user.name", "test")
	git("config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("captured"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "file.txt")
	git("commit", "-qm", "fixture")
	revision, blob := git("rev-parse", "HEAD"), git("rev-parse", "HEAD:file.txt")
	repo := Repository{Root: dir, Revision: &revision}
	file := IndexFile{ID: "file", Path: "file.txt", GitMode: ptr("100644"), GitBlob: &blob, Bytes: ptr(int64(8))}
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("current"), 0600); err != nil {
		t.Fatal(err)
	}
	got := IndexContent(context.Background(), repo, file)
	if got.Content == nil || *got.Content != "captured" {
		data, _ := json.Marshal(got)
		t.Fatal(string(data))
	}
	file.Path = "other.txt"
	if IndexContent(context.Background(), repo, file).Content != nil {
		t.Fatal("unbound index path accepted")
	}
}

func TestGraphRejectsEmptyInvocationAndFalseRatios(t *testing.T) {
	v := graphFixture()
	v.Graph.Nodes[0].Identity.InvocationID = ptr("")
	if err := validateImport(v); err == nil {
		t.Fatal("empty invocation accepted")
	}
	for _, m := range []Metric{
		{ID: "ratio", RunID: "run", Status: "observed", Unit: "ratio", Value: ptr(1.0), Numerator: ptr(2.0), Denominator: nil, SourceArtifactIDs: []string{}, Gaps: []string{}},
		{ID: "ratio", RunID: "run", Status: "observed", Unit: "percent", Value: ptr(100.0), Numerator: ptr(1.0), Denominator: ptr(2.0), SourceArtifactIDs: []string{}, Gaps: []string{}},
	} {
		v = graphFixture()
		v.Graph.Metrics = []Metric{m}
		if err := validateImport(v); err == nil {
			t.Fatal("false ratio accepted")
		}
		v.Graph.Metrics[0].Value = ptr(50.0)
		v.Graph.Metrics[0].Numerator = ptr(1.0)
		v.Graph.Metrics[0].Denominator = ptr(2.0)
		v.Graph.Metrics[0].Unit = "percent"
		if err := validateImport(v); err != nil {
			t.Fatal(err)
		}
	}
}
func TestGraphKeepsOtherRunRuntimeIndexOnSameRepository(t *testing.T) {
	dir := t.TempDir()
	a := graphFixture()
	a.Graph.Repositories[0].SelectedFiles = []IndexFile{{ID: "old_file", Path: "runtime/old", ArtifactIDs: []string{"artifact"}, NodeIDs: []string{"producer"}}}
	if _, _, err := Import(dir, a); err != nil {
		t.Fatal(err)
	}
	b := graphFixture()
	b.Runs[0].ID = "second"
	b.Graph.Artifacts[0].Scope = "original_source"
	for i := range b.Graph.Nodes {
		b.Graph.Nodes[i].RunID = "second"
		b.Graph.Nodes[i].ID += "2"
	}
	for i := range b.Graph.Edges {
		b.Graph.Edges[i].RunID = "second"
		b.Graph.Edges[i].ID += "2"
		b.Graph.Edges[i].FromNodeID += "2"
		b.Graph.Edges[i].ToNodeID += "2"
	}
	b.Graph.Repositories[0].SelectedFiles = []IndexFile{{ID: "new_file", Path: "runtime/new", ArtifactIDs: []string{"artifact"}, NodeIDs: []string{"producer2"}}}
	if _, _, err := Import(dir, b); err != nil {
		t.Fatal(err)
	}
	current, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	files := current.Graph.Repositories[0].SelectedFiles
	if len(files) != 2 {
		t.Fatalf("other run index lost: %+v", files)
	}
}
