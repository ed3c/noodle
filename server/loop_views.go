package server

import (
	"context"
	"io/fs"
	"net/http"
	"strings"

	"github.com/poteto/noodle/internal/loopview"
)

// LoopViewsHandler exposes evidence and explicit review selections, never execution controls.
func LoopViewsHandler(dir string, ui fs.FS, observe func(context.Context, *loopview.View) error) http.Handler {
	mux := http.NewServeMux()
	registerLoopGraph(mux, dir)
	registerReviewSelection(mux, dir)
	mux.HandleFunc("GET /api/loops", func(w http.ResponseWriter, r *http.Request) {
		view, err := loopview.LoadSummary(dir)
		if err != nil {
			http.Error(w, "loop display registry unreadable: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		if observe != nil {
			resources := loopview.Empty()
			err = observe(r.Context(), &resources)
			view.Resources = resources.Resources
			if err != nil {
				http.Error(w, "Host observation unavailable: "+err.Error(), http.StatusServiceUnavailable)
				return
			}
		}
		loopJSON(w, r, 200, view)
	})
	mux.HandleFunc("GET /api/loops/runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		view, ok := graphView(w, dir)
		if !ok {
			return
		}
		for _, summary := range view.Runs {
			if summary.ID != r.PathValue("id") {
				continue
			}
			if requested := r.URL.Query().Get("ref"); requested != "" && requested != summary.Detail.SHA256 {
				http.Error(w, "selected run version changed", 409)
				return
			}
			run, err := loopview.RunDetail(dir, summary)
			if err != nil {
				http.Error(w, "run detail unavailable: "+err.Error(), 503)
				return
			}
			detail := loopview.View{SchemaVersion: 1, Runs: []loopview.Run{run}}
			if observe != nil {
				if err = observe(r.Context(), &detail); err != nil {
					http.Error(w, "Host observation unavailable: "+err.Error(), 503)
					return
				}
			}
			loopJSON(w, r, 200, struct {
				loopview.View
				Source *loopview.DetailRef `json:"projection_source"`
			}{detail, summary.Detail})
			return
		}
		http.Error(w, "registered run not found", 404)
	})
	mux.Handle("GET /loops", uiOrPlaceholder(ui))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/loops", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("GET /assets/", func(w http.ResponseWriter, r *http.Request) {
		if ui == nil {
			http.NotFound(w, r)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/")
		info, err := fs.Stat(ui, path)
		if err != nil || info.IsDir() {
			http.NotFound(w, r)
			return
		}
		http.FileServer(http.FS(ui)).ServeHTTP(w, r)
	})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Method != http.MethodGet && r.Method != http.MethodHead && !(r.Method == http.MethodPost && r.URL.Path == "/api/loops/review-selections") {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "loop display is read only", http.StatusMethodNotAllowed)
			return
		}
		mux.ServeHTTP(w, r)
	})
}
