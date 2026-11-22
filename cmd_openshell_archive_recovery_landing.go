package main

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"

	loopruntime "github.com/poteto/noodle/runtime"
)

type archiveRecoveryLandingProof struct {
	Schema            int                                `json:"schema"`
	Kind              string                             `json:"kind"`
	Request           loopruntime.SessionReadbackFileRef `json:"request"`
	TaskEnvelope      loopruntime.SessionReadbackFileRef `json:"task_envelope"`
	NativeRequest     loopruntime.SessionReadbackFileRef `json:"native_request"`
	Candidate         loopruntime.SessionReadbackFileRef `json:"candidate"`
	Review            loopruntime.SessionReadbackFileRef `json:"review"`
	Publication       loopruntime.SessionReadbackFileRef `json:"publication"`
	LandingCheckpoint loopruntime.SessionReadbackFileRef `json:"landing_checkpoint"`
	ProviderSnapshot  loopruntime.SessionReadbackFileRef `json:"provider_snapshot"`
	OriginalReturn    string                             `json:"original_return"`
}

type archiveRecoveryAppliedIdentity struct {
	OrderID   string `json:"order_id"`
	AttemptID string `json:"attempt_id"`
	SessionID string `json:"session_id"`
	Head      string `json:"head"`
	Tree      string `json:"tree"`
}

type archiveRecoveryPublication struct {
	Owner             string `json:"owner"`
	Status            string `json:"status"`
	ContinuationState string `json:"continuation_state"`
	CompletionScope   string `json:"completion_scope"`
	AuthorizesLanding bool   `json:"authorizes_landing"`
	Repository        string `json:"repository"`
	Subject           string `json:"subject"`
	Head              string `json:"head"`
	Tree              string `json:"tree"`
	PR                struct {
		Number int `json:"number"`
	} `json:"pr"`
}

func verifyArchiveRecoveryLanding(plan archiveRecoveryCleanupPlan) error {
	directory := filepath.Dir(plan.Request.Path)
	if plan.LandingProof.Path != filepath.Join(directory, "archive-landing-proof.json") {
		return fmt.Errorf("archive recovery cleanup requires the original Soodles Owner Landing proof")
	}
	var owner struct {
		ArchiveLanding  loopruntime.SessionReadbackFileRef `json:"archive_landing"`
		ArchiveAdoption struct {
			archiveRecoveryAppliedIdentity
			Schema         int                                `json:"schema"`
			Request        loopruntime.SessionReadbackFileRef `json:"request"`
			TaskEnvelope   loopruntime.SessionReadbackFileRef `json:"task_envelope"`
			Candidate      loopruntime.SessionReadbackFileRef `json:"candidate"`
			MainSessionID  string                             `json:"main_session_id"`
			OriginalReturn string                             `json:"original_return"`
		} `json:"archive_recovery_adoption"`
	}
	ownerRef, err := loopruntime.ReadSessionReadbackFile(filepath.Join(directory, "task-owner.json"), &owner)
	if err != nil {
		return err
	}
	if owner.ArchiveLanding != plan.LandingProof {
		return fmt.Errorf("archive recovery cleanup Landing proof is not retained by the original Soodles Owner")
	}
	var proof archiveRecoveryLandingProof
	if err = readArchiveCleanupRef(plan.LandingProof, &proof); err != nil {
		return err
	}
	identity := archiveRecoveryAppliedIdentity{OrderID: plan.OrderID, AttemptID: plan.AttemptID,
		SessionID: plan.SessionID, Head: plan.Head, Tree: plan.Tree}
	adoption := owner.ArchiveAdoption
	if proof.Schema != 1 || proof.Kind != "archive_recovery_landed" || proof.OriginalReturn != "UNKNOWN" ||
		proof.NativeRequest != plan.Request || proof.Candidate != plan.Candidate ||
		proof.TaskEnvelope.Path != filepath.Join(directory, "task-envelope.json") || adoption.Schema != 1 ||
		adoption.OriginalReturn != "UNKNOWN" || adoption.MainSessionID == "" ||
		adoption.Request != proof.Request || adoption.TaskEnvelope != proof.TaskEnvelope || adoption.archiveRecoveryAppliedIdentity != identity {
		return fmt.Errorf("archive recovery cleanup Landing proof original request or candidate differs")
	}
	if proof.Review.Path != filepath.Join(directory, "review_"+adoption.Candidate.SHA256+"-archive-review.json") ||
		filepath.Dir(adoption.Candidate.Path) != filepath.Join(directory, "candidates") {
		return fmt.Errorf("archive recovery cleanup requires the original Soodles archive review")
	}
	var projected struct {
		archiveRecoveryAppliedIdentity
		Kind           string                             `json:"kind"`
		OriginalReturn string                             `json:"original_return"`
		TaskEnvelope   loopruntime.SessionReadbackFileRef `json:"task_envelope"`
		ArchiveReceipt loopruntime.SessionReadbackFileRef `json:"archive_receipt"`
	}
	if err = readArchiveCleanupRef(adoption.Candidate, &projected); err != nil {
		return err
	}
	if projected.Kind != "archive_recovery" || projected.OriginalReturn != "UNKNOWN" ||
		projected.TaskEnvelope != proof.TaskEnvelope || projected.ArchiveReceipt != plan.Candidate || projected.archiveRecoveryAppliedIdentity != identity {
		return fmt.Errorf("archive recovery cleanup original Soodles candidate projection differs")
	}
	var review struct {
		Schema        int                                `json:"schema"`
		Kind          string                             `json:"kind"`
		OriginalIssue loopruntime.SessionReadbackFileRef `json:"original_issue"`
		TaskEnvelope  loopruntime.SessionReadbackFileRef `json:"task_envelope"`
		Review        struct {
			Decision  string                             `json:"decision"`
			Kind      string                             `json:"kind"`
			Request   loopruntime.SessionReadbackFileRef `json:"request"`
			Candidate loopruntime.SessionReadbackFileRef `json:"candidate"`
			Reviewer  struct {
				SessionID string `json:"session_id"`
			} `json:"reviewer"`
			Acceptance struct {
				Scope      string `json:"scope"`
				Conditions []struct {
					Status string `json:"status"`
				} `json:"conditions"`
			} `json:"acceptance"`
		} `json:"review"`
	}
	if err = readArchiveCleanupRef(proof.Review, &review); err != nil {
		return err
	}
	decision := review.Review
	if review.Schema != 1 || review.Kind != "archive_recovery" || review.TaskEnvelope != proof.TaskEnvelope ||
		decision.Kind != review.Kind || decision.Decision != "approve" || decision.Request != proof.Request ||
		decision.Candidate != adoption.Candidate || decision.Reviewer.SessionID != adoption.MainSessionID ||
		decision.Acceptance.Scope != "candidate" || len(decision.Acceptance.Conditions) == 0 {
		return fmt.Errorf("archive recovery cleanup original Main approval differs")
	}
	for _, condition := range decision.Acceptance.Conditions {
		if condition.Status != "PASS" {
			return fmt.Errorf("archive recovery cleanup Main acceptance is not PASS")
		}
	}
	var issue struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
		URL     string `json:"url"`
	}
	if err = readArchiveCleanupRef(review.OriginalIssue, &issue); err != nil {
		return err
	}
	if issue.HTMLURL == "" {
		issue.HTMLURL = issue.URL
	}
	publication, err := readArchiveRecoveryPublication(proof.Publication)
	if err != nil {
		return err
	}
	if publication.Repository == "" || issue.Number <= 0 ||
		issue.HTMLURL != "https://github.com/"+publication.Repository+"/issues/"+strconv.Itoa(issue.Number) ||
		publication.Subject != publication.Repository+"#"+strconv.Itoa(issue.Number) ||
		publication.Head != plan.Head || publication.Tree != plan.Tree || publication.PR.Number <= 0 {
		return fmt.Errorf("archive recovery cleanup publication differs from the approved candidate")
	}
	if err = verifyArchiveRecoveryCloudLanding(proof, publication, issue.Number); err != nil {
		return err
	}
	return loopruntime.VerifySessionReadbackFiles(map[string]loopruntime.SessionReadbackFileRef{
		"original_soodles_owner": ownerRef, "original_main_request": proof.Request, "original_native_request": proof.NativeRequest,
		"archive_recovery_landing_proof": plan.LandingProof, "original_task_envelope": proof.TaskEnvelope,
		"original_main_review": proof.Review, "original_candidate": plan.Candidate,
		"original_landing_checkpoint": proof.LandingCheckpoint, "original_provider_snapshot": proof.ProviderSnapshot,
		"original_publication": proof.Publication, "original_issue": review.OriginalIssue, "soodles_candidate": adoption.Candidate,
	})
}

func readArchiveRecoveryPublication(ref loopruntime.SessionReadbackFileRef) (archiveRecoveryPublication, error) {
	var wrapper struct {
		archiveRecoveryPublication
		Result *archiveRecoveryPublication `json:"result"`
	}
	if err := readArchiveCleanupRef(ref, &wrapper); err != nil {
		return archiveRecoveryPublication{}, err
	}
	result := wrapper.archiveRecoveryPublication
	if wrapper.Result != nil {
		result = *wrapper.Result
	}
	if result.Owner != "soodles.candidate-publication" || (result.Status != "created" && result.Status != "reused" && result.Status != "amended") ||
		result.ContinuationState != "complete" || result.CompletionScope != "publication" || result.AuthorizesLanding {
		return archiveRecoveryPublication{}, fmt.Errorf("archive recovery cleanup original publication did not complete")
	}
	return result, nil
}

var archiveRecoveryGitSHA = regexp.MustCompile("^[0-9a-f]{40}$")

func verifyArchiveRecoveryCloudLanding(proof archiveRecoveryLandingProof, publication archiveRecoveryPublication, issue int) error {
	var checkpoint struct {
		Schema         int      `json:"schema"`
		Phase          string   `json:"phase"`
		Classification string   `json:"classification"`
		WritesOffered  []string `json:"writes_offered"`
		Observations   []string `json:"observations"`
		MergeSHA       string   `json:"merge_sha"`
		IssueClosedAt  string   `json:"issue_closed_at"`
		Claim          struct {
			Repository string `json:"repository"`
			Issue      int    `json:"issue"`
			PR         int    `json:"pr"`
			Head       string `json:"head"`
			Tree       string `json:"tree"`
			BaseHead   string `json:"base_head"`
			RunID      int64  `json:"run_id"`
			RunAttempt int    `json:"run_attempt"`
		} `json:"claim"`
		Reconciliation struct {
			Mode                        string `json:"mode"`
			MainHead                    string `json:"main_head"`
			MergeSHA                    string `json:"merge_sha"`
			Provider                    string `json:"provider"`
			LocalReconciliationRequired *bool  `json:"local_reconciliation_required"`
		} `json:"provider_reconciliation"`
	}
	if err := readArchiveCleanupRef(proof.LandingCheckpoint, &checkpoint); err != nil {
		return err
	}
	claim, reconciled := checkpoint.Claim, checkpoint.Reconciliation
	if checkpoint.Schema != 2 || checkpoint.Phase != "resolved" || checkpoint.Classification != "RESOLVED" ||
		len(checkpoint.Observations) == 0 || len(checkpoint.WritesOffered) != 2 ||
		checkpoint.WritesOffered[0] != "merge" || checkpoint.WritesOffered[1] != "close" ||
		!archiveRecoveryGitSHA.MatchString(checkpoint.MergeSHA) || checkpoint.IssueClosedAt == "" ||
		claim.Repository != publication.Repository || claim.Issue != issue || claim.PR != publication.PR.Number ||
		claim.Head != publication.Head || claim.Tree != publication.Tree || !archiveRecoveryGitSHA.MatchString(claim.BaseHead) ||
		claim.RunID <= 0 || claim.RunAttempt <= 0 || reconciled.Mode != "cloud" || reconciled.Provider != "GitHub" ||
		reconciled.MergeSHA != checkpoint.MergeSHA || reconciled.LocalReconciliationRequired == nil || *reconciled.LocalReconciliationRequired {
		return fmt.Errorf("archive recovery cleanup exact Cloud Landing is not resolved")
	}
	type gitObject struct {
		SHA string `json:"sha"`
	}
	var provider struct {
		Issue struct {
			Number      int    `json:"number"`
			State       string `json:"state"`
			StateReason string `json:"state_reason"`
			ClosedAt    string `json:"closed_at"`
		} `json:"issue"`
		PR struct {
			Number      int       `json:"number"`
			State       string    `json:"state"`
			Merged      bool      `json:"merged"`
			MergeCommit string    `json:"merge_commit_sha"`
			Head        gitObject `json:"head"`
		} `json:"pr"`
		Merge struct {
			gitObject
			Tree    gitObject   `json:"tree"`
			Parents []gitObject `json:"parents"`
		} `json:"merge_commit"`
		Branch struct {
			Commit gitObject `json:"commit"`
		} `json:"branch"`
		Run struct {
			ID         int64  `json:"id"`
			RunAttempt int    `json:"run_attempt"`
			HeadSHA    string `json:"head_sha"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"run"`
	}
	if err := readArchiveCleanupRef(proof.ProviderSnapshot, &provider); err != nil {
		return err
	}
	if provider.Issue.Number != issue || provider.Issue.State != "closed" || provider.Issue.StateReason != "completed" ||
		provider.Issue.ClosedAt != checkpoint.IssueClosedAt || provider.PR.Number != claim.PR || provider.PR.State != "closed" ||
		!provider.PR.Merged || provider.PR.MergeCommit != checkpoint.MergeSHA || provider.PR.Head.SHA != claim.Head ||
		provider.Merge.SHA != checkpoint.MergeSHA || provider.Merge.Tree.SHA != claim.Tree || len(provider.Merge.Parents) != 2 ||
		provider.Merge.Parents[0].SHA != claim.BaseHead || provider.Merge.Parents[1].SHA != claim.Head ||
		provider.Branch.Commit.SHA != reconciled.MainHead || !archiveRecoveryGitSHA.MatchString(reconciled.MainHead) ||
		provider.Run.ID != claim.RunID || provider.Run.RunAttempt != claim.RunAttempt || provider.Run.HeadSHA != claim.Head ||
		provider.Run.Status != "completed" || provider.Run.Conclusion != "success" {
		return fmt.Errorf("archive recovery cleanup Provider effects differ from exact Cloud Landing")
	}
	return nil
}
