package runtime

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Export only to the external-package integration test, which can import Loop
// without introducing a runtime -> loop production dependency.
func DiagnosticLoopFixtureForTest(t *testing.T) (*OpenShellRuntime, CompletedSessionBinding, string, func()) {
	t.Helper()
	r, f, repository := openShellRuntimeFixture(t)
	repository, err := filepath.EvalSymlinks(repository)
	if err != nil {
		t.Fatal(err)
	}
	r.config.RuntimeDir = filepath.Join(repository, ".noodle")
	if err := os.MkdirAll(filepath.Join(repository, ".git", "info"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repository, ".git", "info", "exclude"), []byte(".noodle/\n.worktrees/\n"), 0600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(repository, ".worktrees", "diagnostic-execution")
	if out, err := exec.Command("git", "-C", repository, "worktree", "add", "--detach", target, "HEAD").CombinedOutput(); err != nil {
		t.Fatalf("linked diagnostic checkout: %v %s", err, out)
	}
	managedMarker(t, f.root, "completion-observation")
	if err := os.WriteFile(r.config.Profile.CLI, []byte("#!/bin/sh\necho 'diagnosed upload failure' >&2\nexit 23\n"), 0700); err != nil {
		t.Fatal(err)
	}
	h, err := r.Dispatch(t.Context(), DispatchRequest{Name: "diagnostic-loop", Provider: "codex", Model: "fixture", Prompt: "task", SystemPrompt: "owner", Runtime: "openshell", WorktreePath: target, EnvVars: map[string]string{"NOODLE_ORDER_ID": "diagnostic-order", "NOODLE_STAGE_INDEX": "0"}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("diagnostic dispatch did not settle")
	}
	if h.Outcome().Status != StatusFailed || h.Outcome().ExitCode != -1 {
		t.Fatalf("diagnostic outcome: %+v", h.Outcome())
	}
	b := CompletedSessionBinding{OrderID: "diagnostic-order", SessionID: h.ID(), Provider: "codex", Model: "fixture", WorktreePath: target}
	return r, b, repository, func() {
		t.Helper()
		records := f.owner.custody.Snapshot().Records
		if len(records) != 1 || records[0].Exec != nil || records[0].Retirement != nil || !records[0].CreateAcknowledged || !records[0].StopAcknowledged || records[0].MainProcessInstanceID == "" {
			t.Fatalf("Loop inspection changed native custody: %+v", records)
		}
	}
}
