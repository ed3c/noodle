package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/loop"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/poteto/noodle/worktree"
)

type rejectedReviewTaskPreparationSource struct {
	Request       loopruntime.SessionReadbackFileRef `json:"request"`
	Control       loopruntime.SessionReadbackFileRef `json:"control"`
	SourceZero    loopruntime.SessionReadbackFileRef `json:"source_zero"`
	SourceRelease loopruntime.SessionReadbackFileRef `json:"source_release"`
	Successor     loopruntime.SessionReadbackFileRef `json:"successor"`
	Lease         admission.MachineSessionLease      `json:"original_worker_lease"`
}

type rejectedReviewTaskPreparation struct {
	Schema      int                                 `json:"schema"`
	Source      rejectedReviewTaskPreparationSource `json:"source"`
	Preparation admission.PreparationRef            `json:"preparation"`
}

func (source rejectedReviewTaskPreparationSource) receiptPath() string {
	return source.SourceZero.Path + ".task-preparation"
}

// Only the Host's selected adoption path supplies this source. The old task
// owner, the exact settled worker nonce and the same checkout remain linked.
func readRejectedReviewTaskPreparationSource(ctx context.Context, root string, source rejectedReviewTaskPreparationSource) (worktree.TaskControl, hostRequestBinding, error) {
	var control worktree.TaskControl
	var request hostRequestBinding
	requestRef, err := loopruntime.ReadSessionReadbackFile(source.Request.Path, &request)
	if err != nil || requestRef != source.Request || request.Schema != 1 || request.Status != "accepted" || request.HostDir != root ||
		(request.Operation != "attach" && request.Operation != "once") ||
		request.RequestID == "" || filepath.Dir(filepath.Dir(request.ProjectDir)) != filepath.Join(root, "controls") {
		return control, request, errors.New("original accepted rejected review request differs")
	}
	controlRef, err := loopruntime.ReadSessionReadbackFile(filepath.Join(filepath.Dir(request.ProjectDir), "control.json"), &control)
	if err != nil || controlRef != source.Control || control.Schema != 2 || control.Status != "prepared" || control.ControlRoot != request.ProjectDir ||
		control.Preparation.ID == "" || control.Preparation.ID != source.Lease.ID || source.Lease.Binding == nil {
		return control, request, errors.New("original rejected review checkout or preparation differs")
	}
	var zero struct {
		Schema      int                                `json:"schema"`
		Intent      loopruntime.SessionReadbackFileRef `json:"intent"`
		Maintenance loopruntime.SessionReadbackFileRef `json:"maintenance"`
		SourceExit  loopruntime.SessionReadbackFileRef `json:"source_exit"`
		SourceOnly  bool                               `json:"source_only"`
	}
	zeroRef, err := loopruntime.ReadSessionReadbackFile(source.SourceZero.Path, &zero)
	if err != nil || zeroRef != source.SourceZero || zero.Schema != 1 || !zero.SourceOnly || zeroRef.Path != zero.Intent.Path+".storage-zero" ||
		zero.Maintenance.Path != zero.Intent.Path+".maintenance-exit" || source.SourceRelease.Path != zero.Intent.Path+".source-release" {
		return control, request, errors.New("original rejected review Host ZERO receipt differs")
	}
	var intent struct {
		Request     loopruntime.SessionReadbackFileRef `json:"request"`
		Successor   loopruntime.SessionReadbackFileRef `json:"successor"`
		Order       string                             `json:"order_id"`
		OwnerBefore struct {
			Current hostReceipt `json:"current"`
		} `json:"owner_before"`
	}
	intentRef, err := loopruntime.ReadSessionReadbackFile(zero.Intent.Path, &intent)
	old := intent.OwnerBefore.Current
	binding := *source.Lease.Binding
	if err != nil || intentRef != zero.Intent || intent.Request != source.Request || intent.Successor != source.Successor ||
		old.HostDir != root || old.RequestID != request.RequestID || old.ProjectDir != request.ProjectDir || old.ContextID != source.Lease.ID ||
		old.ContextID != binding.ContextID || old.ProjectDir != binding.ProjectDir || intent.Order != binding.OrderID ||
		zero.SourceExit.Path != filepath.Join(root, "g", old.HostInstanceID, "resource-resident-exit.json") ||
		old.BinarySHA256 == source.Successor.SHA256 || source.Lease.Owner != hostMachineGenerationOwner(root, old.HostInstanceID) {
		return control, request, errors.New("original rejected review intent or successor preparation identity differs")
	}
	var maintenance struct {
		Release loopruntime.SessionReadbackFileRef `json:"rejected_review_release"`
	}
	maintRef, err := loopruntime.ReadSessionReadbackFile(zero.Maintenance.Path, &maintenance)
	if err != nil || maintRef != zero.Maintenance || maintenance.Release != source.SourceRelease {
		return control, request, errors.New("original rejected review maintenance release differs")
	}
	if err := loopruntime.VerifySessionReadbackFiles(map[string]loopruntime.SessionReadbackFileRef{
		"source_exit": zero.SourceExit, "source_release": source.SourceRelease, "successor": source.Successor,
	}); err != nil {
		return control, request, err
	}
	machine, err := admission.DefaultMachineCapacityDirectory()
	if err != nil {
		return control, request, err
	}
	observed, err := admission.ReadMachinePreparations(machine, control.Preparation)
	oldPreparation := observed[control.Preparation]
	bindings := oldPreparation.Source.Bindings
	if err != nil || !oldPreparation.Closed || !oldPreparation.Promoted || !oldPreparation.SessionBound ||
		oldPreparation.Owner != source.Lease.Owner || oldPreparation.Source.Purpose != "task-preparation" ||
		bindings["control_root"] != control.ControlRoot || bindings["repository"] != control.Repository || bindings["head"] != control.Head ||
		bindings["task_path"] != control.Task.Path || bindings["task_sha256"] != control.Task.SHA256 ||
		bindings["execution_binary"] != old.BinaryPath+"/"+old.BinarySHA256 {
		return control, request, errors.Join(errors.New("original settled preparation source differs"), err)
	}
	settled, err := admission.ReadRejectedReviewMachineSettlement(machine, source.Lease, admission.MachineSettlementFile(source.SourceRelease))
	if err != nil || settled == nil {
		return control, request, errors.Join(errors.New("original rejected review worker lease is not settled"), err)
	}
	if err := worktree.VerifyPreparedTaskControl(ctx, control); err != nil {
		return control, request, err
	}
	return control, request, nil
}

func rejectedReviewTaskPlan(ctx context.Context, root string, control worktree.TaskControl, environment []string, source rejectedReviewTaskPreparationSource) (hostTaskPreparationPlan, error) {
	plan, err := taskPreparationPlan(ctx, root, control, environment, source.Successor)
	if err != nil {
		return plan, err
	}
	plan.OriginalControlPreparation = control.Preparation
	plan.Source.Bindings["original_worker_lease"] = source.Lease.ID
	for role, ref := range map[string]loopruntime.SessionReadbackFileRef{
		"original_request": source.Request, "original_control": source.Control,
		"rejected_review_zero": source.SourceZero, "rejected_review_release": source.SourceRelease,
	} {
		plan.Source.Bindings[role] = ref.Path + "/" + ref.SHA256
		plan.Sources[role] = ref
	}
	return plan, nil
}

func reserveRejectedReviewTaskPreparation(ctx context.Context, root string, environment []string, source rejectedReviewTaskPreparationSource) (loopruntime.SessionReadbackFileRef, error) {
	var receipt loopruntime.SessionReadbackFileRef
	control, _, err := readRejectedReviewTaskPreparationSource(ctx, root, source)
	if err != nil {
		return receipt, err
	}
	guard, err := lockfile.TryLock(source.receiptPath() + ".lock")
	if err != nil {
		return receipt, err
	}
	defer guard.Close()
	plan, err := rejectedReviewTaskPlan(ctx, root, control, environment, source)
	if err != nil {
		return receipt, err
	}
	if err = verifyTaskPreparationPlan(plan); err != nil {
		return receipt, err
	}
	budget, err := preparationBudget(filepath.Dir(control.ControlRoot))
	if err != nil {
		return receipt, err
	}
	var saved rejectedReviewTaskPreparation
	ref, err := loopruntime.ReadSessionReadbackFile(source.receiptPath(), &saved)
	if err == nil {
		if ref.Path != source.receiptPath() || saved.Schema != 1 || !sameRejectedReviewTaskPreparationSource(saved.Source, source) ||
			saved.Preparation.ID == control.Preparation.ID {
			return receipt, errors.New("original supplemental preparation receipt differs")
		}
		if err := budget.VerifyPreparationSource(saved.Preparation, plan.Source, plan.Claim); err != nil {
			return receipt, err
		}
	} else if os.IsNotExist(err) {
		prepared, err := budget.PrepareMachineResources(ctx, plan.Source, plan.Claim, admission.NewSampler())
		if err != nil {
			return receipt, err
		}
		if prepared.ID == control.Preparation.ID {
			return receipt, errors.New("successor preparation cannot use the settled original nonce")
		}
		if err := verifyTaskPreparationPlan(plan); err != nil {
			return receipt, errors.Join(err, budget.CancelUnstartedPreparation(prepared))
		}
		saved = rejectedReviewTaskPreparation{1, source, prepared}
		data, err := json.Marshal(saved)
		if err != nil {
			return receipt, err
		}
		if err = saveHostStorageBytes(source.receiptPath(), data); err != nil {
			return receipt, err
		}
		ref, err = loopruntime.ReadSessionReadbackFile(source.receiptPath(), nil)
		if err != nil {
			return receipt, err
		}
	} else {
		return receipt, err
	}
	if err := verifyTaskPreparationPlan(plan); err != nil {
		return receipt, err
	}
	machine, err := admission.DefaultMachineCapacityDirectory()
	if err != nil {
		return receipt, err
	}
	observed, err := admission.ReadMachinePreparations(machine, saved.Preparation)
	if err != nil || observed[saved.Preparation].Closed {
		return receipt, errors.Join(errors.New("successor preparation was closed or cannot be read back"), err)
	}
	if !observed[saved.Preparation].EffectsPossible {
		if err := budget.StartPreparationEffects(saved.Preparation); err != nil {
			return receipt, err
		}
	}
	return ref, nil
}

func sameRejectedReviewTaskPreparationSource(a, b rejectedReviewTaskPreparationSource) bool {
	leaseA, leaseB := a.Lease.Binding, b.Lease.Binding
	a.Lease.Binding, b.Lease.Binding = nil, nil
	return a == b && leaseA != nil && leaseB != nil && *leaseA == *leaseB
}

func (h *noodleHost) readRejectedReviewTaskPreparation(ctx context.Context, req hostRequest, control worktree.TaskControl, order string) (worktree.TaskControl, hostTaskPreparationPlan, error) {
	var plan hostTaskPreparationPlan
	var saved rejectedReviewTaskPreparation
	ref, err := loopruntime.ReadSessionReadbackFile(req.RejectedReviewAdoption.Path, &saved)
	if err != nil || ref != *req.RejectedReviewAdoption || ref.Path != saved.Source.receiptPath() || saved.Schema != 1 ||
		saved.Preparation.ID == "" || saved.Preparation.ID == control.Preparation.ID || h.rolling == nil {
		return control, plan, errors.New("original successor preparation sidecar differs")
	}
	source := saved.Source
	actual, request, err := readRejectedReviewTaskPreparationSource(ctx, h.controlRoot(), source)
	if err != nil {
		return control, plan, err
	}
	if actual != control || source.Successor != (loopruntime.SessionReadbackFileRef{Path: h.binaryPath, SHA256: h.binarySHA}) ||
		request.RequestID != req.RequestID || request.ProjectDir != req.ProjectDir || request.ConfigPath != req.ConfigPath ||
		request.ConfigSHA256 != req.ConfigSHA256 || source.Lease.Binding.OrderID != order {
		return control, plan, errors.New("original accepted request or successor execution binary differs")
	}
	pool, err := readHostPool(h.controlRoot())
	owner := pool.Owners[req.RequestID]
	target := pool.Generations[h.instanceID]
	if err != nil || pool.Active != h.instanceID || target.State != "active" || target.Binary != h.binaryPath || target.SHA != h.binarySHA ||
		owner.Original.RequestID != req.RequestID || owner.State != "admitting" || owner.Current.ContextID != source.Lease.ContextID ||
		owner.Current.ProjectDir != req.ProjectDir || owner.Current.ContextState != "detached" || owner.Current.TerminalCause != loop.TerminalMigrationDetach ||
		source.Lease.Owner != hostMachineGenerationOwner(h.controlRoot(), owner.Current.HostInstanceID) || owner.RoundState == nil ||
		owner.RoundState.DispatchState != loop.StatePaused || req.RoundState == nil || req.RoundState.DispatchState != loop.StatePaused {
		return control, plan, errors.New("original rejected review source has no selected paused successor admission")
	}
	plan, err = rejectedReviewTaskPlan(ctx, h.controlRoot(), control, req.Environment, source)
	if err != nil {
		return control, plan, err
	}
	plan.Sources["successor_preparation"] = ref
	control.Preparation = saved.Preparation
	return control, plan, nil
}
