package runtime

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/procx"
)

// The original maintenance journal binds the admission fence and empty resources.
// Schema 1 records process retirement. Schema 2 releases custody independently;
// its exclusive lock and v4 ledger fence the inspected v3 client before RPCs.
type OpenShellHostRelease struct {
	Schema                     int
	SourceRoot, SourceInstance string
	ControlRoot                string
	Process                    procx.NativeIdentity
	Plan, EmptyResources       SessionReadbackFileRef
	Cold                       *OpenShellColdRelease `json:",omitempty"`
}

type OpenShellCustodyHandover struct {
	Schema          int
	ControlRoot     string
	Binding         OpenShellCustodyBinding
	Before, Release SessionReadbackFileRef
}

func validateOpenShellControlRoot(root string) error {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return fmt.Errorf("openshell Host control root is not canonical")
	}
	actual, err := filepath.EvalSymlinks(root)
	if err != nil || actual != root {
		return fmt.Errorf("openshell Host control root is unavailable or indirect")
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("openshell Host control root is not a directory")
	}
	return nil
}

func verifyCustodyHandover(ref SessionReadbackFileRef, state OpenShellCustodyState) error {
	var proof OpenShellCustodyHandover
	if err := maintenanceVerify(ref, &proof); err != nil {
		return err
	}
	if proof.Schema != 1 || proof.ControlRoot != state.ControlRoot || proof.Binding != state.Binding {
		return fmt.Errorf("openshell custody handover binding differs")
	}
	var before OpenShellCustodyState
	if err := maintenanceVerify(proof.Before, &before); err != nil {
		return err
	}
	if before.Version != 3 || before.Binding != state.Binding || before.ControlRoot != "" || before.Handover != nil {
		return fmt.Errorf("openshell custody handover original version differs")
	}
	release, err := verifyOpenShellHostRelease(proof.Release)
	if err == nil && release.ControlRoot != state.ControlRoot {
		return fmt.Errorf("openshell release target differs from custody")
	}
	return err
}

func verifyOpenShellHostRelease(ref SessionReadbackFileRef) (OpenShellHostRelease, error) {
	var release OpenShellHostRelease
	if err := maintenanceVerify(ref, &release); err != nil {
		return release, err
	}
	if (release.Schema != 1 && release.Schema != 2 && release.Schema != 3) || !filepath.IsAbs(release.SourceRoot) || release.SourceInstance == "" || !release.Process.SameGeneration(release.Process) || release.Process.UID != os.Getuid() {
		return release, fmt.Errorf("openshell original Host release identity is incomplete")
	}
	var plan struct {
		Root   string                `json:"host_dir"`
		Native *procx.NativeIdentity `json:"process_generation"`
		Source struct {
			Instance string `json:"host_instance_id"`
		} `json:"native_source"`
		Handover *struct{ SharedRoot string } `json:"handover"`
	}
	if err := maintenanceVerify(release.Plan, &plan); err != nil {
		return release, err
	}
	if plan.Root != release.SourceRoot || plan.Native == nil || !plan.Native.SameGeneration(release.Process) || plan.Source.Instance != release.SourceInstance || plan.Handover == nil || plan.Handover.SharedRoot != release.ControlRoot || !filepath.IsAbs(release.ControlRoot) || release.SourceRoot == release.ControlRoot {
		return release, fmt.Errorf("openshell original Host release plan differs")
	}
	var empty struct {
		Instance  string `json:"host_instance_id"`
		PID       int    `json:"host_pid"`
		Contexts  *int   `json:"contexts"`
		Residents *struct {
			Error any `json:"error"`
		} `json:"residents"`
		Resources *struct {
			Workers     *int `json:"active_workers"`
			Residents   *int `json:"active_residents"`
			Allocations *int `json:"active_allocations"`
			Unresolved  *int `json:"unresolved_residents"`
		} `json:"resources"`
	}
	if err := maintenanceVerify(release.EmptyResources, &empty); err != nil {
		return release, err
	}
	if empty.Instance != release.SourceInstance || empty.PID != release.Process.PID || empty.Resources == nil || empty.Residents == nil || (release.Schema != 3 && empty.Residents.Error != nil) {
		return release, fmt.Errorf("openshell original Host empty readback differs")
	}
	if release.Schema == 3 {
		if empty.Contexts == nil || *empty.Contexts < 1 || empty.Resources.Workers == nil || *empty.Resources.Workers != 1 || empty.Resources.Residents == nil || *empty.Resources.Residents != 1 || empty.Resources.Allocations == nil || *empty.Resources.Allocations != 2 || empty.Resources.Unresolved == nil || *empty.Resources.Unresolved != 0 || empty.Residents.Error == nil {
			return release, fmt.Errorf("cold original resource responsibility differs")
		}
		_, err := verifyOpenShellColdRelease(release)
		return release, err
	}
	for _, value := range []*int{empty.Contexts, empty.Resources.Workers, empty.Resources.Residents, empty.Resources.Allocations, empty.Resources.Unresolved} {
		if value == nil || *value != 0 {
			return release, fmt.Errorf("openshell original Host resources are unsettled")
		}
	}
	return release, nil
}

// UpgradeOpenShellCustodyV3 is only a step of a confirmed Host maintenance
// transaction. The caller supplies immutable journal artifacts. Ordinary open
// never upgrades. The original ledger is preserved and no native RPC is sent.
func UpgradeOpenShellCustodyV3(binding OpenShellCustodyBinding, controlRoot string, before, releaseRef SessionReadbackFileRef, receiptPath string) (SessionReadbackFileRef, error) {
	var result SessionReadbackFileRef
	if !filepath.IsAbs(receiptPath) {
		return result, fmt.Errorf("openshell handover receipt path is not absolute")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(receiptPath))
	if err != nil {
		return result, err
	}
	receiptPath = filepath.Join(parent, filepath.Base(receiptPath))
	if err := validateOpenShellControlRoot(controlRoot); err != nil {
		return result, err
	}
	release, err := verifyOpenShellHostRelease(releaseRef)
	if err != nil {
		return result, err
	}
	if release.SourceRoot == controlRoot || release.ControlRoot != controlRoot {
		return result, fmt.Errorf("openshell handover source and target Host are identical")
	}
	if release.Schema == 1 {
		if _, err = procx.ObserveNativeIdentity(release.Process.PID); !errors.Is(err, procx.ErrProcessGone) {
			return result, fmt.Errorf("openshell original Host process exit is unproved")
		}
		absent, err := procx.ProcessGroupAbsent(release.Process.Group)
		if err != nil || !absent {
			return result, fmt.Errorf("openshell original Host process group exit is unproved")
		}
	}
	if release.Schema == 3 {
		var retired OpenShellColdRetirement
		if err := maintenanceVerify(release.Cold.Retirement, &retired); err != nil {
			return result, err
		}
		for _, actor := range retired.Actors {
			gone, err := procx.ObserveNativeExit(actor.Process)
			if err != nil || !gone {
				return result, fmt.Errorf("cold source actor exit is unproved")
			}
		}
	}
	lock, err := lockfile.TryLock(filepath.Join(binding.StateRoot, "owner.lock"))
	if err != nil {
		return result, err
	}
	defer lock.Close()
	var original OpenShellCustodyState
	if err = maintenanceVerify(before, &original); err != nil {
		return result, err
	}
	if original.Version != 3 || original.Binding != binding || original.ControlRoot != "" || original.Handover != nil || original.Blocked != "" || len(original.PendingRetained) != 0 {
		return result, fmt.Errorf("openshell original custody is not a settled v3 owner")
	}
	check := &OpenShellCustody{state: original}
	if err = check.validateLedger(); err != nil {
		return result, err
	}
	var failed *OpenShellCustodyRecord
	if release.Schema == 3 {
		in, err := verifyOpenShellColdRelease(release)
		if err != nil {
			return result, err
		}
		if before.SHA256 != inStateCustodySHA(release) {
			return result, fmt.Errorf("cold custody before-image differs from original failure")
		}
		failed = &in.record
	}
	for _, r := range original.Records {
		if failed != nil && reflect.DeepEqual(r, *failed) {
			continue
		}
		if r.Stopped == nil || (!r.Retained && !r.CreateAcknowledged) || r.Worker.StopPending || (r.StopRequestID != "" && !r.StopAcknowledged) || (r.Exec != nil && !r.Exec.Result.ExitObserved) || (r.ProgressStop != nil && !r.ProgressStop.Result.ExitObserved) {
			return result, fmt.Errorf("openshell original custody has unresolved effects")
		}
	}
	proof := OpenShellCustodyHandover{Schema: 1, ControlRoot: controlRoot, Binding: binding, Before: before, Release: releaseRef}
	encoded, err := json.MarshalIndent(proof, "", "  ")
	if err != nil {
		return result, err
	}
	encoded = append(encoded, '\n')
	owner := filepath.Join(binding.StateRoot, "owner.json")
	if filepath.Clean(before.Path) == filepath.Clean(owner) {
		return result, fmt.Errorf("openshell custody before-image is not independently preserved")
	}
	data, err := os.ReadFile(owner)
	if err != nil {
		return result, err
	}
	var current OpenShellCustodyState
	if err = json.Unmarshal(data, &current); err != nil {
		return result, err
	}
	if current.Version == openShellCustodyVersion {
		if current.ControlRoot != controlRoot || current.Handover == nil || current.Handover.Path != receiptPath {
			return result, fmt.Errorf("openshell handover was completed by a different transaction")
		}
		return *current.Handover, verifyCustodyHandover(*current.Handover, current)
	}
	if retainedRecoveryHash(data) != before.SHA256 {
		return result, fmt.Errorf("openshell custody changed after the confirmed handover")
	}
	if prior, readErr := os.ReadFile(receiptPath); readErr == nil {
		if string(prior) != string(encoded) {
			return result, fmt.Errorf("openshell handover receipt changed")
		}
	} else if !os.IsNotExist(readErr) {
		return result, readErr
	} else if err = maintenanceWriteBytes(receiptPath, encoded); err != nil {
		return result, err
	}
	result = SessionReadbackFileRef{Path: receiptPath, SHA256: retainedRecoveryHash(encoded)}
	original.Version, original.ControlRoot, original.Handover = openShellCustodyVersion, controlRoot, &result
	encoded, err = json.Marshal(original)
	if err != nil {
		return result, err
	}
	return result, filex.WriteFileAtomicDurable(owner, encoded)
}

func inStateCustodySHA(release OpenShellHostRelease) string {
	var p StorageFailureStopPlan
	if release.Cold == nil || maintenanceVerify(release.Cold.StopPlan, &p) != nil {
		return ""
	}
	return p.Custody.SHA256
}
