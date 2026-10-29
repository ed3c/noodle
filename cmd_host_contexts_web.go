package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	loopruntime "github.com/poteto/noodle/runtime"
	"io"
	"mime"
	"net/http"
)

type contextActionAvailability struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason"`
}
type contextActions struct {
	Start        contextActionAvailability `json:"start"`
	Stop         contextActionAvailability `json:"stop"`
	NewAttempt   contextActionAvailability `json:"new_attempt"`
	NativeResume contextActionAvailability `json:"native_resume"`
}
type contextAcceptedArtifact struct {
	ID     string         `json:"id"`
	Label  string         `json:"label"`
	Format string         `json:"format"`
	Source contextFileRef `json:"source"`
}
type contextAcceptance struct {
	Status    string                    `json:"status"`
	Scope     string                    `json:"scope"`
	Record    *contextFileRef           `json:"record"`
	Head      *string                   `json:"head"`
	Artifacts []contextAcceptedArtifact `json:"artifacts"`
}
type contextTaskIdentity struct {
	OrderID   *string `json:"order_id"`
	ContextID *string `json:"context_id"`
	SessionID *string `json:"session_id"`
}
type contextNext struct {
	Owner     string `json:"owner"`
	Operation string `json:"operation"`
	Tool      string `json:"tool"`
}
type contextObservation struct {
	Schema       int                        `json:"schema"`
	Owner        string                     `json:"owner"`
	Status       string                     `json:"status"`
	Version      string                     `json:"version"`
	Summary      string                     `json:"summary"`
	OriginalHost contextOwnerIdentity       `json:"original_host"`
	Request      contextFileRef             `json:"request"`
	Composition  *contextRequestComposition `json:"composition"`
	Actions      contextActions             `json:"actions"`
	Candidate    *contextFileRef            `json:"candidate"`
	Acceptance   contextAcceptance          `json:"acceptance"`
	Identity     contextTaskIdentity        `json:"identity"`
	Sources      map[string]contextFileRef  `json:"sources"`
	Next         *contextNext               `json:"next"`
	Effects      []string                   `json:"effects"`
}
type contextWebAction struct {
	ID              string `json:"id"`
	RegistryVersion string `json:"registry_version"`
	ExpectedVersion string `json:"expected_version"`
	Action          string `json:"action"`
}
type contextOwnerCall func(context.Context, contextRegistration, string, string) (contextObservation, error)

// The callback runs in the HTTP goroutine. An owner command may call the Host
// socket itself, so it must never run inside serveHost's serialized call queue.
func contextControlsHandler(dir string, identity contextOwnerIdentity, fallback http.Handler, call contextOwnerCall) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /contexts", func(w http.ResponseWriter, r *http.Request) {
		clone := r.Clone(r.Context())
		clone.URL.Path = "/loops"
		fallback.ServeHTTP(w, clone)
	})
	mux.HandleFunc("GET /api/contexts", func(w http.ResponseWriter, r *http.Request) {
		entries, err := loadContextRegistrations(dir)
		if err != nil {
			http.Error(w, "context registrations unavailable", 503)
			return
		}
		choices, err := contextChoices(entries, identity)
		if err != nil {
			http.Error(w, "registered context source changed; Session must inspect", 409)
			return
		}
		contextJSON(w, struct {
			Schema  int             `json:"schema"`
			Choices []contextChoice `json:"choices"`
		}{1, choices})
	})
	mux.HandleFunc("POST /api/contexts/observe", func(w http.ResponseWriter, r *http.Request) { contextWebCall(w, r, dir, identity, call, true) })
	mux.HandleFunc("POST /api/contexts/action", func(w http.ResponseWriter, r *http.Request) { contextWebCall(w, r, dir, identity, call, false) })
	mux.HandleFunc("POST /api/contexts/preview", func(w http.ResponseWriter, r *http.Request) { contextAcceptedPreview(w, r, dir, identity, call) })
	mux.Handle("/", fallback)
	return mux
}
func contextJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_ = json.NewEncoder(w).Encode(value)
}
func contextWebCall(w http.ResponseWriter, r *http.Request, dir string, identity contextOwnerIdentity, call contextOwnerCall, inspect bool) {
	// Browsers send Origin for same-origin JSON POST. No permissive localhost
	// allowlist and no origin-free mutation endpoint are exposed here.
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
		http.Error(w, "control request exceeds limit", 413)
		return
	}
	var request contextWebAction
	if err = decodeContextJSON(data, &request); err != nil {
		http.Error(w, "invalid control request", 400)
		return
	}
	if !contextIDPattern.MatchString(request.ID) || !contextHashPattern.MatchString(request.RegistryVersion) {
		http.Error(w, "registered selection and version required", 400)
		return
	}
	if inspect {
		if request.Action != "" || request.ExpectedVersion != "" {
			http.Error(w, "observation has no action or prior version", 400)
			return
		}
	} else {
		switch request.Action {
		case "start", "stop", "new_attempt":
		default:
			http.Error(w, "action is not supported; native session resume is unavailable", 422)
			return
		}
		if !contextHashPattern.MatchString(request.ExpectedVersion) {
			http.Error(w, "original owner observation version required", 400)
			return
		}
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
	action := request.Action
	if inspect {
		action = "inspect"
	}
	result, err := call(r.Context(), entry, action, request.ExpectedVersion)
	if err != nil {
		// Owner diagnostics can contain private request data. Only the bounded,
		// typed original projection is public; execution failures require inspect.
		message := "original owner operation unconfirmed; inspect before another action"
		var operationError *contextOwnerOperationError
		if errors.As(err, &operationError) {
			if operationError.DiagnosticFailed {
				message += "; private process diagnostics could not be saved"
			} else if operationError.DiagnosticID != "" {
				message += "; private diagnostic " + operationError.DiagnosticID
			}
		}
		http.Error(w, message, 502)
		return
	}
	if err = validateContextObservation(result, entry); err != nil {
		http.Error(w, "original owner projection binding differs", 502)
		return
	}
	contextJSON(w, result)
}
func validateContextObservation(o contextObservation, r contextRegistration) error {
	if o.Schema != 1 || o.Owner != "soodles.loop-ui" || !contextHashPattern.MatchString(o.Version) || o.Request != r.Request || o.OriginalHost != r.OriginalHost || o.Status == "" || len(o.Status) > 100 || len(o.Summary) > 4000 || o.Composition == nil || o.Effects == nil {
		return fmt.Errorf("original owner projection identity incomplete")
	}
	var request struct {
		Composition *contextRequestComposition `json:"composition"`
	}
	ref, err := loopruntime.ReadSessionReadbackFile(r.Request.Path, &request)
	if err != nil || ref != r.Request || request.Composition == nil || *request.Composition != *o.Composition {
		return fmt.Errorf("original owner composition differs")
	}
	if o.Actions.NativeResume.Available {
		return fmt.Errorf("native session resume is unsupported")
	}
	for _, a := range []contextActionAvailability{o.Actions.Start, o.Actions.Stop, o.Actions.NewAttempt, o.Actions.NativeResume} {
		if len(a.Reason) > 2000 {
			return fmt.Errorf("owner action diagnostic exceeds limit")
		}
	}
	if len(o.Effects) > 32 {
		return fmt.Errorf("owner effects exceed limit")
	}
	for _, e := range o.Effects {
		if len(e) > 500 {
			return fmt.Errorf("owner effect exceeds limit")
		}
	}
	if len(o.Acceptance.Artifacts) > 128 {
		return fmt.Errorf("owner artifacts exceed limit")
	}
	if len(o.Acceptance.Artifacts) > 0 && (o.Acceptance.Status != "PASS" || o.Acceptance.Scope != "integrated" || o.Acceptance.Record == nil || o.Candidate == nil || o.Acceptance.Head == nil || *o.Acceptance.Head == "") {
		return fmt.Errorf("accepted artifacts have no integrated source binding")
	}
	seen := map[string]bool{}
	for _, artifact := range o.Acceptance.Artifacts {
		if artifact.ID == "" || len(artifact.ID) > 128 || len(artifact.Label) > 1024 || seen[artifact.ID] || !contextHashPattern.MatchString(artifact.Source.SHA256) {
			return fmt.Errorf("invalid accepted artifact source")
		}
		seen[artifact.ID] = true
	}
	return nil
}
