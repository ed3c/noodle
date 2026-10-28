package dispatcher

import (
	"context"
	"github.com/poteto/noodle/skill"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRemoteHarnessPiKeepsGuestSessionIdentity(t *testing.T) {
	for _, mode := range []string{"success", "wrong-model"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			guest := filepath.Join(root, "guest-sessions")
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			req := DispatchRequest{Name: "remote", Provider: "pi", Runtime: "openshell", Model: "openrouter/deepseek/deepseek-v4.1-flash-1017", Prompt: "unique remote task", SystemPrompt: "instructions", WorktreePath: root}
			plan, err := PrepareRemoteHarness(filepath.Join(root, "local"), skill.Resolver{}, req, "/sandbox/payload/pi", guest)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(plan.prompt, root) || !strings.Contains(plan.prompt, "/sandbox/repo") {
				t.Fatal("remote prompt leaked host worktree location or omitted guest directory")
			}
			if err := os.MkdirAll(filepath.Join(plan.GuestSessionDir, "pi"), 0700); err != nil {
				t.Fatal(err)
			}
			args := append([]string{"-test.run=^TestPiRPCChild$", "--"}, plan.Command()[1:]...)
			cmd := exec.Command(binary, args...)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "NOODLE_FAKE_PI=1", "NOODLE_FAKE_PI_MODE="+mode)
			process, err := StartProcess(cmd)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			session, err := plan.Start(ctx, process, nil)
			if err != nil {
				t.Fatal(err)
			}
			waitHarnessSession(t, session)
			input, err := os.ReadFile(filepath.Join(root, "received"))
			if err != nil {
				t.Fatal(err)
			}
			if mode == "success" {
				if session.Outcome().Status != StatusCompleted || !strings.Contains(string(input), "unique remote task") {
					t.Fatalf("outcome=%+v input=%s", session.Outcome(), input)
				}
			} else {
				if session.Outcome().Status != StatusFailed || strings.Contains(string(input), "unique remote task") {
					t.Fatalf("mismatch sent prompt: %+v %s", session.Outcome(), input)
				}
			}
			if _, err := plan.Start(ctx, process, nil); err == nil {
				t.Fatal("remote launch replay accepted")
			}
		})
	}
}

func TestRemoteHarnessRejectsResumeBeforeEvidence(t *testing.T) {
	root := t.TempDir()
	runtimeDir := filepath.Join(root, "runtime")
	req := DispatchRequest{Name: "resume", Runtime: "openshell", Provider: "codex", Model: "fixture", Prompt: "task", WorktreePath: root, ResumeSessionID: "previous"}
	if _, err := PrepareRemoteHarness(runtimeDir, skill.Resolver{}, req, "/sandbox/codex", "/sandbox/sessions"); err == nil {
		t.Fatal("resume accepted")
	}
	if _, err := os.Stat(runtimeDir); !os.IsNotExist(err) {
		t.Fatalf("rejected launch wrote evidence: %v", err)
	}
}
