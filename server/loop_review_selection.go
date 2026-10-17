package server

import (
	"encoding/json"
	"github.com/poteto/noodle/internal/loopview"
	"io"
	"net/http"
)

func registerReviewSelection(mux *http.ServeMux, dir string) {
	mux.HandleFunc("POST /api/loops/review-selections", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "http://"+r.Host || r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "same-origin JSON selection required", http.StatusForbidden)
			return
		}
		var input struct {
			GraphVersion string `json:"graph_version"`
			RunID        string `json:"run_id"`
			Kind         string `json:"kind"`
			TargetID     string `json:"target_id"`
		}
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&input); err != nil {
			http.Error(w, "invalid selection", 400)
			return
		}
		if decoder.Decode(new(any)) != io.EOF {
			http.Error(w, "trailing selection data", 400)
			return
		}
		result, err := loopview.SaveReviewSelection(dir, input.GraphVersion, input.RunID, input.Kind, input.TargetID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		loopJSON(w, r, 200, result)
	})
}
