package runtime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/poteto/noodle/dispatcher"
)

func completedInstructionFixture(t *testing.T) (*OpenShellRuntime, CompletedSessionBinding, *managedFixture, *dispatcher.InstructionBundleRef) {
	t.Helper()
	r, f, target := openShellRuntimeFixture(t)
	managedMarker(t, f.root, "completion-observation")
	r.config.Profile.InstructionVerifier = "/sandbox/payload/noodle-instructions"
	if err := os.WriteFile(filepath.Join(r.config.Profile.PayloadDirectory, "noodle-instructions"), []byte("fixture verifier"), 0700); err != nil {
		t.Fatal(err)
	}
	var err error
	r.config.Profile.PayloadSHA256, err = openShellPayloadHash(r.config.Profile.PayloadDirectory)
	if err != nil {
		t.Fatal(err)
	}
	ref := runtimeInstructionRef(t)
	h, err := r.Dispatch(t.Context(), DispatchRequest{Name: "instruction-readback", Runtime: "openshell", Provider: "codex", Model: "fixture", Prompt: "task", SystemPrompt: "owner", InstructionBundle: ref, EnvVars: map[string]string{"NOODLE_ORDER_ID": "original-order", "NOODLE_STAGE_INDEX": "0"}, WorktreePath: target, AllowPrimaryCheckout: true})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-h.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("instruction attempt did not settle")
	}
	if h.Outcome().Status != StatusCompleted {
		t.Fatalf("outcome: %+v", h.Outcome())
	}
	t.Cleanup(func() {
		if _, err := f.owner.Release("a"); err != nil {
			t.Error(err)
		}
	})
	writeReadbackJSON(t, filepath.Join(r.config.RuntimeDir, "sessions", h.ID(), "meta.json"), map[string]any{"session_id": h.ID(), "provider": "codex", "model": "fixture", "runtime": "openshell", "status": "exited", "alive": false})
	return r, CompletedSessionBinding{OrderID: "original-order", SessionID: h.ID(), Provider: "codex", Model: "fixture", WorktreePath: target}, f, ref
}

func TestInstructionReadbackBindsTransportedInputsWithoutOriginalSourceRead(t *testing.T) {
	r, b, _, ref := completedInstructionFixture(t)
	if err := os.Remove(ref.Path); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(filepath.Dir(ref.Path), "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	result, err := r.ReadCompletedSession(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	if result.Instructions == nil || result.Instructions.Manifest.SHA256 != ref.SHA256 || len(result.Sources) != 13 || result.Sources["instruction_manifest"] != result.Instructions.Manifest {
		t.Fatal("completion lost the transported manifest or bounded input evidence")
	}
	assertOriginalReadbackArchives(t, result.Sources)
}

func TestInstructionReadbackRejectsChangedInputsAndNativeCommand(t *testing.T) {
	for _, kind := range []string{"file", "mode", "native-command", "mapping", "missing-binding", "downgraded-command"} {
		t.Run(kind, func(t *testing.T) {
			r, b, f, ref := completedInstructionFixture(t)
			dir := filepath.Join(r.config.RuntimeDir, "sessions", b.SessionID)
			file := filepath.Join(dir, "instructions-"+ref.SHA256, "skills", "main", "SKILL.md")
			switch kind {
			case "file":
				if err := os.WriteFile(file, []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err := os.Chmod(file, 0700); err != nil {
					t.Fatal(err)
				}
			case "missing-binding", "downgraded-command":
				var spawn map[string]any
				if _, err := ReadSessionReadbackFile(filepath.Join(dir, "spawn.json"), &spawn); err != nil {
					t.Fatal(err)
				}
				delete(spawn, "instruction_bundle")
				writeReadbackJSON(t, filepath.Join(dir, "spawn.json"), spawn)
				if err := os.Remove(filepath.Join(dir, "instruction-mapping.json")); err != nil {
					t.Fatal(err)
				}
				if kind == "missing-binding" {
					break
				}
				fallthrough
			case "native-command":
				name := filepath.Join(f.spec.Binding.StateRoot, "owner.json")
				before, err := os.ReadFile(name)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.WriteFile(name, before, 0600); err != nil {
						t.Error(err)
					}
				})
				var custody OpenShellCustodyState
				if _, err := ReadSessionReadbackFile(name, &custody); err != nil {
					t.Fatal(err)
				}
				if kind == "downgraded-command" {
					custody.Records[0].Exec.Command.Argv = custody.Records[0].Exec.Command.Argv[8:]
				} else {
					custody.Records[0].Exec.Command.Argv[0] = "/sandbox/payload/codex"
				}
				writeReadbackJSON(t, name, custody)
			case "mapping":
				writeReadbackJSON(t, filepath.Join(dir, "instruction-mapping.json"), map[string]any{})
			}
			if _, err := r.ReadCompletedSession(t.Context(), b); err == nil {
				t.Fatal("unbound or changed instructions reported observed")
			}
		})
	}
}

func TestInstructionReadbackRechecksFilesAfterNativeObservation(t *testing.T) {
	r, b, _, ref := completedInstructionFixture(t)
	_, err := r.readCompletedSession(t.Context(), b, func(ctx context.Context, identity OpenShellIdentity) (OpenShellCompletedObservation, error) {
		observed, err := r.config.Gateway.ObserveCompleted(ctx, r.config.ContextID, identity)
		if err != nil {
			return observed, err
		}
		file := filepath.Join(r.config.RuntimeDir, "sessions", b.SessionID, "instructions-"+ref.SHA256, "skills", "main", "SKILL.md")
		return observed, os.WriteFile(file, []byte("changed during native readback"), 0600)
	})
	if err == nil {
		t.Fatal("instruction change during native observation accepted")
	}
}
