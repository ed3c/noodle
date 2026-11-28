package runtime

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"time"

	dm "github.com/NVIDIA/OpenShell/sdk/go/proto/datamodelv1"
	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/worktree/archive"
)

type ArchiveRecoveryVMRetirementReadback struct {
	Retirement     CleanupSessionRetirement `json:"retirement"`
	Custody        SessionReadbackFileRef   `json:"custody"`
	OriginalReturn string                   `json:"original_return"`
}

type archiveRecoveryRetirementDisposition struct {
	Schema                int                           `json:"schema"`
	Kind                  string                        `json:"kind"`
	Candidate             SessionReadbackFileRef        `json:"candidate"`
	LandingProof          SessionReadbackFileRef        `json:"landing_proof"`
	Request               SessionReadbackFileRef        `json:"request"`
	Stop                  SessionReadbackFileRef        `json:"stop"`
	Archive               SessionReadbackFileRef        `json:"archive"`
	Hold                  ArchiveRecoveryDispatchHold   `json:"hold"`
	Checkout              archive.CheckoutIdentity      `json:"checkout"`
	Lease                 admission.MachineSessionLease `json:"lease"`
	ContextID             string                        `json:"context_id"`
	OrderID               string                        `json:"order_id"`
	AttemptID             string                        `json:"attempt_id"`
	SessionID             string                        `json:"session_id"`
	NativeAttemptID       string                        `json:"native_attempt_id"`
	SandboxID             string                        `json:"sandbox_id"`
	MainProcessInstanceID string                        `json:"main_process_instance_id"`
	Head                  string                        `json:"head"`
	Tree                  string                        `json:"tree"`
	OriginalReturn        string                        `json:"original_return"`
}

type archiveRecoveryVMProof struct {
	disposition archiveRecoveryRetirementDisposition
	candidate   archiveRecoveryCandidate
	capture     OpenShellWorkspaceCapture
	request     ProgressMaintenance
	dispatch    openShellDispatchReceipt
	sources     map[string]SessionReadbackFileRef
}

// RetireArchiveRecoveryVM runs only through the original resident custody. The
// historical unreturned/failed attempt remains unchanged; the separate archive
// Stop acknowledgement qualifies resource reclamation.
func (r *OpenShellRuntime) RetireArchiveRecoveryVM(ctx context.Context, disposition SessionReadbackFileRef, checkout archive.CheckoutReclamationEvidence) (ArchiveRecoveryVMRetirementReadback, error) {
	result := ArchiveRecoveryVMRetirementReadback{OriginalReturn: "UNKNOWN"}
	proof, err := readArchiveRecoveryVMProof(r.config.RuntimeDir, r.config.ContextID, r.config.Profile, disposition)
	if err != nil {
		return result, err
	}
	if err = proof.verifyCheckoutReclamation(checkout); err != nil {
		return result, err
	}
	task := proof.task()
	err = r.withArchiveRecoveryCustody(proof, func(custody *OpenShellCustody, storage openShellVMStorage) error {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		var retireErr error
		result.Retirement, retireErr = custody.retireCleanupSession(ctx, task, storage, proof.sources, &proof)
		result.Retirement.OutcomeStatus = proof.dispatch.Outcome.Status
		if retireErr != nil {
			return retireErr
		}
		var ledger OpenShellCustodyState
		result.Custody, retireErr = ReadSessionReadbackFile(filepath.Join(r.config.Profile.Gateway.Binding.StateRoot, "owner.json"), &ledger)
		if retireErr != nil {
			return retireErr
		}
		return verifySavedArchiveRecoveryVMRetirement(disposition, proof.disposition, result, ledger)
	})
	return result, err
}

// VerifyArchiveRecoveryVMForReclamation is the active original Owner readback
// before checkout removal. It does not return the in-process handle or lease.
func (r *OpenShellRuntime) VerifyArchiveRecoveryVMForReclamation(ctx context.Context, disposition SessionReadbackFileRef) error {
	return r.archiveRecoveryStoppedVM(ctx, disposition, false)
}

// ReleaseArchiveRecoveryHandle drops only the exact inert in-process handle
// after a fresh original native Stop readback. The machine lease stays held.
func (r *OpenShellRuntime) ReleaseArchiveRecoveryHandle(ctx context.Context, disposition SessionReadbackFileRef) error {
	return r.archiveRecoveryStoppedVM(ctx, disposition, true)
}

func (r *OpenShellRuntime) archiveRecoveryStoppedVM(ctx context.Context, disposition SessionReadbackFileRef, release bool) error {
	proof, err := readArchiveRecoveryVMProof(r.config.RuntimeDir, r.config.ContextID, r.config.Profile, disposition)
	if err != nil {
		return err
	}
	var completed *openShellSession
	err = r.withArchiveRecoveryCustody(proof, func(c *OpenShellCustody, storage openShellVMStorage) error {
		var err error
		completed, err = r.observeArchiveRecoveryStoppedVM(ctx, c, proof, storage, release)
		return err
	})
	if completed != nil {
		close(completed.done)
	}
	return err
}

// Called with the original runtime and gateway owners locked.
func (r *OpenShellRuntime) observeArchiveRecoveryStoppedVM(ctx context.Context, c *OpenShellCustody, proof archiveRecoveryVMProof, storage openShellVMStorage, release bool) (*openShellSession, error) {
	if !c.mu.TryLock() {
		return nil, fmt.Errorf("original custody operation in progress")
	}
	defer c.mu.Unlock()
	if err := c.mutable(); err != nil {
		return nil, err
	}
	if err := proof.observeStoppedVM(ctx, c.state, c.client, storage); err != nil {
		return nil, err
	}
	if release {
		if session := r.active[proof.disposition.SessionID]; session != nil && session.removeActiveLocked() {
			return session, nil
		}
	}
	return nil, nil
}

// ObserveOpenShellArchiveRecoveryStopped reads the exact original VM without
// using the old Host. The maintenance owner must hold the original custody
// lock throughout observation and any accounting transition that follows.
func ObserveOpenShellArchiveRecoveryStopped(ctx context.Context, runtimeDir, contextID string, profile OpenShellProfile, disposition, custody SessionReadbackFileRef) error {
	return observeOpenShellArchiveRecoveryAfterExit(ctx, runtimeDir, contextID, profile, disposition, custody, nil, nil)
}

// ObserveOpenShellArchiveRecoveryAfterExit accepts already-offered retirement
// only for the exact restored checkout and only after native and disk absence.
func ObserveOpenShellArchiveRecoveryAfterExit(ctx context.Context, runtimeDir, contextID string, profile OpenShellProfile, disposition, custody SessionReadbackFileRef, checkout *archive.CheckoutReclamationEvidence) error {
	return observeOpenShellArchiveRecoveryAfterExit(ctx, runtimeDir, contextID, profile, disposition, custody, checkout, nil)
}

// RetireOpenShellArchiveRecoveryAfterExit borrows the original Host exit fence.
// It never initializes custody, starts a Gateway, or restores the old worker.
func RetireOpenShellArchiveRecoveryAfterExit(ctx context.Context, runtimeDir, contextID string, profile OpenShellProfile, disposition, custody SessionReadbackFileRef, checkout archive.CheckoutReclamationEvidence, ownerLock *lockfile.Lock) (ArchiveRecoveryVMRetirementReadback, error) {
	result := ArchiveRecoveryVMRetirementReadback{OriginalReturn: "UNKNOWN"}
	if !ownerLock.HoldsExisting(filepath.Join(profile.Gateway.Binding.StateRoot, "owner.lock")) {
		return result, fmt.Errorf("original archive recovery requires the held custody lock")
	}
	err := observeOpenShellArchiveRecoveryAfterExit(ctx, runtimeDir, contextID, profile, disposition, custody, &checkout,
		func(ctx context.Context, proof archiveRecoveryVMProof, state OpenShellCustodyState, client pb.OpenShellClient, storage openShellVMStorage) error {
			if !ownerLock.HoldsExisting(filepath.Join(state.Binding.StateRoot, "owner.lock")) {
				return fmt.Errorf("original archive recovery requires the held custody lock")
			}
			// The checked exit fence owns this existing ledger for the transaction.
			original := &OpenShellCustody{state: state, client: client, initialized: true}
			var err error
			result.Retirement, err = original.retireCleanupSession(ctx, proof.task(), storage, proof.sources, &proof)
			result.Retirement.OutcomeStatus = proof.dispatch.Outcome.Status
			if err != nil {
				return err
			}
			var saved OpenShellCustodyState
			result.Custody, err = ReadSessionReadbackFile(custody.Path, &saved)
			if err != nil {
				return err
			}
			return verifySavedArchiveRecoveryVMRetirement(disposition, proof.disposition, result, saved)
		})
	return result, err
}

func observeOpenShellArchiveRecoveryAfterExit(ctx context.Context, runtimeDir, contextID string, profile OpenShellProfile, disposition, custody SessionReadbackFileRef, checkout *archive.CheckoutReclamationEvidence, effect func(context.Context, archiveRecoveryVMProof, OpenShellCustodyState, pb.OpenShellClient, openShellVMStorage) error) error {
	proof, err := readArchiveRecoveryVMProof(runtimeDir, contextID, profile, disposition)
	if err != nil {
		return err
	}
	storage, err := openShellVMStorageForProfile(profile)
	if err != nil {
		return err
	}
	var state OpenShellCustodyState
	if custody.Path != filepath.Join(profile.Gateway.Binding.StateRoot, "owner.json") ||
		maintenanceVerify(custody, &state) != nil {
		return fmt.Errorf("original archive recovery custody differs or is blocked")
	}
	var pending *OpenShellCustodyRecord
	if checkout != nil {
		if err = proof.verifyCheckoutReclamation(*checkout); err != nil {
			return err
		}
		for _, row := range state.Records {
			if row.Task == proof.task() && row.Retirement != nil {
				if pending != nil || !maps.Equal(row.Retirement.Sources, proof.sources) || !reflect.DeepEqual(row.Retirement.Storage, storage) {
					return fmt.Errorf("original archive recovery prior VM retirement differs")
				}
				pending = &row
			}
		}
	}
	var recovering []OpenShellTask
	if pending != nil {
		recovering = []OpenShellTask{pending.Task}
	}
	if err = qualifyIndependentArchiveCustody(state, profile.Gateway, proof.request.SessionDirectory, recovering...); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := newOpenShellConnection(profile.Gateway.Binding.Gateway, profile.Gateway.TLS)
	if err != nil {
		return err
	}
	defer conn.Close()
	client := pb.NewOpenShellClient(conn)
	if pending != nil {
		if err := proof.verifyStop(state, *pending); err != nil {
			return err
		}
	}
	// Inventory refuses a sandbox with an offered retirement if it is still
	// present, and verifies original disk absence before readback can resume.
	if err = observeIndependentArchiveInventory(ctx, state, client); err != nil {
		return err
	}
	if pending == nil {
		if err = proof.observeStoppedVM(ctx, state, client, storage); err != nil {
			return err
		}
	}
	if err = VerifySessionReadbackFiles(map[string]SessionReadbackFileRef{"original_custody": custody}); err != nil || effect == nil {
		return err
	}
	return effect(ctx, proof, state, client, storage)
}

func (proof archiveRecoveryVMProof) observeStoppedVM(ctx context.Context, state OpenShellCustodyState, client pb.OpenShellClient, storage openShellVMStorage) error {
	var record *OpenShellCustodyRecord
	for i := range state.Records {
		row := &state.Records[i]
		if row.Task == proof.task() {
			if record != nil {
				return fmt.Errorf("original archive recovery VM is ambiguous")
			}
			record = row
		}
	}
	if record == nil || record.Retirement != nil {
		return fmt.Errorf("original archive recovery Stopped VM is unavailable or already reserved")
	}
	if err := proof.verifyStop(state, *record); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	selector := &dm.WorkspaceSelector{Selection: &dm.WorkspaceSelector_Workspace{Workspace: state.Binding.Workspace}}
	response, err := client.GetSandbox(ctx, &pb.GetSandboxRequest{Name: record.Name, WorkspaceScope: selector})
	if err != nil {
		return err
	}
	if err = verifyArchiveRecoveryStopped(*record, response); err != nil {
		return err
	}
	if response.GetSandbox().GetMetadata().GetResourceVersion() == 0 {
		return fmt.Errorf("original archive recovery VM conditional retirement identity is incomplete")
	}
	if _, err = storage.sandboxPresent(record.Worker.Identity.Sandbox.SandboxID); err != nil {
		return err
	}
	return VerifySessionReadbackFiles(proof.sources)
}

func (r *OpenShellRuntime) withArchiveRecoveryCustody(proof archiveRecoveryVMProof, run func(*OpenShellCustody, openShellVMStorage) error) error {
	if !r.mu.TryLock() {
		return fmt.Errorf("original runtime operation in progress")
	}
	defer r.mu.Unlock()
	if _, err := r.inertArchiveRecoveryHandle(proof); err != nil {
		return err
	}
	storage, err := openShellVMStorageForProfile(r.config.Profile)
	if err != nil {
		return err
	}
	gateway := r.config.Gateway
	if gateway == nil || !gateway.mu.TryLock() {
		return fmt.Errorf("original archive cleanup gateway unavailable or busy")
	}
	defer gateway.mu.Unlock()
	custody, _, err := gateway.completionOwner(r.config.ContextID)
	if err != nil {
		return err
	}
	return run(custody, storage)
}

// ArchiveRecoveryFrozenSession tells Loop whether the exact archived session
// can stay frozen at startup. Its original machine lease can still be held.
func (r *OpenShellRuntime) ArchiveRecoveryFrozenSession(sessionID, orderID, attemptID string) (bool, error) {
	if sessionID == "" || filepath.Base(sessionID) != sessionID || sessionID == "." || sessionID == ".." {
		return false, fmt.Errorf("invalid original archive recovery session ID")
	}
	path := filepath.Join(r.config.RuntimeDir, "sessions", sessionID, "openshell-dispatch.json")
	disposition, err := r.archiveRecoveryRetiredAtStartup(path)
	if err != nil || disposition == nil {
		return false, err
	}
	if disposition.OrderID != orderID || disposition.AttemptID != attemptID {
		return false, fmt.Errorf("original archive recovery startup attempt differs")
	}
	return true, nil
}

func (r *OpenShellRuntime) archiveRecoveryRetiredAtStartup(dispatchPath string) (*archiveRecoveryRetirementDisposition, error) {
	dir := filepath.Dir(dispatchPath)
	path := filepath.Join(dir, "archive-recovery-cleanup-disposition.json")
	var disposition archiveRecoveryRetirementDisposition
	ref, err := ReadSessionReadbackFile(path, &disposition)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if ref.Path != path || disposition.Schema != 1 || disposition.Kind != "archive_recovery_cleanup_disposition" ||
		disposition.SessionID != filepath.Base(dir) || disposition.ContextID != r.config.ContextID ||
		disposition.OriginalReturn != "UNKNOWN" || disposition.Lease.Binding == nil ||
		disposition.Lease.Binding.ContextID != disposition.ContextID || disposition.Lease.Binding.SessionID != disposition.SessionID ||
		disposition.Lease.Binding.AttemptID != disposition.AttemptID || disposition.Lease.Binding.OrderID != disposition.OrderID {
		return nil, fmt.Errorf("original archive recovery startup disposition differs")
	}
	var state OpenShellCustodyState
	if _, err = ReadSessionReadbackFile(filepath.Join(r.config.Profile.Gateway.Binding.StateRoot, "owner.json"), &state); err != nil {
		return nil, err
	}
	if state.Binding != r.config.Profile.Gateway.Binding {
		return nil, fmt.Errorf("original archive recovery startup custody differs")
	}
	var retired *OpenShellCustodyRecord
	for i := range state.Records {
		record := &state.Records[i]
		if record.Task != (OpenShellTask{disposition.OrderID, disposition.NativeAttemptID, disposition.SessionID}) {
			continue
		}
		if retired != nil {
			return nil, fmt.Errorf("original archive recovery startup custody is ambiguous")
		}
		retired = record
	}
	if retired == nil || retired.Retirement == nil || !retired.Retirement.Retired ||
		retired.Retirement.Sources["archive_recovery_disposition"] != ref ||
		retired.Worker.Identity.Sandbox.SandboxID != disposition.SandboxID ||
		retired.MainProcessInstanceID != disposition.MainProcessInstanceID {
		return nil, fmt.Errorf("original archive recovery startup lacks confirmed retirement; preserve frozen dispatch")
	}
	if err = validateVMRetirement(*retired); err != nil {
		return nil, err
	}
	var receipt openShellDispatchReceipt
	actual, err := ReadSessionReadbackFile(dispatchPath, &receipt)
	if err != nil {
		return nil, err
	}
	if actual != retired.Retirement.Sources["archive_recovery_dispatch"] ||
		receipt.ContextID != disposition.ContextID || receipt.OrderID != disposition.OrderID ||
		receipt.SessionID != disposition.SessionID || receipt.AttemptID != disposition.NativeAttemptID ||
		receipt.Phase != "progress-unknown" || receipt.Error == "" || receipt.Outcome.Status != StatusFailed {
		return nil, fmt.Errorf("original archive recovery startup frozen dispatch differs")
	}
	directory, err := admission.DefaultMachineCapacityDirectory()
	if err != nil {
		return nil, err
	}
	proof, err := admission.ReadArchivedArchiveRecoverySettlement(directory, disposition.Lease, admission.MachineSettlementFile(ref))
	if err != nil {
		return nil, err
	}
	sources := retired.Retirement.Sources
	if err = VerifySessionReadbackFiles(sources); err != nil {
		return nil, err
	}
	checkout := archive.CheckoutReclamationEvidence{
		Identity:            disposition.Checkout,
		Packet:              archive.FileRef(sources["checkout_packet"]),
		Removal:             archive.FileRef(sources["checkout_removal"]),
		VerificationRelease: archive.FileRef(sources["checkout_verification_release"]),
	}
	bound := archiveRecoveryVMProof{disposition: disposition, sources: map[string]SessionReadbackFileRef{}}
	if err = bound.verifyCheckoutReclamation(checkout); err != nil {
		return nil, err
	}
	// The ledger also verifies an original lease that remains held. That lease
	// can finish settlement without reopening the retired worker.
	if proof != nil {
		result := retirementResult(*retired)
		if proof.RuntimeState != result.State || proof.NativeAttemptID != result.NativeAttemptID ||
			proof.SandboxID != result.SandboxID || proof.NativeDeletionOutcome != result.NativeDeletionOutcome {
			return nil, fmt.Errorf("original archive recovery startup machine settlement differs")
		}
		for _, key := range []string{"archive_recovery_landing", "archive_recovery_stop", "checkout_packet", "checkout_removal", "checkout_verification_release"} {
			if proof.Sources[key] != admission.MachineSettlementFile(sources[key]) {
				return nil, fmt.Errorf("original archive recovery startup settlement source differs: %s", key)
			}
		}
	}
	present, err := retired.Retirement.Storage.sandboxPresent(disposition.SandboxID)
	if err != nil {
		return nil, err
	}
	if present {
		return nil, fmt.Errorf("original archive recovery retired disk reappeared")
	}
	return &disposition, nil
}

// Called with r.mu held. Only the exact live-process handle with a positive run
// exit may remain registered across this archive reclamation route.
func (r *OpenShellRuntime) inertArchiveRecoveryHandle(proof archiveRecoveryVMProof) (*openShellSession, error) {
	if len(r.active) == 0 {
		return nil, nil
	}
	session := r.active[proof.disposition.SessionID]
	if len(r.active) != 1 || session == nil || session.owner != r || session.id != proof.disposition.SessionID || session.dir != proof.request.SessionDirectory {
		return nil, fmt.Errorf("original archive cleanup runtime has other or different active sessions")
	}
	select {
	case <-session.runExited:
	default:
		return nil, fmt.Errorf("original archive cleanup runtime execution exit is unobserved")
	}
	select {
	case <-session.progressStopDone:
	default:
		return nil, fmt.Errorf("original archive cleanup runtime supervisor control is unresolved")
	}
	session.mu.Lock()
	defer session.mu.Unlock()
	if session.receipt.Phase != "progress-unknown" || session.receipt.Error == "" ||
		session.receipt.Outcome.Status != StatusFailed || !reflect.DeepEqual(session.receipt, proof.dispatch) {
		return nil, fmt.Errorf("original archive cleanup runtime handle differs from frozen dispatch")
	}
	return session, nil
}

func (p *archiveRecoveryVMProof) task() OpenShellTask {
	return OpenShellTask{p.disposition.OrderID, p.disposition.NativeAttemptID, p.disposition.SessionID}
}

func (p *archiveRecoveryVMProof) verifyCheckoutReclamation(checkout archive.CheckoutReclamationEvidence) error {
	if p.disposition.Checkout != checkout.Identity || checkout.Packet.Path == "" ||
		checkout.Removal.Path == "" || checkout.VerificationRelease.Path == "" {
		return fmt.Errorf("archive recovery VM original checkout identity differs")
	}
	var packet archive.CheckoutPacket
	if err := maintenanceVerify(SessionReadbackFileRef(checkout.Packet), &packet); err != nil {
		return err
	}
	archiveEvidence, err := archive.ReadCheckoutReclamation(packet)
	if err != nil || archiveEvidence != checkout {
		return fmt.Errorf("archive recovery VM checkout restoration/removal proof differs: %v", err)
	}
	p.sources["checkout_packet"] = SessionReadbackFileRef(checkout.Packet)
	p.sources["checkout_removal"] = SessionReadbackFileRef(checkout.Removal)
	p.sources["checkout_verification_release"] = SessionReadbackFileRef(checkout.VerificationRelease)
	return nil
}

func readArchiveRecoveryVMProof(runtimeDir, contextID string, profile OpenShellProfile, ref SessionReadbackFileRef) (proof archiveRecoveryVMProof, err error) {
	d := &proof.disposition
	if err = maintenanceVerify(ref, d); err != nil {
		return proof, err
	}
	sessionDir := filepath.Join(runtimeDir, "sessions", d.SessionID)
	if d.Schema != 1 || d.Kind != "archive_recovery_cleanup_disposition" || d.OriginalReturn != "UNKNOWN" ||
		d.ContextID != contextID || d.ContextID == "" || d.SessionID == "" || d.SessionID == "." || d.SessionID == ".." ||
		filepath.Base(d.SessionID) != d.SessionID || ref.Path != filepath.Join(sessionDir, "archive-recovery-cleanup-disposition.json") ||
		!filepath.IsAbs(d.Checkout.Path) || d.Checkout.Incarnation == "" {
		return proof, fmt.Errorf("archive recovery VM original disposition or checkout identity differs")
	}
	candidate := &proof.candidate
	if err = maintenanceVerify(d.Candidate, candidate); err != nil {
		return proof, err
	}
	if d.Candidate.Path != filepath.Join(sessionDir, "archive-recovery-candidate.json") ||
		candidate.Intent.Path != filepath.Join(sessionDir, "archive-recovery-apply-intent.json") ||
		candidate.Schema != 1 || candidate.Kind != "archive_recovery_candidate" || candidate.OriginalReturn != "UNKNOWN" ||
		candidate.AuthorizesLanding || candidate.Request != d.Request || candidate.Stop != d.Stop ||
		candidate.OrderID != d.OrderID || candidate.AttemptID != d.AttemptID || candidate.SessionID != d.SessionID ||
		candidate.NativeAttemptID != d.NativeAttemptID || candidate.SandboxID != d.SandboxID ||
		candidate.MainProcessInstanceID != d.MainProcessInstanceID || candidate.CandidateHead != d.Head ||
		candidate.CandidateTree != d.Tree || d.Checkout.Head != d.Head ||
		d.LandingProof.Path != filepath.Join(filepath.Dir(d.Request.Path), "archive-landing-proof.json") {
		return proof, fmt.Errorf("archive recovery VM candidate differs from original disposition")
	}
	var intent archiveRecoveryApplyRecord
	if err = maintenanceVerify(candidate.Intent, &intent); err != nil {
		return proof, err
	}
	if intent != candidate.archiveRecoveryApplyRecord {
		return proof, fmt.Errorf("archive recovery VM original Apply intent differs")
	}
	capture, err := ReadOpenShellWorkspaceCapture(candidate.Capture)
	if err != nil {
		return proof, err
	}
	if capture.OrderID != d.OrderID || capture.SessionID != d.SessionID || capture.NativeAttemptID != d.NativeAttemptID ||
		capture.SandboxID != d.SandboxID || capture.MainProcessInstanceID != d.MainProcessInstanceID ||
		capture.Archive != d.Archive || capture.Sources["request"] != d.Request || capture.BaselineHead != candidate.BaselineHead ||
		capture.BaselineSource.Path != filepath.Join(sessionDir, "openshell-dispatch.json") {
		return proof, fmt.Errorf("archive recovery VM original capture differs")
	}
	if err = maintenanceVerify(capture.BaselineSource, &proof.dispatch); err != nil {
		return proof, err
	}
	original := proof.dispatch
	profileDigest, err := OpenShellDispatchSHA256(profile)
	if err != nil {
		return proof, err
	}
	target, err := filepath.EvalSymlinks(filepath.Dir(original.Target))
	if err != nil || filepath.Join(target, filepath.Base(original.Target)) != d.Checkout.Path ||
		original.Version != 1 || original.ProfileSHA256 != profileDigest ||
		original.OrderID != d.OrderID || original.SessionID != d.SessionID || original.AttemptID != d.NativeAttemptID ||
		original.ContextID != d.ContextID || original.Worker == nil || original.Phase != "progress-unknown" || original.Error == "" ||
		original.Outcome.Status != StatusFailed || original.Progress != nil || original.Delivery != nil {
		return proof, fmt.Errorf("archive recovery VM original failed dispatch differs")
	}
	if err = verifyRetainedArchiveRecoveryBytes(capture.Archive); err != nil {
		return proof, err
	}
	proof.request = ProgressMaintenance{Request: d.Request, Profile: capture.Sources["profile"],
		ContextID: contextID, SessionDirectory: sessionDir}
	proof.sources = map[string]SessionReadbackFileRef{
		"archive_recovery_disposition": ref, "archive_recovery_landing": d.LandingProof, "archive_recovery_candidate": d.Candidate,
		"archive_recovery_apply_intent": candidate.Intent, "archive_recovery_capture": candidate.Capture,
		"archive_recovery_review": candidate.Review, "archive_recovery_stop": d.Stop, "archive_recovery_original_request": d.Request,
		"archive_recovery_dispatch": capture.BaselineSource, "archive_recovery_dispatch_hold": d.Hold.Acknowledgements,
	}
	return proof, VerifySessionReadbackFiles(proof.sources)
}

func validateArchiveRecoveryStoppedWorker(record OpenShellCustodyRecord) error {
	if !record.CreateAcknowledged || record.Retained || record.Name == "" || record.MainProcessInstanceID == "" ||
		record.Worker.Identity.Sandbox.SandboxID == "" || record.StopRequestID != "" || record.StopAcknowledged ||
		record.Worker.StopPending || record.Worker.StopError != "" || record.ProgressStop != nil || record.Exec == nil {
		return fmt.Errorf("archive recovery VM original worker or Stop effects differ")
	}
	e := record.Exec
	if e.AttemptID != record.Task.AttemptID || e.Command.RequestID == "" || e.Result.RequestID != e.Command.RequestID ||
		!e.Result.StartAttempted || !e.Result.ExitObserved || e.Result.ExitCode != 74 || e.Result.InputError != "" || e.Result.StreamError != "" {
		return fmt.Errorf("archive recovery VM original failed archive exit differs")
	}
	return nil
}

func (p *archiveRecoveryVMProof) verifyStop(state OpenShellCustodyState, record OpenShellCustodyRecord) error {
	if err := validateArchiveRecoveryStoppedWorker(record); err != nil {
		return err
	}
	if record.Worker.Identity != p.dispatch.Worker.Identity || record.Exec.Result != p.dispatch.Worker.ExecResult ||
		record.Task != p.task() || record.Worker.Identity.OrderID != record.Task.OrderID ||
		record.Worker.Identity.AttemptID != record.Task.AttemptID || record.Worker.Identity.SessionID != record.Task.SessionID ||
		record.MainProcessInstanceID != p.disposition.MainProcessInstanceID || record.Worker.Identity.Sandbox.SandboxID != p.disposition.SandboxID {
		return fmt.Errorf("archive recovery VM current custody differs from original dispatch")
	}
	input := OpenShellArchiveRecoveryApplyInput{Review: p.candidate.Review, Capture: p.candidate.Capture,
		Stop: p.disposition.Stop, DispatchHold: p.disposition.Hold}
	return verifyArchiveRecoveryApplyStop(p.request, input, storageFailureInputs{state: state, record: record})
}

// VerifyArchiveRecoveryVMRetirementReadback binds a machine settlement to the
// precise retired original custody row before its lease can release capacity.
func VerifyArchiveRecoveryVMRetirementReadback(ref SessionReadbackFileRef, observed ArchiveRecoveryVMRetirementReadback) error {
	var disposition archiveRecoveryRetirementDisposition
	if err := maintenanceVerify(ref, &disposition); err != nil {
		return err
	}
	if disposition.Schema != 1 || disposition.Kind != "archive_recovery_cleanup_disposition" ||
		disposition.OriginalReturn != "UNKNOWN" || observed.OriginalReturn != "UNKNOWN" ||
		observed.Retirement.OutcomeStatus != StatusFailed || observed.Retirement.State != CleanupSessionRetired {
		return fmt.Errorf("archive recovery VM original retirement disposition is incomplete")
	}
	var candidate archiveRecoveryCandidate
	if err := maintenanceVerify(disposition.Candidate, &candidate); err != nil {
		return err
	}
	capture, err := ReadOpenShellWorkspaceCapture(candidate.Capture)
	if err != nil {
		return err
	}
	if observed.Custody.Path != capture.Sources["custody"].Path {
		return fmt.Errorf("archive recovery VM retirement custody is outside the original binding")
	}
	var state OpenShellCustodyState
	if err = maintenanceVerify(observed.Custody, &state); err != nil {
		return err
	}
	return verifySavedArchiveRecoveryVMRetirement(ref, disposition, observed, state)
}

func verifySavedArchiveRecoveryVMRetirement(ref SessionReadbackFileRef, disposition archiveRecoveryRetirementDisposition, observed ArchiveRecoveryVMRetirementReadback, state OpenShellCustodyState) error {
	var saved *OpenShellCustodyRecord
	for i := range state.Records {
		record := &state.Records[i]
		if record.Task != (OpenShellTask{disposition.OrderID, disposition.NativeAttemptID, disposition.SessionID}) {
			continue
		}
		if saved != nil {
			return fmt.Errorf("archive recovery VM retired original record is ambiguous")
		}
		saved = record
	}
	if saved == nil || saved.Worker.Identity.Sandbox.SandboxID != disposition.SandboxID ||
		saved.MainProcessInstanceID != disposition.MainProcessInstanceID ||
		saved.Retirement == nil || !saved.Retirement.Retired || saved.Retirement.Sources["archive_recovery_disposition"] != ref {
		return fmt.Errorf("archive recovery VM retired original record is missing or differs")
	}
	if err := validateVMRetirement(*saved); err != nil {
		return err
	}
	result := retirementResult(*saved)
	result.OutcomeStatus = StatusFailed
	if result != observed.Retirement {
		return fmt.Errorf("archive recovery VM original retirement result differs")
	}
	return nil
}
