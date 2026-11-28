package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/poteto/noodle/internal/admission"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/poteto/noodle/worktree"
)

func TestRejectedReviewPreparationRequiresExactOriginalControlAndStorageZero(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(root, "controls", "original", "checkout")
	if err := os.MkdirAll(project, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(path string, value any) loopruntime.SessionReadbackFileRef {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		ref, err := loopruntime.ReadSessionReadbackFile(path, nil)
		if err != nil {
			t.Fatal(err)
		}
		return ref
	}
	nonce := admission.PreparationRef{ID: "original-lease", SHA256: strings.Repeat("a", 64)}
	request := write(filepath.Join(root, "request.json"), hostRequestBinding{Schema: 1, Operation: "attach", RequestID: "original-request", ProjectDir: project, HostDir: root, Status: "accepted"})
	control := write(filepath.Join(filepath.Dir(project), "control.json"), worktree.TaskControl{Schema: 2, Status: "prepared", ControlRoot: project, Preparation: nonce})
	intent := write(filepath.Join(root, "intent.json"), map[string]bool{"original": true})
	zero := write(intent.Path+".storage-zero", map[string]any{"schema": 1, "intent": intent, "source_only": false})
	source := rejectedReviewTaskPreparationSource{Request: request, Control: control, SourceZero: zero,
		Lease: admission.MachineSessionLease{ID: nonce.ID, Binding: &admission.MachineSessionBinding{ContextID: nonce.ID, ProjectDir: project}}}
	if _, _, err := readRejectedReviewTaskPreparationSource(t.Context(), root, source); err == nil || !strings.Contains(err.Error(), "ZERO receipt differs") {
		t.Fatalf("a receipt without verified storage ZERO was accepted: %v", err)
	}
	source.Lease.ID = "different-lease"
	if _, _, err := readRejectedReviewTaskPreparationSource(t.Context(), root, source); err == nil || !strings.Contains(err.Error(), "checkout or preparation differs") {
		t.Fatalf("a foreign original nonce was accepted: %v", err)
	}
}
