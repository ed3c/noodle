package runtime

import (
	"context"
	"fmt"
	"maps"
	"path/filepath"
	"slices"

	"github.com/poteto/noodle/internal/workspaceartifacts"
)

type RejectedReviewRetirementAttempt struct {
	Session         CompletedSessionBinding
	NativeAttemptID string
	CandidateHead   string
}

// AcquireRejectedReviewMaintenance joins the exact guarded resident without
// publishing worker custody or starting background credential delivery.
func (o *OpenShellGateway) AcquireRejectedReviewMaintenance(ctx context.Context, contextID string) error {
	if !o.mu.TryLock() {
		return fmt.Errorf("original gateway operation in progress")
	}
	defer o.mu.Unlock()
	if contextID == "" || o.serviceOnly || (o.attempted && !o.retirementOnly) {
		return fmt.Errorf("original rejected review maintenance requires an unused gateway owner")
	}
	o.retirementOnly = true
	if !o.attempted {
		if _, err := o.acquireResidentLocked(ctx, contextID); err != nil {
			return err
		}
	}
	if o.generation == nil || !slices.Equal(o.resident.Contexts, []string{contextID}) {
		return fmt.Errorf("original rejected review maintenance context is unavailable")
	}
	_, err := o.readback(ctx)
	return err
}

// RetireRejectedReviewSessions borrows an already acquired maintenance
// resident with the original profile and custody. Its caller owns the release,
// canonical review history, attempt selection and explicit checkout retention.
func (o *OpenShellGateway) RetireRejectedReviewSessions(ctx context.Context, profile OpenShellProfile, residentContext, originalContext, runtimeDir, repository string,
	attempts []RejectedReviewRetirementAttempt, reviewSources map[string]SessionReadbackFileRef) ([]CleanupSessionRetirement, error) {
	if !filepath.IsAbs(runtimeDir) || !filepath.IsAbs(repository) || originalContext == "" || residentContext == originalContext || len(attempts) == 0 {
		return nil, fmt.Errorf("original rejected review retirement scope is incomplete")
	}
	sources := map[string]SessionReadbackFileRef{}
	for _, name := range []string{"canonical_snapshot", "rejected_review_events", "rejected_review_release"} {
		ref := reviewSources[name]
		if ref.Path == "" {
			return nil, fmt.Errorf("original rejected review retirement source missing: %s", name)
		}
		sources[name] = ref
	}
	if err := VerifySessionReadbackFiles(sources); err != nil {
		return nil, err
	}
	if !o.mu.TryLock() {
		return nil, fmt.Errorf("original gateway operation in progress")
	}
	defer o.mu.Unlock()
	if !o.retirementOnly {
		return nil, fmt.Errorf("original rejected review maintenance owner unavailable")
	}
	custody, generation, err := o.completionOwner(residentContext)
	if err != nil {
		return nil, err
	}
	spec := profile.Gateway
	spec.Binding.StateRoot, err = filepath.EvalSymlinks(spec.Binding.StateRoot)
	if err != nil {
		return nil, err
	}
	configured, err := OpenShellGatewayGuardID(spec)
	if err != nil || configured != o.resident.GuardID || o.spec.Binding != spec.Binding {
		return nil, fmt.Errorf("original rejected review gateway binding differs: %v", err)
	}
	storage, err := openShellVMStorageForProfile(profile)
	if err != nil {
		return nil, err
	}
	reader := openShellSessionEvidenceReader{profile, runtimeDir, originalContext, o.spec, false}
	type selection struct {
		task    OpenShellTask
		sources map[string]SessionReadbackFileRef
	}
	selected := make([]selection, 0, len(attempts))
	sessions, nativeAttempts := map[string]bool{}, map[string]bool{}
	for i, attempt := range attempts {
		binding, first := attempt.Session, attempts[0].Session
		if binding.RetryCount != i || binding.OrderID != first.OrderID || binding.StageIndex != first.StageIndex || binding.WorktreePath != first.WorktreePath ||
			attempt.NativeAttemptID == "" || attempt.CandidateHead == "" || sessions[binding.SessionID] || nativeAttempts[attempt.NativeAttemptID] {
			return nil, fmt.Errorf("original rejected review attempt set differs")
		}
		sessions[binding.SessionID], nativeAttempts[attempt.NativeAttemptID] = true, true
		observed, err := reader.read(ctx, binding,
			func(ctx context.Context, identity OpenShellIdentity) (OpenShellCompletedObservation, error) {
				return custody.observeSettled(ctx, identity, cleanupCompletedObservation)
			}, false, func(ctx context.Context, receipt openShellDispatchReceipt, head string) error {
				return workspaceartifacts.VerifyRetainedPublication(ctx, repository, receipt.Baseline, receipt.Delivery, head)
			})
		if err != nil {
			return nil, fmt.Errorf("original rejected review session %s: %w", binding.SessionID, err)
		}
		execution := observed.completed
		if observed.status != StatusCompleted || execution.NativeAttemptID != attempt.NativeAttemptID || execution.CandidateHead != attempt.CandidateHead ||
			!execution.StopAcknowledged || execution.NativeExecExitCode != 0 {
			return nil, fmt.Errorf("original rejected review stopped execution differs: %s", binding.SessionID)
		}
		proof := maps.Clone(execution.Sources)
		delete(proof, "native_custody")
		maps.Copy(proof, sources)
		selected = append(selected, selection{OpenShellTask{binding.OrderID, attempt.NativeAttemptID, binding.SessionID}, proof})
	}
	results := make([]CleanupSessionRetirement, 0, len(selected))
	for _, attempt := range selected {
		if err := generation.Running(); err != nil {
			return results, err
		}
		result, err := custody.retireCleanupSession(ctx, attempt.task, storage, attempt.sources, nil)
		if result.SessionID != "" {
			result.OutcomeStatus = StatusCompleted // The original native execution completed before its rejected review.
			results = append(results, result)
		}
		if err != nil {
			return results, err
		}
	}
	return results, generation.Running()
}
