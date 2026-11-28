package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRetiredGoalVMStorageRequiresRetiredOriginalCustody(t *testing.T) {
	r, binding, fixture, storage := retirementRuntimeFixture(t, "completed")
	gateway := fixture.owner.spec
	if before := fixture.owner.custody.Snapshot(); verifyRetiredGoalVMCustody(before.ControlRoot, gateway, storage, before) == nil {
		t.Fatal("unretired original VM was accepted")
	}
	settled, err := r.RetireCleanupSession(t.Context(), binding, binding.WorktreePath, StatusCompleted)
	if err != nil || settled.State != CleanupSessionRetired {
		t.Fatalf("retire original VM: %+v %v", settled, err)
	}
	state := fixture.owner.custody.Snapshot()
	if err := verifyRetiredGoalVMCustody(state.ControlRoot, gateway, storage, state); err != nil {
		t.Fatal("original native retirement was rejected", err)
	}
	foreignStorage := storage
	foreignStorage.Root = filepath.Join(state.ControlRoot, "other-vm")
	if verifyRetiredGoalVMCustody(state.ControlRoot, gateway, foreignStorage, state) == nil ||
		verifyRetiredGoalVMCustody(filepath.Dir(state.ControlRoot), gateway, storage, state) == nil {
		t.Fatal("foreign original Goal or VM path was accepted")
	}
}

func TestRetiredGoalVMObservationBindsExactReservationWithoutLoadingWorkerPayload(t *testing.T) {
	host, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	template := goalProfileFixture(t)
	receipt, err := MaterializeGoalOpenShellProfile(template, host, "")
	if err != nil {
		t.Fatal(err)
	}
	payload := filepath.Join(filepath.Dir(template), "payload")
	if err := os.Rename(payload, payload+"-retained"); err != nil {
		t.Fatal(err)
	}
	if _, err := ObserveRetiredGoalVMCustody(host, receipt.Template, receipt.Template); err == nil {
		t.Fatal("foreign profile was accepted")
	}
	_, err = ObserveRetiredGoalVMCustody(host, receipt.Profile, receipt.Template)
	var missing *os.PathError
	if !errors.Is(err, os.ErrNotExist) || !errors.As(err, &missing) || missing.Path != filepath.Join(host, "openshell", "custody", "owner.json") {
		t.Fatalf("observation must reach the original custody ledger without worker payload: %v", err)
	}
}
