package runtime

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func completedReadbackFixture(t *testing.T) (*OpenShellRuntime, CompletedSessionBinding, *managedFixture) {
	t.Helper()
	r, f, target := openShellRuntimeFixture(t)
	managedMarker(t, f.root, "completion-observation")
	h, err := r.Dispatch(t.Context(), DispatchRequest{Name: "readback", Provider: "codex", Model: "fixture", Prompt: "produce result", SystemPrompt: "test", Runtime: "openshell", WorktreePath: target, AllowPrimaryCheckout: true, EnvVars: map[string]string{"NOODLE_ORDER_ID": "readback", "NOODLE_STAGE_INDEX": "0"}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("dispatch did not settle")
	}
	if h.Outcome().Status != StatusCompleted {
		t.Fatalf("dispatch: %+v", h.Outcome())
	}
	t.Cleanup(func() {
		if _, err := f.owner.Release("a"); err != nil {
			t.Error(err)
		}
	})
	b := CompletedSessionBinding{OrderID: "readback", SessionID: h.ID(), Provider: "codex", Model: "fixture", WorktreePath: target}
	writeReadbackJSON(t, filepath.Join(r.config.RuntimeDir, "sessions", h.ID(), "meta.json"), map[string]any{"session_id": h.ID(), "provider": "codex", "model": "fixture", "runtime": "openshell", "status": "exited", "alive": false})
	return r, b, f
}
func writeReadbackJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
func TestOpenShellSessionReadbackOriginalExecutionAndSources(t *testing.T) {
	r, b, f := completedReadbackFixture(t)
	before, err := os.ReadFile(filepath.Join(f.spec.Binding.StateRoot, "owner.json"))
	if err != nil {
		t.Fatal(err)
	}
	eventsBefore, _ := os.ReadFile(filepath.Join(f.root, "events"))
	result, err := r.ReadCompletedSession(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	if result.ProfileSHA256 == "" || result.NativeAttemptID == "" || result.CandidateHead == "" || result.NativePhase != "Stopped" || !result.StopAcknowledged || len(result.Sources) != 7 {
		t.Fatalf("incomplete observation: %+v", result)
	}
	eventsAfter, _ := os.ReadFile(filepath.Join(f.root, "events"))
	if string(eventsAfter[len(eventsBefore):]) != "get\n" {
		t.Fatalf("readback made non-Get native calls: %q", eventsAfter[len(eventsBefore):])
	}
	after, err := os.ReadFile(filepath.Join(f.spec.Binding.StateRoot, "owner.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("readback wrote native custody")
	}
	raw, _ := json.Marshal(result)
	if strings.Contains(string(raw), "Argv") || strings.Contains(string(raw), "Environment") {
		t.Fatal("readback exposed executable inputs")
	}
}
func TestOpenShellSessionReadbackRejectsChangedBindingsAndEvidence(t *testing.T) {
	r, b, _ := completedReadbackFixture(t)
	dir := filepath.Join(r.config.RuntimeDir, "sessions", b.SessionID)
	for _, variant := range []string{"order", "session", "model", "provider", "target", "stage", "attempt", "unknown-delivery", "profile-drift", "meta-alive", "candidate-drift", "source-drift", "runtime-busy"} {
		t.Run(variant, func(t *testing.T) {
			if variant == "runtime-busy" {
				r.mu.Lock()
				defer r.mu.Unlock()
			}
			changed := b
			file := ""
			var original []byte
			switch variant {
			case "order":
				changed.OrderID = "foreign"
			case "session":
				changed.SessionID = "foreign"
			case "model":
				changed.Model = "foreign"
			case "provider":
				changed.Provider = "pi"
			case "target":
				changed.WorktreePath = t.TempDir()
			case "stage":
				changed.StageIndex = 1
			case "attempt":
				changed.RetryCount = 1
			case "profile-drift":
				file = filepath.Join(dir, "openshell-dispatch.json")
			case "unknown-delivery":
				file = filepath.Join(dir, "delivery/result.json")
			case "meta-alive":
				file = filepath.Join(dir, "meta.json")
			}
			if file != "" {
				original, _ = os.ReadFile(file)
				defer os.WriteFile(file, original, 0600)
				var values map[string]any
				_ = json.Unmarshal(original, &values)
				if variant == "profile-drift" {
					values["ProfileSHA256"] = "unadmitted-profile"
				} else if variant == "unknown-delivery" {
					values["state"] = "unknown"
				} else {
					values["alive"] = true
				}
				writeReadbackJSON(t, file, values)
			}
			observe := func(ctx context.Context, id OpenShellIdentity) (OpenShellCompletedObservation, error) {
				return r.config.Gateway.ObserveCompleted(ctx, r.config.ContextID, id)
			}
			if variant == "source-drift" {
				path := filepath.Join(dir, "meta.json")
				original, _ = os.ReadFile(path)
				defer os.WriteFile(path, original, 0600)
				observe = func(ctx context.Context, id OpenShellIdentity) (OpenShellCompletedObservation, error) {
					value, err := r.config.Gateway.ObserveCompleted(ctx, r.config.ContextID, id)
					_ = os.WriteFile(path, append(original, ' '), 0600)
					return value, err
				}
			}
			if variant == "candidate-drift" {
				head, err := sessionReadbackHead(t.Context(), b.WorktreePath)
				if err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command("git", "-C", b.WorktreePath, "-c", "user.name=fixture", "-c", "user.email=fixture@test", "commit", "--allow-empty", "-m", "drift")
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git: %v %s", err, out)
				}
				defer exec.Command("git", "-C", b.WorktreePath, "reset", "--hard", head).Run()
			}
			if _, err := r.readCompletedSession(t.Context(), changed, observe); err == nil {
				t.Fatal("changed binding/evidence accepted")
			}
		})
	}
}

func TestOpenShellSessionReadbackDoesNotExecuteGitConfiguration(t *testing.T) {
	r, b, f := completedReadbackFixture(t)
	markerRoot := t.TempDir()
	monitorMarker, filterMarker := filepath.Join(markerRoot, "monitor-ran"), filepath.Join(markerRoot, "filter-ran")
	script := filepath.Join(markerRoot, "monitor")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf ran > "+strconv.Quote(monitorMarker)+"\nprintf '\\0'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", b.WorktreePath}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git config: %v %s", err, out)
		}
	}
	run("config", "core.fsmonitor", script)
	run("config", "filter.sideeffect.clean", "printf ran > "+strconv.Quote(filterMarker)+"; cat")
	if err := os.MkdirAll(filepath.Join(b.WorktreePath, ".git", "info"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.WorktreePath, ".git", "info", "attributes"), []byte("*.txt filter=sideeffect\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Contaminated caller selection must not redirect the original target.
	t.Setenv("GIT_DIR", filepath.Join(markerRoot, "nonexistent.git"))
	t.Setenv("GIT_INDEX_FILE", filepath.Join(markerRoot, "nonexistent.index"))
	if _, err := r.ReadCompletedSession(t.Context(), b); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.WorktreePath, "result.txt"), []byte("changed raw output\n"), 0644); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(f.root, "events"))
	if _, err := r.ReadCompletedSession(t.Context(), b); err == nil {
		t.Fatal("dirty candidate accepted")
	}
	after, _ := os.ReadFile(filepath.Join(f.root, "events"))
	if string(before) != string(after) {
		t.Fatal("dirty candidate reached native readback")
	}
	for _, marker := range []string{monitorMarker, filterMarker} {
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("readback executed Git configuration: %s", marker)
		}
	}
}
