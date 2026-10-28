package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/poteto/noodle/internal/loopview"
)

func loopJSON(w http.ResponseWriter, r *http.Request, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		http.Error(w, "loop display encoding failed", 500)
		return
	}
	if len(data) > loopview.MaxBytes {
		http.Error(w, "loop detail response exceeds document limit", http.StatusUnprocessableEntity)
		return
	}
	digest := sha256.Sum256(data)
	etag := `"` + hex.EncodeToString(digest[:]) + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Set("Content-Type", "application/json")
	if status == 200 && r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Length", fmt.Sprint(len(data)))
	w.WriteHeader(status)
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}
func graphView(w http.ResponseWriter, dir string) (loopview.View, bool) {
	view, err := loopview.LoadSummary(dir)
	if err != nil {
		http.Error(w, "loop display registry unreadable: "+err.Error(), http.StatusServiceUnavailable)
		return view, false
	}
	return view, true
}
func registerLoopGraph(mux *http.ServeMux, dir string) {
	registerLoopPreview(mux, dir)
	mux.HandleFunc("GET /api/loops/graph", func(w http.ResponseWriter, r *http.Request) {
		view, ok := graphView(w, dir)
		if !ok {
			return
		}
		loopJSON(w, r, 200, struct {
			loopview.View
			ReviewVersion string `json:"review_version"`
		}{loopview.Metadata(view), loopview.ReviewVersion(view)})
	})
	mux.HandleFunc("GET /api/loops/artifacts/{id}", func(w http.ResponseWriter, r *http.Request) {
		view, ok := graphView(w, dir)
		if !ok {
			return
		}
		if view.Graph != nil {
			for _, a := range view.Graph.Artifacts {
				if a.ID == r.PathValue("id") {
					result := loopview.ArtifactContent(a)
					status := 200
					if result.Status == "stale" {
						status = 409
					} else if result.Content == nil {
						status = 422
					}
					loopJSON(w, r, status, result)
					return
				}
			}
		}
		http.Error(w, "registered artifact not found", http.StatusNotFound)
	})
	mux.HandleFunc("GET /api/loops/repositories/{id}/files", func(w http.ResponseWriter, r *http.Request) {
		view, ok := graphView(w, dir)
		if !ok {
			return
		}
		offset, limit := 0, 100
		if raw := r.URL.Query().Get("offset"); raw != "" {
			var err error
			offset, err = strconv.Atoi(raw)
			if err != nil || offset < 0 {
				http.Error(w, "invalid index offset", 400)
				return
			}
		}
		if raw := r.URL.Query().Get("limit"); raw != "" {
			var err error
			limit, err = strconv.Atoi(raw)
			if err != nil || limit < 1 || limit > 500 {
				http.Error(w, "invalid index limit", 400)
				return
			}
		}
		if view.Graph != nil {
			for _, repo := range view.Graph.Repositories {
				if repo.ID == r.PathValue("id") {
					repo, err := readRepository(r, dir, repo)
					if err != nil {
						http.Error(w, "repository detail unavailable: "+err.Error(), 409)
						return
					}
					files := []loopview.IndexFile{}
					section := r.URL.Query().Get("section")
					if section != "" && section != "tracked" && section != "selected" {
						http.Error(w, "invalid index section", 400)
						return
					}
					if section != "selected" {
						files = append(files, repo.TrackedFiles...)
					}
					if section != "tracked" {
						files = append(files, repo.SelectedFiles...)
					}
					if search := strings.ToLower(r.URL.Query().Get("search")); search != "" {
						filtered := []loopview.IndexFile{}
						for _, f := range files {
							if strings.Contains(strings.ToLower(f.Path), search) {
								filtered = append(filtered, f)
							}
						}
						files = filtered
					}
					total := len(files)
					if offset > total {
						offset = total
					}
					end := offset + limit
					if end > total {
						end = total
					}
					loopJSON(w, r, 200, map[string]any{"repository_id": repo.ID, "revision": repo.Revision, "index_status": repo.IndexStatus, "offset": offset, "total": total, "files": files[offset:end], "ref": repo.Detail})
					return
				}
			}
		}
		http.Error(w, "registered repository not found", 404)
	})
	mux.HandleFunc("GET /api/loops/repositories/{id}/files/{fileID}", func(w http.ResponseWriter, r *http.Request) {
		view, ok := graphView(w, dir)
		if !ok {
			return
		}
		if view.Graph != nil {
			for _, repo := range view.Graph.Repositories {
				if repo.ID != r.PathValue("id") {
					continue
				}
				repo, err := readRepository(r, dir, repo)
				if err != nil {
					http.Error(w, "repository detail unavailable: "+err.Error(), 409)
					return
				}
				for _, file := range append(append([]loopview.IndexFile{}, repo.TrackedFiles...), repo.SelectedFiles...) {
					if file.ID == r.PathValue("fileID") {
						result := loopview.IndexContent(r.Context(), repo, file)
						status := 200
						if result.Content == nil {
							status = 422
						}
						loopJSON(w, r, status, result)
						return
					}
				}
			}
		}
		http.Error(w, "registered index file not found", 404)
	})
}

func readRepository(r *http.Request, dir string, summary loopview.Repository) (loopview.Repository, error) {
	if expected := r.URL.Query().Get("ref"); expected != "" && expected != summary.Detail.SHA256 {
		return loopview.Repository{}, fmt.Errorf("selected repository version changed")
	}
	repo, err := loopview.RepositoryDetail(dir, summary)
	if err != nil {
		return repo, err
	}
	repo.Detail = summary.Detail
	return repo, nil
}
