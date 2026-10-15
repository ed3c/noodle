package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/poteto/noodle/internal/loopview"
)

func TestLoopGraphReadOnlyBoundArtifactsAndIndex(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(dir, "input.txt")
	content := "<script>inert</script>"
	if err := os.WriteFile(source, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(content))
	digest := hex.EncodeToString(sum[:])
	size := int64(len(content))
	graph := &loopview.Graph{Objects: []loopview.Object{}, Ports: []loopview.Port{}, Repositories: []loopview.Repository{{ID: "repo", Root: dir, IndexStatus: "unknown", TrackedFiles: []loopview.IndexFile{{ID: "file", Path: "input.txt", ArtifactIDs: []string{"a"}, NodeIDs: []string{}}}, SelectedFiles: []loopview.IndexFile{}, Gaps: []string{}}}, Nodes: []loopview.Node{}, Artifacts: []loopview.Artifact{{ID: "a", Status: "available", Source: &loopview.ArtifactSource{Path: source, SHA256: &digest, Bytes: &size}}}, Edges: []loopview.Edge{}, Findings: []loopview.Finding{}, Metrics: []loopview.Metric{}}
	if _, _, err := loopview.Import(dir, loopview.View{SchemaVersion: 2, Runs: []loopview.Run{{ID: "run", Repository: "repo", Mode: "inner-only", Status: "observed", Stages: []loopview.Stage{{ID: "stage", Kind: "native", Status: "observed", ConsumerStatus: "unknown", Prompt: content}}, Cards: []loopview.Card{{ID: "card", Body: content}}, Notes: []loopview.Note{{ID: "note", Body: content}}, Breakpoints: []loopview.Breakpoint{}}}, Graph: graph}); err != nil {
		t.Fatal(err)
	}
	handler := LoopViewsHandler(dir, nil, nil)
	call := func(method, path string) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(method, path, nil))
		return w
	}
	w := call("GET", "/api/loops/graph")
	if w.Code != 200 || strings.Contains(w.Body.String(), "inert") {
		t.Fatalf("metadata: %d %s", w.Code, w.Body.String())
	}
	w = call("GET", "/api/loops")
	if w.Code != 200 || !strings.Contains(w.Body.String(), "inert") {
		t.Fatal("legacy body lost")
	}
	w = call("GET", "/api/loops/artifacts/a")
	var result loopview.ArtifactResult
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || result.Content == nil || *result.Content != content || w.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("artifact: %d %s", w.Code, w.Body.String())
	}
	etag := w.Header().Get("ETag")
	request := httptest.NewRequest("GET", "/api/loops/artifacts/a", nil)
	request.Header.Set("If-None-Match", etag)
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, request)
	if w.Code != 304 {
		t.Fatal("ETag not honored")
	}
	if w = call("HEAD", "/api/loops/artifacts/a"); w.Code != 200 || w.Body.Len() != 0 {
		t.Fatal("HEAD returned body")
	}
	if w = call("GET", "/api/loops/repositories/repo/files?offset=0&limit=1"); w.Code != 200 || !strings.Contains(w.Body.String(), `"total":1`) {
		t.Fatalf("index: %s", w.Body.String())
	}
	for _, path := range []string{"/api/loops/graph", "/api/loops/artifacts/a", "/api/loops/repositories/repo/files"} {
		if w = call("POST", path); w.Code != 405 {
			t.Fatalf("write accepted: %s", path)
		}
	}
	if w = call("GET", "/api/loops/artifacts/not-registered"); w.Code != http.StatusNotFound {
		t.Fatal("unknown ID accepted")
	}
	if w = call("GET", "/api/loops/repositories/repo/files?limit=99999"); w.Code != 400 {
		t.Fatal("unbounded index request accepted")
	}
	if err := os.WriteFile(source, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	if w = call("GET", "/api/loops/artifacts/a"); w.Code != 409 || !strings.Contains(w.Body.String(), `"content":null`) {
		t.Fatalf("version drift: %d %s", w.Code, w.Body.String())
	}
}
