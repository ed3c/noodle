package runtime

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/poteto/noodle/monitor"
)

// The fixture fails the real transfer child before StartOwned can persist Exec.
func preexecReadbackFixture(t *testing.T) (*OpenShellRuntime, CompletedSessionBinding, *managedFixture) {
	t.Helper()
	r, f, target := openShellRuntimeFixture(t)
	managedMarker(t, f.root, "completion-observation")
	if err := os.WriteFile(r.config.Profile.CLI, []byte("#!/bin/sh\necho original-upload-failure >&2\nexit 31\n"), 0700); err != nil {
		t.Fatal(err)
	}
	var err error
	r.config.Profile.ClientSHA256, err = openShellClientHash(r.config.Profile)
	if err != nil {
		t.Fatal(err)
	}
	r.config.Profile.InstructionVerifier = "/sandbox/payload/noodle-instructions"
	if err := os.WriteFile(filepath.Join(r.config.Profile.PayloadDirectory, "noodle-instructions"), []byte("fixture verifier"), 0700); err != nil {
		t.Fatal(err)
	}
	r.config.Profile.PayloadSHA256, err = openShellPayloadHash(r.config.Profile.PayloadDirectory)
	if err != nil {
		t.Fatal(err)
	}
	h, err := r.Dispatch(t.Context(), DispatchRequest{Name: "preexec", Runtime: "openshell", Provider: "codex", Model: "fixture", Prompt: "original task", SystemPrompt: "owner", InstructionBundle: runtimeInstructionRef(t), EnvVars: map[string]string{"NOODLE_ORDER_ID": "original-order", "NOODLE_STAGE_INDEX": "0"}, WorktreePath: target, AllowPrimaryCheckout: true})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("pre-exec dispatch did not settle")
	}
	if h.Outcome().Status != StatusFailed || !strings.Contains(h.Outcome().Reason, "upload failed") {
		t.Fatalf("outcome: %+v", h.Outcome())
	}
	t.Cleanup(func() {
		if _, err := f.owner.Release("a"); err != nil {
			t.Error(err)
		}
	})
	b := CompletedSessionBinding{OrderID: "original-order", SessionID: h.ID(), Provider: "codex", Model: "fixture", WorktreePath: target}
	rows, err := monitor.NewMonitor(r.config.RuntimeDir).RunOnce(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || string(rows[0].Status) != "failed" || rows[0].Alive || rows[0].Provider != "" || rows[0].Model != "" || rows[0].Runtime != "" {
		t.Fatalf("fixture did not retain original pre-exec monitor shape: %+v", rows)
	}
	return r, b, f
}

func TestOpenShellPreexecReadbackIsFreshAndReadOnly(t *testing.T) {
	r, b, f := preexecReadbackFixture(t)
	ledger := filepath.Join(f.spec.Binding.StateRoot, "owner.json")
	before, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatal(err)
	}
	events, _ := os.ReadFile(filepath.Join(f.root, "events"))
	got, err := r.ReadSavedProgress(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(got)
	var fields map[string]any
	_ = json.Unmarshal(raw, &fields)
	if got.Status != "not_started" || got.ProgressHead != "" || got.CommandSHA256 != "" || got.Instructions != nil || fields["baseline_head"] == "" || fields["original_error"] == "" || !got.StopAcknowledged {
		t.Fatalf("invented progress or lost failure: %s", raw)
	}
	after, _ := os.ReadFile(ledger)
	afterEvents, _ := os.ReadFile(filepath.Join(f.root, "events"))
	if !bytes.Equal(before, after) || string(afterEvents[len(events):]) != "get\n" {
		t.Fatal("pre-exec readback mutated custody or made non-Get calls")
	}
}

func TestOpenShellPreexecReadbackRejectsUnsettledOrChangedInputs(t *testing.T) {
	for _, kind := range []string{"active", "terminal", "terminal-provider", "terminal-model", "terminal-runtime", "terminal-empty", "terminal-null", "terminal-exit-pending", "terminal-unknown", "profile", "spawn", "mapping", "instructions", "input", "exec-intent", "stop-pending", "dirty-index", "untracked", "head"} {
		t.Run(kind, func(t *testing.T) {
			r, b, f := preexecReadbackFixture(t)
			dir := filepath.Join(r.config.RuntimeDir, "sessions", b.SessionID)
			switch kind {
			case "active":
				r.mu.Lock()
				r.active[b.SessionID] = &openShellSession{}
				r.mu.Unlock()
			case "terminal":
				writeReadbackJSON(t, filepath.Join(dir, "meta.json"), map[string]any{"session_id": b.SessionID, "provider": b.Provider, "model": b.Model, "runtime": "openshell", "status": "exited", "alive": true})
			case "terminal-provider", "terminal-model", "terminal-runtime", "terminal-empty", "terminal-null", "terminal-exit-pending", "terminal-unknown":
				var v map[string]any
				if _, err := ReadSessionReadbackFile(filepath.Join(dir, "meta.json"), &v); err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "terminal-provider":
					v["provider"] = "foreign"
				case "terminal-model":
					v["model"] = "foreign"
				case "terminal-runtime":
					v["runtime"] = "foreign"
				case "terminal-empty":
					v["provider"] = ""
				case "terminal-null":
					v["provider"] = nil
				case "terminal-exit-pending":
					v["exit_pending"] = true
				case "terminal-unknown":
					v["status"] = "unknown"
				}
				writeReadbackJSON(t, filepath.Join(dir, "meta.json"), v)
			case "profile":
				var v openShellDispatchReceipt
				_, _ = ReadSessionReadbackFile(filepath.Join(dir, "openshell-dispatch.json"), &v)
				v.ProfileSHA256 = "foreign"
				writeReadbackJSON(t, filepath.Join(dir, "openshell-dispatch.json"), v)
			case "spawn":
				var v map[string]any
				_, _ = ReadSessionReadbackFile(filepath.Join(dir, "spawn.json"), &v)
				v["retry_count"] = 1
				writeReadbackJSON(t, filepath.Join(dir, "spawn.json"), v)
			case "mapping":
				writeReadbackJSON(t, filepath.Join(dir, "instruction-mapping.json"), map[string]any{})
			case "instructions":
				var v struct {
					InstructionBundle struct {
						SHA256 string `json:"sha256"`
					} `json:"instruction_bundle"`
				}
				_, _ = ReadSessionReadbackFile(filepath.Join(dir, "spawn.json"), &v)
				if err := os.WriteFile(filepath.Join(dir, "instructions-"+v.InstructionBundle.SHA256, "skills/main/SKILL.md"), []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			case "input":
				if err := os.WriteFile(filepath.Join(dir, "input.txt"), []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			case "exec-intent", "stop-pending":
				path := filepath.Join(f.spec.Binding.StateRoot, "owner.json")
				var c OpenShellCustodyState
				_, _ = ReadSessionReadbackFile(path, &c)
				if kind == "exec-intent" {
					c.Records[0].Exec = &OpenShellCustodyExec{AttemptID: "unknown"}
				} else {
					c.Records[0].Worker.StopPending = true
				}
				writeReadbackJSON(t, path, c)
			case "dirty-index", "untracked":
				if err := os.WriteFile(filepath.Join(b.WorktreePath, "unexpected"), []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
				if kind == "dirty-index" {
					cmd := exec.Command("git", "-C", b.WorktreePath, "add", "unexpected")
					if out, err := cmd.CombinedOutput(); err != nil {
						t.Fatalf("git: %s %v", out, err)
					}
				}
			case "head":
				cmd := exec.Command("git", "-C", b.WorktreePath, "commit", "--allow-empty", "-m", "drift")
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("git: %s %v", out, err)
				}
			}
			ledger := filepath.Join(f.spec.Binding.StateRoot, "owner.json")
			before, _ := os.ReadFile(ledger)
			if _, err := r.ReadSavedProgress(t.Context(), b); err == nil {
				t.Fatal("unsettled or changed pre-exec inputs accepted")
			}
			after, _ := os.ReadFile(ledger)
			if !bytes.Equal(before, after) {
				t.Fatal("refusal changed custody")
			}
		})
	}
}

func TestOpenShellPreexecReadbackAcceptsMatchingOptionalMetadata(t *testing.T) {
	r, b, _ := preexecReadbackFixture(t)
	p := filepath.Join(r.config.RuntimeDir, "sessions", b.SessionID, "meta.json")
	for _, status := range []string{"failed", "exited"} {
		writeReadbackJSON(t, p, map[string]any{"session_id": b.SessionID, "status": status, "alive": false, "exit_pending": false, "provider": b.Provider, "model": b.Model, "runtime": "openshell"})
		if got, err := r.ReadSavedProgress(t.Context(), b); err != nil || got.Status != "not_started" {
			t.Fatalf("matching optional metadata rejected: %+v %v", got, err)
		}
	}
}
