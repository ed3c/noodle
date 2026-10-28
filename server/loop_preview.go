package server

import (
	"net/http"

	"github.com/poteto/noodle/internal/loopview"
)

func registerLoopPreview(mux *http.ServeMux, dir string) {
	mux.HandleFunc("GET /api/loops/artifacts/{id}/preview", func(w http.ResponseWriter, r *http.Request) {
		view, ok := graphView(w, dir)
		if !ok {
			return
		}
		version := r.URL.Query().Get("version")
		if version == "" || version != loopview.ReviewVersion(view) {
			http.Error(w, "selected graph version changed or missing", http.StatusConflict)
			return
		}
		if view.Graph != nil {
			for _, a := range view.Graph.Artifacts {
				if a.ID != r.PathValue("id") {
					continue
				}
				if a.Source == nil || a.Source.SHA256 == nil || r.URL.Query().Get("ref") != *a.Source.SHA256 {
					http.Error(w, "selected artifact version changed or missing", http.StatusConflict)
					return
				}
				if a.Format != "text/html" || a.Source.Bytes == nil || *a.Source.Bytes > loopview.MaxPreviewBytes {
					http.Error(w, "artifact is not bounded HTML", http.StatusUnprocessableEntity)
					return
				}
				result := loopview.ArtifactContent(a)
				if result.Content == nil {
					loopJSON(w, r, http.StatusConflict, result)
					return
				}
				preview, err := loopview.StaticPreview(*result.Content)
				if err != nil {
					http.Error(w, err.Error(), http.StatusUnprocessableEntity)
					return
				}
				loopJSON(w, r, 200, struct {
					Artifact     loopview.Artifact `json:"artifact"`
					GraphVersion string            `json:"graph_version"`
					Document     string            `json:"document"`
					Acceptance   string            `json:"acceptance"`
					Effects      []string          `json:"effects"`
				}{result.Artifact, version, preview, "unknown", []string{}})
				return
			}
		}
		http.Error(w, "registered artifact not found", http.StatusNotFound)
	})
}
