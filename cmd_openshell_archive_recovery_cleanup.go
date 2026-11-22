package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/execution"
	"github.com/poteto/noodle/internal/lockfile"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/poteto/noodle/worktree"
	"github.com/spf13/cobra"
)

type archiveRecoveryCleanupPlan struct {
	Schema                int                                    `json:"schema"`
	Owner                 string                                 `json:"owner"`
	Status                string                                 `json:"status"`
	Candidate             loopruntime.SessionReadbackFileRef     `json:"candidate"`
	LandingProof          loopruntime.SessionReadbackFileRef     `json:"landing_proof"`
	Request               loopruntime.SessionReadbackFileRef     `json:"request"`
	Stop                  loopruntime.SessionReadbackFileRef     `json:"stop"`
	Archive               loopruntime.SessionReadbackFileRef     `json:"archive"`
	OrderID               string                                 `json:"order_id"`
	AttemptID             string                                 `json:"attempt_id"`
	SessionID             string                                 `json:"session_id"`
	NativeAttemptID       string                                 `json:"native_attempt_id"`
	SandboxID             string                                 `json:"sandbox_id"`
	MainProcessInstanceID string                                 `json:"main_process_instance_id"`
	Head                  string                                 `json:"head"`
	Tree                  string                                 `json:"tree"`
	OriginalReturn        string                                 `json:"original_return"`
	Effects               []string                               `json:"effects"`
	Disposition           *loopruntime.SessionReadbackFileRef    `json:"disposition,omitempty"`
	WorkerDrain           *loopruntime.SessionReadbackFileRef    `json:"worker_drain,omitempty"`
	Reclamation           *loopruntime.SessionReadbackFileRef    `json:"reclamation,omitempty"`
	HostRetirement        *archiveRecoveryHostRetirementReadback `json:"host_retirement,omitempty"`
	HostActivation        *loopruntime.SessionReadbackFileRef    `json:"host_activation,omitempty"`
	IndependentExit       *archiveRecoveryIndependentExit        `json:"independent_exit,omitempty"`
	SettlementProblem     string                                 `json:"settlement_problem,omitempty"`
	DispositionSHA256     string                                 `json:"disposition_sha256,omitempty"`
	archiveRecoveryContinuation
}

func newArchiveRecoveryCleanupCmd(reserve bool) *cobra.Command {
	var candidate, candidateSHA, landing, landingSHA, dispositionSHA string
	name, description := "plan-workspace-capture-cleanup", "Qualify the original recovered candidate for post-Landing resource cleanup"
	if reserve {
		name, description = archiveRecoveryCleanupReserveOperation, "Record the exact original archive cleanup disposition without resource effects"
	}
	cmd := &cobra.Command{Use: name, Short: description, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
		defer cancel()
		var selectCandidate struct {
			Review loopruntime.SessionReadbackFileRef `json:"review"`
		}
		ref, err := loopruntime.ReadSessionReadbackFile(candidate, &selectCandidate)
		if err != nil {
			return err
		}
		if ref.Path != candidate || ref.SHA256 != candidateSHA {
			return fmt.Errorf("archive recovery cleanup selected candidate changed")
		}
		result := archiveRecoveryCleanupPlan{Schema: 1, Owner: "Noodle", Candidate: ref, OriginalReturn: "UNKNOWN", Effects: []string{}}
		result.LandingProof = loopruntime.SessionReadbackFileRef{Path: landing, SHA256: landingSHA}
		if (landing == "") != (landingSHA == "") {
			return fmt.Errorf("archive recovery cleanup requires the exact Owner Landing proof FileRef")
		}
		if reserve && (landing == "" || dispositionSHA == "") {
			return fmt.Errorf("archive recovery cleanup requires the original approved Landing and disposition digest")
		}
		retained := false
		if landing != "" {
			retained, err = readReservedArchiveRecoveryCleanup(&result, dispositionSHA)
			if err != nil {
				return err
			}
		}
		if !retained {
			if _, err = runOriginalArchiveRecoveryControl(ctx, selectCandidate.Review, archiveRecoveryControl{cleanup: &result, cleanupExpected: dispositionSHA}); err != nil {
				return err
			}
		}
		if result.Disposition != nil {
			if err = planArchiveRecoveryReclaim(ctx, &result); err != nil {
				return err
			}
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
	}}
	cmd.Flags().StringVar(&candidate, "candidate-file", "", "Original Noodle archive-recovery-candidate.json")
	cmd.Flags().StringVar(&candidateSHA, "expected-candidate-sha256", "", "Exact original candidate digest")
	cmd.Flags().StringVar(&landing, "landing-proof-file", "", "Original Soodles Owner-confirmed Cloud Landing proof")
	cmd.Flags().StringVar(&landingSHA, "expected-landing-proof-sha256", "", "Exact original Landing proof digest")
	_ = cmd.MarkFlagRequired("candidate-file")
	_ = cmd.MarkFlagRequired("expected-candidate-sha256")
	if reserve {
		cmd.Flags().StringVar(&dispositionSHA, "expected-disposition-sha256", "", "Exact original Owner planned disposition digest")
		_ = cmd.MarkFlagRequired("landing-proof-file")
		_ = cmd.MarkFlagRequired("expected-landing-proof-sha256")
		_ = cmd.MarkFlagRequired("expected-disposition-sha256")
	}
	return cmd
}

func readReservedArchiveRecoveryCleanup(result *archiveRecoveryCleanupPlan, expected string) (bool, error) {
	path := filepath.Join(filepath.Dir(result.Candidate.Path), archiveRecoveryCleanupDispositionFile)
	ref, err := loopruntime.ReadSessionReadbackFile(path, nil)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	d, _, err := readOriginalArchiveRecoveryDisposition(ref)
	if err != nil {
		return false, err
	}
	if (expected != "" && expected != ref.SHA256) || d.Candidate != result.Candidate || d.LandingProof != result.LandingProof {
		return false, fmt.Errorf("original reclamation disposition differs from selected candidate or Landing")
	}
	result.Request, result.Stop, result.Archive = d.Request, d.Stop, d.Archive
	result.OrderID, result.AttemptID, result.SessionID = d.OrderID, d.AttemptID, d.SessionID
	result.NativeAttemptID, result.SandboxID, result.MainProcessInstanceID = d.NativeAttemptID, d.SandboxID, d.MainProcessInstanceID
	result.Head, result.Tree = d.Head, d.Tree
	result.Status, result.Disposition, result.DispositionSHA256 = "archive_recovery_cleanup_verified", &ref, ref.SHA256
	if err = verifyArchiveRecoveryLanding(*result); err != nil {
		return false, err
	}
	return true, nil
}

func inspectArchiveRecoveryCleanup(ctx context.Context, result *archiveRecoveryCleanupPlan, p loopruntime.ProgressMaintenance, owner *hostOwnerBinding, binding hostRequestBinding, cfg config.Config, profile loopruntime.OpenShellProfile, reviewRef loopruntime.SessionReadbackFileRef, review archiveRecoveryReview, capture loopruntime.OpenShellWorkspaceCapture, cleanupExpected string, verifyOwner func() error) error {
	if cleanupExpected != "" {
		lock, err := lockfile.TryLock(filepath.Join(p.SessionDirectory, "maintenance.lock"))
		if err != nil {
			return err
		}
		defer lock.Close()
	}
	runtimeDir := filepath.Dir(filepath.Dir(p.SessionDirectory))
	hold, err := archiveRecoveryPaused(runtimeDir)
	if err != nil {
		return err
	}
	subject, err := readArchiveRecoveryScopeBinding(ctx, binding.ProjectDir, cfg, capture)
	if err != nil {
		return err
	}
	if !binding.CleanupCompleted || subject.Attempt.AttemptID != review.AttemptID {
		return fmt.Errorf("archive recovery cleanup original ownership or policy differs")
	}
	owner.Sources["canonical_snapshot"], owner.Sources["integration_binding"] = subject.Snapshot, subject.Integration
	stop, err := loopruntime.ReadSessionReadbackFile(filepath.Join(p.SessionDirectory, "archive-recovery-stop", "archive-recovery-stop.json"), nil)
	if err != nil {
		return err
	}
	input := loopruntime.OpenShellArchiveRecoveryApplyInput{Review: reviewRef, Capture: review.Capture, Stop: stop, DispatchHold: hold, AttemptID: subject.Attempt.AttemptID, Tree: review.Tree, Scope: subject.Scope, Target: subject.Custody.Physical}
	if err = verifyOwner(); err != nil {
		return err
	}
	native, err := loopruntime.ObserveOpenShellArchiveRecoveryCleanup(ctx, p, profile, input, result.Candidate)
	if err != nil {
		return err
	}
	directory, err := admission.DefaultMachineCapacityDirectory()
	if err != nil {
		return err
	}
	expected := admission.MachineSessionBinding{ContextID: p.ContextID, ProjectDir: binding.ProjectDir,
		OrderID: capture.OrderID, AttemptID: review.AttemptID, SessionID: capture.SessionID, Runtime: "openshell", WorktreePath: subject.Custody.Physical}
	lease, err := readArchiveRecoveryCleanupLease(filepath.Join(directory, "capacity.json"), owner, expected)
	if err != nil {
		return err
	}
	result.Request, result.Stop, result.Archive = p.Request, native.Stop, native.Archive
	result.OrderID, result.AttemptID, result.SessionID = capture.OrderID, review.AttemptID, capture.SessionID
	result.NativeAttemptID, result.SandboxID, result.MainProcessInstanceID = capture.NativeAttemptID, capture.SandboxID, capture.MainProcessInstanceID
	result.Head, result.Tree = native.Head, native.Tree
	result.Status, result.WaitFor = "archive_recovery_cleanup_waiting", "original_archive_recovery_landing"
	if result.LandingProof.Path != "" {
		if err = verifyArchiveRecoveryLanding(*result); err != nil {
			return err
		}
		result.Status = "archive_recovery_cleanup_verified"
	}
	freshHold, err := archiveRecoveryPaused(runtimeDir)
	if err != nil || freshHold != hold {
		return fmt.Errorf("archive recovery cleanup original pause changed: %v", err)
	}
	if err = verifyOwner(); err != nil {
		return err
	}
	if result.LandingProof.Path == "" {
		return nil
	}
	e, err := execution.New(binding.ProjectDir, os.Environ())
	if err != nil {
		return err
	}
	if err = validateHostEnvironment(e); err != nil {
		return err
	}
	checkoutOwner, err := worktree.NewAppWithExecution(e)
	if err != nil {
		return err
	}
	checkout, err := checkoutOwner.ReadCheckoutIdentity(subject.Custody.Physical)
	if err != nil {
		return err
	}
	if checkout.Head != result.Head || checkout.Repo != subject.Custody.CommonDir ||
		checkout.Registration != subject.Custody.GitAdmin || checkout.Ref != "refs/heads/"+subject.Attempt.WorktreeName {
		return fmt.Errorf("archive recovery cleanup exact checkout incarnation or candidate differs")
	}
	disposition := newArchiveRecoveryCleanupDisposition(*result, p, hold, checkout, lease)
	return archiveRecoveryCleanupContinue(p.SessionDirectory, result, disposition, cleanupExpected)
}

func readArchiveRecoveryCleanupLease(path string, owner *hostOwnerBinding, expected admission.MachineSessionBinding) (admission.MachineSessionLease, error) {
	var ledger struct {
		Leases map[string]struct {
			Owner           string                           `json:"owner"`
			Source          string                           `json:"source"`
			EffectsPossible bool                             `json:"effects_possible"`
			Binding         *admission.MachineSessionBinding `json:"session_binding"`
			Settlement      json.RawMessage                  `json:"settlement"`
		} `json:"leases"`
	}
	var result admission.MachineSessionLease
	if _, err := loopruntime.ReadSessionReadbackFile(path, &ledger); err != nil {
		return result, err
	}
	for id, lease := range ledger.Leases {
		if lease.Owner != hostMachineGenerationOwner(owner.HostRoot, owner.Current.HostInstanceID) {
			continue
		}
		if result.ID != "" || id == "" || lease.Source == "" || !lease.EffectsPossible || lease.Binding == nil || *lease.Binding != expected ||
			(len(lease.Settlement) != 0 && string(lease.Settlement) != "null") {
			return admission.MachineSessionLease{}, fmt.Errorf("archive recovery cleanup original machine lease session differs or is ambiguous")
		}
		result = admission.MachineSessionLease{ID: id, Owner: lease.Owner, Source: lease.Source,
			Binding: lease.Binding, ContextID: expected.ContextID, State: "held"}
	}
	if result.ID == "" {
		return result, fmt.Errorf("archive recovery cleanup original machine lease is missing")
	}
	return result, nil
}

func readArchiveCleanupRef(ref loopruntime.SessionReadbackFileRef, value any) error {
	actual, err := loopruntime.ReadSessionReadbackFile(ref.Path, value)
	if err != nil {
		return err
	}
	if actual != ref {
		return fmt.Errorf("archive recovery cleanup source changed: %s", ref.Path)
	}
	return nil
}
