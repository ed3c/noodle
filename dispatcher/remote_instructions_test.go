package dispatcher

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/poteto/noodle/skill"
)

func remoteInstructionFixture(t *testing.T) (string, *InstructionBundleRef) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "SKILL.md")
	body := []byte("Read the complete selected skill.")
	if err := os.WriteFile(file, body, 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(body)
	digest := hex.EncodeToString(hash[:])
	data, err := json.Marshal(map[string]any{"schema_version": 1, "order_id": "original-order", "files": []any{map[string]any{"source": map[string]string{"path": file, "sha256": digest}, "original_source": map[string]string{"path": "/original/selected/SKILL.md", "sha256": digest}, "destination": "skills/selected/SKILL.md", "mode": uint32(info.Mode().Perm())}}, "bindings": []any{map[string]string{"kind": "selected_skill", "name": "selected", "path": "skills/selected/SKILL.md"}}})
	if err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(root, "bundle.json")
	if err := os.WriteFile(manifest, data, 0600); err != nil {
		t.Fatal(err)
	}
	hash = sha256.Sum256(data)
	return root, &InstructionBundleRef{Path: manifest, SHA256: hex.EncodeToString(hash[:])}
}

func TestRemoteHarnessMaterializesBothProvidersWithoutRewritingTask(t *testing.T) {
	for _, provider := range []string{"codex", "pi"} {
		t.Run(provider, func(t *testing.T) {
			root, ref := remoteInstructionFixture(t)
			model := "fixture"
			if provider == "pi" {
				model = "openrouter/deepseek/deepseek-v4.1-flash-1017"
			}
			req := DispatchRequest{Name: "remote", Runtime: "openshell", Provider: provider, Model: model, Prompt: "original canonical task", SystemPrompt: "frozen owner wrapper", WorktreePath: root, ReasoningLevel: "high", InstructionBundle: ref, EnvVars: map[string]string{"NOODLE_ORDER_ID": "original-order"}}
			plan, err := PrepareRemoteHarness(filepath.Join(root, "runtime"), skill.Resolver{}, req, "/sandbox/payload/"+provider, "/sandbox/sessions", "/sandbox/payload/noodle-instructions")
			if err != nil {
				t.Fatal(err)
			}
			if plan.Instructions == nil || plan.Command()[0] != "/sandbox/payload/noodle-instructions" {
				t.Fatal("verified launch missing")
			}
			argv := strings.Join(plan.Command(), " ")
			if !strings.Contains(argv, "-- /sandbox/payload/"+provider) || !strings.Contains(argv, "--order-id original-order") {
				t.Fatal("harness or order identity missing")
			}
			canonical, err := os.ReadFile(filepath.Join(plan.Directory, "prompt.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if string(canonical) != req.Prompt {
				t.Fatal("canonical prompt rewritten")
			}
			input, err := os.ReadFile(filepath.Join(plan.Directory, "input.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(input), "# Materialized task instructions") || !strings.Contains(string(input), plan.Instructions.GuestDirectory+"/skills/selected/SKILL.md") {
				t.Fatal("actual harness input lacks guest mapping")
			}
			var metadata dispatchMetadata
			if err := decodeJSONFile(filepath.Join(plan.Directory, "spawn.json"), &metadata); err != nil {
				t.Fatal(err)
			}
			if metadata.ReasoningLevel != "high" || metadata.InstructionBundle == nil || *metadata.InstructionBundle != *ref {
				t.Fatal("dispatch provenance lost")
			}
			if _, err := os.Stat(filepath.Join(plan.Directory, "instruction-mapping.json")); err != nil {
				t.Fatal(err)
			}
			if plan.pi != nil && !strings.Contains(plan.pi.prompt, "# Materialized task instructions") {
				t.Fatal("Pi received canonical rather than derived input")
			}
		})
	}
}

func TestRemoteHarnessInstructionBundleRequiresVerifierBeforeEvidence(t *testing.T) {
	root, ref := remoteInstructionFixture(t)
	runtimeDir := filepath.Join(root, "runtime")
	req := DispatchRequest{Name: "remote", Runtime: "openshell", Provider: "codex", Model: "fixture", Prompt: "task", SystemPrompt: "fixture", WorktreePath: root, InstructionBundle: ref, EnvVars: map[string]string{"NOODLE_ORDER_ID": "original-order"}}
	if _, err := PrepareRemoteHarness(runtimeDir, skill.Resolver{}, req, "/sandbox/payload/codex", "/sandbox/sessions", ""); err == nil {
		t.Fatal("manifest accepted without verifier")
	}
	if _, err := os.Stat(runtimeDir); !os.IsNotExist(err) {
		t.Fatal("refused launch wrote session evidence")
	}
}
