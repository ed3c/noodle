package loop

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"time"

	"github.com/poteto/noodle/event"
	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
)

// This is local reconciliation of an externally landed claim, not a merge or
// provider authorization. The original attempt, including any merge error, stays.
type PublicationReconciliation struct {
	Owner             string `json:"owner"`
	Status            string `json:"status"`
	OrderID           string `json:"order_id"`
	Head              string `json:"head"`
	MergeHead         string `json:"merge_head"`
	EvidencePath      string `json:"evidence_path"`
	AuthorizesLanding bool   `json:"authorizes_landing"`
}

type publicationReconcileIntent struct {
	Claim       PublicationClaim `json:"claim"`
	ClaimSHA256 string           `json:"claim_sha256"`
	MergeHead   string           `json:"merge_head"`
	Before      []byte           `json:"before_snapshot"`
	After       []byte           `json:"after_snapshot"`
}

var reconciliationSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// ReconcilePublication holds the same stopped-owner lock as admission recovery.
// No loop, adapter, scheduler, writer, merge or cleanup is started here.
func ReconcilePublication(project, claimPath, digest, mergeHead string) (PublicationReconciliation, error) {
	return reconcilePublication(project, claimPath, digest, mergeHead, nil)
}

func reconcilePublication(project, claimPath, digest, mergeHead string, barrier func(string)) (PublicationReconciliation, error) {
	r := PublicationReconciliation{Owner: "Noodle", Status: "refused", MergeHead: mergeHead}
	project, err := canonicalDirectory(project)
	if err != nil {
		return r, err
	}
	if !filepath.IsAbs(claimPath) || !reconciliationSHA.MatchString(mergeHead) {
		return r, fmt.Errorf("reconciliation claim path or merge identity is invalid")
	}
	raw, err := readAdmissionFile(claimPath)
	if err != nil {
		return r, err
	}
	if publicationDigest(raw) != digest {
		return r, fmt.Errorf("publication claim digest changed")
	}
	var claim PublicationClaim
	if err := decodeStoppedReview(raw, &claim); err != nil {
		return r, err
	}
	r.OrderID, r.Head = claim.OrderID, claim.Head
	dir := filepath.Join(project, ".noodle")
	lock, err := lockfile.TryLock(filepath.Join(dir, "noodle.lock"))
	if err != nil {
		return r, err
	}
	defer lock.Close()
	r.EvidencePath = filepath.Join(dir, "publication-reconciliations", publicationDigest([]byte(claim.OrderID+"\n"+claim.Subject)))
	intentPath := filepath.Join(r.EvidencePath, "intent.json")
	var intent publicationReconcileIntent
	data, err := readAdmissionFile(intentPath)
	exists := err == nil
	if err != nil && !os.IsNotExist(err) {
		return r, err
	}
	current, err := readAdmissionFile(filepath.Join(dir, "state.snapshot.json"))
	if err != nil {
		return r, err
	}
	if exists {
		if err := decodeStoppedReview(data, &intent); err != nil {
			return r, err
		}
		selected, _ := json.Marshal(claim)
		retained, _ := json.Marshal(intent.Claim)
		if !bytes.Equal(selected, retained) || intent.ClaimSHA256 != digest || intent.MergeHead != mergeHead {
			return r, fmt.Errorf("reconciliation intent identity changed")
		}
		var after reducer.DurableSnapshot
		if err := decodeStoppedReview(intent.After, &after); err != nil {
			return r, err
		}
		expected, err := publicationReconcileAfter(intent.Before, claim, mergeHead, after.GeneratedAt)
		if err != nil {
			return r, err
		}
		if !bytes.Equal(expected, intent.After) {
			return r, fmt.Errorf("reconciliation transition changed")
		}
		if !bytes.Equal(current, intent.Before) && !bytes.Equal(current, intent.After) {
			return r, fmt.Errorf("canonical state changed outside original reconciliation")
		}
	} else {
		intent = publicationReconcileIntent{Claim: claim, ClaimSHA256: digest, MergeHead: mergeHead, Before: current}
		intent.After, err = publicationReconcileAfter(current, claim, mergeHead, time.Now().UTC())
		if err != nil {
			return r, err
		}
	}
	if err := validateMergedPublication(project, intent.Before, claim, mergeHead); err != nil {
		return r, err
	}
	if !exists {
		if err := os.MkdirAll(r.EvidencePath, 0700); err != nil {
			return r, err
		}
		encoded, err := json.MarshalIndent(intent, "", "  ")
		if err != nil {
			return r, err
		}
		if err := filex.WriteFileAtomicDurable(intentPath, append(encoded, '\n')); err != nil {
			return r, err
		}
	}
	if barrier != nil {
		barrier("after_intent")
	}
	if bytes.Equal(current, intent.Before) {
		if err := validateMergedPublication(project, intent.Before, claim, mergeHead); err != nil {
			return r, err
		}
		fresh, err := readAdmissionFile(filepath.Join(dir, "state.snapshot.json"))
		if err != nil {
			return r, err
		}
		if !bytes.Equal(fresh, current) {
			return r, fmt.Errorf("canonical state changed before reconciliation")
		}
		if err := filex.WriteFileAtomicDurable(filepath.Join(dir, "state.snapshot.json"), intent.After); err != nil {
			return r, err
		}
	}
	if barrier != nil {
		barrier("after_canonical")
	}
	if err := stoppedReviewProjection(dir, intent.After, true); err != nil {
		return r, err
	}
	r.Status = "completed"
	return r, nil
}

func validateMergedPublication(project string, before []byte, c PublicationClaim, mergeHead string) error {
	if c.SchemaVersion != publicationClaimSchema || c.Owner != "Noodle" || c.AuthorizesLanding || c.AuthorizesProviderWrite || !reconciliationSHA.MatchString(c.Head) || !reconciliationSHA.MatchString(c.Tree) {
		return fmt.Errorf("publication claim identity is invalid")
	}
	match := publicationSubjectPattern.FindStringSubmatch(c.Subject)
	if match == nil || match[1] != c.Repository {
		return fmt.Errorf("publication subject differs from repository")
	}
	remote, err := publicationGit(project, "remote", "get-url", "--push", "origin")
	if err != nil {
		return err
	}
	repo, err := githubRepository(remote)
	if err != nil || repo != c.Repository || remote != c.RemoteURL {
		return fmt.Errorf("publication repository changed")
	}
	if filepath.Base(c.SessionID) != c.SessionID || c.SessionID == "" || c.SessionID == "." {
		return fmt.Errorf("publication session path is invalid")
	}
	if c.Branch != c.WorktreeName && c.Branch != "noodle/"+c.WorktreeName {
		return fmt.Errorf("publication branch differs from original worktree")
	}
	top, err := publicationGit(project, "rev-parse", "--show-toplevel")
	if err != nil || top != project {
		return fmt.Errorf("reconciliation project is not its Git root")
	}
	var s reducer.DurableSnapshot
	if err := decodeStoppedReview(before, &s); err != nil {
		return err
	}
	for order := range s.State.PendingReviews {
		if order != c.OrderID {
			return fmt.Errorf("stopped reconciliation has another pending review %q", order)
		}
	}
	// Validate the prospective state too: only this order's obsolete pending
	// merge is retired; foreign/active effects remain blockers.
	after, err := publicationReconcileAfter(before, c, mergeHead, s.GeneratedAt)
	if err != nil {
		return err
	}
	var validated reducer.DurableSnapshot
	if err := decodeStoppedReview(after, &validated); err != nil {
		return err
	}
	if err := validateAdmissionSnapshot(validated); err != nil {
		return err
	}
	dir := filepath.Join(project, ".noodle")
	if err := observeAdmissionSessions(dir, s.State); err != nil {
		return err
	}
	stage := s.State.Orders[c.OrderID].Stages[c.StageIndex] // checked by publicationReconcileAfter
	attempt := stage.Attempts[len(stage.Attempts)-1]
	if attempt.AttemptID != c.AttemptID || attempt.SessionID != c.SessionID || attempt.WorktreeName != c.WorktreeName || (attempt.Status != state.AttemptCompleted && attempt.Status != state.AttemptFailed) {
		return fmt.Errorf("publication original attempt differs")
	}
	outcome, err := readRequiredStageOutcomeForIdentity(dir, c.SessionID, c.OrderID, c.StageIndex)
	if err != nil {
		return err
	}
	if outcome.Outcome != event.StageOutcomeCompleted || outcome.IsBlocking() {
		return fmt.Errorf("original writer has no completed outcome")
	}
	events, err := readAdmissionFile(filepath.Join(dir, "sessions", c.SessionID, "events.ndjson"))
	if err != nil {
		return err
	}
	if publicationDigest(events) != c.Evidence.SessionEventsSHA256 {
		return fmt.Errorf("publication session evidence changed")
	}
	spawnBytes, err := readAdmissionFile(filepath.Join(dir, "sessions", c.SessionID, "spawn.json"))
	if err != nil {
		return err
	}
	var spawn struct {
		SessionID    string `json:"session_id"`
		WorktreePath string `json:"worktree_path"`
		RetryCount   int    `json:"retry_count"`
	}
	if json.Unmarshal(spawnBytes, &spawn) != nil || spawn.SessionID != c.SessionID || spawn.WorktreePath != c.WorktreePath || spawn.RetryCount != len(stage.Attempts)-1 {
		return fmt.Errorf("publication spawn custody differs")
	}
	if _, err := stoppedReviewResidue(project, c); err != nil {
		return err
	}
	tree, err := exactPublicationObject(project, c.Head+"^{tree}")
	if err != nil || tree != c.Tree {
		return fmt.Errorf("publication candidate tree changed")
	}
	for _, pair := range [][2]string{{c.Head, mergeHead}, {mergeHead, "HEAD"}} {
		if err := publicationGitRun(project, "merge-base", "--is-ancestor", pair[0], pair[1]); err != nil {
			return fmt.Errorf("publication is not included in observed merge/control HEAD: %w", err)
		}
	}
	dirty, err := publicationGit(project, "status", "--porcelain", "--untracked-files=all")
	if err != nil || dirty != "" {
		return fmt.Errorf("reconciliation control checkout is not clean")
	}
	return nil
}

func publicationReconcileAfter(before []byte, c PublicationClaim, mergeHead string, at time.Time) ([]byte, error) {
	var s reducer.DurableSnapshot
	if err := decodeStoppedReview(before, &s); err != nil {
		return nil, err
	}
	o, ok := s.State.Orders[c.OrderID]
	if !ok || o.OrderID != c.OrderID || len(o.Stages) != 1 || c.StageIndex != 0 || o.Stages[0].StageIndex != 0 {
		return nil, fmt.Errorf("reconciliation requires original single-stage order")
	}
	stage := o.Stages[0]
	if (o.Status != state.OrderActive && o.Status != state.OrderFailed) || (stage.Status != state.StageReview && stage.Status != state.StageMerging && stage.Status != state.StageFailed) || len(stage.Attempts) == 0 {
		return nil, fmt.Errorf("original order is not awaiting merged reconciliation")
	}
	// Preserve every attempt and its error; completion is a separate owner fact.
	if stage.Extra == nil {
		stage.Extra = map[string]json.RawMessage{}
	}
	evidence, err := json.Marshal(map[string]string{"subject": c.Subject, "head": c.Head, "merge_head": mergeHead, "session_id": c.SessionID})
	if err != nil {
		return nil, err
	}
	stage.Extra["publication_reconciliation"] = evidence
	stage.Status, stage.Merge = state.StageCompleted, nil
	o.Stages[0], o.Status, o.UpdatedAt = stage, state.OrderCompleted, at
	s.State.Orders[c.OrderID] = o
	delete(s.State.PendingReviews, c.OrderID)
	for i := range s.EffectLedger {
		r := &s.EffectLedger[i]
		if r.Effect.Type != reducer.EffectMerge || r.Status != reducer.EffectLedgerPending {
			continue
		}
		var p struct {
			OrderID      string `json:"order_id"`
			StageIndex   int    `json:"stage_index"`
			WorktreeName string `json:"worktree_name"`
		}
		if err := json.Unmarshal(r.Effect.Payload, &p); err != nil {
			return nil, err
		}
		if p.OrderID != c.OrderID {
			continue
		}
		if p.StageIndex != c.StageIndex || p.WorktreeName != c.WorktreeName || r.Result != nil {
			return nil, fmt.Errorf("pending merge custody differs")
		}
		// This records cancellation now, not a fabricated historical execution.
		r.Status = reducer.EffectLedgerDone
		r.Result = &reducer.EffectResult{EffectID: r.EffectID, Status: reducer.EffectResultCancelled, Timestamp: at, Error: "superseded by observed external merge " + mergeHead}
	}
	s.GeneratedAt = at
	encoded, err := json.MarshalIndent(s, "", "  ")
	return append(encoded, '\n'), err
}
