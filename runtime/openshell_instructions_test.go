package runtime

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"github.com/poteto/noodle/dispatcher"
)

func runtimeInstructionRef(t *testing.T) *dispatcher.InstructionBundleRef {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "SKILL.md")
	body := []byte("selected instructions")
	if err := os.WriteFile(file, body, 0600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(body)
	hash := hex.EncodeToString(sum[:])
	b, err := json.Marshal(map[string]any{"schema_version": 1, "order_id": "original-order", "files": []any{map[string]any{"source": map[string]string{"path": file, "sha256": hash}, "original_source": map[string]string{"path": "/unavailable/SKILL.md", "sha256": hash}, "destination": "skills/main/SKILL.md", "mode": 0600}}, "bindings": []any{map[string]string{"kind": "selected_skill", "name": "main", "path": "skills/main/SKILL.md"}}})
	if err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(root, "bundle.json")
	if err := os.WriteFile(manifest, b, 0600); err != nil {
		t.Fatal(err)
	}
	sum = sha256.Sum256(b)
	return &dispatcher.InstructionBundleRef{Path: manifest, SHA256: hex.EncodeToString(sum[:])}
}

func TestOpenShellInstructionsRequireVerifierBeforeNativeOrWorkspace(t *testing.T) {
	r := &OpenShellRuntime{config: OpenShellRuntimeConfig{Profile: OpenShellProfile{Routes: map[string]OpenShellRoute{"codex": {Spec: &pb.SandboxSpec{}}}}}}
	_, err := r.Dispatch(t.Context(), DispatchRequest{Provider: "codex", InstructionBundle: &dispatcher.InstructionBundleRef{Path: "/unread/manifest", SHA256: strings.Repeat("a", 64)}})
	if err == nil || !strings.Contains(err.Error(), "no configured guest verifier") {
		t.Fatalf("missing verifier did not refuse before gateway/workspace access: %v", err)
	}
}

func TestOpenShellInstructionsUploadAndWrapOriginalExecution(t *testing.T) {
	r, f, target := openShellRuntimeFixture(t)
	r.config.Profile.InstructionVerifier = "/sandbox/payload/noodle-instructions"
	if err := os.WriteFile(filepath.Join(r.config.Profile.PayloadDirectory, "noodle-instructions"), []byte("fixture verifier executable"), 0700); err != nil {
		t.Fatal(err)
	}
	var err error
	r.config.Profile.PayloadSHA256, err = openShellPayloadHash(r.config.Profile.PayloadDirectory)
	if err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(filepath.Dir(r.config.RuntimeDir), "uploads")
	r.config.Profile.Environment = append(r.config.Profile.Environment, "NOODLE_TRANSFER_LOG="+log)
	ref := runtimeInstructionRef(t)
	h, err := r.Dispatch(t.Context(), DispatchRequest{Name: "instructions", Runtime: "openshell", Provider: "codex", Model: "fixture", Prompt: "task", SystemPrompt: "owner", InstructionBundle: ref, EnvVars: map[string]string{"NOODLE_ORDER_ID": "original-order"}, WorktreePath: target, AllowPrimaryCheckout: true})
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
	b, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Fields(string(b))
	if len(lines) != 3 || filepath.Base(lines[2]) != "instructions-"+ref.SHA256 {
		t.Fatalf("materialized inputs were not uploaded separately: %q", b)
	}
	s := h.(*openShellSession)
	state := s.custody.Snapshot()
	if len(state.Records) != 1 || state.Records[0].Exec == nil {
		t.Fatal("native execution evidence missing")
	}
	argv := state.Records[0].Exec.Command.Argv
	if len(argv) < 9 || argv[0] != "/sandbox/payload/noodle-instructions" || argv[7] != "--" || argv[8] != "/sandbox/payload/codex" {
		t.Fatalf("native execution did not retain verifier and original harness: %q", argv)
	}
	if s.worker.Record().Boundary != OpenShellBoundaryStopped || s.receipt.CandidateHead == "" {
		t.Fatal("instruction route bypassed stop/publication lifecycle")
	}
	if _, err := os.Stat(filepath.Join(target, "instructions-"+ref.SHA256)); !os.IsNotExist(err) {
		t.Fatal("instructions entered returned workspace")
	}
	if _, err := f.owner.Release("a"); err != nil {
		t.Fatal(err)
	}
}

func TestOpenShellInstructionsRefuseFrozenDriftBeforeGatewaySpawn(t *testing.T) {
	r, f, target := openShellRuntimeFixture(t)
	r.config.Profile.InstructionVerifier = "/sandbox/payload/noodle-instructions"
	ref := runtimeInstructionRef(t)
	if err := os.WriteFile(ref.Path, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	_, err := r.Dispatch(t.Context(), DispatchRequest{Name: "instructions", Runtime: "openshell", Provider: "codex", Model: "fixture", Prompt: "task", SystemPrompt: "owner", InstructionBundle: ref, EnvVars: map[string]string{"NOODLE_ORDER_ID": "original-order"}, WorktreePath: target, AllowPrimaryCheckout: true})
	if err == nil {
		t.Fatal("manifest drift accepted")
	}
	f.spawns(t, 0)
}

func TestOpenShellProfileBindsInstructionVerifierToPayload(t *testing.T) {
	for _, kind := range []string{"valid", "outside", "missing", "nonexecutable", "symlink"} {
		t.Run(kind, func(t *testing.T) {
			filename, profile := profileFixture(t)
			root := filepath.Dir(filename)
			binary := filepath.Join(root, "payload", "noodle-instructions")
			if err := os.WriteFile(binary, []byte("verifier"), 0700); err != nil {
				t.Fatal(err)
			}
			profile.InstructionVerifier = "/sandbox/payload/noodle-instructions"
			switch kind {
			case "outside":
				profile.InstructionVerifier = "/usr/bin/env"
			case "missing":
				profile.InstructionVerifier = "/sandbox/payload/missing"
			case "nonexecutable":
				if err := os.Chmod(binary, 0600); err != nil {
					t.Fatal(err)
				}
			case "symlink":
				if err := os.Remove(binary); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink("codex", binary); err != nil {
					t.Fatal(err)
				}
			}
			writeProfileFixture(t, filename, profile)
			p, err := LoadOpenShellProfile(filename)
			if kind != "valid" {
				if err == nil {
					t.Fatal("invalid instruction verifier accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if p.InstructionVerifier != profile.InstructionVerifier {
				t.Fatal("verifier binding lost")
			}
			if err := os.WriteFile(binary, []byte("changed verifier"), 0700); err != nil {
				t.Fatal(err)
			}
			if VerifyOpenShellPayload(p) == nil {
				t.Fatal("verifier bytes not bound by payload fingerprint")
			}
		})
	}
}
