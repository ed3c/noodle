package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/poteto/noodle/internal/loopview"
)

func TestLoopViewsReadOnlyUpdatesAndETag(t *testing.T) {
	dir := t.TempDir()
	resources := func(_ context.Context, view *loopview.View) error {
		view.Resources = []loopview.Resource{{Label: "Host workers", Value: "0"}}
		return nil
	}
	handler := LoopViewsHandler(dir, fstest.MapFS{"index.html": {Data: []byte("loop page")}, "assets/app.js": {Data: []byte("static asset")}}, resources)
	read := func(method, path, etag string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, nil)
		if etag != "" {
			req.Header.Set("If-None-Match", etag)
		}
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		return res
	}
	initial := read("GET", "/api/loops", "")
	if initial.Code != 200 || !strings.Contains(initial.Body.String(), `"runs":[]`) {
		t.Fatalf("initial: %s", initial.Body.String())
	}
	if got := read("HEAD", "/api/loops", ""); got.Code != 200 || got.Body.Len() != 0 {
		t.Fatal("HEAD returned body")
	}
	if got := read("GET", "/api/loops", initial.Header().Get("ETag")); got.Code != 304 {
		t.Fatal("unchanged view not cached")
	}
	next := "owner readback; do not execute"
	input := loopview.View{SchemaVersion: 1, Runs: []loopview.Run{{ID: "a", Repository: "repo", Mode: "outer-only", Status: "prepared", Stages: []loopview.Stage{}, Cards: []loopview.Card{}, Notes: []loopview.Note{}, Breakpoints: []loopview.Breakpoint{}, Next: &next}}}
	if _, _, err := loopview.Import(dir, input); err != nil {
		t.Fatal(err)
	}
	updated := read("GET", "/api/loops", initial.Header().Get("ETag"))
	if updated.Code != 200 || updated.Header().Get("ETag") == initial.Header().Get("ETag") {
		t.Fatal("import did not change view")
	}
	var view loopview.View
	if err := json.Unmarshal(updated.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Runs[0].Next == nil || *view.Runs[0].Next != next || view.Resources[0].Value != "0" {
		t.Fatal("projection or Host observation missing")
	}
	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE", "OPTIONS"} {
		for _, path := range []string{"/api/loops", "/api/control", "/api/ws", "/loops"} {
			if got := read(method, path, ""); got.Code != http.StatusMethodNotAllowed {
				t.Fatalf("write accepted: %s %s %d", method, path, got.Code)
			}
		}
	}
	for _, path := range []string{"/api/control", "/api/ws", "/api/sessions/a/events", "/assets/", "/loop-views.json"} {
		if got := read("GET", path, ""); got.Code != 404 {
			t.Fatalf("unexpected route: %s %d", path, got.Code)
		}
	}
	if got := read("GET", "/loops", ""); got.Code != 200 || got.Body.String() != "loop page" {
		t.Fatal("UI missing")
	}
	if got := read("GET", "/assets/app.js", ""); got.Code != 200 {
		t.Fatal("asset missing")
	}
	if err := os.WriteFile(loopview.Path(dir), []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := read("GET", "/api/loops", ""); got.Code != 503 {
		t.Fatal("broken registry concealed")
	}
}

func TestLoopRunDetailEnrichesOnlySelectedRunAndRejectsWrongVersion(t *testing.T) {
	dir := t.TempDir()
	runs := []loopview.Run{}
	for _, id := range []string{"first", "second"} {
		runs = append(runs, loopview.Run{ID: id, Repository: "repo", Mode: "manual", Status: "prepared", Stages: []loopview.Stage{{ID: "stage", Kind: "original", Status: "observed", ConsumerStatus: "unknown", Prompt: "original body"}}, Cards: []loopview.Card{}, Notes: []loopview.Note{}, Breakpoints: []loopview.Breakpoint{}})
	}
	if _, _, err := loopview.Import(dir, loopview.View{SchemaVersion: 1, Runs: runs}); err != nil {
		t.Fatal(err)
	}
	observed := []string{}
	handler := LoopViewsHandler(dir, nil, func(_ context.Context, v *loopview.View) error {
		for i := range v.Runs {
			observed = append(observed, v.Runs[i].ID)
			v.Runs[i].Stages[0].Prompt = "fresh canonical prompt"
		}
		return nil
	})
	call := func(path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return w
	}
	catalog := call("/api/loops")
	if catalog.Code != 200 || strings.Contains(catalog.Body.String(), "original body") || len(observed) != 0 {
		t.Fatal("catalog expanded native bodies", catalog.Body.String())
	}
	var view loopview.View
	if err := json.Unmarshal(catalog.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	selected := view.Runs[0]
	detail := call("/api/loops/runs/first?ref=" + selected.Detail.SHA256)
	if detail.Code != 200 || !strings.Contains(detail.Body.String(), "fresh canonical prompt") || len(observed) != 1 || observed[0] != "first" {
		t.Fatal("selected native enrichment lost", detail.Body.String(), observed)
	}
	if got := call("/api/loops/runs/first?ref=changed"); got.Code != 409 || len(observed) != 1 {
		t.Fatal("wrong selected version reached observer")
	}
	if got := call("/api/loops/runs/unknown"); got.Code != 404 {
		t.Fatal("unregistered run read")
	}
}
