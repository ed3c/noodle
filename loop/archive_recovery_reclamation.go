package loop

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"

	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/state"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/poteto/noodle/worktree"
	"github.com/poteto/noodle/worktree/archive"
)

// ArchiveRecoveryReclamation carries only a validated original Host disposition.
type ArchiveRecoveryReclamation struct {
	Disposition loopruntime.SessionReadbackFileRef
	Lease       admission.MachineSessionLease
	Checkout    archive.CheckoutIdentity
	Hold        loopruntime.ArchiveRecoveryDispatchHold
	Head, Tree  string
}

type ArchiveRecoveryReclamationResult struct {
	Status         string                                           `json:"status"`
	Disposition    loopruntime.SessionReadbackFileRef               `json:"disposition"`
	WorkerDrain    *loopruntime.SessionReadbackFileRef              `json:"worker_drain,omitempty"`
	Reclamation    *loopruntime.SessionReadbackFileRef              `json:"reclamation,omitempty"`
	Checkout       *archive.CheckoutReclamationEvidence             `json:"checkout,omitempty"`
	VM             *loopruntime.ArchiveRecoveryVMRetirementReadback `json:"vm,omitempty"`
	OriginalReturn string                                           `json:"original_return"`
	Effects        []string                                         `json:"effects"`
}

const ArchiveRecoveryReclamationFile = "archive-recovery-reclamation.json"
const ArchiveRecoveryWorkerDrainFile = "archive-recovery-worker-drain.json"
const archiveRecoveryReclamationSource = "archive_recovery_reclamation"

type archiveRecoveryWorkerDrainReceipt struct {
	Schema         int                                `json:"schema"`
	Kind           string                             `json:"kind"`
	Disposition    loopruntime.SessionReadbackFileRef `json:"disposition"`
	Lease          admission.MachineSessionLease      `json:"held_machine_lease"`
	OriginalReturn string                             `json:"original_return"`
}

type archiveRecoveryReclamationReceipt struct {
	Schema         int                                 `json:"schema"`
	Kind           string                              `json:"kind"`
	OriginalReturn string                              `json:"original_return"`
	Disposition    loopruntime.SessionReadbackFileRef  `json:"disposition"`
	Lease          admission.MachineSessionLease       `json:"lease"`
	Checkout       archive.CheckoutReclamationEvidence `json:"checkout"`
	Settlement     admission.MachineReclamation        `json:"settlement"`
}

func originalArchiveRecoveryReceipt(disposition loopruntime.SessionReadbackFileRef, original admission.MachineSessionLease, settlement *admission.MachineReclamation) (archiveRecoveryReclamationReceipt, error) {
	var receipt archiveRecoveryReclamationReceipt
	var selected struct {
		Schema         int                                `json:"schema"`
		Kind           string                             `json:"kind"`
		OriginalReturn string                             `json:"original_return"`
		Lease          admission.MachineSessionLease      `json:"lease"`
		Checkout       archive.CheckoutIdentity           `json:"checkout"`
		Landing        loopruntime.SessionReadbackFileRef `json:"landing_proof"`
		Stop           loopruntime.SessionReadbackFileRef `json:"stop"`
		NativeAttempt  string                             `json:"native_attempt_id"`
		Sandbox        string                             `json:"sandbox_id"`
	}
	ref, err := loopruntime.ReadSessionReadbackFile(disposition.Path, &selected)
	if err != nil {
		return receipt, err
	}
	if ref != disposition || selected.Schema != 1 || selected.Kind != "archive_recovery_cleanup_disposition" ||
		selected.OriginalReturn != "UNKNOWN" || settlement == nil || original.Binding == nil ||
		!reflect.DeepEqual(selected.Lease, original) || settlement.Binding != *original.Binding ||
		settlement.CheckoutOperation != "archive-recovery-checkout/"+disposition.SHA256 ||
		settlement.Sources["archive_recovery_disposition"] != admission.MachineSettlementFile(disposition) ||
		settlement.Sources["archive_recovery_landing"] != admission.MachineSettlementFile(selected.Landing) ||
		settlement.Sources["archive_recovery_stop"] != admission.MachineSettlementFile(selected.Stop) ||
		settlement.RuntimeState != loopruntime.CleanupSessionRetired || settlement.NativeAttemptID == "" ||
		settlement.NativeAttemptID != selected.NativeAttempt || settlement.SandboxID == "" ||
		settlement.SandboxID != selected.Sandbox || settlement.NativeDeletionOutcome == "" ||
		selected.Checkout.Path != original.Binding.WorktreePath {
		return receipt, fmt.Errorf("archive recovery terminal proof differs from the original disposition or lease settlement")
	}
	immutable := make(map[string]loopruntime.SessionReadbackFileRef)
	for _, role := range []string{"archive_recovery_disposition", "archive_recovery_landing", "archive_recovery_stop",
		"checkout_packet", "checkout_removal", "checkout_verification_release"} {
		immutable[role] = loopruntime.SessionReadbackFileRef(settlement.Sources[role])
	}
	if err = loopruntime.VerifySessionReadbackFiles(immutable); err != nil {
		return receipt, err
	}
	// The original Budget sealed this custody witness while the VM was retired.
	// Its shared source can acquire unrelated rows after the original settlement.
	custody := settlement.Sources["native_custody"]
	if !filepath.IsAbs(custody.Path) || len(custody.SHA256) != 64 {
		return receipt, fmt.Errorf("archive recovery settlement lacks its original native custody witness")
	}
	checkout := archive.CheckoutReclamationEvidence{
		Identity: selected.Checkout, Packet: archive.FileRef(immutable["checkout_packet"]),
		Removal:             archive.FileRef(immutable["checkout_removal"]),
		VerificationRelease: archive.FileRef(immutable["checkout_verification_release"]),
	}
	saved := *settlement
	saved.Sources = maps.Clone(saved.Sources)
	delete(saved.Sources, archiveRecoveryReclamationSource)
	return archiveRecoveryReclamationReceipt{Schema: 1, Kind: "archive_recovery_reclamation",
		OriginalReturn: "UNKNOWN", Disposition: disposition, Lease: original, Checkout: checkout, Settlement: saved}, nil
}

func sameArchiveRecoveryReceipt(existing, expected archiveRecoveryReclamationReceipt) bool {
	// A new read of the same retired row can have another digest after unrelated
	// work updates the shared native custody file. Preserve the first witness.
	old, current := existing.Settlement.Sources["native_custody"], expected.Settlement.Sources["native_custody"]
	if old.Path != current.Path || len(old.SHA256) != 64 {
		return false
	}
	expected.Settlement.Sources["native_custody"] = old
	return reflect.DeepEqual(existing, expected)
}

// ReadArchiveRecoveryReclamation binds a terminal receipt to the original
// native Budget's exact settled nonce, with no live runtime effects.
func ReadArchiveRecoveryReclamation(disposition loopruntime.SessionReadbackFileRef, original admission.MachineSessionLease, settlement *admission.MachineReclamation) (loopruntime.SessionReadbackFileRef, error) {
	var selected archiveRecoveryReclamationReceipt
	path := filepath.Join(filepath.Dir(disposition.Path), ArchiveRecoveryReclamationFile)
	actual, err := loopruntime.ReadSessionReadbackFile(path, &selected)
	if err != nil {
		return actual, err
	}
	if settlement == nil || settlement.Sources[archiveRecoveryReclamationSource] != admission.MachineSettlementFile(actual) {
		return actual, fmt.Errorf("archive recovery terminal receipt is not sealed in the original machine settlement")
	}
	expected, err := originalArchiveRecoveryReceipt(disposition, original, settlement)
	if err != nil {
		return actual, err
	}
	if !sameArchiveRecoveryReceipt(selected, expected) {
		return actual, fmt.Errorf("archive recovery terminal receipt differs from original sealed settlement")
	}
	return actual, nil
}

func prepareArchiveRecoveryReclamation(disposition loopruntime.SessionReadbackFileRef, lease admission.MachineSessionLease, settlement *admission.MachineReclamation) error {
	record, err := originalArchiveRecoveryReceipt(disposition, lease, settlement)
	if err != nil {
		return err
	}
	path := filepath.Join(filepath.Dir(disposition.Path), ArchiveRecoveryReclamationFile)
	var existing archiveRecoveryReclamationReceipt
	ref, err := loopruntime.ReadSessionReadbackFile(path, &existing)
	if os.IsNotExist(err) {
		if err = writeLandedMergeJSON(path, record); err != nil {
			return err
		}
		ref, err = loopruntime.ReadSessionReadbackFile(path, &existing)
	}
	if err != nil {
		return err
	}
	if !sameArchiveRecoveryReceipt(existing, record) {
		return fmt.Errorf("prepared archive recovery receipt is bound to different evidence")
	}
	settlement.Sources[archiveRecoveryReclamationSource] = admission.MachineSettlementFile(ref)
	return nil
}

func sealArchiveRecoveryReclamation(result *ArchiveRecoveryReclamationResult, scope ArchiveRecoveryReclamation, settlement *admission.MachineReclamation) error {
	ref, err := ReadArchiveRecoveryReclamation(scope.Disposition, scope.Lease, settlement)
	if err != nil {
		return err
	}
	record, err := originalArchiveRecoveryReceipt(scope.Disposition, scope.Lease, settlement)
	if err != nil {
		return err
	}
	result.Status = "archive_recovery_reclaimed"
	result.Reclamation, result.Checkout = &ref, &record.Checkout
	return nil
}

type archiveRecoveryRelease struct {
	input  ArchiveRecoveryReclamation
	result ArchiveRecoveryReclamationResult
}

type archiveRecoveryRuntime interface {
	VerifyArchiveRecoveryVMForReclamation(context.Context, loopruntime.SessionReadbackFileRef) error
	RetireArchiveRecoveryVM(context.Context, loopruntime.SessionReadbackFileRef, archive.CheckoutReclamationEvidence) (loopruntime.ArchiveRecoveryVMRetirementReadback, error)
	ReleaseArchiveRecoveryHandle(context.Context, loopruntime.SessionReadbackFileRef) error
}

// A physically retired archive attempt keeps its canonical historical status.
// Only the original runtime can prove that it must not be reenrolled or reset.
func (l *Loop) archiveRecoveryStartupHold(order state.OrderNode, stage state.StageNode) (bool, error) {
	if stage.Runtime != "openshell" || len(stage.Attempts) == 0 || !l.isAdmittedLocalTask(order, stage) {
		return false, nil
	}
	attempt := stage.Attempts[len(stage.Attempts)-1]
	if !cleanupSessionName(attempt.SessionID) {
		return false, nil
	}
	path := filepath.Join(l.runtimeDir, "sessions", attempt.SessionID, "archive-recovery-cleanup-disposition.json")
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	reader, ok := l.deps.Runtimes[stage.Runtime].(interface {
		ArchiveRecoveryFrozenSession(sessionID, orderID, attemptID string) (bool, error)
	})
	if !ok {
		return false, fmt.Errorf("original archive recovery needs native restart ownership readback")
	}
	frozen, err := reader.ArchiveRecoveryFrozenSession(attempt.SessionID, order.OrderID, attempt.AttemptID)
	if err != nil {
		return false, err
	}
	if !frozen {
		return false, fmt.Errorf("original archive recovery disposition needs exact restart ownership readback")
	}
	return true, nil
}

func (l *Loop) archiveRecoveryStartupOrders() (map[string]bool, error) {
	if len(l.config.Runtime.AdmittedOrders) != 1 {
		return nil, nil
	}
	order, ok := l.canonical.Orders[l.config.Runtime.AdmittedOrders[0]]
	if !ok || len(order.Stages) != 1 {
		return nil, nil
	}
	held, err := l.archiveRecoveryStartupHold(order, order.Stages[0])
	if err != nil || !held {
		return nil, err
	}
	return map[string]bool{order.OrderID: true}, nil
}

func (l *Loop) archiveRecoveryOriginalCook(bound admission.MachineSessionBinding) (*cookHandle, *admission.Permit, error) {
	if _, adopted := l.cooks.adoptedTargets[bound.OrderID]; adopted {
		return nil, nil, fmt.Errorf("original archive recovery worker is adopted without its retained permit")
	}
	for _, session := range l.cooks.adoptedSessions {
		if session == bound.SessionID {
			return nil, nil, fmt.Errorf("original archive recovery worker is adopted without its retained permit")
		}
	}
	cook := l.cooks.activeCooksByOrder[bound.OrderID]
	if cook == nil {
		return nil, nil, nil
	}
	wrapped, ok := cook.session.(*resourceSession)
	if !ok || wrapped == nil || wrapped.SessionHandle == nil || wrapped.permit == nil || wrapped.ID() != bound.SessionID || cook.orderID != bound.OrderID ||
		cook.stageIndex != bound.StageIndex || cook.attempt != bound.RetryCount || cook.stage.TaskKey != "local-task" ||
		cook.dispatchedRuntime != bound.Runtime {
		return nil, nil, fmt.Errorf("retained worker differs from original archive recovery attempt")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(cook.worktreePath))
	if err != nil || filepath.Join(parent, filepath.Base(cook.worktreePath)) != bound.WorktreePath {
		return nil, nil, fmt.Errorf("retained worker differs from original archive recovery checkout")
	}
	select {
	case <-wrapped.Done():
		return cook, nil, nil
	default:
		return cook, wrapped.permit, nil
	}
}

func (l *Loop) releaseArchiveRecoveryCook(ctx context.Context, cook *cookHandle) error {
	if cook == nil {
		return nil
	}
	if l.cooks.activeCooksByOrder[cook.orderID] != cook {
		return fmt.Errorf("original archive recovery cook changed before resource release")
	}
	// Reclaim runs in the original Loop actor. Dropping its exact map entry
	// makes the completion watcher harmless without changing canonical history.
	delete(l.cooks.activeCooksByOrder, cook.orderID)
	if l.activeSummary.Total > 0 {
		l.activeSummary.Total--
	}
	decrementSummaryBucket(l.activeSummary.ByTaskKey, cook.stage.TaskKey)
	decrementSummaryBucket(l.activeSummary.ByStatus, "active")
	decrementSummaryBucket(l.activeSummary.ByRuntime, cook.dispatchedRuntime)
	select {
	case <-cook.session.Done():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ReclaimArchiveRecovery uses the existing Loop actor's ownership channel,
// without terminating, resuming or reclassifying the original worker.
func (l *Loop) ReclaimArchiveRecovery(ctx context.Context, scope ArchiveRecoveryReclamation) (ArchiveRecoveryReclamationResult, error) {
	selected := &archiveRecoveryRelease{input: scope}
	request := releaseRequest{ctx: ctx, reply: make(chan error, 1), archiveRecovery: selected}
	select {
	case l.releaseRequests <- request:
	case <-ctx.Done():
		return ArchiveRecoveryReclamationResult{}, ctx.Err()
	case <-l.runDone:
		return ArchiveRecoveryReclamationResult{}, fmt.Errorf("original Loop is unavailable")
	}
	select {
	case err := <-request.reply:
		return selected.result, err
	case <-ctx.Done():
		return ArchiveRecoveryReclamationResult{}, ctx.Err()
	}
}

func (l *Loop) reclaimArchiveRecovery(ctx context.Context, scope ArchiveRecoveryReclamation) (result ArchiveRecoveryReclamationResult, err error) {
	result = ArchiveRecoveryReclamationResult{Status: "archive_recovery_reclamation_waiting", Disposition: scope.Disposition,
		OriginalReturn: "UNKNOWN", Effects: []string{}}
	control, ack, lockPath := l.controlPaths()
	lock, err := lockfile.TryLock(lockPath)
	if err != nil {
		return result, err
	}
	defer lock.Close()
	bound := scope.Lease.Binding
	project, err := canonicalDirectory(l.projectDir)
	if err != nil {
		return result, err
	}
	if bound == nil || bound.ContextID != l.deps.AdmissionContext || bound.ProjectDir != project ||
		bound.Runtime != "openshell" || bound.StageIndex != 0 || bound.WorktreePath != scope.Checkout.Path ||
		len(l.config.Runtime.AdmittedOrders) != 1 || l.config.Runtime.AdmittedOrders[0] != bound.OrderID ||
		!l.deps.CleanupCompleted || l.state != StatePaused || l.deps.Admission == nil || l.deps.CheckoutArchivePolicy == nil ||
		l.resourceUnknown != nil {
		return result, fmt.Errorf("original paused archive recovery owner or cleanup policy differs")
	}
	for _, pending := range []string{control, filepath.Join(l.runtimeDir, "orders-next.json")} {
		data, readErr := os.ReadFile(pending)
		if len(data) != 0 || (readErr != nil && !os.IsNotExist(readErr)) {
			return result, fmt.Errorf("original Loop input remains pending: %s", filepath.Base(pending))
		}
	}
	if scope.Hold.Acknowledgements.Path != ack || loopruntime.VerifySessionReadbackFiles(map[string]loopruntime.SessionReadbackFileRef{
		"original_disposition": scope.Disposition, "original_pause": scope.Hold.Acknowledgements,
	}) != nil {
		return result, fmt.Errorf("original archive recovery disposition or pause acknowledgement changed")
	}
	order, ok := l.canonical.Orders[bound.OrderID]
	if !ok || len(order.Stages) != 1 {
		return result, fmt.Errorf("original archive recovery order is unavailable")
	}
	stage := order.Stages[0]
	if !l.isAdmittedLocalTask(order, stage) || len(stage.Attempts) != bound.RetryCount+1 || stage.Runtime != bound.Runtime {
		return result, fmt.Errorf("original archive recovery attempt or stage changed")
	}
	current := stage.Attempts[bound.RetryCount]
	if current.AttemptID != bound.AttemptID || current.SessionID != bound.SessionID ||
		!cleanupSessionName(current.WorktreeName) || scope.Checkout.Ref != "refs/heads/"+current.WorktreeName ||
		scope.Checkout.Head != scope.Head {
		return result, fmt.Errorf("original archive checkout or attempt differs")
	}
	cook, permit, err := l.archiveRecoveryOriginalCook(*bound)
	if err != nil {
		return result, err
	}
	settlement, err := l.deps.Admission.ReadArchiveRecoverySettlementForCleanup(scope.Lease, admission.MachineSettlementFile(scope.Disposition), permit)
	if err != nil {
		return result, err
	}
	if settlement != nil {
		if cook != nil {
			return result, fmt.Errorf("archive recovery still has the original worker handle")
		}
		err = sealArchiveRecoveryReclamation(&result, scope, settlement)
		return result, err
	}
	drain, drainErr := ReadArchiveRecoveryWorkerDrain(scope.Disposition, scope.Lease)
	if drainErr != nil && !os.IsNotExist(drainErr) {
		return result, drainErr
	}
	if drainErr == nil {
		if cook != nil {
			return result, fmt.Errorf("original worker drain receipt conflicts with a retained worker handle")
		}
		result.WorkerDrain = &drain
	}
	app, ok := l.deps.Worktree.(*worktree.App)
	if !ok {
		return result, fmt.Errorf("original checkout archive owner is unavailable")
	}
	runtime, ok := l.deps.Runtimes[bound.Runtime].(archiveRecoveryRuntime)
	if !ok {
		return result, fmt.Errorf("original archive recovery runtime owner is unavailable")
	}
	if cook != nil {
		if permit != nil {
			if err = runtime.ReleaseArchiveRecoveryHandle(ctx, scope.Disposition); err != nil {
				return result, err
			}
		}
		if err = l.releaseArchiveRecoveryCook(ctx, cook); err != nil {
			return result, err
		}
		// Dropping the known inert handle returns only its worker slot. Its
		// original machine lease stays held until checkout and VM proof agree.
		settlement, err = l.deps.Admission.ReadArchiveRecoverySettlement(scope.Lease, admission.MachineSettlementFile(scope.Disposition))
		if err != nil {
			return result, err
		}
		if settlement != nil {
			return result, fmt.Errorf("original archive recovery machine lease is no longer held")
		}
	}
	operation := "archive-recovery-checkout/" + scope.Disposition.SHA256
	policy := *l.deps.CheckoutArchivePolicy
	// After native removal only the original archive packet may prove absence;
	// do not try to recreate the checkout or an earlier lifecycle observation.
	_, statErr := os.Lstat(scope.Checkout.Path)
	if statErr == nil {
		if err = runtime.VerifyArchiveRecoveryVMForReclamation(ctx, scope.Disposition); err != nil {
			return result, err
		}
	} else if !os.IsNotExist(statErr) {
		return result, statErr
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	if statErr == nil && os.IsNotExist(drainErr) {
		path := filepath.Join(filepath.Dir(scope.Disposition.Path), ArchiveRecoveryWorkerDrainFile)
		if err = writeLandedMergeJSON(path, archiveRecoveryWorkerDrain(scope.Disposition, scope.Lease)); err != nil {
			return result, err
		}
		if drain, err = ReadArchiveRecoveryWorkerDrain(scope.Disposition, scope.Lease); err != nil {
			return result, err
		}
		result.WorkerDrain = &drain
	}
	expected := worktree.CheckoutCandidate{Head: scope.Head, Tree: scope.Tree, Branch: scope.Checkout.Ref}
	if err = app.ReclaimArchiveCheckout(ctx, current.WorktreeName, operation, expected, policy, scope.Checkout); err != nil {
		return result, err
	}
	packet, err := archive.ReadCheckoutPacket(policy, operation)
	if err != nil {
		return result, err
	}
	checkout, err := archive.ReadCheckoutReclamation(packet)
	if err != nil || checkout.Identity != scope.Checkout {
		return result, fmt.Errorf("original restored checkout removal is unproven: %v", err)
	}
	result.Checkout = &checkout
	vm, err := runtime.RetireArchiveRecoveryVM(ctx, scope.Disposition, checkout)
	if err != nil {
		return result, err
	}
	result.VM = &vm
	err = finalizeArchiveRecoveryReclamation(&result, l.deps.Admission, scope, operation, checkout, vm)
	return result, err
}

func archiveRecoveryWorkerDrain(disposition loopruntime.SessionReadbackFileRef, lease admission.MachineSessionLease) archiveRecoveryWorkerDrainReceipt {
	return archiveRecoveryWorkerDrainReceipt{Schema: 1, Kind: "archive_recovery_worker_drain", Disposition: disposition,
		Lease: lease, OriginalReturn: "UNKNOWN"}
}

// ReadArchiveRecoveryWorkerDrain binds the original actor's historical drain
// receipt to one disposition and exact lease. It supplies no Host takeover.
func ReadArchiveRecoveryWorkerDrain(disposition loopruntime.SessionReadbackFileRef, lease admission.MachineSessionLease) (loopruntime.SessionReadbackFileRef, error) {
	path := filepath.Join(filepath.Dir(disposition.Path), ArchiveRecoveryWorkerDrainFile)
	ref, err := loopruntime.ReadSessionReadbackFile(path, nil)
	if err != nil {
		return ref, err
	}
	if err = loopruntime.VerifySessionReadbackFiles(map[string]loopruntime.SessionReadbackFileRef{"original_disposition": disposition}); err != nil {
		return ref, fmt.Errorf("original archive recovery worker drain source changed: %v", err)
	}
	want, err := json.MarshalIndent(archiveRecoveryWorkerDrain(disposition, lease), "", "  ")
	if err != nil {
		return ref, err
	}
	if ref.Path != path || ref.SHA256 != fmt.Sprintf("%x", sha256.Sum256(append(want, '\n'))) {
		return ref, fmt.Errorf("original archive recovery worker drain receipt differs")
	}
	return ref, nil
}
