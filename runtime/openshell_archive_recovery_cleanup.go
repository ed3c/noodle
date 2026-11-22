package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	dm "github.com/NVIDIA/OpenShell/sdk/go/proto/datamodelv1"
	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
)

// ArchiveRecoveryCleanupCandidate reports the original native facts the cleanup
// Owner can independently verify before any resource reclamation.
type ArchiveRecoveryCleanupCandidate struct {
	Head, Tree string
	Stop       SessionReadbackFileRef
	Archive    SessionReadbackFileRef
}

// ObserveOpenShellArchiveRecoveryCleanup verifies the exact already-applied
// candidate and original Stop. It never creates an intent or mutates custody.
func ObserveOpenShellArchiveRecoveryCleanup(ctx context.Context, p ProgressMaintenance, profile OpenShellProfile, input OpenShellArchiveRecoveryApplyInput, candidateRef SessionReadbackFileRef) (ArchiveRecoveryCleanupCandidate, error) {
	var result ArchiveRecoveryCleanupCandidate
	var candidate archiveRecoveryCandidate
	if err := maintenanceVerify(candidateRef, &candidate); err != nil {
		return result, err
	}
	if candidateRef.Path != filepath.Join(p.SessionDirectory, "archive-recovery-candidate.json") ||
		candidate.Intent.Path != filepath.Join(p.SessionDirectory, "archive-recovery-apply-intent.json") ||
		candidate.Request != p.Request || candidate.Review != input.Review || candidate.Capture != input.Capture ||
		candidate.Stop != input.Stop || candidate.AttemptID != input.AttemptID || candidate.CandidateTree != input.Tree.CandidateTree ||
		candidate.OriginalReturn != "UNKNOWN" || candidate.AuthorizesLanding || candidate.Schema != 1 {
		return result, fmt.Errorf("archive recovery cleanup original candidate binding differs")
	}
	capture, in, err := archiveRecoveryStopInputs(p, profile, input.Review, input.Capture, true, true)
	if err != nil {
		return result, err
	}
	if candidate.OrderID != capture.OrderID || candidate.SessionID != capture.SessionID || candidate.NativeAttemptID != capture.NativeAttemptID ||
		candidate.MainProcessInstanceID != capture.MainProcessInstanceID || candidate.SandboxID != capture.SandboxID ||
		candidate.BaselineHead != capture.BaselineHead {
		return result, fmt.Errorf("archive recovery cleanup original attempt identity differs")
	}
	if err = verifyArchiveRecoveryApplyStop(p, input, in); err != nil {
		return result, err
	}
	var intent archiveRecoveryApplyRecord
	if err = maintenanceVerify(candidate.Intent, &intent); err != nil {
		return result, err
	}
	var dispatch openShellDispatchReceipt
	if err = maintenanceVerify(capture.BaselineSource, &dispatch); err != nil {
		return result, err
	}
	physical, err := filepath.EvalSymlinks(dispatch.Target)
	if err != nil || physical != input.Target || !filepath.IsAbs(physical) {
		return result, fmt.Errorf("archive recovery cleanup original checkout differs")
	}
	observed, err := readArchiveRecoveryCandidate(ctx, candidateRef.Path, candidate.Intent, intent, physical, dispatch.Baseline, in.record)
	if err != nil || observed != candidateRef {
		return result, fmt.Errorf("archive recovery cleanup original candidate changed: %v", err)
	}
	if err = verifyRetainedArchiveRecoveryBytes(capture.Archive); err != nil {
		return result, err
	}
	conn, err := newOpenShellConnection(profile.Gateway.Binding.Gateway, profile.Gateway.TLS)
	if err != nil {
		return result, err
	}
	defer conn.Close()
	selector := &dm.WorkspaceSelector{Selection: &dm.WorkspaceSelector_Workspace{Workspace: in.record.Worker.Identity.Sandbox.Workspace}}
	response, err := pb.NewOpenShellClient(conn).GetSandbox(ctx, &pb.GetSandboxRequest{Name: in.record.Name, WorkspaceScope: selector})
	if err != nil {
		return result, err
	}
	if err = verifyArchiveRecoveryStopped(in.record, response); err != nil {
		return result, err
	}
	return ArchiveRecoveryCleanupCandidate{Head: candidate.CandidateHead, Tree: candidate.CandidateTree, Stop: candidate.Stop, Archive: capture.Archive}, nil
}

func verifyRetainedArchiveRecoveryBytes(ref SessionReadbackFileRef) error {
	before, err := os.Lstat(ref.Path)
	if err != nil || !before.Mode().IsRegular() {
		return fmt.Errorf("archive recovery retained workspace is unavailable")
	}
	digest, err := pairingDigestFile(ref.Path)
	if err != nil || digest != ref.SHA256 {
		return fmt.Errorf("archive recovery retained workspace bytes changed")
	}
	after, err := os.Lstat(ref.Path)
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return fmt.Errorf("archive recovery retained workspace changed during readback")
	}
	return nil
}
