package runtime

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/poteto/noodle/internal/workspaceartifacts"
)

func failedMaintenanceTransferFixture(t *testing.T) (*OpenShellRuntime, *managedFixture, ProgressMaintenance, SessionReadbackFileRef, SessionReadbackFileRef, SessionReadbackFileRef) {
	t.Helper()
	r, f, p, plan := maintenanceFixture(t)
	stop, err := StopProgressMaintenance(t.Context(), plan, r.config.Profile)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.owner.custody.closeLocal(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(plan.Path)
	custody, err := os.ReadFile(p.Sources["custody"].Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := maintenanceWriteBytes(filepath.Join(dir, "custody-before-restore.json"), custody); err != nil {
		t.Fatal(err)
	}
	before, err := ReadSessionReadbackFile(filepath.Join(dir, "custody-before-restore.json"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := maintenanceWrite(filepath.Join(dir, "restore-intent.json"), progressMaintenanceRestoreIntent{Plan: plan, Stop: stop, Custody: before}); err != nil {
		t.Fatal(err)
	}
	if err := workspaceartifacts.Extract(t.Context(), p.Archives["noodle-result.tar"].Path, filepath.Join(p.SessionDirectory, "returned")); err != nil {
		t.Fatal(err)
	}
	// Reproduce the original implementation's failure before any Apply or Publish.
	if err := workspaceartifacts.Extract(t.Context(), p.Archives["noodle-session.tar"].Path, filepath.Join(p.SessionDirectory, "guest-session")); err == nil || !strings.Contains(err.Error(), "unsafe symlink") {
		t.Fatalf("original transfer did not reproduce: %v", err)
	}
	row := map[string]any{"request_id": "original", "project_dir": r.config.RuntimeDir, "binary_sha256": "original-binary", "config_sha256": "original-config", "host_instance_id": p.HostInstance, "host_pid": p.HostPID, "context_id": p.ContextID, "context_state": "failed"}
	originalPath := filepath.Join(dir, "original-native-response.json")
	if err := maintenanceWrite(originalPath, row); err != nil {
		t.Fatal(err)
	}
	original, err := ReadSessionReadbackFile(originalPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	historyPath := filepath.Join(dir, "original-host-history.json")
	if err := maintenanceWrite(historyPath, ProgressMaintenanceHostHistory{Schema: 1, Receipts: []SessionReadbackFileRef{original}}); err != nil {
		t.Fatal(err)
	}
	history, err := ReadSessionReadbackFile(historyPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	hostPath := filepath.Join(dir, "host-receipts-before.json")
	if err := maintenanceWrite(hostPath, []map[string]any{row}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(hostPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p.HostDir, "receipts.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	host, err := ReadSessionReadbackFile(hostPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	return r, f, p, plan, host, history
}

func TestProgressMaintenanceContinuesOnlyTheOriginalPrePublicationTransfer(t *testing.T) {
	r, f, p, plan, host, history := failedMaintenanceTransferFixture(t)
	repair, err := InspectProgressMaintenanceRestore(t.Context(), plan, r.config.Profile, host, history)
	if err != nil {
		t.Fatal(err)
	}
	again, err := InspectProgressMaintenanceRestore(t.Context(), plan, r.config.Profile, host, history)
	if err != nil || again != repair {
		t.Fatalf("inspection lost original packet: %v", err)
	}
	before, err := os.ReadFile(filepath.Join(filepath.Dir(plan.Path), "restore-intent.json"))
	if err != nil {
		t.Fatal(err)
	}
	finish := func(complete SessionReadbackFileRef) error { return maintenanceHostFixture(t, r, p, plan, complete) }
	if _, err := ContinueProgressMaintenanceRestore(t.Context(), plan, r.config.Profile, repair, finish); err != nil {
		t.Fatal(err)
	}
	if err := VerifyProgressMaintenanceHandoff(plan); err != nil {
		t.Fatal(err)
	}
	var d openShellDispatchReceipt
	if _, err := ReadSessionReadbackFile(p.Sources["dispatch"].Path, &d); err != nil {
		t.Fatal(err)
	}
	c, err := maintenanceCompletion(d)
	if err != nil || c.RestoreRepair == nil || *c.RestoreRepair != repair {
		t.Fatalf("repair lineage missing: %v", err)
	}
	after, err := os.ReadFile(filepath.Join(filepath.Dir(plan.Path), "restore-intent.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("original failed restore intent was overwritten")
	}
	if err := verifyCleanupProgressArchives(t.Context(), p.SessionDirectory, d); err != nil {
		t.Fatal(err)
	}
	if _, err := ContinueProgressMaintenanceRestore(t.Context(), plan, r.config.Profile, repair, finish); err == nil {
		t.Fatal("restore continuation replayed")
	}
	events, err := os.ReadFile(filepath.Join(f.root, "events"))
	if err != nil || strings.Count(string(events), "stop\n") != 1 {
		t.Fatal("transfer recovery repeated native Stop")
	}
}

func TestProgressMaintenanceTransferRepairRefusesChangedOrAdvancedState(t *testing.T) {
	for _, kind := range []string{"target", "returned", "delivery", "custody", "host", "host-and-before", "completion"} {
		t.Run(kind, func(t *testing.T) {
			r, _, p, plan, host, history := failedMaintenanceTransferFixture(t)
			var d openShellDispatchReceipt
			if _, err := ReadSessionReadbackFile(p.Sources["dispatch"].Path, &d); err != nil {
				t.Fatal(err)
			}
			var err error
			switch kind {
			case "target":
				err = os.WriteFile(filepath.Join(d.Target, "foreign.txt"), []byte("foreign"), 0600)
			case "returned":
				err = os.WriteFile(filepath.Join(p.SessionDirectory, "returned", "result.txt"), []byte("changed"), 0600)
			case "delivery":
				err = os.Mkdir(filepath.Join(p.SessionDirectory, "delivery"), 0700)
			case "custody":
				err = os.WriteFile(p.Sources["custody"].Path, []byte("changed"), 0600)
			case "host":
				err = os.WriteFile(filepath.Join(p.HostDir, "receipts.json"), []byte("changed"), 0600)
			case "host-and-before":
				var rows []map[string]any
				if _, err := ReadSessionReadbackFile(host.Path, &rows); err != nil {
					t.Fatal(err)
				}
				rows = append(rows, map[string]any{"request_id": "foreign", "context_state": "detached"})
				data, marshalErr := json.Marshal(rows)
				if marshalErr != nil {
					t.Fatal(marshalErr)
				}
				err = os.WriteFile(filepath.Join(p.HostDir, "receipts.json"), data, 0600)
				if err == nil {
					err = os.WriteFile(host.Path, data, 0600)
				}
				if err == nil {
					host, err = ReadSessionReadbackFile(host.Path, nil)
				}
			case "completion":
				err = os.WriteFile(filepath.Join(filepath.Dir(plan.Path), "runtime-complete.json"), []byte("later phase"), 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := InspectProgressMaintenanceRestore(t.Context(), plan, r.config.Profile, host, history); err == nil {
				t.Fatal("changed or advanced restore became resumable")
			}
			if _, err := os.Lstat(filepath.Join(filepath.Dir(plan.Path), "restore-repair-intent.json")); !os.IsNotExist(err) {
				t.Fatal("inspection reserved a publication")
			}
		})
	}
}
