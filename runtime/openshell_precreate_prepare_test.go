package runtime

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/poteto/noodle/dispatcher"
	"github.com/poteto/noodle/internal/workspaceartifacts"
	"github.com/poteto/noodle/skill"
)

func sourceAuthPreparedFixture(t *testing.T) (string, string, string, *dispatcher.RemoteHarness) {
	t.Helper()
	original, template, _, _ := sourceAuthFixture(t)
	root, err := filepath.EvalSymlinks(filepath.Dir(original))
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{original, template} {
		data, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		var document openShellProfileFile
		if err = json.Unmarshal(data, &document); err != nil {
			t.Fatal(err)
		}
		document.InstructionVerifier = "/sandbox/payload/noodle-instructions"
		writeProfileFixture(t, source, document)
	}
	if err := os.WriteFile(filepath.Join(root, "payload", "noodle-instructions"), []byte("original guest verifier"), 0700); err != nil {
		t.Fatal(err)
	}
	worktree := filepath.Join(root, "worktree")
	if err := os.Mkdir(worktree, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(worktree, "source.txt"), []byte("original captured source"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "--quiet", "--template="}, {"add", "source.txt"}, {"-c", "user.name=fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-m", "baseline"}} {
		c := exec.Command("git", args...)
		c.Dir = worktree
		if raw, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, raw)
		}
	}
	ref := runtimeInstructionRef(t)
	req := dispatcher.DispatchRequest{Name: "original-precreate", Runtime: "openshell", Provider: "codex", Model: "original-model", ReasoningLevel: "high", Prompt: "original canonical task", SystemPrompt: "original wrapper", WorktreePath: worktree, InstructionBundle: ref, EnvVars: map[string]string{"NOODLE_ORDER_ID": "original-order", "NOODLE_STAGE_INDEX": "0"}}
	p, err := dispatcher.PrepareRemoteHarness(filepath.Join(root, "runtime"), skill.Resolver{}, req, "/sandbox/payload/codex", "/sandbox/noodle-sessions", "/sandbox/payload/noodle-instructions")
	if err != nil {
		t.Fatal(err)
	}
	baseline, err := workspaceartifacts.Capture(t.Context(), worktree, filepath.Join(p.Directory, "repo"))
	if err != nil {
		t.Fatal(err)
	}
	profile, err := LoadOpenShellProfile(original)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := OpenShellDispatchSHA256(profile)
	if err != nil {
		t.Fatal(err)
	}
	dispatch := filepath.Join(p.Directory, "openshell-dispatch.json")
	writeReadbackJSON(t, dispatch, openShellDispatchReceipt{Version: 1, ContextID: "context", OrderID: "original-order", SessionID: p.ID, AttemptID: "original-native-attempt", ProfileSHA256: hash, Target: worktree, Phase: "credential_waiting", Baseline: baseline, Outcome: SessionOutcome{Status: StatusFailed, Reason: "OpenShell attempt has not completed", ExitCode: -1}})
	// Original materialization remains valid after today's source is unavailable.
	if err := os.RemoveAll(filepath.Dir(ref.Path)); err != nil {
		t.Fatal(err)
	}
	return original, template, dispatch, p
}

func TestSourceAuthPlanVerifiesSavedPreCreateInputClosure(t *testing.T) {
	original, template, dispatch, p := sourceAuthPreparedFixture(t)
	plan, err := InspectSourceAuthPlan(original, template, dispatch, "context", filepath.Dir(original))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Saved *struct {
			Status     string `json:"status"`
			ArgvSHA256 string `json:"argv_sha256"`
		} `json:"saved_input_closure"`
	}
	if err = json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	if result.Saved == nil || result.Saved.Status != "verified" || result.Saved.ArgvSHA256 == "" {
		t.Fatalf("original plan did not verify saved harness closure: %+v", result.Saved)
	}
	if plan.Status != "unknown" || plan.Executable || len(plan.Effects) != 0 || plan.SessionID != p.ID {
		t.Fatal("prepared closure changed original plan authority")
	}
}

func TestSourceAuthSavedClosureRejectsMissingOrContradictoryInputs(t *testing.T) {
	for _, fault := range []string{"missing-input", "session", "order", "worktree", "invalid-effort", "input", "prompt", "startup-size", "startup-write", "mapping", "instruction-mode", "snapshot-bytes", "snapshot-mode", "snapshot-extra", "snapshot-escape", "creating"} {
		t.Run(fault, func(t *testing.T) {
			original, template, dispatch, p := sourceAuthPreparedFixture(t)
			mutate := func(name string, change func(map[string]any)) {
				raw, err := os.ReadFile(filepath.Join(p.Directory, name))
				if err != nil {
					t.Fatal(err)
				}
				var document map[string]any
				if err = json.Unmarshal(raw, &document); err != nil {
					t.Fatal(err)
				}
				change(document)
				writeReadbackJSON(t, filepath.Join(p.Directory, name), document)
			}
			switch fault {
			case "missing-input":
				if err := os.Remove(filepath.Join(p.Directory, "input.txt")); err != nil {
					t.Fatal(err)
				}
			case "session", "order", "worktree", "invalid-effort":
				mutate("spawn.json", func(v map[string]any) {
					key := map[string]string{"session": "session_id", "order": "order_id", "worktree": "worktree_path", "invalid-effort": "reasoning_level"}[fault]
					v[key] = "foreign"
				})
			case "input", "prompt":
				name := fault + ".txt"
				raw, err := os.ReadFile(filepath.Join(p.Directory, name))
				if err != nil {
					t.Fatal(err)
				}
				raw[0] = 'X'
				if err = os.WriteFile(filepath.Join(p.Directory, name), raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "startup-size":
				mutate("codex-startup.json", func(v map[string]any) { v["stdin"].(map[string]any)["expected_bytes"] = 1 })
			case "startup-write":
				mutate("codex-startup.json", func(v map[string]any) {
					v["stdin"].(map[string]any)["write_started_at"] = time.Now().UTC().Format(time.RFC3339Nano)
				})
			case "mapping":
				mutate("instruction-mapping.json", func(v map[string]any) { v["prepared"].(map[string]any)["guest_directory"] = "/sandbox/foreign" })
			case "instruction-mode":
				if err := os.Chmod(filepath.Join(p.Instructions.Directory, "skills", "main", "SKILL.md"), 0700); err != nil {
					t.Fatal(err)
				}
			case "snapshot-bytes":
				if err := os.WriteFile(filepath.Join(p.Directory, "repo", "source.txt"), []byte("changed captured source"), 0600); err != nil {
					t.Fatal(err)
				}
			case "snapshot-mode":
				if err := os.Chmod(filepath.Join(p.Directory, "repo", "source.txt"), 0700); err != nil {
					t.Fatal(err)
				}
			case "snapshot-extra":
				if err := os.WriteFile(filepath.Join(p.Directory, "repo", "extra.txt"), []byte("unexpected"), 0600); err != nil {
					t.Fatal(err)
				}
			case "snapshot-escape":
				mutate("openshell-dispatch.json", func(v map[string]any) { v["Baseline"].(map[string]any)["directory"] = t.TempDir() })
			case "creating":
				mutate("openshell-dispatch.json", func(v map[string]any) { v["Phase"] = "creating" })
			}
			plan, err := InspectSourceAuthPlan(original, template, dispatch, "context", filepath.Dir(original))
			if err != nil {
				t.Fatal(err)
			}
			if plan.SavedInputClosure == nil || plan.SavedInputClosure.Status == "verified" || plan.SavedInputClosure.Error == "" {
				t.Fatalf("accepted %s: %+v", fault, plan.SavedInputClosure)
			}
			if fault == "missing-input" && plan.SavedInputClosure.Status != "missing" {
				t.Fatal("missing file lost typed missing outcome")
			}
			if plan.Executable || len(plan.Effects) != 0 || plan.Status != "unknown" {
				t.Fatal("closure refusal changed original auth-only authority")
			}
		})
	}
}

func TestSourceAuthSavedClosureRetainsBytesAndBindsSavedModelEffort(t *testing.T) {
	original, template, dispatch, p := sourceAuthPreparedFixture(t)
	loaded, err := LoadOpenShellProfile(original)
	if err != nil {
		t.Fatal(err)
	}
	files := []string{original, template, dispatch, filepath.Join(loaded.Gateway.Binding.StateRoot, "owner.json"), filepath.Join(p.Directory, "spawn.json"), filepath.Join(p.Directory, "codex-startup.json"), filepath.Join(p.Directory, "input.txt"), filepath.Join(p.Directory, "prompt.txt"), filepath.Join(p.Directory, "instruction-mapping.json"), filepath.Join(p.Directory, "repo", "source.txt")}
	var states []struct {
		path string
		data []byte
		info os.FileInfo
	}
	for _, file := range files {
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(file)
		if err != nil {
			t.Fatal(err)
		}
		states = append(states, struct {
			path string
			data []byte
			info os.FileInfo
		}{file, data, info})
	}
	plan, err := InspectSourceAuthPlan(original, template, dispatch, "context", filepath.Dir(original))
	if err != nil {
		t.Fatal(err)
	}
	saved := plan.SavedInputClosure
	if saved == nil || saved.Status != "verified" || saved.Model != "original-model" || saved.ReasoningLevel != "high" || saved.GuestSessionDirectory != "/sandbox/noodle-sessions/"+p.ID {
		t.Fatalf("original saved carrier lost: %+v", saved)
	}
	expected := []string{"/sandbox/payload/noodle-instructions", "--directory", p.Instructions.GuestDirectory, "--sha256", p.Instructions.ManifestSHA256, "--order-id", "original-order", "--", "/sandbox/payload/codex", "exec", "--skip-git-repo-check", "--json", "--model", "original-model", "--dangerously-bypass-approvals-and-sandbox", "-c", `model_reasoning_effort="high"`, "-"}
	command, err := json.Marshal(expected)
	if err != nil {
		t.Fatal(err)
	}
	commandSum := sha256.Sum256(command)
	if saved.ArgvSHA256 != hex.EncodeToString(commandSum[:]) {
		t.Fatal("original model/effort/instruction argv was not preserved")
	}
	if saved.BaselineEntries != 1 || saved.BaselineHead == "" {
		t.Fatal("captured original repository evidence missing")
	}
	encoded, err := json.Marshal(saved)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"original canonical task", "original wrapper", "PRIVATE KEY", "SealedPayload", "Argv"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatal("prepared summary exposed raw inputs", secret)
		}
	}
	for _, before := range states {
		after, err := os.ReadFile(before.path)
		if err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(before.path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before.data, after) || !os.SameFile(before.info, info) || before.info.Mode() != info.Mode() || !before.info.ModTime().Equal(info.ModTime()) {
			t.Fatal("pure prepare wrote original input", before.path)
		}
	}
	// Saved producer values are source, not guessed current defaults. Once pinned,
	// changing model/effort invalidates that original plan's file fence.
	for _, name := range []string{"model", "reasoning_level"} {
		data, err := os.ReadFile(filepath.Join(p.Directory, "spawn.json"))
		if err != nil {
			t.Fatal(err)
		}
		var spawn map[string]any
		if err = json.Unmarshal(data, &spawn); err != nil {
			t.Fatal(err)
		}
		spawn[name] = "different"
		writeReadbackJSON(t, filepath.Join(p.Directory, "spawn.json"), spawn)
		if err = VerifySessionReadbackFiles(plan.SavedInputClosure.Sources); err == nil {
			t.Fatal(fmt.Sprintf("stale saved %s remained bound", name))
		}
	}
}

func TestSourceAuthSavedClosureDriftDoesNotAddAuthProposalGate(t *testing.T) {
	for _, change := range []string{"changed-prepared", "missing-prepared", "changed-core"} {
		t.Run(change, func(t *testing.T) {
			original, template, dispatch, p := sourceAuthPreparedFixture(t)
			plan, err := InspectSourceAuthPlan(original, template, dispatch, "context", filepath.Dir(original))
			if err != nil {
				t.Fatal(err)
			}
			if plan.SavedInputClosure.Status != "verified" {
				t.Fatal("fixture not prepared")
			}
			target := filepath.Join(p.Directory, "input.txt")
			switch change {
			case "changed-prepared":
				raw, err := os.ReadFile(target)
				if err != nil {
					t.Fatal(err)
				}
				raw[0] = 'X'
				if err = os.WriteFile(target, raw, 0600); err != nil {
					t.Fatal(err)
				}
			case "missing-prepared":
				if err = os.Remove(target); err != nil {
					t.Fatal(err)
				}
			case "changed-core":
				target = plan.Sources["custody"].Path
				raw, err := os.ReadFile(target)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(target, append(raw, ' '), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if change != "changed-core" {
				// The old coupled fence would wrongly reject the auth proposal.
				coupled := map[string]SessionReadbackFileRef{}
				for role, ref := range plan.Sources {
					coupled[role] = ref
				}
				for role, ref := range plan.SavedInputClosure.Sources {
					coupled["saved:"+role] = ref
				}
				if err := VerifySessionReadbackFiles(coupled); err == nil {
					t.Fatal("negative control did not detect prepared-only drift")
				}
			}
			err = verifySourceAuthPlanSources(&plan)
			if change == "changed-core" {
				if err == nil {
					t.Fatal("original core source drift was weakened")
				}
				return
			}
			if err != nil {
				t.Fatal("prepared-only drift added auth proposal gate", err)
			}
			want := "refused"
			if change == "missing-prepared" {
				want = "missing"
			}
			if plan.SavedInputClosure.Status != want || plan.Status != "unknown" || plan.Executable || len(plan.Effects) != 0 {
				t.Fatal("typed evidence/authority boundary changed")
			}
		})
	}
}
