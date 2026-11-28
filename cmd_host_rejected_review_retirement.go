package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/procx"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/loop"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/poteto/noodle/worktree"
)

type rejectedReviewMaintenanceReceipt struct {
	Schema      int                                           `json:"schema"`
	Release     loopruntime.SessionReadbackFileRef            `json:"rejected_review_release"`
	Sources     map[string]loopruntime.SessionReadbackFileRef `json:"sources"`
	Retirements []loopruntime.CleanupSessionRetirement        `json:"retirements"`
}

type rejectedReviewMaintenanceBefore struct {
	Schema   int                                           `json:"schema"`
	Release  loopruntime.SessionReadbackFileRef            `json:"rejected_review_release"`
	Resident admission.Resident                            `json:"resident"`
	Gateway  hostOperatorActor                             `json:"gateway"`
	Sources  map[string]loopruntime.SessionReadbackFileRef `json:"sources"`
}

func retireReleasedRejectedReview(ctx context.Context, p rejectedReviewReleasePlan, intent loopruntime.SessionReadbackFileRef,
	result rejectedReviewRoundRelease) (rejectedReviewRoundRelease, error) {
	result, err := readRejectedReviewRoundRelease(ctx, p, intent, result)
	if err != nil || result.Status != "source_round_released" {
		return result, err
	}
	release, err := saveRejectedReviewSourceRelease(intent, result)
	if err != nil {
		return result, err
	}
	result.SourceRelease = release
	result.Status, result.Next = "source_retirement_pending", p.next("reconcile", intent.SHA256)
	if err := verifyRejectedReviewRetainedCheckout(ctx, p); err != nil {
		return result, err
	}
	root := p.OwnerBefore.Current.HostDir
	if err := verifyGenerationPreparation(root, p.Generation.Binary, p.Generation.SHA, *p.Generation.Preparation); err != nil {
		return result, err
	}
	budget, err := rejectedReviewSettlementBudget(ctx, p)
	if err != nil {
		return result, err
	}
	var maintenance rejectedReviewMaintenanceReceipt
	maintenancePath := intent.Path + ".maintenance-exit"
	receipt, err := loopruntime.ReadSessionReadbackFile(maintenancePath, &maintenance)
	if os.IsNotExist(err) {
		maintenance, err = runRejectedReviewMaintenance(ctx, p, intent, *release, budget)
		if err != nil {
			return result, err
		}
		raw, err := json.Marshal(maintenance)
		if err != nil {
			return result, err
		}
		if err = saveHostStorageBytes(maintenancePath, raw); err != nil {
			return result, err
		}
		receipt, err = loopruntime.ReadSessionReadbackFile(maintenancePath, nil)
	}
	if err != nil {
		return result, err
	}
	if maintenance.Schema != 1 || maintenance.Release != *release || len(maintenance.Retirements) != len(p.Attempts) ||
		maintenance.Sources["maintenance_exit"].Path == "" || maintenance.Sources["native_custody"].Path == "" {
		return result, errors.New("original rejected review maintenance exit or retirement set differs")
	}
	if err := loopruntime.VerifySessionReadbackFiles(maintenance.Sources); err != nil {
		return result, err
	}
	if err := settleRejectedReviewMachineLeases(p, maintenance, budget); err != nil {
		return result, err
	}
	result.Maintenance = &receipt
	result.Status = "source_worker_resources_settled"
	result.Next = p.next("finish", intent.SHA256)
	result.Effects = []string{"original_stopped_vms_retired", "original_bound_worker_leases_settled", "maintenance_gateway_normal_exit"}
	return result, nil
}

func rejectedReviewSettlementBudget(ctx context.Context, p rejectedReviewReleasePlan) (*admission.Budget, error) {
	root := p.OwnerBefore.Current.HostDir
	owner := hostMachineGenerationOwner(root, p.Generation.ID)
	budget, err := preparationBudget(owner)
	if err != nil {
		return nil, err
	}
	if err := budget.BindSharedQuota(filepath.Join(root, "quota"), p.Generation.ID); err != nil {
		return nil, err
	}
	var control worktree.TaskControl
	project := p.OwnerBefore.Current.ProjectDir
	if _, err := loopruntime.ReadSessionReadbackFile(filepath.Join(filepath.Dir(project), "control.json"), &control); err != nil {
		return nil, err
	}
	if control.Status != "prepared" || control.ControlRoot != project || filepath.Dir(filepath.Dir(project)) != filepath.Join(root, "controls") {
		return nil, errors.New("original rejected review control preparation differs")
	}
	if p.OwnerBefore.ContextInputs == nil {
		return nil, errors.New("original rejected review execution inputs are unavailable")
	}
	env, err := p.OwnerBefore.ContextInputs.rebuild(project, os.Environ())
	if err != nil {
		return nil, err
	}
	plan, err := taskPreparationPlan(ctx, root, control, env, p.Host.Binary)
	if err != nil {
		return nil, err
	}
	if err := verifyTaskPreparationPlan(plan); err != nil {
		return nil, err
	}
	if plan.Storage.Source.StorageRoot != filepath.Dir(p.RuntimeDir) {
		return nil, errors.New("original rejected review storage root changed")
	}
	plan.Profile.Gateway.ControlRoot = root
	producer := noodleHost{dir: root, budget: budget}
	preparedRoots := append([]string{plan.Storage.Source.CommonDir}, plan.Archive.diskRoots()...)
	producer.bindWorkerResourceSource(p.OwnerBefore.Current.ContextID, project, plan.Storage.Source.StorageRoot,
		&plan.Profile, &plan, preparedRoots...)
	return budget, nil
}

func runRejectedReviewMaintenance(ctx context.Context, p rejectedReviewReleasePlan, intent, release loopruntime.SessionReadbackFileRef,
	budget *admission.Budget) (rejectedReviewMaintenanceReceipt, error) {
	result := rejectedReviewMaintenanceReceipt{Schema: 1, Release: release}
	directory := intent.Path + ".maintenance"
	startMarker := intent.Path + ".maintenance-offer"
	offered, err := releaseMarker(startMarker, intent.SHA256)
	if err != nil {
		return result, err
	}
	if offered {
		return readRejectedReviewMaintenance(p, intent, release)
	}
	profile, err := loadSessionObserverProfile(p.Profile.Path, p.Profile)
	if err != nil {
		return result, err
	}
	profile.Gateway.ControlRoot = p.OwnerBefore.Current.HostDir
	if profile.Gateway.Resident.ForceAfterGrace {
		return result, errors.New("original maintenance Gateway permits forced shutdown")
	}
	history, err := loopRejectedReviewRetirementBindings(p)
	if err != nil {
		return result, err
	}
	maintContext := "rejected-review-maintenance-" + intent.SHA256
	if err := os.MkdirAll(directory, 0700); err != nil {
		return result, err
	}
	residents, err := admission.OpenResidents(directory)
	if err != nil {
		return result, err
	}
	rows, err := residents.Snapshot()
	if err != nil || len(rows) != 0 {
		return result, errors.New("original maintenance resident is already recorded")
	}
	budget.Register(maintContext)
	defer budget.Unregister(maintContext)
	gateway, err := loopruntime.NewOpenShellGateway(residents, budget, profile.Gateway)
	if err != nil {
		return result, err
	}
	if err := saveHostStorageBytes(startMarker, []byte(intent.SHA256)); err != nil {
		return result, errors.Join(err, gateway.Close())
	}
	opened := gateway.AcquireRejectedReviewMaintenance(ctx, maintContext)
	rows, err = residents.Snapshot()
	if err != nil || len(rows) != 1 {
		return result, errors.Join(opened, err, gateway.Close(), errors.New("original maintenance gateway resident witness unavailable"))
	}
	before := rows[0]
	if opened == nil {
		proof, readErr := saveRejectedReviewMaintenanceBefore(p, intent, release, profile, before)
		if readErr == nil {
			result.Retirements, opened = gateway.RetireRejectedReviewSessions(ctx, profile, maintContext, p.OwnerBefore.Current.ContextID,
				p.RuntimeDir, p.OwnerBefore.Current.ProjectDir, history, proof.Sources)
		} else {
			opened = readErr
		}
	}
	// Use the original resident child handle even on a failed retirement. The
	// guarded release refuses unresolved VM effects and never escalates.
	_, stopErr := gateway.Release(maintContext)
	drain, cancel := context.WithTimeout(context.WithoutCancel(ctx), 45*time.Second)
	defer cancel()
	waitErr := residents.WaitClosed(drain)
	closeErr := gateway.Close()
	if err := errors.Join(opened, stopErr, waitErr, closeErr); err != nil {
		return result, err
	}
	return readRejectedReviewMaintenance(p, intent, release)
}

func saveRejectedReviewMaintenanceBefore(p rejectedReviewReleasePlan, intent, release loopruntime.SessionReadbackFileRef,
	profile loopruntime.OpenShellProfile, resident admission.Resident) (rejectedReviewMaintenanceBefore, error) {
	var before rejectedReviewMaintenanceBefore
	binary, err := hostReadbackBinaryRef(profile.Gateway.Resident.Argv[0])
	if err != nil || binary != p.Gateway.Binary {
		return before, errors.New("original maintenance Gateway executable differs")
	}
	actor, err := observeSessionActor(resident.PID, binary)
	if err != nil || actor.Process.ParentPID != os.Getpid() || actor.Process.Group != resident.ProcessGroup {
		return before, errors.New("original maintenance Gateway native ancestry differs")
	}
	snap, err := loopruntime.ReadSessionReadbackFile(filepath.Join(p.RuntimeDir, "state.snapshot.json"), nil)
	if err != nil {
		return before, err
	}
	before = rejectedReviewMaintenanceBefore{Schema: 1, Release: release, Resident: resident, Gateway: actor,
		Sources: map[string]loopruntime.SessionReadbackFileRef{"canonical_snapshot": snap, "rejected_review_events": p.Events, "rejected_review_release": release}}
	raw, err := json.Marshal(before)
	if err != nil {
		return before, err
	}
	return before, saveHostStorageBytes(intent.Path+".maintenance-before", raw)
}

func readRejectedReviewMaintenance(p rejectedReviewReleasePlan, intent, release loopruntime.SessionReadbackFileRef) (rejectedReviewMaintenanceReceipt, error) {
	result := rejectedReviewMaintenanceReceipt{Schema: 1, Release: release}
	var before rejectedReviewMaintenanceBefore
	begin, err := loopruntime.ReadSessionReadbackFile(intent.Path+".maintenance-before", &before)
	if err != nil || before.Schema != 1 || before.Release != release || before.Resident.ForceAfterGrace ||
		before.Resident.PID != before.Gateway.Process.PID || before.Gateway.Binary != p.Gateway.Binary {
		return result, errors.New("original maintenance Gateway start/readback remains unknown; no spawn replay")
	}
	if err := loopruntime.VerifySessionReadbackFiles(before.Sources); err != nil {
		return result, err
	}
	var final map[string]admission.Resident
	exit, err := loopruntime.ReadSessionReadbackFile(filepath.Join(intent.Path+".maintenance", "residents.json"), &final)
	if err != nil || len(final) != 1 {
		return result, errors.New("original maintenance Gateway exit remains unknown; no spawn replay")
	}
	if err := admission.VerifyResidentNormalExit(before.Resident, final[before.Resident.ID]); err != nil {
		return result, err
	}
	absent, err := procx.ProcessGroupAbsent(before.Resident.ProcessGroup)
	if err != nil || !absent {
		return result, errors.New("maintenance Gateway process group remains unresolved")
	}
	gone, err := procx.ObserveNativeExit(before.Gateway.Process)
	if err != nil || !gone {
		return result, errors.New("maintenance Gateway native identity remains unresolved")
	}
	var state loopruntime.OpenShellCustodyState
	root := p.OwnerBefore.Current.HostDir
	custody, err := loopruntime.ReadSessionReadbackFile(filepath.Join(root, "openshell", "custody", "owner.json"), &state)
	if err != nil {
		return result, err
	}
	if state.Blocked != "" || state.ControlRoot != root || len(state.PendingRetained) != 0 {
		return result, errors.New("original maintenance custody has foreign or unresolved VM records")
	}
	for _, row := range state.Records {
		if err := verifyRejectedReviewSandboxAbsent(root, row); err != nil {
			return result, err
		}
	}
	for _, attempt := range p.Attempts {
		var match *loopruntime.OpenShellCustodyRecord
		for i := range state.Records {
			row := &state.Records[i]
			if row.Task.OrderID == p.Order && row.Task.SessionID == attempt.SessionID &&
				row.Task.AttemptID == attempt.NativeOpenShellStoppedReadback.NativeAttemptID {
				if match != nil {
					return result, errors.New("original maintenance custody retirement is ambiguous")
				}
				match = row
			}
		}
		if match == nil || match.Retirement == nil || !match.Retirement.Retired ||
			match.Retirement.Sources["rejected_review_release"] != release || match.Retirement.Storage.Root != filepath.Join(root, "openshell", "vm") {
			return result, errors.New("original maintenance custody has no bound native retirement completion")
		}
		sandbox := match.Worker.Identity.Sandbox.SandboxID
		outcome := "unknown"
		if match.Retirement.Acknowledged {
			outcome = pb.DeletionOutcome(match.Retirement.Outcome).String()
		}
		result.Retirements = append(result.Retirements, loopruntime.CleanupSessionRetirement{OutcomeStatus: loopruntime.StatusCompleted,
			SessionID: attempt.SessionID, NativeAttemptID: match.Task.AttemptID, SandboxID: sandbox,
			State: loopruntime.CleanupSessionRetired, NativeDeletionOutcome: outcome})
	}
	result.Sources = before.Sources
	result.Sources["maintenance_before"], result.Sources["maintenance_exit"], result.Sources["native_custody"] = begin, exit, custody
	if err := loopruntime.VerifySessionReadbackFiles(result.Sources); err != nil {
		return result, err
	}
	return result, nil
}

func verifyRejectedReviewCustodyBeforeRelease(p rejectedReviewReleasePlan) error {
	root := p.OwnerBefore.Current.HostDir
	var custody loopruntime.OpenShellCustodyState
	ref, err := loopruntime.ReadSessionReadbackFile(filepath.Join(root, "openshell", "custody", "owner.json"), &custody)
	if err != nil || custody.Blocked != "" || custody.ControlRoot != root || len(custody.PendingRetained) != 0 {
		return errors.New("original Gateway custody is foreign or unresolved")
	}
	selected := map[string]string{}
	for _, attempt := range p.Attempts {
		execution := attempt.NativeOpenShellStoppedReadback
		if execution.Sources["native_custody"] != ref {
			return errors.New("original selected VM custody changed before release")
		}
		selected[attempt.SessionID] = execution.NativeAttemptID
	}
	for _, row := range custody.Records {
		nativeAttempt, ok := selected[row.Task.SessionID]
		if !ok {
			if err := verifyRejectedReviewSandboxAbsent(root, row); err != nil {
				return errors.New("original Gateway has another unresolved VM")
			}
			continue
		}
		if row.Task.OrderID != p.Order || row.Task.AttemptID != nativeAttempt || row.Retirement != nil ||
			!row.StopAcknowledged || row.Worker.StopPending || row.Worker.StopError != "" {
			return errors.New("original selected Gateway VM is not the qualified stopped attempt")
		}
		delete(selected, row.Task.SessionID)
	}
	if len(selected) != 0 {
		return errors.New("original selected stopped VM inventory is incomplete")
	}
	return nil
}

func verifyRejectedReviewSandboxAbsent(root string, row loopruntime.OpenShellCustodyRecord) error {
	if row.Retirement == nil || !row.Retirement.Retired || row.Retirement.Storage.Root != filepath.Join(root, "openshell", "vm") {
		return errors.New("original maintenance custody has an unretired or foreign VM")
	}
	sandbox := row.Worker.Identity.Sandbox.SandboxID
	if sandbox == "" || sandbox == "." || sandbox == ".." || filepath.Base(sandbox) != sandbox || strings.ContainsRune(sandbox, 0) {
		return errors.New("original maintenance sandbox identity differs")
	}
	path := filepath.Join(row.Retirement.Storage.Root, "sandboxes")
	if canonical, err := filepath.EvalSymlinks(path); err != nil || canonical != path {
		return errors.New("original maintenance sandbox directory cannot confirm disk absence")
	}
	if _, err := os.Lstat(filepath.Join(path, sandbox)); !os.IsNotExist(err) {
		return errors.New("original stopped VM disk remains present or unresolved")
	}
	return nil
}

func loopRejectedReviewRetirementBindings(p rejectedReviewReleasePlan) ([]loopruntime.RejectedReviewRetirementAttempt, error) {
	snapshot, err := reducer.ReadSnapshot(filepath.Join(p.RuntimeDir, "state.snapshot.json"))
	if err != nil {
		return nil, err
	}
	actual := snapshot.State.Orders[p.Order]
	review := snapshot.State.PendingReviews[p.Order]
	attempts := make([]loopruntime.RejectedReviewRetirementAttempt, 0, len(p.Attempts))
	for i, a := range p.Attempts {
		if a.StageIndex < 0 || a.StageIndex >= len(actual.Stages) {
			return nil, errors.New("original rejected review retirement stage missing")
		}
		stage := actual.Stages[a.StageIndex]
		attempts = append(attempts, loopruntime.RejectedReviewRetirementAttempt{
			Session: loopruntime.CompletedSessionBinding{OrderID: p.Order, SessionID: a.SessionID, WorktreePath: review.WorktreePath,
				Provider: stage.Provider, Model: stage.Model, StageIndex: a.StageIndex, RetryCount: i},
			NativeAttemptID: a.NativeOpenShellStoppedReadback.NativeAttemptID, CandidateHead: a.CandidateHead,
		})
	}
	return attempts, nil
}

func settleRejectedReviewMachineLeases(p rejectedReviewReleasePlan, maintenance rejectedReviewMaintenanceReceipt, budget *admission.Budget) error {
	directory, err := admission.DefaultMachineCapacityDirectory()
	if err != nil {
		return err
	}
	for i, a := range p.Attempts {
		retired := maintenance.Retirements[i]
		if retired.SessionID != a.SessionID || retired.NativeAttemptID != a.NativeOpenShellStoppedReadback.NativeAttemptID ||
			retired.State != loopruntime.CleanupSessionRetired || retired.OutcomeStatus != loopruntime.StatusCompleted {
			return errors.New("original rejected review maintenance retirement does not prove the selected stopped VM is absent")
		}
		if a.WorkerLease == nil {
			continue // An unrecorded historical nonce stays unknown.
		}
		lease := *a.WorkerLease
		if lease.Binding == nil || lease.Binding.SessionID != a.SessionID || lease.Binding.AttemptID != a.AttemptID {
			return errors.New("original rejected review machine lease scope differs")
		}
		sources := make(map[string]admission.MachineSettlementFile, len(maintenance.Sources))
		for role, ref := range maintenance.Sources {
			sources[role] = admission.MachineSettlementFile(ref)
		}
		proof := admission.MachineReclamation{Binding: *lease.Binding, CheckoutOperation: admission.RetainedRejectedReviewCheckout,
			Sources: sources, RuntimeState: retired.State, NativeAttemptID: retired.NativeAttemptID, SandboxID: retired.SandboxID, NativeDeletionOutcome: retired.NativeDeletionOutcome}
		if err := budget.SettleMachineSession(lease, proof); err != nil {
			return err
		}
		if observed, err := admission.ReadRejectedReviewMachineSettlement(directory, lease, admission.MachineSettlementFile(maintenance.Release)); err != nil || observed == nil {
			return fmt.Errorf("original rejected review settlement readback unavailable: %v", err)
		}
	}
	held, err := admission.ReadMachineSessionLeases(directory, hostMachineGenerationOwner(p.OwnerBefore.Current.HostDir, p.Generation.ID),
		p.OwnerBefore.Current.ContextID, p.OwnerBefore.Current.ProjectDir)
	if err != nil || len(held) != 0 {
		return errors.New("original rejected review held machine lease remains unknown")
	}
	return nil
}

func finishRejectedReviewSource(ctx context.Context, p rejectedReviewReleasePlan, intent loopruntime.SessionReadbackFileRef,
	result rejectedReviewRoundRelease) (rejectedReviewRoundRelease, error) {
	current, err := reconcileRejectedReviewHandoff(ctx, p, intent, result)
	if err != nil {
		return current, err
	}
	if current.Status == "source_host_retired" || current.Status == "source_host_zero" {
		return readRejectedReviewSourceStorage(ctx, p, intent, current, true)
	}
	if current.Status != "source_worker_resources_settled" && current.Status != "source_controller_pending" {
		return current, errors.New("original stopped VMs and machine leases remain unresolved")
	}
	current, err = retireRejectedReviewController(ctx, p, intent, current)
	if err != nil {
		return current, err
	}
	current, err = reconcileRejectedReviewHandoff(ctx, p, intent, current)
	if err != nil || (current.Status != "source_host_retired" && current.Status != "source_host_zero") {
		return current, err
	}
	return readRejectedReviewSourceStorage(ctx, p, intent, current, true)
}

func reconcileRejectedReviewHandoff(ctx context.Context, p rejectedReviewReleasePlan, intent loopruntime.SessionReadbackFileRef,
	result rejectedReviewRoundRelease) (rejectedReviewRoundRelease, error) {
	if _, err := os.Lstat(intent.Path + ".successor-start"); !os.IsNotExist(err) {
		return readRejectedReviewSuccessor(ctx, p, intent, result)
	}
	if preparation, err := readRejectedReviewSuccessorPreparationIntent(p, intent); !os.IsNotExist(err) {
		if err != nil {
			return result, err
		}
		result.Status, result.Next = "successor_admission_pending", p.next("adopt", intent.SHA256)
		result.HostZero, result.Storage = "ZERO", &preparation.SourceZero
		return result, nil
	}
	var receipt rejectedReviewMaintenanceReceipt
	ref, err := loopruntime.ReadSessionReadbackFile(intent.Path+".maintenance-exit", &receipt)
	if os.IsNotExist(err) {
		current, err := readRejectedReviewRoundRelease(ctx, p, intent, result)
		if err == nil && current.Status == "source_round_released" {
			current.Next = p.next("retire", intent.SHA256)
			offered, offerErr := releaseMarker(intent.Path+".maintenance-offer", intent.SHA256)
			if offerErr != nil {
				return current, offerErr
			}
			if offered {
				current.Status, current.Next = "source_retirement_pending", p.next("reconcile", intent.SHA256)
				release, readErr := loopruntime.ReadSessionReadbackFile(intent.Path+".source-release", nil)
				if readErr == nil {
					if _, readErr = readRejectedReviewMaintenance(p, intent, release); readErr == nil {
						current.SourceRelease = &release
						current.Next = p.next("retire", intent.SHA256)
					}
				}
			}
		}
		return current, err
	}
	if err != nil {
		return result, err
	}
	var source rejectedReviewSourceRelease
	sourceRef, err := loopruntime.ReadSessionReadbackFile(intent.Path+".source-release", &source)
	if err != nil || source.Schema != 1 || source.Intent != intent || sourceRef != receipt.Release ||
		receipt.Schema != 1 || source.RoundState != (loop.HostContinuation{Mode: p.LoopMode, DispatchState: loop.StatePaused}) ||
		source.GatewayExit.Path != filepath.Join(p.Generation.Directory, "residents.json") ||
		len(receipt.Retirements) != len(p.Attempts) {
		return result, errors.New("original successful source release or maintenance receipt differs")
	}
	if err := loopruntime.VerifySessionReadbackFiles(receipt.Sources); err != nil {
		return result, err
	}
	result.SourceRelease, result.Maintenance, result.RoundState, result.GatewayExit = &sourceRef, &ref, &source.RoundState, &source.GatewayExit
	result.Status, result.Next = "source_worker_resources_settled", p.next("finish", intent.SHA256)
	directory, err := admission.DefaultMachineCapacityDirectory()
	if err != nil {
		return result, err
	}
	for i, attempt := range p.Attempts {
		retired := receipt.Retirements[i]
		if retired.State != loopruntime.CleanupSessionRetired || retired.SessionID != attempt.SessionID ||
			retired.NativeAttemptID != attempt.NativeOpenShellStoppedReadback.NativeAttemptID {
			return result, errors.New("original stopped review VM retirement differs")
		}
		if lease := attempt.WorkerLease; lease != nil {
			proof, err := admission.ReadRejectedReviewMachineSettlement(directory, *lease, admission.MachineSettlementFile(sourceRef))
			if err != nil {
				return result, err
			}
			if proof == nil {
				result.Status, result.Next = "source_retirement_pending", p.next("retire", intent.SHA256)
				return result, nil
			}
		}
	}
	offered, err := releaseMarker(intent.Path+".controller-retire", intent.SHA256)
	if err != nil || !offered {
		return result, err
	}
	current, err := readRejectedReviewController(p, intent, result)
	if err != nil || current.Status != "source_host_retired" {
		return current, err
	}
	return readRejectedReviewSourceStorage(ctx, p, intent, current, false)
}

type rejectedReviewStorageReceipt struct {
	Schema      int                                `json:"schema"`
	Intent      loopruntime.SessionReadbackFileRef `json:"intent"`
	Maintenance loopruntime.SessionReadbackFileRef `json:"maintenance"`
	SourceExit  loopruntime.SessionReadbackFileRef `json:"source_exit"`
	SourceOnly  bool                               `json:"source_only"`
	Successor   string                             `json:"successor,omitempty"`
}

func readRejectedReviewSourceStorage(ctx context.Context, p rejectedReviewReleasePlan, intent loopruntime.SessionReadbackFileRef,
	result rejectedReviewRoundRelease, apply bool) (rejectedReviewRoundRelease, error) {
	if result.SourceExit == nil || result.Maintenance == nil {
		return result, errors.New("original source exit or stopped VM retirement has no proof")
	}
	root := p.OwnerBefore.Current.HostDir
	// The original attach path needs pool.lock before admitting a new context.
	// Actor resource RPCs stay outside the lock so rolling ticks can finish.
	fence, err := lockfile.TryLockExisting(filepath.Join(root, "pool.lock"))
	if err != nil {
		return result, err
	}
	defer fence.Close()
	reservation, err := loopruntime.ReadGoalOpenShellReservation(root)
	if err != nil || reservation.Profile != p.Profile {
		return result, errors.New("original retired Goal storage reservation differs")
	}
	var maintenance rejectedReviewMaintenanceReceipt
	ref, err := loopruntime.ReadSessionReadbackFile(result.Maintenance.Path, &maintenance)
	if err != nil || ref != *result.Maintenance {
		return result, errors.New("original maintenance settlement witness changed")
	}
	verifyExit := func() error {
		if !fence.HoldsExisting(filepath.Join(root, "pool.lock")) {
			return errors.New("original pool exclusion changed during storage retirement")
		}
		observed, err := readRejectedReviewController(p, intent, result)
		if err != nil {
			return err
		}
		if observed.Status != "source_host_retired" {
			return errors.New("original controller is not exclusively retired")
		}
		return nil
	}
	remaining, err := reclaimFailedReviewRuntimeStorage(root, p.Generation.ID, p.Profile, reservation.Template, maintenance.Sources["native_custody"], apply, verifyExit)
	if err != nil {
		return result, err
	}
	if remaining {
		result.Next = p.next("finish", intent.SHA256)
		return result, nil
	}
	storage := rejectedReviewStorageReceipt{Schema: 1, Intent: intent, Maintenance: *result.Maintenance, SourceExit: *result.SourceExit, SourceOnly: true}
	storageRaw, err := json.Marshal(storage)
	if err != nil {
		return result, err
	}
	storagePath := intent.Path + ".storage-zero"
	if apply {
		if err := saveHostStorageBytes(storagePath, storageRaw); err != nil {
			return result, err
		}
		result.Effects = append(result.Effects, "original_generated_vm_storage_zero")
	}
	saved, err := loopruntime.ReadSessionReadbackFile(storagePath, nil)
	if os.IsNotExist(err) {
		result.Next = p.next("finish", intent.SHA256)
		return result, nil
	}
	if err != nil || saved.SHA256 != hostDigest(storageRaw) {
		return result, errors.New("original retired storage ZERO receipt differs")
	}
	if err := fence.Close(); err != nil {
		return result, err
	}
	if observed, err := readRejectedReviewController(p, intent, result); err != nil || observed.Status != "source_host_retired" {
		return result, errors.New("original retired source scope changed after storage retirement")
	}
	zero := int64(0)
	result.Status, result.HostZero, result.SourceDisk, result.Storage = "source_host_zero", "ZERO", &zero, &saved
	result.Next = p.next("adopt", intent.SHA256)
	return result, nil
}
