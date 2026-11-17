package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func scratchHookRuntimeFixture(t *testing.T, operation string) (*OpenShellRuntime, *managedFixture, string) {
	t.Helper()
	r, f, target := openShellRuntimeFixture(t, func(f *managedFixture) {
		managedMarker(t, f.root, "completion-observation")
	})
	hooks := t.TempDir()
	root := "'" + strings.ReplaceAll(filepath.Join(r.config.RuntimeDir, "sessions"), "'", "'\\''") + "'"
	script := fmt.Sprintf("#!/bin/sh\nfind %s -type d -name %s\n", root, operation)
	if err := os.WriteFile(filepath.Join(hooks, "post-commit"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "config", "core.hooksPath", hooks)
	cmd.Dir = target
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatal(err, string(output))
	}
	return r, f, target
}

func TestOpenShellScratchRefusalPreservesPublishedTerminalAndRecovery(t *testing.T) {
	r, f, target := scratchHookRuntimeFixture(t, "patch-repository -exec touch '{}/original-post-commit-note' ';'")
	h, err := r.Dispatch(t.Context(), DispatchRequest{Name: "scratch-terminal", Provider: "codex", Model: "fixture", Prompt: "produce result", SystemPrompt: "test", Runtime: "openshell", WorktreePath: target, AllowPrimaryCheckout: true, EnvVars: map[string]string{"NOODLE_ORDER_ID": "scratch-terminal", "NOODLE_STAGE_INDEX": "0"}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("scratch refusal stranded the original published session", h.Outcome())
	}
	s := h.(*openShellSession)
	var saved openShellDispatchReceipt
	raw, err := os.ReadFile(filepath.Join(s.dir, "openshell-dispatch.json"))
	if err != nil || json.Unmarshal(raw, &saved) != nil {
		t.Fatal("terminal evidence unavailable", err)
	}
	if h.Outcome().Status != StatusCompleted || !r.CapacityAvailable() || saved.Phase != "settled" || saved.Error != "" || saved.CandidateHead == "" || !strings.Contains(saved.PublicationScratchError, "scratch changed after original apply") {
		t.Fatal("task terminal and refused cleanup disagree", h.Outcome(), saved)
	}
	if _, err := os.Stat(filepath.Join(s.dir, "delivery", "patch-repository", "original-post-commit-note")); err != nil {
		t.Fatal("refused scratch or hook evidence was removed", err)
	}
	writeReadbackJSON(t, filepath.Join(s.dir, "meta.json"), map[string]any{"session_id": h.ID(), "provider": "codex", "model": "fixture", "runtime": "openshell", "status": "exited", "alive": false})
	binding := CompletedSessionBinding{OrderID: "scratch-terminal", SessionID: h.ID(), Provider: "codex", Model: "fixture", WorktreePath: target}
	if _, err := r.ReadCleanupSession(t.Context(), binding, target, StatusCompleted); err != nil {
		t.Fatal("retained publication was invalidated by refused scratch cleanup", err)
	}
	fresh, err := NewOpenShellRuntime(r.config)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := fresh.Recover(t.Context())
	if err != nil || len(recovered) != 0 || !fresh.CapacityAvailable() {
		t.Fatal("settled publication disagrees with original recovery", recovered, err)
	}
	after, err := os.ReadFile(filepath.Join(s.dir, "openshell-dispatch.json"))
	if err != nil || string(raw) != string(after) {
		t.Fatal("read-only recovery changed original cleanup failure history", err)
	}
	if _, err := f.owner.Release("a"); err != nil {
		t.Fatal(err)
	}
}

func TestOpenShellRequiredPublicationFailureKeepsOriginalRecovery(t *testing.T) {
	r, _, target := scratchHookRuntimeFixture(t, "returned -exec touch '{}/changed-required-evidence' ';'")
	h, err := r.Dispatch(t.Context(), DispatchRequest{Name: "required-evidence", Provider: "codex", Model: "fixture", Prompt: "produce result", SystemPrompt: "test", Runtime: "openshell", WorktreePath: target, AllowPrimaryCheckout: true, EnvVars: map[string]string{"NOODLE_ORDER_ID": "required-evidence", "NOODLE_STAGE_INDEX": "0"}})
	if err != nil {
		t.Fatal(err)
	}
	s := h.(*openShellSession)
	deadline := time.Now().Add(10 * time.Second)
	for {
		s.mu.Lock()
		phase, reason, diagnostic, head := s.receipt.Phase, s.receipt.Error, s.receipt.PublicationScratchError, s.receipt.CandidateHead
		s.mu.Unlock()
		if reason != "" {
			if phase != "publishing" || diagnostic != "" || head == "" || h.Outcome().Status != StatusFailed || r.CapacityAvailable() {
				t.Fatal("required evidence failure became a completed task", phase, reason, diagnostic, h.Outcome())
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("required evidence tamper did not reach original refusal")
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case <-h.Done():
		t.Fatal("unresolved original publication released its session")
	default:
	}
	if _, err := os.Stat(filepath.Join(s.dir, "delivery", "patch-repository")); err != nil {
		t.Fatal("required evidence refusal deleted scratch", err)
	}
	fresh, err := NewOpenShellRuntime(r.config)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := fresh.Recover(t.Context())
	if err != nil || len(recovered) != 1 || recovered[0].SessionHandle.ID() != h.ID() || fresh.CapacityAvailable() {
		t.Fatal("required evidence failure lost its original recovery entry", recovered, err)
	}
}
