package runtime

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOpenShellArchivePolicyDispatchReturnsChangedLockfile(t *testing.T) {
	for _, old := range []bool{false, true} {
		t.Run(map[bool]string{false: "qualified", true: "old-supervisor-holds"}[old], func(t *testing.T) {
			r, f, target := openShellRuntimeFixture(t, func(f *managedFixture) {
				managedMarker(t, f.root, "completion-observation")
			})
			for name, body := range map[string]string{"package.json": `{"dependencies":{"old":"1"}}`, "pnpm-lock.yaml": "original lockfile\n"} {
				if err := os.WriteFile(filepath.Join(target, name), []byte(body), 0644); err != nil {
					t.Fatal(err)
				}
			}
			for _, args := range [][]string{{"add", "."}, {"commit", "-m", "test: declare original dependency rebuild sources"}} {
				cmd := exec.Command("git", args...)
				cmd.Dir = target
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatal(err, string(output))
				}
			}
			r.config.Profile.Environment = append(r.config.Profile.Environment, "NOODLE_PROGRESS_FIXTURE_DEPENDENCIES=1")
			if old {
				r.config.Profile.Environment = append(r.config.Profile.Environment, "NOODLE_PROGRESS_FIXTURE_OLD_SUPERVISOR=1")
			}
			h, err := r.Dispatch(t.Context(), DispatchRequest{Name: "dependencies", Provider: "codex", Model: "fixture", Prompt: "update dependencies", SystemPrompt: "test", Runtime: "openshell", WorktreePath: target, AllowPrimaryCheckout: true, EnvVars: map[string]string{"NOODLE_ORDER_ID": "dependencies", "NOODLE_STAGE_INDEX": "0"}})
			if err != nil {
				t.Fatal(err)
			}
			s := h.(*openShellSession)
			if old {
				deadline := time.Now().Add(10 * time.Second)
				for {
					s.mu.Lock()
					phase, reason := s.receipt.Phase, s.receipt.Error
					s.mu.Unlock()
					if phase == "progress-unknown" {
						if !strings.Contains(reason, "did not consume") || r.CapacityAvailable() {
							t.Fatal("old supervisor acquired new producer qualification", reason)
						}
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("unconsumed policy was not held", h.Outcome())
					}
					time.Sleep(5 * time.Millisecond)
				}
				select {
				case <-h.Done():
					t.Fatal("unqualified producer released original session")
				default:
				}
				return
			}
			select {
			case <-h.Done():
			case <-time.After(10 * time.Second):
				t.Fatal("qualified dispatch unresolved", h.Outcome())
			}
			if h.Outcome().Status != StatusCompleted || s.receipt.Progress.Schema != 2 || s.receipt.Progress.ArchivePolicySHA256 == "" {
				t.Fatal("qualified return failed", h.Outcome())
			}
			data, err := os.ReadFile(filepath.Join(target, "pnpm-lock.yaml"))
			if err != nil || string(data) != "legitimately updated lockfile\n" {
				t.Fatal("admitted lockfile update lost", string(data), err)
			}
			writeReadbackJSON(t, filepath.Join(s.dir, "meta.json"), map[string]any{"session_id": h.ID(), "provider": "codex", "model": "fixture", "runtime": "openshell", "status": "exited", "alive": false})
			binding := CompletedSessionBinding{OrderID: "dependencies", SessionID: h.ID(), Provider: "codex", Model: "fixture", WorktreePath: target}
			if _, err := r.ReadCleanupSession(t.Context(), binding, target, StatusCompleted); err != nil {
				t.Fatal("new archive policy broke original retained cleanup", err)
			}
			if _, err := f.owner.Release("a"); err != nil {
				t.Fatal(err)
			}
		})
	}
}
