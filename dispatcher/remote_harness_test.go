package dispatcher

import (
	"context"
	"encoding/json"
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
			plan, err := PrepareRemoteHarness(filepath.Join(root, "local"), skill.Resolver{}, req, "/sandbox/payload/pi", guest, "")
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
	if _, err := PrepareRemoteHarness(runtimeDir, skill.Resolver{}, req, "/sandbox/codex", "/sandbox/sessions", ""); err == nil {
		t.Fatal("resume accepted")
	}
	if _, err := os.Stat(runtimeDir); !os.IsNotExist(err) {
		t.Fatalf("rejected launch wrote evidence: %v", err)
	}
}

func TestRemoteHarnessPiStderrFailureKeepsOriginalProcess(t *testing.T) {
	root := t.TempDir()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	req := DispatchRequest{Name: "stderr-failure", Provider: "pi", Runtime: "openshell", Model: "openrouter/deepseek/deepseek-v4.1-flash-1017", Prompt: "unique remote task", WorktreePath: root}
	plan, err := PrepareRemoteHarness(filepath.Join(root, "local"), skill.Resolver{}, req, "/sandbox/payload/pi", filepath.Join(root, "guest"), "")
	if err != nil {
		t.Fatal(err)
	}
	// A directory is a real, consistently unwritable stderr evidence destination,
	// including when this test runs as root.
	plan.stderr = plan.Directory
	args := append([]string{"-test.run=^TestPiRPCChild$", "--"}, plan.Command()[1:]...)
	cmd := exec.Command(binary, args...)
	cmd.Dir, cmd.Env = root, append(os.Environ(), "NOODLE_FAKE_PI=1", "NOODLE_FAKE_PI_MODE=no-read")
	process, err := StartProcess(cmd)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = process.ForceKill(); <-process.Done() })
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	session, err := plan.Start(ctx, process, nil)
	if err != nil || session == nil {
		t.Fatalf("started process lost: session=%v err=%v", session, err)
	}
	waitHarnessSession(t, session)
	if _, exited := process.ExitCode(); !exited {
		t.Fatal("failed stderr did not join original process")
	}
	var receipt piReceipt
	data, err := os.ReadFile(filepath.Join(plan.Directory, "pi-rpc.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &receipt); err != nil {
		t.Fatal(err)
	}
	if session.Outcome().Status != StatusFailed || !strings.Contains(receipt.Error, plan.Directory) || receipt.PromptSent {
		t.Fatalf("stderr failure lost diagnostic or sent task: outcome=%+v receipt=%+v", session.Outcome(), receipt)
	}
}
