package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/poteto/noodle/internal/loopview"
)

func TestLoopStaticPreviewBindsRegisteredSourceWithoutPromotingAcceptance(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source := `<html><head><style>svg{background:url(https://preview-blocked.invalid/pixel)}</style></head><body><svg viewBox="0 0 300 200"><rect width="50" height="30"/><text x="10" y="60">S1 = 24</text></svg></body></html>`
	path := filepath.Join(dir, "drawing.html")
	if err := os.WriteFile(path, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256([]byte(source))
	hash := hex.EncodeToString(digest[:])
	size := int64(len(source))
	a := loopview.Artifact{ID: "drawing", Format: "text/html", Status: "available", Source: &loopview.ArtifactSource{Path: path, SHA256: &hash, Bytes: &size}}
	v := loopview.View{SchemaVersion: 2, Runs: []loopview.Run{}, Graph: &loopview.Graph{Objects: []loopview.Object{}, Ports: []loopview.Port{}, Repositories: []loopview.Repository{}, Nodes: []loopview.Node{}, Artifacts: []loopview.Artifact{a}, Edges: []loopview.Edge{}, Findings: []loopview.Finding{}, Metrics: []loopview.Metric{}}}
	if _, _, err := loopview.Import(dir, v); err != nil {
		t.Fatal(err)
	}
	summary, err := loopview.LoadSummary(dir)
	if err != nil {
		t.Fatal(err)
	}
	url := "/api/loops/artifacts/drawing/preview?ref=" + hash + "&version=" + loopview.ReviewVersion(summary)
	handler := LoopViewsHandler(dir, nil, nil)
	call := func(method, path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest(method, path, nil))
		return w
	}
	w := call("GET", url)
	var result struct {
		Document   string   `json:"document"`
		Acceptance string   `json:"acceptance"`
		Effects    []string `json:"effects"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || result.Acceptance != "unknown" || len(result.Effects) != 0 || !strings.Contains(result.Document, "S1 = 24") {
		t.Fatalf("preview: %d %s", w.Code, w.Body.String())
	}
	golden, err := os.ReadFile("testdata/static-preview.html")
	if err != nil {
		t.Fatal(err)
	}
	if string(golden) != result.Document {
		t.Fatal("browser proof fixture differs from actual API document")
	}
	if call("POST", url).Code != 405 || call("HEAD", url).Body.Len() != 0 {
		t.Fatal("preview is not readonly")
	}
	if call("GET", "/api/loops/artifacts/drawing/preview?ref="+hash).Code != 409 || call("GET", strings.Replace(url, hash, strings.Repeat("a", 64), 1)).Code != 409 {
		t.Fatal("unbound version accepted")
	}
	if call("GET", strings.Replace(url, "/drawing/", "/missing/", 1)).Code != 404 {
		t.Fatal("unregistered path accepted")
	}
	if err := os.WriteFile(path, []byte("drift"), 0600); err != nil {
		t.Fatal(err)
	}
	if call("GET", url).Code != 409 {
		t.Fatal("source drift accepted")
	}
}
