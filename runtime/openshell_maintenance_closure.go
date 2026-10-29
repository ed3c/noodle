package runtime

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"time"

	"github.com/poteto/noodle/internal/admission"
)

func ProgressMaintenanceResidentMatches(a, b admission.Resident) bool {
	return a.ID == b.ID && a.Name == b.Name && a.PID == b.PID && a.ProcessGroup == b.ProcessGroup && a.Started == b.Started && a.Fingerprint == b.Fingerprint && a.GuardID == b.GuardID
}

func ValidateProgressMaintenanceClosure(ref SessionReadbackFileRef, p ProgressMaintenance) (SessionReadbackFileRef, SessionReadbackFileRef, error) {
	var release ProgressMaintenanceRelease
	var exit ProgressMaintenanceExit
	fail := func(err error) (SessionReadbackFileRef, SessionReadbackFileRef, error) {
		return SessionReadbackFileRef{}, SessionReadbackFileRef{}, err
	}
	directory := filepath.Dir(ref.Path)
	releaseRef, err := ReadSessionReadbackFile(filepath.Join(directory, "release-complete.json"), &release)
	if err != nil {
		return fail(err)
	}
	exitRef, err := ReadSessionReadbackFile(filepath.Join(directory, "host-exit.json"), &exit)
	if err != nil {
		return fail(err)
	}
	if release.Plan != ref || exit.Plan != ref || release.ObservedAt.IsZero() || !release.ObservedAt.Before(exit.ObservedAt) || exit.HostInstance != p.HostInstance || exit.HostPID != p.HostPID || p.LauncherSession <= 0 || exit.LauncherSession != p.LauncherSession {
		return fail(fmt.Errorf("maintenance original launcher exit is not after resident release"))
	}
	if !ProgressMaintenanceResidentMatches(release.Resident, p.Resident) || release.Resident.State != "exited" || len(release.Resident.Contexts) != 0 || !slices.Contains(release.Resident.Released, p.ContextID) {
		return fail(fmt.Errorf("maintenance original resident was not released"))
	}
	intent, response, requestedAt, err := ProgressMaintenanceReleaseSources(ref, p)
	if err != nil {
		return fail(err)
	}
	stopAt, err := time.Parse(time.RFC3339Nano, release.Resident.StopRequested)
	if err != nil || stopAt.Before(requestedAt) || stopAt.After(release.ObservedAt) || intent != release.Intent || !reflect.DeepEqual(response, release.Response) {
		return fail(fmt.Errorf("maintenance release is not the original requested child stop"))
	}
	var packet struct {
		Instance  string `json:"host_instance_id"`
		PID       int    `json:"host_pid"`
		Directory string `json:"host_dir"`
		Residents struct {
			Records []admission.Resident `json:"records"`
			Error   json.RawMessage      `json:"error"`
		} `json:"residents"`
	}
	if err := maintenanceVerify(release.Resources, &packet); err != nil {
		return fail(err)
	}
	if packet.Instance != p.HostInstance || packet.PID != p.HostPID || packet.Directory != p.HostDir || string(packet.Residents.Error) != "null" {
		return fail(fmt.Errorf("maintenance release was not observed by the original live Host"))
	}
	count := 0
	for _, r := range packet.Residents.Records {
		if reflect.DeepEqual(r, release.Resident) {
			count++
		} else if r.State != "exited" || len(r.Contexts) != 0 {
			return fail(fmt.Errorf("another original resident remains"))
		}
	}
	if count != 1 {
		return fail(fmt.Errorf("maintenance original resident exit receipt is missing"))
	}
	var wait struct {
		ExitCode  *int            `json:"exit_code"`
		SessionID json.RawMessage `json:"session_id"`
	}
	if err := maintenanceVerify(exit.WaitResult, &wait); err != nil {
		return fail(err)
	}
	if wait.ExitCode == nil || len(wait.SessionID) != 0 {
		return fail(fmt.Errorf("original launcher waiter has not returned a terminal result"))
	}
	if err := maintenanceWait(p, exit, release.ObservedAt); err != nil {
		return fail(err)
	}
	return releaseRef, exitRef, nil
}
