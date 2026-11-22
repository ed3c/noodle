package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/loop"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/spf13/cobra"
)

const archiveRecoveryReclaimOperation = "reclaim-workspace-capture-cleanup"
const archiveRecoveryReclaimRequest = "task_archive_recovery_reclaim"
const archiveRecoveryReclaimTimeout = 5 * time.Minute

type hostArchiveRecoveryReclamation struct {
	loop.ArchiveRecoveryReclamationResult
	Schema         int                  `json:"schema"`
	Owner          string               `json:"owner"`
	HostInstanceID string               `json:"host_instance_id"`
	ContextID      string               `json:"context_id"`
	RequestID      string               `json:"request_id"`
	Error          string               `json:"error,omitempty"`
	Next           *archiveRecoveryNext `json:"next"`
}

type hostArchiveRecoveryReclaimer interface {
	ReclaimArchiveRecovery(context.Context, loop.ArchiveRecoveryReclamation) (loop.ArchiveRecoveryReclamationResult, error)
}

type archiveRecoveryIndependentExit struct {
	HostInstanceID string                             `json:"host_instance_id"`
	Disposition    loopruntime.SessionReadbackFileRef `json:"disposition"`
	Custody        loopruntime.SessionReadbackFileRef `json:"custody"`
	Profile        loopruntime.SessionReadbackFileRef `json:"profile"`
}

func newArchiveRecoveryReclaimCmd() *cobra.Command {
	var path, sha string
	var independentExit bool
	cmd := &cobra.Command{Use: archiveRecoveryReclaimOperation, Short: "Reclaim the exact approved original archive recovery disposition through its original Owner", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, cancel := context.WithTimeout(cmd.Context(), archiveRecoveryReclaimTimeout)
		defer cancel()
		ref := loopruntime.SessionReadbackFileRef{Path: path, SHA256: sha}
		reclaim := callOriginalArchiveRecoveryReclaim
		if independentExit {
			reclaim = callExitedArchiveRecoveryReclaim
		}
		result, err := reclaim(ctx, ref)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
	}}
	cmd.Flags().StringVar(&path, "disposition-file", "", "Original archive recovery disposition")
	cmd.Flags().StringVar(&sha, "expected-disposition-sha256", "", "Exact original disposition digest")
	cmd.Flags().BoolVar(&independentExit, "independent-exit", false, "Reclaim only after verified original controller and group exit")
	_ = cmd.MarkFlagRequired("disposition-file")
	_ = cmd.MarkFlagRequired("expected-disposition-sha256")
	return cmd
}

func planArchiveRecoveryReclaim(ctx context.Context, result *archiveRecoveryCleanupPlan) error {
	ref := *result.Disposition
	d, request, err := readOriginalArchiveRecoveryDisposition(ref)
	if err != nil {
		return err
	}
	result.HostActivation = observeArchiveRecoveryActivation(ref, d, request)
	path := filepath.Join(filepath.Dir(ref.Path), loop.ArchiveRecoveryReclamationFile)
	if _, err = os.Lstat(path); err == nil {
		reclaimed, readErr := readOriginalArchiveRecoveryReclamation(ref, d)
		if readErr != nil {
			return readErr
		}
		if reclaimed.Path != "" {
			result.Status, result.Reclamation = "archive_recovery_reclaimed", &reclaimed
			result.WaitFor, result.Next = "", nil
			return nil
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	drained, err := loop.ReadArchiveRecoveryWorkerDrain(ref, d.Lease)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if drained.Path != "" {
		directory, err := admission.DefaultMachineCapacityDirectory()
		if err != nil {
			return err
		}
		settlement, err := admission.ReadArchivedArchiveRecoverySettlement(directory, d.Lease, admission.MachineSettlementFile(ref))
		if err != nil {
			return err
		}
		if settlement != nil {
			reclaimed, err := loop.ReadArchiveRecoveryReclamation(ref, d.Lease, settlement)
			if err != nil {
				return err
			}
			result.Status, result.Reclamation = "archive_recovery_reclaimed", &reclaimed
			result.WaitFor, result.Next = "", nil
			return nil
		}
		result.WorkerDrain = &drained
	}
	client, err := originalHostTaskClient(request.ProjectDir, d.Request.Path, d.OrderID, d.SessionID, archiveRecoveryReclaimRequest)
	if err != nil {
		observeArchiveRecoveryHostWait(ctx, result, ref, d, request)
		return nil
	}
	info, err := os.Lstat(filepath.Join(client.HostDir, "host.sock"))
	if err != nil || info.Mode()&os.ModeSocket == 0 || client.Request.InstanceID != d.HostInstanceID ||
		client.Request.ContextID != d.ContextID {
		observeArchiveRecoveryHostWait(ctx, result, ref, d, request)
		return nil
	}
	if _, err = observeSessionActor(client.HostPID, client.BinaryRef); err != nil {
		observeArchiveRecoveryHostWait(ctx, result, ref, d, request)
		return nil
	}
	binary := client.BinaryRef
	result.WaitFor = ""
	result.Next = &archiveRecoveryNext{Owner: "Noodle", Kind: "executable", Operation: archiveRecoveryReclaimOperation,
		Executable: binary, Argv: []string{binary.Path, "openshell", "progress-maintenance", archiveRecoveryReclaimOperation,
			"--disposition-file", ref.Path, "--expected-disposition-sha256", ref.SHA256}}
	return nil
}

func observeArchiveRecoveryHostWait(ctx context.Context, result *archiveRecoveryCleanupPlan, ref loopruntime.SessionReadbackFileRef, d archiveRecoveryCleanupDisposition, request hostRequestBinding) {
	result.WaitFor = "original_archive_recovery_host_handoff"
	result.Next = nil
	err := withOriginalArchiveHostExitFence(ctx, ref, func(fence *archiveRecoveryHostExitFence) error {
		result.IndependentExit = &archiveRecoveryIndependentExit{HostInstanceID: d.HostInstanceID,
			Disposition: ref, Custody: fence.Custody, Profile: fence.Profile}
		directory, err := admission.DefaultMachineCapacityDirectory()
		if err != nil {
			return err
		}
		if _, err = prepareExitedArchiveReclamation(ctx, directory, fence); err != nil {
			return err
		}
		executable, err := os.Executable()
		if err != nil {
			return err
		}
		binary, err := hostReadbackBinaryRef(executable)
		if err != nil {
			return err
		}
		result.WaitFor = ""
		result.Next = &archiveRecoveryNext{Owner: "Noodle", Kind: "executable", Operation: archiveRecoveryReclaimOperation,
			Executable: binary, Argv: []string{binary.Path, "openshell", "progress-maintenance", archiveRecoveryReclaimOperation,
				"--disposition-file", ref.Path, "--expected-disposition-sha256", ref.SHA256, "--independent-exit"}}
		return nil
	})
	if err == nil {
		return
	}
	if result.IndependentExit != nil {
		result.WaitFor = "original_archive_recovery_settlement_owner"
		result.SettlementProblem = err.Error()
		return
	}
	retirement := readArchiveRecoveryHostRetirement(ctx, d, request)
	result.HostRetirement = &retirement
}

func readOriginalArchiveRecoveryReclamation(ref loopruntime.SessionReadbackFileRef, d archiveRecoveryCleanupDisposition) (loopruntime.SessionReadbackFileRef, error) {
	directory, err := admission.DefaultMachineCapacityDirectory()
	if err != nil {
		return loopruntime.SessionReadbackFileRef{}, err
	}
	settled, err := admission.ReadArchivedArchiveRecoverySettlement(directory, d.Lease, admission.MachineSettlementFile(ref))
	if err != nil {
		return loopruntime.SessionReadbackFileRef{}, err
	}
	if settled == nil {
		return loopruntime.SessionReadbackFileRef{}, nil
	}
	return loop.ReadArchiveRecoveryReclamation(ref, d.Lease, settled)
}

func readOriginalArchiveRecoveryDisposition(ref loopruntime.SessionReadbackFileRef) (archiveRecoveryCleanupDisposition, hostRequestBinding, error) {
	var d archiveRecoveryCleanupDisposition
	var request hostRequestBinding
	if err := readArchiveCleanupRef(ref, &d); err != nil {
		return d, request, err
	}
	if err := readArchiveCleanupRef(d.Request, &request); err != nil {
		return d, request, err
	}
	if d.Schema != 1 || d.Kind != "archive_recovery_cleanup_disposition" || d.OriginalReturn != "UNKNOWN" ||
		request.Schema != 1 || request.Operation != "attach" || request.Status != "accepted" || !request.CleanupCompleted ||
		request.RequiredHostInstanceID != d.HostInstanceID || d.Lease.Binding == nil ||
		d.Lease.Owner != hostMachineGenerationOwner(request.HostDir, d.HostInstanceID) ||
		d.Lease.ContextID != d.ContextID || d.Lease.Binding.ContextID != d.ContextID ||
		d.Lease.Binding.OrderID != d.OrderID || d.Lease.Binding.AttemptID != d.AttemptID ||
		d.Lease.Binding.SessionID != d.SessionID || d.Lease.Binding.WorktreePath != d.Checkout.Path ||
		d.SessionID == "" || filepath.Base(d.SessionID) != d.SessionID || d.SessionID == "." || d.SessionID == ".." {
		return d, request, fmt.Errorf("original reclamation disposition or lease binding differs")
	}
	session, err := filepath.EvalSymlinks(filepath.Join(request.ProjectDir, ".noodle", "sessions", d.SessionID))
	if err != nil || ref.Path != filepath.Join(session, archiveRecoveryCleanupDispositionFile) {
		return d, request, fmt.Errorf("original reclamation disposition is outside its attempt")
	}
	return d, request, nil
}

func callOriginalArchiveRecoveryReclaim(ctx context.Context, ref loopruntime.SessionReadbackFileRef) (result hostArchiveRecoveryReclamation, err error) {
	d, request, err := readOriginalArchiveRecoveryDisposition(ref)
	if err != nil {
		return result, err
	}
	bound, err := originalHostTaskClient(request.ProjectDir, d.Request.Path, d.OrderID, d.SessionID, archiveRecoveryReclaimRequest)
	if err != nil {
		return result, err
	}
	bound.Request.ArchiveRecoveryDisposition = &ref
	sent, err := bound.exchange(ctx, &result)
	if err != nil {
		if !sent {
			return result, err
		}
		return archiveRecoveryReclaimWaiting(bound.Request, "Original Owner acknowledgement needs readback: "+err.Error()), nil
	}
	if result.Schema != 1 || result.Owner != "Noodle" || result.RequestID != bound.Request.RequestID ||
		result.ContextID != d.ContextID || result.HostInstanceID != d.HostInstanceID ||
		result.Disposition != ref || result.OriginalReturn != "UNKNOWN" || result.Next != nil {
		return archiveRecoveryReclaimWaiting(bound.Request, "Original Owner response identity needs readback"), nil
	}
	if err = bound.verify(); err != nil {
		return archiveRecoveryReclaimWaiting(bound.Request, err.Error()), nil
	}
	switch result.Status {
	case "archive_recovery_reclamation_waiting":
		if result.Reclamation != nil {
			return archiveRecoveryReclaimWaiting(bound.Request, "Pending original Owner response carried terminal evidence"), nil
		}
	case "archive_recovery_reclaimed":
		reclaimed, err := readOriginalArchiveRecoveryReclamation(ref, d)
		if err != nil || result.Reclamation == nil || *result.Reclamation != reclaimed ||
			result.Checkout == nil || result.Checkout.Identity != d.Checkout || result.Error != "" {
			return archiveRecoveryReclaimWaiting(bound.Request, "Original terminal reclamation proof needs readback"), nil
		}
	default:
		return archiveRecoveryReclaimWaiting(bound.Request, "Original Owner response status needs readback"), nil
	}
	return result, nil
}

func archiveRecoveryReclaimWaiting(req hostRequest, problem string) hostArchiveRecoveryReclamation {
	var ref loopruntime.SessionReadbackFileRef
	if req.ArchiveRecoveryDisposition != nil {
		ref = *req.ArchiveRecoveryDisposition
	}
	return hostArchiveRecoveryReclamation{ArchiveRecoveryReclamationResult: loop.ArchiveRecoveryReclamationResult{
		Status: "archive_recovery_reclamation_waiting", Disposition: ref, OriginalReturn: "UNKNOWN", Effects: []string{}},
		Schema: 1, Owner: "Noodle", HostInstanceID: req.InstanceID, ContextID: req.ContextID, RequestID: req.RequestID, Error: problem}
}

func (h *noodleHost) reclaimArchiveRecovery(ctx context.Context, req hostRequest) hostArchiveRecoveryReclamation {
	result := archiveRecoveryReclaimWaiting(req, "")
	if req.ArchiveRecoveryDisposition == nil {
		result.Error = "Original archive recovery disposition is unavailable"
		return result
	}
	owner, sources, err := h.originalTaskContext(req)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	d, _, err := readOriginalArchiveRecoveryDisposition(*req.ArchiveRecoveryDisposition)
	if err == nil && (d.Request != req.ReadbackRequest || d.OrderID != req.ReadbackOrderID || d.SessionID != req.ReadbackSessionID ||
		d.HostInstanceID != h.instanceID || d.ContextID != req.ContextID) {
		err = fmt.Errorf("original reclamation request does not match active Host task")
	}
	if err == nil {
		err = verifyArchiveRecoveryLanding(archiveRecoveryCleanupPlan{
			Candidate: d.Candidate, LandingProof: d.LandingProof, Request: d.Request,
			OrderID: d.OrderID, AttemptID: d.AttemptID, SessionID: d.SessionID,
			Head: d.Head, Tree: d.Tree, OriginalReturn: "UNKNOWN",
		})
	}
	if err == nil {
		err = loopruntime.VerifySessionReadbackFiles(sources)
	}
	if err != nil {
		result.Error = err.Error()
		return result
	}
	reclaimer, ok := owner.runtime.(hostArchiveRecoveryReclaimer)
	if !ok {
		result.Error = "Original Loop reclamation owner is unavailable"
		return result
	}
	observation, err := reclaimer.ReclaimArchiveRecovery(ctx, loop.ArchiveRecoveryReclamation{
		Disposition: *req.ArchiveRecoveryDisposition, Lease: d.Lease, Checkout: d.Checkout, Hold: d.Hold,
		Head: d.Head, Tree: d.Tree,
	})
	if observation.Status != "" {
		result.ArchiveRecoveryReclamationResult = observation
	}
	if err != nil {
		result.Error = err.Error()
	}
	return result
}
