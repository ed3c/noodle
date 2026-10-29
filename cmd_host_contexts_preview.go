package main

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"mime"
	"net/http"
	"os"

	"github.com/poteto/noodle/internal/loopview"
)

func contextAcceptedPreview(w http.ResponseWriter, r *http.Request, dir string, identity contextOwnerIdentity, call contextOwnerCall) {
	if r.Header.Get("Origin") != "http://"+r.Host || r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		http.Error(w, "untrusted Origin", 403)
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		http.Error(w, "JSON request required", 415)
		return
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4096))
	if err != nil {
		http.Error(w, "preview request exceeds limit", 413)
		return
	}
	var request struct {
		ID              string `json:"id"`
		RegistryVersion string `json:"registry_version"`
		ExpectedVersion string `json:"expected_version"`
		ArtifactID      string `json:"artifact_id"`
	}
	if err = decodeContextJSON(data, &request); err != nil || !contextIDPattern.MatchString(request.ID) || !contextHashPattern.MatchString(request.RegistryVersion) || !contextHashPattern.MatchString(request.ExpectedVersion) || request.ArtifactID == "" {
		http.Error(w, "invalid accepted artifact selection", 400)
		return
	}
	entries, err := loadContextRegistrations(dir)
	if err != nil {
		http.Error(w, "context registrations unavailable", 503)
		return
	}
	entry, ok := entries[request.ID]
	if !ok {
		http.Error(w, "registered context not found", 404)
		return
	}
	choice, err := validateContextRegistration(entry, identity)
	if err != nil || choice.Version != request.RegistryVersion {
		http.Error(w, "registered context source or owner changed", 409)
		return
	}
	observation, err := call(r.Context(), entry, "inspect", "")
	if err != nil || validateContextObservation(observation, entry) != nil {
		http.Error(w, "original acceptance observation unavailable", 502)
		return
	}
	if observation.Version != request.ExpectedVersion {
		http.Error(w, "original acceptance observation changed", 409)
		return
	}
	for _, artifact := range observation.Acceptance.Artifacts {
		if artifact.ID != request.ArtifactID {
			continue
		}
		if artifact.Format != "html" {
			http.Error(w, "artifact is not static HTML", 422)
			return
		}
		if err = verifyContextRef(artifact.Source); err != nil {
			http.Error(w, "accepted artifact source changed", 409)
			return
		}
		file, err := os.Open(artifact.Source.Path)
		if err != nil {
			http.Error(w, "accepted artifact unavailable", 409)
			return
		}
		data, err = io.ReadAll(io.LimitReader(file, loopview.MaxPreviewBytes+1))
		closeErr := file.Close()
		sum := sha256.Sum256(data)
		if err != nil || closeErr != nil || len(data) > loopview.MaxPreviewBytes || hex.EncodeToString(sum[:]) != artifact.Source.SHA256 {
			http.Error(w, "accepted artifact content changed or exceeds limit", 409)
			return
		}
		preview, err := loopview.StaticPreview(string(data))
		if err != nil {
			http.Error(w, "accepted HTML contains unsupported active content", 422)
			return
		}
		if err = verifyContextRef(artifact.Source); err != nil {
			http.Error(w, "accepted artifact source changed", 409)
			return
		}
		contextJSON(w, struct {
			Schema     int                     `json:"schema"`
			Version    string                  `json:"version"`
			Artifact   contextAcceptedArtifact `json:"artifact"`
			Candidate  *contextFileRef         `json:"candidate"`
			Acceptance contextAcceptance       `json:"acceptance"`
			Document   string                  `json:"document"`
		}{1, observation.Version, artifact, observation.Candidate, observation.Acceptance, preview})
		return
	}
	http.Error(w, "artifact has no original integrated acceptance binding", 404)
}
