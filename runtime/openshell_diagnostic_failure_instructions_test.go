package runtime

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/poteto/noodle/dispatcher"
)

func TestOpenShellDiagnosticFailurePreservesLocallyPreparedInstructions(t *testing.T) {
	r, b, _ := diagnosticFailureInstructionFixture(t, true)
	dir := filepath.Join(r.config.RuntimeDir, "sessions", b.SessionID)
	var spawn struct {
		InstructionBundle *dispatcher.InstructionBundleRef `json:"instruction_bundle"`
	}
	if _, err := ReadSessionReadbackFile(filepath.Join(dir, "spawn.json"), &spawn); err != nil {
		t.Fatal(err)
	}
	// The original source is provenance only. Use the locally prepared bundle.
	if err := os.RemoveAll(filepath.Dir(spawn.InstructionBundle.Path)); err != nil {
		t.Fatal(err)
	}
	before, err := r.ReadCleanupSession(t.Context(), b, b.WorktreePath, StatusFailed)
	if err != nil {
		t.Fatal(err)
	}
	if before.Diagnostic == nil || before.Diagnostic.Instructions == nil || before.Diagnostic.Instructions.Basis != "locally_prepared_upload_unproven" || before.Execution.Instructions != nil || before.Execution.CommandSHA256 != "" {
		t.Fatalf("prepared input represented as executed/uploaded: %+v", before)
	}
	c := r.config.Gateway.custody
	native := &reclamationClient{OpenShellClient: c.client, id: c.Snapshot().Records[0].Worker.Identity.Sandbox.SandboxID}
	c.client = native
	got, err := r.ReclaimCleanupSession(t.Context(), b, b.WorktreePath, StatusFailed, "original owner closed pre-upload failure", failedDiagnosticAuthority(t, b.OrderID))
	if err != nil {
		t.Fatal(err)
	}
	if got.Diagnostic.Instructions.Manifest != before.Diagnostic.Instructions.Manifest {
		t.Fatal("instruction evidence changed during closure")
	}
	kept := map[string]SessionReadbackFileRef{}
	for name, ref := range before.Execution.Sources {
		if name != "native_custody" {
			kept[name] = ref
		}
	}
	if err := VerifySessionReadbackFiles(kept); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(got.Diagnostic.Instructions.Directory, "skills", "main", "SKILL.md"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ReadCleanupSession(t.Context(), b, b.WorktreePath, StatusFailed); err == nil {
		t.Fatal("changed retained instruction mode accepted")
	}
	if native.deletes != 1 {
		t.Fatal("instruction readback replayed retirement")
	}
}

func TestOpenShellDiagnosticFailureRejectsIncompletePreparedInstructions(t *testing.T) {
	for _, kind := range []string{"file", "mode", "mapping", "spawn-binding"} {
		t.Run(kind, func(t *testing.T) {
			r, b, _ := diagnosticFailureInstructionFixture(t, true)
			dir := filepath.Join(r.config.RuntimeDir, "sessions", b.SessionID)
			var spawn map[string]any
			if _, err := ReadSessionReadbackFile(filepath.Join(dir, "spawn.json"), &spawn); err != nil {
				t.Fatal(err)
			}
			ref := spawn["instruction_bundle"].(map[string]any)
			file := filepath.Join(dir, "instructions-"+ref["sha256"].(string), "skills", "main", "SKILL.md")
			switch kind {
			case "file":
				if err := os.WriteFile(file, []byte("changed"), 0600); err != nil {
					t.Fatal(err)
				}
			case "mode":
				if err := os.Chmod(file, 0700); err != nil {
					t.Fatal(err)
				}
			case "mapping":
				writeReadbackJSON(t, filepath.Join(dir, "instruction-mapping.json"), map[string]any{})
			case "spawn-binding":
				delete(spawn, "instruction_bundle")
				writeReadbackJSON(t, filepath.Join(dir, "spawn.json"), spawn)
			}
			if _, err := r.ReadCleanupSession(t.Context(), b, b.WorktreePath, StatusFailed); err == nil {
				t.Fatal("incomplete local instruction evidence accepted")
			}
		})
	}
}
