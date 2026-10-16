package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"strings"

	"github.com/poteto/noodle/internal/loopview"
)

// LoopViewsHandler exposes display data and static assets only. It has no control handler.
func LoopViewsHandler(dir string, ui fs.FS, observe func(context.Context, *loopview.View) error) http.Handler {
	mux := http.NewServeMux()
	registerLoopGraph(mux, dir)
	mux.HandleFunc("GET /api/loops", func(w http.ResponseWriter, r *http.Request) {
		view, err := loopview.Load(dir)
		if err != nil {
			http.Error(w, "loop display registry unreadable: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		if observe != nil {
			err = observe(r.Context(), &view)
			if err != nil {
				http.Error(w, "Host observation unavailable: "+err.Error(), http.StatusServiceUnavailable)
				return
			}
		}
		data, err := json.Marshal(view)
		if err != nil {
			http.Error(w, "loop display encoding failed", http.StatusInternalServerError)
			return
		}
		digest := sha256.Sum256(data)
		etag := `"` + hex.EncodeToString(digest[:]) + `"`
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "private, no-cache")
		w.Header().Set("Content-Type", "application/json")
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		if r.Method != http.MethodHead {
			_, _ = w.Write(data)
		}
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
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "loop display is read only", http.StatusMethodNotAllowed)
			return
		}
		mux.ServeHTTP(w, r)
	})
}
