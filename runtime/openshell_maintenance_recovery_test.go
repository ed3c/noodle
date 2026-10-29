package runtime

import (
	"os"
	"path/filepath"
	"testing"
)

func TestProcessRecoveryLeavesVerifiedMaintainedSessionToNativeOwner(t *testing.T) {
	r, f, p, plan := maintenanceFixture(t)
	if _, err := StopProgressMaintenance(t.Context(), plan, r.config.Profile); err != nil {
		t.Fatal(err)
	}
	if err := f.owner.custody.closeLocal(); err != nil {
		t.Fatal(err)
	}
	if _, err := RestoreProgressMaintenance(t.Context(), plan, r.config.Profile, func(complete SessionReadbackFileRef) error {
		return maintenanceHostFixture(t, r, p, plan, complete)
	}); err != nil {
		t.Fatal(err)
	}
	metaPath := filepath.Join(p.SessionDirectory, "meta.json")
	before, err := os.ReadFile(metaPath)
	if err != nil {
		t.Fatal(err)
	}
	process := NewProcessRuntime(nil, r.config.RuntimeDir, 1)
	if recovered, err := process.Recover(t.Context()); err != nil || len(recovered) != 0 {
		t.Fatalf("process runtime claimed verified native history: %v %v", recovered, err)
	}
	after, err := os.ReadFile(metaPath)
	if err != nil || string(before) != string(after) {
		t.Fatal("process discovery changed native terminal metadata")
	}
	for _, kind := range []string{"process-claim", "incomplete-handoff", "foreign-session"} {
		t.Run(kind, func(t *testing.T) {
			var file string
			switch kind {
			case "process-claim":
				file = filepath.Join(p.SessionDirectory, "process.json")
			case "incomplete-handoff":
				file = filepath.Join(filepath.Dir(plan.Path), "host-handoff.json")
			case "foreign-session":
				file = filepath.Join(p.SessionDirectory, "spawn.json")
			}
			original, err := os.ReadFile(file)
			missing := os.IsNotExist(err)
			if err != nil && !missing {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if missing {
					if err := os.Remove(file); err != nil {
						t.Error(err)
					}
				} else if err := os.WriteFile(file, original, 0600); err != nil {
					t.Error(err)
				}
			})
			if err := os.WriteFile(file, []byte(`{"session_id":"foreign"}`), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := process.Recover(t.Context()); err == nil {
				t.Fatal("ambiguous or unverified native history bypassed process recovery")
			}
		})
	}
}
