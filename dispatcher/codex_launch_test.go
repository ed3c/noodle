package dispatcher

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// These are argv contract controls, not authenticated Agent execution receipts.
func TestCodexProcessLaunchUsesHostPermissions(t *testing.T) {
	d := NewProcessDispatcher(ProcessDispatcherConfig{})
	cmd, err := d.buildCmd(DispatchRequest{Provider: "codex", Model: "test-model"}, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"exec", "--skip-git-repo-check", "--json", "--model", "test-model"}
	if !reflect.DeepEqual(cmd.Args[1:], want) {
		t.Fatalf("process dispatcher changed host permission selection: got %q, want %q", cmd.Args[1:], want)
	}
}

func TestCodexLaunchFailurePreservesOwningDiagnostic(t *testing.T) {
	for _, tc := range []struct {
		name, provider, reason, stderr string
		status                         SessionStatus
		wantDiagnostic                 bool
	}{
		{"invalid sandbox", "codex", "no events emitted", "error: invalid value 'invalid-mode' for '--sandbox <SANDBOX_MODE>'\n", StatusFailed, true},
		{"host requirements", "codex", "no events emitted", "Reading additional input from stdin...\nError: approval_policy=never conflicts with sandbox_mode=danger-full-access\n", StatusFailed, true},
		{"unknown stderr", "codex", "no events emitted", "connection ended\n", StatusFailed, false},
		{"after init", "codex", "no work produced", "error: later failure\n", StatusFailed, false},
		{"cancelled", "codex", "context cancelled before completion", "error: interrupted\n", StatusCancelled, false},
		{"other provider", "claude", "no events emitted", "Error: other provider\n", StatusFailed, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "stderr.log")
			if err := os.WriteFile(path, []byte(tc.stderr), 0600); err != nil {
				t.Fatal(err)
			}
			done := make(chan struct{})
			close(done)
			original := SessionOutcome{Status: tc.status, Reason: tc.reason, ExitCode: 2}
			s := &processSession{provider: tc.provider, stderrPath: path, stderrDone: done}
			s.outcome = original
			got := s.Outcome()
			if !tc.wantDiagnostic {
				if got != original {
					t.Fatalf("unrelated outcome changed: %+v", got)
				}
				return
			}
			if got.Status != original.Status || got.ExitCode != original.ExitCode || got.HasDeliverable {
				t.Fatalf("diagnostic changed lifecycle truth: %+v", got)
			}
			for _, part := range []string{"Codex exec configuration", "next: codex exec --help"} {
				if !strings.Contains(got.Reason, part) {
					t.Fatalf("missing %q: %s", part, got.Reason)
				}
			}
			for _, line := range strings.Split(tc.stderr, "\n") {
				if strings.HasPrefix(strings.ToLower(line), "error:") && !strings.Contains(got.Reason, line) {
					t.Fatalf("lost exact Codex diagnostic: %s", got.Reason)
				}
			}
		})
	}
}

func TestCodexLaunchFailureWaitsForStderrReceipt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stderr.log")
	done := make(chan struct{})
	s := &processSession{provider: "codex", stderrPath: path, stderrDone: done}
	s.outcome = SessionOutcome{Status: StatusFailed, Reason: "no events emitted", ExitCode: 2}
	result := make(chan SessionOutcome, 1)
	go func() { result <- s.Outcome() }()
	if err := os.WriteFile(path, []byte("error: invalid value 'invalid-mode' for '--sandbox'\n"), 0600); err != nil {
		close(done)
		t.Fatal(err)
	}
	close(done)
	select {
	case got := <-result:
		if !strings.Contains(got.Reason, "invalid-mode") {
			t.Fatalf("stderr receipt lost: %+v", got)
		}
	case <-time.After(time.Second):
		t.Fatal("Outcome did not finish after stderr receipt")
	}
}

func TestCodexProcessLaunchPreservesConfiguredArguments(t *testing.T) {
	for _, args := range [][]string{
		{"--sandbox", "workspace-write"},
		{"--profile", "ci"},
		{"--sandbox", "read-only", "--ephemeral"},
		{"--sandbox", "invalid-mode"}, // Codex owns validation; never guess a replacement.
	} {
		t.Run(args[1], func(t *testing.T) {
			d := NewProcessDispatcher(ProcessDispatcherConfig{
				ProviderConfigs: ProviderConfigs{Codex: ProviderConfig{Args: args}},
			})
			cmd, err := d.buildCmd(DispatchRequest{Provider: "codex", Model: "test-model"}, "")
			if err != nil {
				t.Fatal(err)
			}
			want := append([]string{"exec", "--skip-git-repo-check", "--json", "--model", "test-model"}, args...)
			if !reflect.DeepEqual(cmd.Args[1:], want) {
				t.Fatalf("configured arguments changed: got %q, want %q", cmd.Args[1:], want)
			}
		})
	}
}

func TestCodexSpriteLaunchKeepsExistingCarrierContract(t *testing.T) {
	// Sprite is a separate externally sandboxed carrier. The process correction
	// must not silently change its existing launch contract.
	want := []string{"codex", "exec", "--dangerously-bypass-approvals-and-sandbox", "--skip-git-repo-check", "--json", "--model", "test-model"}
	if got := buildSpriteCodexArgs(DispatchRequest{Model: "test-model"}); !reflect.DeepEqual(got, want) {
		t.Fatalf("Sprite argv = %q, want %q", got, want)
	}
}
