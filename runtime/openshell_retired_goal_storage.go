package runtime

import (
	"errors"
	"path/filepath"
	"strings"
)

// ObserveRetiredGoalVMCustody verifies the saved Goal and current retired
// Gateway storage. It does not load worker routes or hash the guest payload.
func ObserveRetiredGoalVMCustody(hostRoot string, profile, template SessionReadbackFileRef) (SessionReadbackFileRef, error) {
	receipt, err := ReadGoalOpenShellReservation(hostRoot)
	if err != nil || receipt.Profile != profile || receipt.Template != template {
		return SessionReadbackFileRef{}, errors.Join(errors.New("original retired Goal storage reservation differs"), err)
	}
	spec, err := LoadOpenShellGatewaySpec(profile.Path)
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	storage, err := qualifyRetiredGoalVMStorage(hostRoot, receipt, spec)
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	return readRetiredGoalVMCustody(hostRoot, profile, template, storage, spec)
}

func qualifyRetiredGoalVMStorage(hostRoot string, receipt GoalProfileReceipt, gateway OpenShellGatewaySpec) (openShellVMStorage, error) {
	var storage openShellVMStorage
	dir, config := filepath.Join(hostRoot, "openshell"), gateway.Binding.ConfigSource
	if gateway.ComputeDriver != "vm" || gateway.Resident.Directory != dir || filepath.Dir(config) != dir {
		return storage, errors.New("original retired OpenShell runtime is not exclusively Goal-owned")
	}
	ref, err := ReadSessionReadbackFile(config, nil)
	if err != nil || ref.Path != config || ref.SHA256 != receipt.Files[filepath.Base(config)] {
		return storage, errors.Join(errors.New("original retired VM configuration changed"), err)
	}
	root, err := readOpenShellVMRoot(ref)
	if err != nil || root != filepath.Join(dir, "vm") {
		return storage, errors.Join(errors.New("original retired VM state directory differs"), err)
	}
	return openShellVMStorage{Config: ref, Root: root}, nil
}

func readRetiredGoalVMCustody(hostRoot string, profile, template SessionReadbackFileRef, storage openShellVMStorage, spec OpenShellGatewaySpec) (SessionReadbackFileRef, error) {
	var zero SessionReadbackFileRef
	dir := filepath.Join(hostRoot, "openshell")
	if spec.Binding.StateRoot != filepath.Join(dir, "custody") {
		return zero, errors.New("original retired OpenShell runtime is not exclusively Goal-owned")
	}
	database := ""
	for _, entry := range spec.Resident.Environment {
		if value, found := strings.CutPrefix(entry, "OPENSHELL_DB_URL="); found {
			if database != "" || value != "sqlite://"+filepath.Join(dir, "gateway.db")+"?mode=rwc" {
				return zero, errors.New("original retired Gateway database is not Goal-owned")
			}
			database = value
		}
	}
	if database == "" {
		return zero, errors.New("original retired Gateway database is not bound")
	}
	var state OpenShellCustodyState
	custodyPath := filepath.Join(spec.Binding.StateRoot, "owner.json")
	custody, err := ReadSessionReadbackFile(custodyPath, &state)
	if err != nil || custody.Path != custodyPath {
		return zero, errors.Join(errors.New("original retired Goal custody is unavailable or moved"), err)
	}
	if err := verifyRetiredGoalVMCustody(hostRoot, spec, storage, state); err != nil {
		return zero, err
	}
	return custody, VerifySessionReadbackFiles(map[string]SessionReadbackFileRef{
		"profile": profile, "template": template, "vm_config": storage.Config, "retired_custody": custody,
	})
}

func verifyRetiredGoalVMCustody(hostRoot string, gateway OpenShellGatewaySpec, storage openShellVMStorage, state OpenShellCustodyState) error {
	if state.Binding != gateway.Binding || state.ControlRoot != hostRoot || state.Blocked != "" ||
		len(state.PendingRetained) != 0 || len(state.Records) == 0 || !openShellCustodySupportsProfile(state, gateway) {
		return errors.New("original retired Goal custody is different or unresolved")
	}
	if err := validateOpenShellCustodyLedger(state); err != nil {
		return err
	}
	if state.Handover != nil {
		if err := verifyCustodyHandover(*state.Handover, state); err != nil {
			return err
		}
	}
	for _, credential := range state.Credentials {
		if credential.Pending != nil || credential.SourceReadPending ||
			(!credential.InstallationObserved && (credential.MutationID != "" || len(credential.TargetReceipts) != 0)) {
			return errors.New("original retired Gateway has an unresolved credential effect")
		}
	}
	for _, row := range state.Records {
		if row.Retirement == nil || !row.Retirement.Retired || row.Retirement.Storage != storage ||
			(!row.Retained && !row.CreateAcknowledged) || row.Worker.StopPending || row.Worker.StopError != "" ||
			(row.StopRequestID != "" && !row.StopAcknowledged) {
			return errors.New("original Goal has an unretired or foreign VM")
		}
	}
	return nil
}
