package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	loopruntime "github.com/poteto/noodle/runtime"
)

func contextTestFile(t *testing.T, path string, data []byte, mode os.FileMode) contextFileRef {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		t.Fatal(err)
	}
	ref, err := loopruntime.ReadSessionReadbackFile(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}
func contextTestJSON(t *testing.T, path string, value any) contextFileRef {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return contextTestFile(t, path, data, 0600)
}
func contextTestRegistration(t *testing.T) (string, contextRegistration, contextObservation) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	identity := contextOwnerIdentity{"original-host", strings.Repeat("a", 64), dir}
	definition := contextTestJSON(t, filepath.Join(dir, "catalog.json"), map[string]any{"schema": 1})
	composition := contextRequestComposition{definition, "codex-drawing", "draw"}
	request := contextTestJSON(t, filepath.Join(dir, "request.json"), map[string]any{"composition": composition})
	python := contextTestFile(t, filepath.Join(dir, "python"), []byte("fixture interpreter"), 0700)
	entry := contextTestFile(t, filepath.Join(dir, "bundle/runtime/soodles.py"), []byte("# fixture owner"), 0600)
	manifest := contextTestJSON(t, filepath.Join(dir, "bundle/manifest.json"), map[string]any{"schema": 1, "runtime": []map[string]string{{"path": "runtime/soodles.py", "sha256": entry.SHA256}}})
	r := contextRegistration{1, "drawing", "Drawing task", request, contextLauncher{python, entry, manifest, map[string]string{"HOME": dir, "PATH": "/usr/bin:/bin", "NOODLE_HOST_DIR": dir}}, identity}
	contextTestJSON(t, contextRegistryPath(dir), map[string]contextRegistration{r.ID: r})
	observation := contextObservation{Schema: 1, Owner: "soodles.loop-ui", Status: "prepared", Version: strings.Repeat("b", 64), Summary: "Prepared", OriginalHost: identity, Request: request, Composition: &composition, Effects: []string{}, Sources: map[string]contextFileRef{}, Acceptance: contextAcceptance{Status: "UNKNOWN", Scope: "candidate", Artifacts: []contextAcceptedArtifact{}}, Actions: contextActions{Start: contextActionAvailability{true, "prepared"}, NativeResume: contextActionAvailability{false, "unsupported"}}}
	observation.ActiveStage = &contextActiveStage{request, composition, []contextFileRef{}}
	return dir, r, observation
}
func contextTestHandler(dir string, r contextRegistration, call contextOwnerCall) http.Handler {
	return contextControlsHandler(dir, r.OriginalHost, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "read only", 405) }), call)
}
func contextTestPost(handler http.Handler, path, body, origin, contentType string) *httptest.ResponseRecorder {
	request := httptest.NewRequest("POST", "http://127.0.0.1:12345"+path, strings.NewReader(body))
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	request.Header.Set("Content-Type", contentType)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
func TestContextControlAdmission(t *testing.T) {
	dir, registration, observation := contextTestRegistration(t)
	calls := 0
	handler := contextTestHandler(dir, registration, func(_ context.Context, r contextRegistration, action, version string) (contextObservation, error) {
		calls++
		if r.Request != registration.Request || action != "start" || version != observation.Version {
			t.Fatal("owner invocation differs")
		}
		return observation, nil
	})
	body := fmt.Sprintf(`{"id":"drawing","registry_version":%q,"expected_version":%q,"action":"start"}`, contextVersion(registration), observation.Version)
	for _, tc := range []struct {
		name, body, origin, contentType string
		want                            int
	}{
		{"no origin", body, "", "application/json", 403},
		{"other localhost", body, "http://localhost:9999", "application/json", 403},
		{"form", body, "http://127.0.0.1:12345", "text/plain", 415},
		{"argv", strings.TrimSuffix(body, "}") + `,"argv":["arbitrary"]}`, "http://127.0.0.1:12345", "application/json", 400},
		{"trailing", body + `{}`, "http://127.0.0.1:12345", "application/json", 400},
		{"native resume", strings.Replace(body, `"start"`, `"native_resume"`, 1), "http://127.0.0.1:12345", "application/json", 422},
		{"stale registration", strings.Replace(body, contextVersion(registration), strings.Repeat("f", 64), 1), "http://127.0.0.1:12345", "application/json", 409},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := contextTestPost(handler, "/api/contexts/action", tc.body, tc.origin, tc.contentType)
			if response.Code != tc.want {
				t.Fatalf("%d %s", response.Code, response.Body.String())
			}
		})
	}
	if calls != 0 {
		t.Fatal("rejected web requests reached owner")
	}
	response := contextTestPost(handler, "/api/contexts/action", body, "http://127.0.0.1:12345", "application/json")
	if response.Code != 200 || calls != 1 {
		t.Fatalf("valid action: %d %s", response.Code, response.Body.String())
	}
	if strings.Contains(response.Body.String(), "launcher") {
		t.Fatal("launcher leaked")
	}
}
func TestContextControlsReadOnlyCatalogAndChangedSources(t *testing.T) {
	dir, r, o := contextTestRegistration(t)
	calls := 0
	handler := contextTestHandler(dir, r, func(context.Context, contextRegistration, string, string) (contextObservation, error) {
		calls++
		return o, nil
	})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest("GET", "http://local/api/contexts", nil))
	if response.Code != 200 || calls != 0 || strings.Contains(response.Body.String(), r.Request.Path) {
		t.Fatalf("catalog leaked authority: %s", response.Body.String())
	}
	if err := os.WriteFile(r.Launcher.Entry.Path, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"id":"drawing","registry_version":%q}`, contextVersion(r))
	response = contextTestPost(handler, "/api/contexts/observe", body, "http://127.0.0.1:12345", "application/json")
	if response.Code != 409 || calls != 0 {
		t.Fatal("changed owner sources admitted")
	}
}
func TestContextRegistrationRejectsChangedAuthority(t *testing.T) {
	for _, name := range []string{"other host", "secret env", "unlisted helper", "cached bytecode", "catalog drift", "manifest traversal"} {
		t.Run(name, func(t *testing.T) {
			dir, r, _ := contextTestRegistration(t)
			identity := r.OriginalHost
			switch name {
			case "other host":
				identity.InstanceID = "replacement"
			case "secret env":
				r.Launcher.Environment["OPENROUTER_API_KEY"] = "not-a-key"
			case "unlisted helper":
				contextTestFile(t, filepath.Join(dir, "bundle/runtime/side.py"), []byte("# added"), 0600)
			case "cached bytecode":
				contextTestFile(t, filepath.Join(dir, "bundle/runtime/__pycache__/soodles.pyc"), []byte("cache"), 0600)
			case "catalog drift":
				contextTestFile(t, filepath.Join(dir, "catalog.json"), []byte("changed"), 0600)
			case "manifest traversal":
				r.Launcher.Manifest = contextTestJSON(t, r.Launcher.Manifest.Path, map[string]any{"schema": 1, "runtime": []map[string]string{{"path": "runtime/../../elsewhere", "sha256": r.Launcher.Entry.SHA256}}})
			}
			if _, err := validateContextRegistration(r, identity); err == nil {
				t.Fatal("changed authority accepted")
			}
		})
	}
}
func TestContextOwnerProjectionMismatchRefused(t *testing.T) {
	dir, r, o := contextTestRegistration(t)
	o.Request.SHA256 = strings.Repeat("c", 64)
	handler := contextTestHandler(dir, r, func(context.Context, contextRegistration, string, string) (contextObservation, error) { return o, nil })
	body := fmt.Sprintf(`{"id":"drawing","registry_version":%q}`, contextVersion(r))
	response := contextTestPost(handler, "/api/contexts/observe", body, "http://127.0.0.1:12345", "application/json")
	if response.Code != 502 {
		t.Fatal("wrong original request accepted")
	}
}
func TestContextAcceptedPreviewBindsOriginalAcceptance(t *testing.T) {
	dir, r, o := contextTestRegistration(t)
	source := contextTestFile(t, filepath.Join(dir, "accepted.html"), []byte(`<html><body><svg><text x="2" y="20">Accepted diagram</text></svg></body></html>`), 0600)
	candidate := contextTestJSON(t, filepath.Join(dir, "candidate.json"), map[string]any{"head": "fixed-head"})
	record := contextTestJSON(t, filepath.Join(dir, "acceptance.json"), map[string]any{"candidate": candidate})
	head := "fixed-head"
	o.Candidate = &candidate
	o.Acceptance = contextAcceptance{"PASS", "integrated", &record, &head, []contextAcceptedArtifact{{"drawing", "drawing.html", "html", source}}}
	handler := contextTestHandler(dir, r, func(_ context.Context, _ contextRegistration, action, version string) (contextObservation, error) {
		if action != "inspect" || version != "" {
			t.Fatal("preview performed task action")
		}
		return o, nil
	})
	body := fmt.Sprintf(`{"id":"drawing","registry_version":%q,"expected_version":%q,"artifact_id":"drawing"}`, contextVersion(r), o.Version)
	response := contextTestPost(handler, "/api/contexts/preview", body, "http://127.0.0.1:12345", "application/json")
	if response.Code != 200 || !strings.Contains(response.Body.String(), "Accepted diagram") || !strings.Contains(response.Body.String(), "Content-Security-Policy") {
		t.Fatalf("preview: %d %s", response.Code, response.Body.String())
	}
	o.Acceptance.Status = "UNKNOWN"
	response = contextTestPost(handler, "/api/contexts/preview", body, "http://127.0.0.1:12345", "application/json")
	if response.Code != 502 {
		t.Fatal("artifact displayed without original acceptance")
	}
	o.Acceptance.Status = "PASS"
	contextTestFile(t, source.Path, []byte("changed accepted artifact"), 0600)
	response = contextTestPost(handler, "/api/contexts/preview", body, "http://127.0.0.1:12345", "application/json")
	if response.Code != 409 {
		t.Fatal("drifted artifact displayed")
	}
}

func TestContextOwnerDiagnosticIsPrivateAndBounded(t *testing.T) {
	dir, r, _ := contextTestRegistration(t)
	output := contextOwnerOutput{limit: 64}
	text := strings.Repeat("private fixture output", 100)
	n, err := output.Write([]byte(text))
	if err != nil || n != len(text) || output.Len() != 64 || !output.exceeded {
		t.Fatal("diagnostic output was not bounded")
	}
	// exec.Cmd uses io.Copy; an embedded bytes.Buffer would promote
	// ReadFrom and bypass the writer cap even though direct Write is bounded.
	copied := contextOwnerOutput{limit: 64}
	if _, err := io.Copy(&copied, struct{ io.Reader }{strings.NewReader(text)}); err != nil || copied.Len() != 64 || !copied.exceeded {
		t.Fatal("io.Copy bypassed process output bound")
	}
	id, err := saveContextOwnerDiagnostic(dir, contextOwnerDiagnostic{Schema: 1, Request: r.Request, OriginalHost: r.OriginalHost, Stderr: output.String(), StderrTruncated: output.exceeded, ExitCode: 7})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "context-control-diagnostics", id+".json")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("diagnostic file is not private")
	}
	info, err = os.Stat(filepath.Dir(path))
	if err != nil || info.Mode().Perm() != 0700 {
		t.Fatal("diagnostic directory is not private")
	}
	handler := contextTestHandler(dir, r, func(context.Context, contextRegistration, string, string) (contextObservation, error) {
		return contextObservation{}, &contextOwnerOperationError{Cause: fmt.Errorf("private fixture output"), DiagnosticID: id}
	})
	body := fmt.Sprintf(`{"id":"drawing","registry_version":%q}`, contextVersion(r))
	response := contextTestPost(handler, "/api/contexts/observe", body, "http://127.0.0.1:12345", "application/json")
	if response.Code != 502 || strings.Contains(response.Body.String(), "private fixture output") || !strings.Contains(response.Body.String(), id) {
		t.Fatal("private diagnostic disclosure or missing public identifier")
	}
	if err = os.Chmod(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if _, err = saveContextOwnerDiagnostic(dir, contextOwnerDiagnostic{}); err == nil {
		t.Fatal("nonprivate diagnostic path accepted")
	}
}
