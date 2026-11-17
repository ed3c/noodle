package loop

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"time"

	"github.com/poteto/noodle/internal/admission"
	loopruntime "github.com/poteto/noodle/runtime"
)

type cleanupSessionRetirer interface {
	RetireCleanupSession(context.Context, loopruntime.CompletedSessionBinding, string, loopruntime.SessionStatus) (loopruntime.CleanupSessionRetirement, error)
}

func (l *Loop) bindResourceSession(permit *admission.Permit, req loopruntime.DispatchRequest, session string) error {
	stage, err := strconv.Atoi(req.EnvVars["NOODLE_STAGE_INDEX"])
	if err != nil {
		return fmt.Errorf("original resource stage binding unavailable: %w", err)
	}
	project, err := canonicalDirectory(l.projectDir)
	if err != nil {
		return err
	}
	worktree, err := canonicalDirectory(req.WorktreePath)
	if err != nil {
		return err
	}
	order := req.EnvVars["NOODLE_ORDER_ID"]
	return permit.BindSession(admission.MachineSessionBinding{ContextID: l.deps.AdmissionContext,
		ProjectDir: project, OrderID: order, StageIndex: stage, RetryCount: req.RetryCount,
		AttemptID: dispatchAttemptID(order, stage, req.RetryCount), SessionID: session, Runtime: req.Runtime, WorktreePath: worktree})
}

// Run startup passes false: it can consume only existing native retirement
// readback. Only the original applying cleanup actor may initiate VM retirement.
// Both paths require the same completed checkout proof and exact lease binding.
func (l *Loop) settleCompletedMachineSessions(applyVM bool) error {
	project, err := canonicalDirectory(l.projectDir)
	if err != nil {
		return err
	}
	var leases []admission.MachineSessionLease
	if l.deps.Admission != nil {
		leases, err = l.deps.Admission.MachineSessionLeases(l.deps.AdmissionContext, project)
		if err != nil {
			return err
		}
	}
	var bound []admission.MachineSessionLease
	var held []error
	for _, lease := range leases {
		if lease.Binding == nil {
			l.logger.Warn("machine resource lease remains held", "lease", lease.ID, "owner", lease.Owner, "reason", lease.HoldReason)
			if lease.ContextID == l.deps.AdmissionContext {
				held = append(held, fmt.Errorf("original resource lease %s has no native session binding", lease.ID))
			}
			continue
		}
		bound = append(bound, lease)
	}
	if len(bound) == 0 && (!applyVM || l.deps.CheckoutArchivePolicy == nil) {
		return errors.Join(held...)
	}
	rows, err := l.verifyCompletedCheckoutReclamation()
	if err != nil {
		return fmt.Errorf("original resource checkout proof remains held: %w", err)
	}
	bySession := make(map[string]completedCheckoutReclamation, len(rows))
	for _, row := range rows {
		if _, exists := bySession[row.SessionID]; exists {
			return fmt.Errorf("original resource settlement session is ambiguous")
		}
		bySession[row.SessionID] = row
	}
	for _, lease := range bound {
		row, exists := bySession[lease.Binding.SessionID]
		if !exists || row.OrderID != lease.Binding.OrderID || row.StageIndex != lease.Binding.StageIndex ||
			row.AttemptID != lease.Binding.AttemptID || row.RetryCount != lease.Binding.RetryCount ||
			row.Runtime != lease.Binding.Runtime || row.Archive.Identity.Path != lease.Binding.WorktreePath {
			return fmt.Errorf("original resource lease %s has no exact reclaimed session", lease.ID)
		}
	}
	for _, row := range rows {
		proof, err := l.machineReclamationProof(project, row, applyVM)
		if err != nil {
			held = append(held, fmt.Errorf("original runtime session %s remains held: %w", row.SessionID, err))
			continue
		}
		// Seal this fresh custody source before retiring the next session can
		// legitimately update the same original owner.json.
		for _, lease := range bound {
			if lease.Binding.SessionID != row.SessionID {
				continue
			}
			proof.Binding = *lease.Binding // Only the original dispatch may bind a nonce.
			if err := l.deps.Admission.SettleMachineSession(lease, proof); err != nil {
				held = append(held, fmt.Errorf("original resource lease %s remains held: %w", lease.ID, err))
			}
		}
	}
	return errors.Join(held...)
}

func (l *Loop) machineReclamationProof(project string, row completedCheckoutReclamation, applyVM bool) (admission.MachineReclamation, error) {
	proof := admission.MachineReclamation{CheckoutOperation: row.Operation,
		Sources: map[string]admission.MachineSettlementFile{
			"main_ack":                      {Path: row.MainACK.Path, SHA256: row.MainACK.SHA256},
			"checkout_packet":               {Path: row.Archive.Packet.Path, SHA256: row.Archive.Packet.SHA256},
			"checkout_removal":              {Path: row.Archive.Removal.Path, SHA256: row.Archive.Removal.SHA256},
			"checkout_verification_release": {Path: row.Archive.VerificationRelease.Path, SHA256: row.Archive.VerificationRelease.SHA256},
		}}
	switch row.Runtime {
	case "process":
		if err := observeAdmissionProcessSession(l.runtimeDir, row.SessionID); err != nil {
			return proof, err
		}
		for _, name := range []string{"spawn.json", "meta.json", "process.json"} {
			ref, err := loopruntime.ReadSessionReadbackFile(filepath.Join(l.runtimeDir, "sessions", row.SessionID, name), nil)
			if err != nil {
				return proof, err
			}
			proof.Sources["session/"+name] = admission.MachineSettlementFile{Path: ref.Path, SHA256: ref.SHA256}
		}
		proof.RuntimeState = "process_exited"
	case "openshell":
		reader, ok := l.deps.Runtimes[row.Runtime].(cleanupSessionReader)
		if !ok {
			return proof, fmt.Errorf("original VM resource reader unavailable")
		}
		stage := l.canonical.Orders[row.OrderID].Stages[row.StageIndex]
		attempt := stage.Attempts[row.RetryCount]
		expected := loopruntime.SessionStatus(row.AttemptStatus)
		if cleanupReviewRejected(attempt) {
			expected = loopruntime.StatusCompleted
		}
		original := loopruntime.CompletedSessionBinding{OrderID: row.OrderID, SessionID: row.SessionID,
			WorktreePath: filepath.Join(project, ".worktrees", row.WorktreeName), Provider: stage.Provider,
			Model: stage.Model, StageIndex: row.StageIndex, RetryCount: row.RetryCount}
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		if applyVM {
			retirer, ok := l.deps.Runtimes[row.Runtime].(cleanupSessionRetirer)
			if !ok {
				return proof, fmt.Errorf("original VM retirement owner unavailable")
			}
			retired, err := retirer.RetireCleanupSession(ctx, original, project, expected)
			if err != nil {
				return proof, err
			}
			if retired.State != loopruntime.CleanupSessionRetired || retired.OutcomeStatus != expected || retired.SessionID != row.SessionID {
				return proof, fmt.Errorf("original VM retirement is not completed")
			}
		}
		// Retirement mutates custody: consume its fresh original readback, not
		// the pre-retirement owner FileRef or the delete ACK alone.
		observed, err := reader.ReadCleanupSession(ctx, original, project, expected)
		if err != nil {
			return proof, err
		}
		if err := verifyCleanupRuntimeOutcome(observed, original, expected); err != nil {
			return proof, err
		}
		retired := observed.Execution.Retirement
		if retired == nil || retired.State != loopruntime.CleanupSessionRetired {
			return proof, fmt.Errorf("original VM retirement and disk absence remain unresolved")
		}
		ref := observed.Execution.Sources["native_custody"]
		proof.Sources["native_custody"] = admission.MachineSettlementFile{Path: ref.Path, SHA256: ref.SHA256}
		proof.RuntimeState, proof.NativeAttemptID, proof.SandboxID, proof.NativeDeletionOutcome = retired.State, retired.NativeAttemptID, retired.SandboxID, retired.NativeDeletionOutcome
	default:
		return proof, fmt.Errorf("original runtime %q has no resource retirement proof", row.Runtime)
	}
	return proof, nil
}

func verifyCleanupRuntimeOutcome(observed loopruntime.CleanupSessionReadback, binding loopruntime.CompletedSessionBinding, expected loopruntime.SessionStatus) error {
	e := observed.Execution
	if observed.OutcomeStatus != expected || observed.PublishedHead == "" || len(e.Sources) == 0 || !e.StopAcknowledged || e.NativeExecExitCode != 0 {
		return fmt.Errorf("cleanup original runtime returned incomplete outcome: %s", binding.SessionID)
	}
	if e.Retirement == nil {
		if e.NativePhase != "Stopped" {
			return fmt.Errorf("cleanup original runtime phase is unresolved")
		}
		return nil
	}
	r := e.Retirement
	ref := e.Sources["native_custody"]
	// A source-verified Retiring observation only permits continuation of the
	// existing intent. machineReclamationProof still requires Retired to settle.
	phase := e.NativePhase == "Retired" && r.State == loopruntime.CleanupSessionRetired || e.NativePhase == "Retiring" && r.State == "pending"
	if !phase || r.OutcomeStatus != expected ||
		r.SessionID != binding.SessionID || r.NativeAttemptID == "" || r.NativeAttemptID != e.NativeAttemptID ||
		r.SandboxID == "" || ref.Path == "" || ref.SHA256 == "" {
		return fmt.Errorf("cleanup original retirement identity or source closure is unresolved")
	}
	return loopruntime.VerifySessionReadbackFiles(e.Sources)
}
