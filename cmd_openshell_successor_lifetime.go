package main

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/poteto/noodle/internal/admission"
	loopruntime "github.com/poteto/noodle/runtime"
)

type successorResources struct {
	Schema     *int   `json:"schema"`
	Protocol   string `json:"protocol"`
	Instance   string `json:"host_instance_id"`
	PID        int    `json:"host_pid"`
	Directory  string `json:"host_dir"`
	Binary     string `json:"binary_sha256"`
	Contexts   *int   `json:"contexts"`
	WebURL     string `json:"web_url"`
	Accounting struct {
		Workers     *int `json:"active_workers"`
		Residents   *int `json:"active_residents"`
		Allocations *int `json:"active_allocations"`
		Unknown     *int `json:"unresolved_residents"`
	} `json:"resources"`
	Residents struct {
		Records []admission.Resident `json:"records"`
		Error   json.RawMessage      `json:"error"`
	} `json:"residents"`
}

func validateClosedSuccessor(closed closedSuccessor, receipt hostReceipt, plan loopruntime.ProgressMaintenance) error {
	var launch struct {
		Argv        []string          `json:"argv"`
		Environment map[string]string `json:"environment"`
		Binary      string            `json:"binary_sha256"`
		PID         int               `json:"launcher_pid"`
		CreatedAt   time.Time         `json:"created_at"`
	}
	var shutdown struct {
		Instance    string                             `json:"host_instance_id"`
		PID         int                                `json:"pid"`
		Session     int                                `json:"launcher_session"`
		Resources   loopruntime.SessionReadbackFileRef `json:"resources"`
		RequestedAt time.Time                          `json:"requested_at"`
	}
	var wait struct {
		ExitCode *int            `json:"exit_code"`
		Session  json.RawMessage `json:"session_id"`
	}
	var resources successorResources
	for _, source := range []struct {
		ref   loopruntime.SessionReadbackFileRef
		value any
	}{{closed.Launcher, &launch}, {closed.Shutdown, &shutdown}, {closed.WaitResult, &wait}, {closed.Resources, &resources}} {
		if err := readRetirementSource(source.ref, source.value); err != nil {
			return err
		}
	}
	if wait.ExitCode == nil || *wait.ExitCode != 0 || len(wait.Session) != 0 {
		return fmt.Errorf("successor retirement requires the original normal Host exit")
	}
	if launch.PID != receipt.HostPID || launch.Binary != receipt.BinarySHA256 || len(launch.Argv) != 3 || launch.Argv[0] != receipt.BinaryPath || launch.Argv[1] != "host" || launch.Argv[2] != "serve" || launch.Environment["NOODLE_HOST_DIR"] != receipt.HostDir || launch.CreatedAt.IsZero() {
		return fmt.Errorf("retirement original Host launcher identity differs")
	}
	if err := readRetirementSource(loopruntime.SessionReadbackFileRef{Path: receipt.BinaryPath, SHA256: receipt.BinarySHA256}, nil); err != nil {
		return err
	}
	if shutdown.Instance != closed.HostInstance || shutdown.PID != closed.HostPID || shutdown.Session != closed.LauncherSession || shutdown.Resources != closed.Resources || shutdown.RequestedAt.Before(launch.CreatedAt) {
		return fmt.Errorf("retirement original Host shutdown intent differs")
	}
	if resources.Schema == nil || *resources.Schema != 1 || resources.Protocol != hostProtocol || resources.Instance != closed.HostInstance || resources.PID != closed.HostPID || resources.Directory != receipt.HostDir || resources.Binary != receipt.BinarySHA256 || resources.Contexts == nil || *resources.Contexts != 1 {
		return fmt.Errorf("retirement original resource owner differs")
	}
	for _, count := range []*int{resources.Accounting.Workers, resources.Accounting.Residents, resources.Accounting.Allocations, resources.Accounting.Unknown} {
		if count == nil || *count != 0 {
			return fmt.Errorf("retirement resource accounting is missing or unsettled")
		}
	}
	if string(resources.Residents.Error) != "null" || len(resources.Residents.Records) != 1 {
		return fmt.Errorf("retirement resident history is unresolved")
	}
	resident := resources.Residents.Records[0]
	stoppedAt, err := time.Parse(time.RFC3339Nano, resident.StopRequested)
	if err != nil || resident.State != "exited" || len(resident.Contexts) != 0 || !slices.Contains(resident.Released, receipt.ContextID) || resident.ID != plan.Resident.ID || resident.Name != plan.Resident.Name || resident.Fingerprint != plan.Resident.Fingerprint || resident.GuardID != plan.Resident.GuardID || resident.PID <= 1 || resident.ProcessGroup != resident.PID || resident.Started == "" || stoppedAt.Before(launch.CreatedAt) || shutdown.RequestedAt.Before(stoppedAt) {
		return fmt.Errorf("retirement is not the original released successor resident generation")
	}
	return loopruntime.ValidateOriginalHostLifetime(closed.OriginalHostLifetime, shutdown.RequestedAt)
}
