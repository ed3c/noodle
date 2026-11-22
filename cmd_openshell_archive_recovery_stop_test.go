package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/poteto/noodle/internal/admission"
)

func TestArchiveRecoveryMachineLeasesUseNativeGenerationOwner(t *testing.T) {
	root := t.TempDir()
	owner := &hostOwnerBinding{
		HostRoot: root, GenerationDir: filepath.Join(root, "g", "original"),
		Current: hostReceipt{HostInstanceID: "original", ContextID: "original-context"},
	}
	state := filepath.Join(root, "capacity.json")
	for _, test := range []struct {
		name, workerOwner, context, errText string
	}{
		{"native ledger", root + "/generation/original", "original-context", ""},
		{"physical socket directory", owner.GenerationDir, "original-context", "leases are incomplete"},
		{"different context", root + "/generation/original", "foreign-context", "worker lease differs"},
	} {
		t.Run(test.name, func(t *testing.T) {
			data, err := json.Marshal(map[string]any{
				"policy": admission.MachineAdmissionPolicy,
				"leases": map[string]any{
					"original-worker": map[string]any{"owner": test.workerOwner, "context_id": test.context,
						"order_id": "original-order", "effects_possible": true},
					"original-resident": map[string]any{"owner": root + "/resident", "effects_possible": true},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(state, data, 0600); err != nil {
				t.Fatal(err)
			}
			err = verifyArchiveRecoveryMachineLeases(state, owner, "original-order")
			if test.errText == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.errText) {
				t.Fatalf("expected native Owner refusal %q, got %v", test.errText, err)
			}
		})
	}
}
