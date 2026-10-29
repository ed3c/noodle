package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenShellDiagnosticFailureRejectsNonterminalOrForeignMetadata(t *testing.T) {
	r, b, _ := diagnosticFailureFixture(t)
	dir := filepath.Join(r.config.RuntimeDir, "sessions", b.SessionID)
	path := filepath.Join(dir, "meta.json")
	original := map[string]any{}
	if _, err := ReadSessionReadbackFile(path, &original); err != nil {
		t.Fatal(err)
	}
	if original["status"] != "failed" || original["alive"] != false {
		t.Fatalf("fixture bypassed formal failed Monitor observation: %+v", original)
	}
	c := r.config.Gateway.custody
	native := &reclamationClient{OpenShellClient: c.client, id: c.Snapshot().Records[0].Worker.Identity.Sandbox.SandboxID}
	c.client = native
	authority := failedDiagnosticAuthority(t, b.OrderID)
	for _, tc := range []struct {
		name  string
		field string
		value any
	}{
		{"alive", "alive", true},
		{"missing-alive", "alive", nil},
		{"exit-pending", "exit_pending", true},
		{"running", "status", "running"},
		{"missing-status", "status", nil},
		{"empty-status", "status", ""},
		{"unknown-status", "status", "unknown"},
		{"completed-is-not-monitor-status", "status", "completed"},
		{"stopped-is-not-monitor-status", "status", "stopped"},
		{"cancelled-is-not-monitor-status", "status", "cancelled"},
		{"session", "session_id", "foreign"},
		{"runtime", "runtime", "process"},
		{"provider", "provider", "pi"},
		{"model", "model", "foreign"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			meta := make(map[string]any, len(original))
			for key, value := range original {
				meta[key] = value
			}
			if tc.value == nil {
				delete(meta, tc.field)
			} else {
				meta[tc.field] = tc.value
			}
			// Mutate only adversarial input; legitimate meta comes from Monitor.
			writeReadbackJSON(t, path, meta)
			if _, err := r.ReclaimCleanupSession(t.Context(), b, b.WorktreePath, StatusFailed, "explicit closure", authority); err == nil {
				t.Fatal("nonterminal/foreign metadata enabled reclamation")
			}
			if native.deletes != 0 {
				t.Fatal("refused metadata reached native Delete")
			}
			if _, err := os.Stat(filepath.Join(dir, "repo")); err != nil {
				t.Fatalf("refused metadata removed diagnostic baseline: %v", err)
			}
		})
	}
}
