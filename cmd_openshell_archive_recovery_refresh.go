package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"time"

	"github.com/poteto/noodle/internal/filex"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/spf13/cobra"
)

const archiveRefreshOperation = "refresh-workspace-capture"
const archiveRefreshWait = "original_archive_recovery_refresh_readback"

type archiveRecoveryRefreshIntent struct {
	archiveRecoveryPausePlan
	Custody loopruntime.SessionReadbackFileRef `json:"current_custody"`
	Output  string                             `json:"output"`
}

type archiveRecoveryRefreshReadback struct {
	Schema            int                                 `json:"schema"`
	Status            string                              `json:"status"`
	Review            loopruntime.SessionReadbackFileRef  `json:"review"`
	Intent            *loopruntime.SessionReadbackFileRef `json:"intent,omitempty"`
	Capture           *loopruntime.SessionReadbackFileRef `json:"capture,omitempty"`
	QualifiedReview   *loopruntime.SessionReadbackFileRef `json:"qualified_review,omitempty"`
	ReviewSuccessor   *loopruntime.SessionReadbackFileRef `json:"review_successor,omitempty"`
	Problem           string                              `json:"problem,omitempty"`
	OriginalReturn    string                              `json:"original_return"`
	AuthorizesLanding bool                                `json:"authorizes_landing"`
	Effects           []string                            `json:"effects"`
	archiveRecoveryContinuation
}

type archiveRecoveryReviewSuccessor struct {
	Schema         int                                `json:"schema"`
	Kind           string                             `json:"kind"`
	OriginalReview loopruntime.SessionReadbackFileRef `json:"original_review"`
	Review         loopruntime.SessionReadbackFileRef `json:"review"`
	Capture        loopruntime.SessionReadbackFileRef `json:"capture"`
	Intent         loopruntime.SessionReadbackFileRef `json:"intent"`
	Request        loopruntime.SessionReadbackFileRef `json:"request"`
	AttemptID      string                             `json:"attempt_id"`
}

func newRefreshOpenShellArchiveRecoveryCmd() *cobra.Command {
	var review, expected string
	cmd := &cobra.Command{Use: archiveRefreshOperation, Short: "Qualify one fresh capture under the original OpenShell attempt", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
		defer cancel()
		var result archiveRecoveryRefreshReadback
		if _, err := runOriginalArchiveRecoveryControl(ctx, loopruntime.SessionReadbackFileRef{Path: review, SHA256: expected}, archiveRecoveryControl{refresh: &result}); err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
	}}
	cmd.Flags().StringVar(&review, "review-file", "", "Original archive recovery review evidence")
	cmd.Flags().StringVar(&expected, "expected-sha256", "", "Exact historical reviewed FileRef SHA256")
	_ = cmd.MarkFlagRequired("review-file")
	_ = cmd.MarkFlagRequired("expected-sha256")
	return cmd
}

func archiveRecoveryRefreshIntentPath(session string) string {
	return filepath.Join(session, "archive-recovery-refresh-intent.json")
}

func archiveRecoveryRefreshOutput(p loopruntime.ProgressMaintenance, profile loopruntime.OpenShellProfile, capture loopruntime.SessionReadbackFileRef) (string, error) {
	parent := filepath.Dir(filepath.Dir(capture.Path))
	project := filepath.Dir(filepath.Dir(filepath.Dir(p.SessionDirectory)))
	parent, err := workspaceCapturePrivateParent(parent, project, profile.Gateway.Binding.StateRoot, p.HostDir)
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, "archive-recovery-refresh-"+capture.SHA256), nil
}

func verifyArchiveRecoveryRefresh(ctx context.Context, p loopruntime.ProgressMaintenance, profile loopruntime.OpenShellProfile, reviewRef loopruntime.SessionReadbackFileRef, review archiveRecoveryReview, verifyOwner func() error) (loopruntime.SessionReadbackFileRef, error) {
	var empty loopruntime.SessionReadbackFileRef
	custody, err := loopruntime.ReadSessionReadbackFile(filepath.Join(profile.Gateway.Binding.StateRoot, "owner.json"), nil)
	if err != nil {
		return empty, err
	}
	if err := verifyRetainedArchiveRecoveryTree(ctx, review, loopruntime.InspectHistoricalOpenShellWorkspaceTree); err != nil {
		return empty, err
	}
	if err := verifyOwner(); err != nil {
		return empty, err
	}
	if err := loopruntime.ObserveOpenShellArchiveRecapture(ctx, p, profile, reviewRef, review.Capture); err != nil {
		return empty, err
	}
	if err := verifyOwner(); err != nil {
		return empty, err
	}
	return custody, loopruntime.VerifySessionReadbackFiles(map[string]loopruntime.SessionReadbackFileRef{"prospective_custody": custody, "historical_review": reviewRef})
}

func verifyRetainedArchiveRecoveryTree(ctx context.Context, review archiveRecoveryReview, inspect workspaceCaptureTreeInspector) error {
	historical, err := inspectWorkspaceCaptureBound(ctx, review.Capture, inspect)
	if err != nil {
		return err
	}
	if historical["attempt_id"] != review.AttemptID || !reflect.DeepEqual(historical["tree"], review.Tree) || !reflect.DeepEqual(historical["write_paths"], review.WritePaths) {
		return fmt.Errorf("archive recovery historical reviewed attempt, tree or scope changed")
	}
	return nil
}

func reserveArchiveRecoveryRefresh(path string, intent archiveRecoveryRefreshIntent) (loopruntime.SessionReadbackFileRef, error) {
	var empty loopruntime.SessionReadbackFileRef
	data, err := json.Marshal(intent)
	if err != nil {
		return empty, err
	}
	if err = writeAdmissionOnce(path, data); err != nil {
		return empty, err
	}
	if err = filex.SyncDir(filepath.Dir(path)); err != nil {
		return empty, err
	}
	return loopruntime.ReadSessionReadbackFile(path, nil)
}

func refreshOriginalArchiveRecovery(ctx context.Context, p loopruntime.ProgressMaintenance, profile loopruntime.OpenShellProfile, reviewRef loopruntime.SessionReadbackFileRef, review archiveRecoveryReview, oldCapture loopruntime.OpenShellWorkspaceCapture, verifyOwner func() error) (archiveRecoveryRefreshReadback, error) {
	result := archiveRecoveryRefreshReadback{Schema: 1, Status: "archive_recovery_refresh_unknown", Review: reviewRef, OriginalReturn: "UNKNOWN", Effects: []string{},
		archiveRecoveryContinuation: archiveRecoveryContinuation{WaitFor: archiveRefreshWait}}
	output, err := archiveRecoveryRefreshOutput(p, profile, review.Capture)
	if err != nil {
		return result, err
	}
	expected := archiveRecoveryPausePlan{Schema: 1, Review: reviewRef, Capture: review.Capture, Request: p.Request, Host: p.HostInstance, ContextID: p.ContextID, AttemptID: review.AttemptID}
	path := archiveRecoveryRefreshIntentPath(p.SessionDirectory)
	var intent archiveRecoveryRefreshIntent
	intentRef, err := loopruntime.ReadSessionReadbackFile(path, &intent)
	if os.IsNotExist(err) {
		op, _, _, err := archiveRecoveryPlanStage(p.SessionDirectory, filepath.Dir(filepath.Dir(p.SessionDirectory)), expected)
		if err != nil {
			return result, err
		}
		if op != archivePauseOperation {
			return result, fmt.Errorf("archive recovery refresh requires the original pre-Pause state")
		}
		if _, err := os.Lstat(output); !os.IsNotExist(err) {
			return result, fmt.Errorf("archive recovery refresh output already exists without its original intent")
		}
		custody, err := verifyArchiveRecoveryRefresh(ctx, p, profile, reviewRef, review, verifyOwner)
		if err != nil {
			return result, err
		}
		intent = archiveRecoveryRefreshIntent{archiveRecoveryPausePlan: expected, Custody: custody, Output: output}
		intentRef, err = reserveArchiveRecoveryRefresh(path, intent)
		if err != nil {
			return result, err
		}
		result.Intent = &intentRef
		capture, err := loopruntime.CaptureOpenShellWorkspace(ctx, p, profile, oldCapture.OrderID, output, func() error {
			if err := loopruntime.VerifySessionReadbackFiles(map[string]loopruntime.SessionReadbackFileRef{"prospective_custody": custody, "refresh_intent": intentRef}); err != nil {
				return err
			}
			return verifyOwner()
		})
		if err != nil {
			result.Problem = err.Error()
			return result, nil
		}
		result.Capture = &capture
		result.Effects = []string{"workspace_archive_captured"}
	} else if err != nil {
		return result, err
	}
	if intent.archiveRecoveryPausePlan != expected || intent.Output != output || intent.Custody.Path != oldCapture.Sources["custody"].Path || intent.Custody.SHA256 == oldCapture.Sources["custody"].SHA256 {
		return result, fmt.Errorf("archive recovery refresh original intent differs")
	}
	result.Intent = &intentRef
	captured, err := loopruntime.ReadSessionReadbackFile(filepath.Join(output, "workspace-capture.json"), nil)
	if os.IsNotExist(err) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	result.Capture = &captured
	if err := verifyOwner(); err != nil {
		return result, err
	}
	fresh, err := loopruntime.ReadOpenShellWorkspaceCapture(captured)
	if err != nil {
		return result, err
	}
	if err := validateArchiveRecoveryFreshCapture(oldCapture, fresh, intent); err != nil {
		return result, err
	}
	verified, err := inspectOriginalWorkspaceCapture(ctx, captured)
	if err != nil {
		return result, err
	}
	if verified["attempt_id"] != review.AttemptID || !reflect.DeepEqual(verified["tree"], review.Tree) || !reflect.DeepEqual(verified["write_paths"], review.WritePaths) {
		return result, fmt.Errorf("archive recovery fresh capture differs from the exact historical Git tree or scope")
	}
	if err := loopruntime.VerifySessionReadbackFiles(map[string]loopruntime.SessionReadbackFileRef{"historical_review": reviewRef, "refresh_intent": intentRef}); err != nil {
		return result, err
	}
	verified["historical_review"] = reviewRef
	verified["refresh_intent"] = intentRef
	data, err := json.Marshal(verified)
	if err != nil {
		return result, err
	}
	reviewPath := filepath.Join(output, "archive-recovery-review.json")
	if err = writeAdmissionOnce(reviewPath, data); err != nil {
		return result, err
	}
	if err = filex.SyncDir(output); err != nil {
		return result, err
	}
	qualified, err := loopruntime.ReadSessionReadbackFile(reviewPath, nil)
	if err != nil {
		return result, err
	}
	link := archiveRecoveryReviewSuccessor{Schema: 1, Kind: "archive_recovery_review_successor", OriginalReview: reviewRef, Review: qualified,
		Capture: captured, Intent: intentRef, Request: p.Request, AttemptID: review.AttemptID}
	linkRaw, err := json.Marshal(link)
	if err != nil {
		return result, err
	}
	linkPath := filepath.Join(output, "archive-recovery-review-successor.json")
	if err = writeAdmissionOnce(linkPath, linkRaw); err != nil {
		return result, err
	}
	if err = filex.SyncDir(output); err != nil {
		return result, err
	}
	linkRef, err := loopruntime.ReadSessionReadbackFile(linkPath, nil)
	if err != nil {
		return result, err
	}
	result.Status, result.QualifiedReview, result.ReviewSuccessor = "archive_recovery_refresh_qualified", &qualified, &linkRef
	result.WaitFor = ""
	// A rerun can return the same qualified receipt, but the original pause
	// intent is never offered again once reserved.
	result.archiveRecoveryContinuation, err = archiveRecoveryOfferAfter(p.SessionDirectory, qualified, "pause", archivePauseOperation)
	return result, err
}

func inspectExistingArchiveRecoveryRefresh(ctx context.Context, r archiveRecoveryPlanReadback, p loopruntime.ProgressMaintenance, profile loopruntime.OpenShellProfile, expected archiveRecoveryPausePlan) (archiveRecoveryPlanReadback, bool, error) {
	var intent archiveRecoveryRefreshIntent
	intentRef, err := loopruntime.ReadSessionReadbackFile(archiveRecoveryRefreshIntentPath(p.SessionDirectory), &intent)
	if os.IsNotExist(err) || (err == nil && intent.Review != expected.Review) {
		return r, false, nil
	}
	if err != nil {
		return r, true, err
	}
	output, err := archiveRecoveryRefreshOutput(p, profile, expected.Capture)
	if err != nil {
		return r, true, err
	}
	if intent.archiveRecoveryPausePlan != expected || intent.Output != output {
		return r, true, fmt.Errorf("archive recovery refresh original intent differs")
	}
	var link archiveRecoveryReviewSuccessor
	linkRef, err := loopruntime.ReadSessionReadbackFile(filepath.Join(output, "archive-recovery-review-successor.json"), &link)
	if os.IsNotExist(err) {
		captureRef, err := loopruntime.ReadSessionReadbackFile(filepath.Join(output, "workspace-capture.json"), nil)
		if os.IsNotExist(err) {
			r.WaitFor = archiveRefreshWait
			return r, true, nil
		}
		if err != nil {
			return r, true, err
		}
		fresh, err := loopruntime.ReadOpenShellWorkspaceCapture(captureRef)
		if err != nil {
			return r, true, err
		}
		old, err := loopruntime.ReadOpenShellWorkspaceCapture(expected.Capture)
		if err != nil {
			return r, true, err
		}
		if err := validateArchiveRecoveryFreshCapture(old, fresh, intent); err != nil {
			return r, true, err
		}
		// The native result already exists. The original refresh action may only
		// verify and qualify it because its durable intent is already present.
		r.Status = "archive_recovery_ready"
		r.Next, err = archiveRecoveryNextAction(archiveRefreshOperation, expected.Review)
		return r, true, err
	}
	if err != nil {
		return r, true, err
	}
	if link.Schema != 1 || link.Kind != "archive_recovery_review_successor" || link.OriginalReview != expected.Review || link.Intent != intentRef || link.Request != p.Request || link.AttemptID != expected.AttemptID || link.Review.Path != filepath.Join(output, "archive-recovery-review.json") || link.Capture.Path != filepath.Join(output, "workspace-capture.json") {
		return r, true, fmt.Errorf("archive recovery fresh review successor differs")
	}
	var approved struct {
		archiveRecoveryReview
		HistoricalReview loopruntime.SessionReadbackFileRef `json:"historical_review"`
		RefreshIntent    loopruntime.SessionReadbackFileRef `json:"refresh_intent"`
	}
	reviewRef, err := loopruntime.ReadSessionReadbackFile(link.Review.Path, &approved)
	if err != nil {
		return r, true, err
	}
	if reviewRef != link.Review || approved.Capture != link.Capture || approved.HistoricalReview != expected.Review || approved.RefreshIntent != intentRef || approved.AttemptID != expected.AttemptID {
		return r, true, fmt.Errorf("archive recovery fresh review successor source differs")
	}
	var historical archiveRecoveryReview
	historicalRef, err := loopruntime.ReadSessionReadbackFile(expected.Review.Path, &historical)
	if err != nil {
		return r, true, err
	}
	if historicalRef != expected.Review || !reflect.DeepEqual(approved.Tree, historical.Tree) || !reflect.DeepEqual(approved.WritePaths, historical.WritePaths) {
		return r, true, fmt.Errorf("archive recovery fresh review differs from the exact historical Git tree or scope")
	}
	var current archiveRecoveryPlanReadback
	if _, err := runOriginalArchiveRecoveryControl(ctx, link.Review, archiveRecoveryControl{plan: &current}); err != nil {
		return r, true, err
	}
	if current.Request != r.Request || current.ContextID != r.ContextID || current.OrderID != r.OrderID || current.SessionID != r.SessionID || current.AttemptID != r.AttemptID || current.NativeAttemptID != r.NativeAttemptID || current.SandboxID != r.SandboxID || current.MainProcessInstanceID != r.MainProcessInstanceID {
		return r, true, fmt.Errorf("archive recovery fresh Owner plan does not bind the same original attempt or VM")
	}
	current.OriginalReview, current.ReviewSuccessor = &expected.Review, &linkRef
	return current, true, nil
}

func validateArchiveRecoveryFreshCapture(old, fresh loopruntime.OpenShellWorkspaceCapture, intent archiveRecoveryRefreshIntent) error {
	if fresh.OrderID != old.OrderID || fresh.SessionID != old.SessionID || fresh.NativeAttemptID != old.NativeAttemptID || fresh.SandboxID != old.SandboxID || fresh.MainProcessInstanceID != old.MainProcessInstanceID || fresh.BaselineHead != old.BaselineHead || fresh.BaselineIndexSHA256 != old.BaselineIndexSHA256 || fresh.BaselineSource != old.BaselineSource || fresh.CaptureExecRequestID == "" || fresh.CaptureExecRequestID == old.CaptureExecRequestID || fresh.Sources["custody"] != intent.Custody || fresh.Sources["request"] != intent.Request || fresh.Sources["profile"] != old.Sources["profile"] {
		return fmt.Errorf("archive recovery fresh capture does not bind the same original attempt and current custody")
	}
	return nil
}
