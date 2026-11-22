package main

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/poteto/noodle/internal/admission"
	loopruntime "github.com/poteto/noodle/runtime"
)

// Retirement readback is an observation only. The original controller can
// acknowledge detach only after its own worker handles and inputs are drained.
type archiveRecoveryHostRetirementReadback struct {
	Status     string                             `json:"status"`
	Host       string                             `json:"host_instance_id"`
	Executable loopruntime.SessionReadbackFileRef `json:"executable"`
	Workers    *int                               `json:"owned_workers,omitempty"`
	Residents  *int                               `json:"owned_residents,omitempty"`
	Error      string                             `json:"error,omitempty"`
}

func readArchiveRecoveryHostRetirement(ctx context.Context, d archiveRecoveryCleanupDisposition, request hostRequestBinding) archiveRecoveryHostRetirementReadback {
	result := archiveRecoveryHostRetirementReadback{Status: "original_host_retirement_unverified", Host: d.HostInstanceID}
	fail := func(err error) archiveRecoveryHostRetirementReadback {
		result.Error = err.Error()
		return result
	}
	owner, err := readHostOwnerBinding(d.Request.Path)
	if err != nil {
		return fail(err)
	}
	if owner.Request != d.Request || owner.HostRoot != request.HostDir ||
		owner.Current.HostInstanceID != d.HostInstanceID || owner.Current.ContextID != d.ContextID ||
		owner.Current.RequestID != request.RequestID || owner.Current.ContextState != "active" {
		return fail(fmt.Errorf("original archive recovery Host ownership changed"))
	}
	result.Executable = owner.ExecutionBinary
	before, err := observeSessionActor(owner.Current.HostPID, owner.ExecutionBinary)
	if err != nil {
		return fail(err)
	}
	var raw json.RawMessage
	if err = readOriginalSessionHostCLI(ctx, owner, d.Request.Path, "resources", &raw); err != nil {
		return fail(err)
	}
	after, err := observeSessionActor(owner.Current.HostPID, owner.ExecutionBinary)
	if err != nil || !before.Process.SameGeneration(after.Process) {
		return fail(fmt.Errorf("original archive recovery Host generation changed during readback: %v", err))
	}
	if err = loopruntime.VerifySessionReadbackFiles(owner.Sources); err != nil {
		return fail(err)
	}
	if err = readOriginalArchiveHostOccupancy(owner, raw, &result); err != nil {
		return fail(err)
	}
	return result
}

func readOriginalArchiveHostOccupancy(owner *hostOwnerBinding, raw json.RawMessage, result *archiveRecoveryHostRetirementReadback) error {
	var observed struct {
		Protocol   string `json:"protocol"`
		Root       string `json:"host_root"`
		Directory  string `json:"host_dir"`
		Generation string `json:"host_instance_id"`
		PID        int    `json:"host_pid"`
		BinaryPath string `json:"binary_path"`
		BinarySHA  string `json:"binary_sha256"`
		Contexts   *int   `json:"contexts"`
		Shutdown   struct {
			Requested *bool `json:"requested"`
		} `json:"shutdown"`
		Resources struct {
			Workers     *int                           `json:"active_workers"`
			Residents   *int                           `json:"active_residents"`
			Allocations *int                           `json:"owned_allocations"`
			Unresolved  *int                           `json:"unresolved_residents"`
			Quota       *admission.SharedQuotaSnapshot `json:"shared_quota"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(raw, &observed); err != nil {
		return err
	}
	r := observed.Resources
	if observed.Protocol != hostProtocol || observed.Root != owner.HostRoot || observed.Directory != owner.GenerationDir ||
		observed.Generation != owner.Current.HostInstanceID || observed.PID != owner.Current.HostPID ||
		observed.BinaryPath != owner.ExecutionBinary.Path || observed.BinarySHA != owner.ExecutionBinary.SHA256 ||
		observed.Contexts == nil || *observed.Contexts != 1 || observed.Shutdown.Requested == nil || *observed.Shutdown.Requested ||
		r.Workers == nil || *r.Workers < 0 || r.Residents == nil || *r.Residents < 0 ||
		r.Allocations == nil || *r.Allocations != *r.Workers+*r.Residents || r.Unresolved == nil || *r.Unresolved != 0 ||
		r.Quota == nil || r.Quota.Error != "" || r.Quota.Maximum <= 0 || r.Quota.Owned != *r.Allocations ||
		r.Quota.Allocations != *r.Allocations || r.Quota.Residents != *r.Residents {
		return fmt.Errorf("original archive recovery Host occupancy or shutdown is unverified")
	}
	result.Workers, result.Residents = r.Workers, r.Residents
	if *r.Workers != 0 {
		result.Status = "original_host_worker_handle_retained"
		return nil
	}
	result.Status = "original_host_retirement_witness_required"
	return nil
}
