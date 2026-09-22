package loop

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/orderx"
	"github.com/poteto/noodle/internal/reducer"
)

func decodeStoppedReview(data []byte, out any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("recovery evidence has trailing JSON")
	}
	return nil
}

func stoppedReviewDigest(i stoppedReviewIntent) string {
	data, _ := json.Marshal(struct {
		Claim  PublicationClaim
		Before []byte
		Files  map[string][]byte
	}{i.Claim, i.Before, i.Files})
	return publicationDigest(data)
}

func captureStoppedReview(project, order, subject string) (stoppedReviewIntent, error) {
	var i stoppedReviewIntent
	claim, err := InspectPublicationClaim(project, order, subject)
	if err != nil {
		return i, err
	}
	if filepath.Base(claim.SessionID) != claim.SessionID || claim.SessionID == "." || filepath.Base(claim.WorktreeName) != claim.WorktreeName || claim.WorktreeName == "." {
		return i, fmt.Errorf("review custody path is not an exact component")
	}
	if claim.Branch != claim.WorktreeName && claim.Branch != "noodle/"+claim.WorktreeName {
		return i, fmt.Errorf("worktree branch does not match cleanup owner")
	}
	i.Claim = claim
	dir := filepath.Join(project, ".noodle")
	i.Before, err = readAdmissionFile(filepath.Join(dir, "state.snapshot.json"))
	if err != nil {
		return i, err
	}
	if publicationDigest(i.Before) != claim.Evidence.CanonicalSnapshotSHA256 {
		return i, fmt.Errorf("snapshot changed during custody readback")
	}
	var s reducer.DurableSnapshot
	if err := decodeStoppedReview(i.Before, &s); err != nil {
		return i, err
	}
	if !orderx.ValidOrderRevision(s.OrderRevision) || s.GeneratedAt.IsZero() || s.EffectLedger == nil {
		return i, fmt.Errorf("canonical recovery evidence is incomplete")
	}
	if len(s.State.PendingReviews) != 1 {
		return i, fmt.Errorf("stopped rejection requires one exact pending review")
	}
	if err := validateAdmissionSnapshot(s); err != nil {
		return i, err
	}
	if err := observeAdmissionSessions(dir, s.State); err != nil {
		return i, err
	}
	i.Files = map[string][]byte{}
	for _, name := range append(append([]string{}, recoverySessionFiles...), "meta.json") {
		data, err := readAdmissionFile(filepath.Join(dir, "sessions", claim.SessionID, name))
		if err != nil {
			return i, err
		}
		i.Files[name] = data
	}
	var spawn struct {
		SessionID    string `json:"session_id"`
		WorktreePath string `json:"worktree_path"`
		RetryCount   int    `json:"retry_count"`
	}
	if json.Unmarshal(i.Files["spawn.json"], &spawn) != nil || spawn.SessionID != claim.SessionID || spawn.RetryCount != len(s.State.Orders[order].Stages[claim.StageIndex].Attempts)-1 {
		return i, fmt.Errorf("review spawn identity differs")
	}
	spawnPath, err := canonicalDirectory(spawn.WorktreePath)
	if err != nil || spawnPath != claim.WorktreePath {
		return i, fmt.Errorf("review spawn worktree differs")
	}
	if publicationDigest(i.Files["events.ndjson"]) != claim.Evidence.SessionEventsSHA256 {
		return i, fmt.Errorf("session events changed during readback")
	}
	// Canonical custody owns the transition. Require its public review mirror to
	// agree before beginning, rather than silently repair unrelated old state.
	reviews, err := ReadPendingReview(dir)
	if err != nil {
		return i, err
	}
	if len(reviews) != 1 || reviews[0].OrderID != order || reviews[0].StageIndex != claim.StageIndex || reviews[0].SessionID != claim.SessionID || reviews[0].WorktreePath != s.State.PendingReviews[order].WorktreePath || reviews[0].WorktreeName != claim.WorktreeName {
		return i, fmt.Errorf("pending review projection differs")
	}
	orders, err := readOrders(filepath.Join(dir, "orders.json"))
	if err != nil {
		return i, err
	}
	if err := validateStoppedProjection(s.State, orders, true); err != nil {
		return i, err
	}
	i.Digest = stoppedReviewDigest(i)
	return i, nil
}

func validateStoppedSessions(dir string, i stoppedReviewIntent, current []byte) error {
	var s reducer.DurableSnapshot
	if err := decodeStoppedReview(current, &s); err != nil {
		return err
	}
	if err := validateAdmissionSnapshot(s); err != nil {
		return err
	}
	if err := observeAdmissionSessions(dir, s.State); err != nil {
		return err
	}
	for name, want := range i.Files {
		if filepath.Base(name) != name {
			return fmt.Errorf("archive session path is invalid")
		}
		got, err := readAdmissionFile(filepath.Join(dir, "sessions", i.Claim.SessionID, name))
		if err != nil {
			return err
		}
		if !bytes.Equal(got, want) {
			return fmt.Errorf("original session %s changed", name)
		}
	}
	return nil
}

func archiveStoppedCandidate(project, path string, i *stoppedReviewIntent) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	bundle := filepath.Join(path, "candidate.bundle")
	if _, err := os.Lstat(bundle); os.IsNotExist(err) {
		if err := publicationGitRun(project, "bundle", "create", bundle, "refs/heads/"+i.Claim.Branch); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	data, err := readAdmissionFile(bundle)
	if err != nil {
		return err
	}
	i.Bundle = publicationDigest(data)
	// Sync the complete recoverable object archive before recording intent.
	if err := filex.WriteFileAtomicDurable(bundle, data); err != nil {
		return err
	}
	return validateStoppedIntent(project, path, *i)
}

func validateStoppedIntent(project, path string, i stoppedReviewIntent) error {
	var after reducer.DurableSnapshot
	if err := decodeStoppedReview(i.After, &after); err != nil {
		return err
	}
	expected, err := stoppedReviewAfter(i.Before, i.Claim, after.GeneratedAt)
	if err != nil {
		return err
	}
	if !bytes.Equal(expected, i.After) {
		return fmt.Errorf("archived transition differs from existing review reducer")
	}
	bundle := filepath.Join(path, "candidate.bundle")
	data, err := readAdmissionFile(bundle)
	if err != nil {
		return err
	}
	if i.Bundle == "" || publicationDigest(data) != i.Bundle {
		return fmt.Errorf("candidate archive digest changed")
	}
	heads, err := publicationGit(project, "bundle", "list-heads", bundle)
	if err != nil {
		return err
	}
	if heads != i.Claim.Head+" refs/heads/"+i.Claim.Branch {
		return fmt.Errorf("candidate archive identity differs")
	}
	return publicationGitRun(project, "bundle", "verify", bundle)
}

// Observe absence separately from errors; a failing Git read is never proof
// that a branch/worktree disappeared. Partial cleanup resumes only exact refs.
func stoppedReviewResidue(project string, c PublicationClaim) (bool, error) {
	if c.WorktreeName == "" || filepath.Base(c.WorktreeName) != c.WorktreeName || c.WorktreeName == "." || c.WorktreePath != filepath.Join(project, ".worktrees", c.WorktreeName) {
		return false, fmt.Errorf("cleanup path differs from exact owner")
	}
	// Cleanup resolves an unprefixed branch before noodle/<name>. Never allow
	// that compatibility lookup to select a different branch from this claim.
	for _, branch := range []string{c.WorktreeName, "noodle/" + c.WorktreeName} {
		if branch == c.Branch {
			continue
		}
		alias, err := publicationGit(project, "for-each-ref", "--format=%(refname)", "refs/heads/"+branch)
		if err != nil {
			return false, err
		}
		if alias != "" {
			return false, fmt.Errorf("cleanup branch alias has foreign custody")
		}
	}
	refs, err := publicationGit(project, "for-each-ref", "--format=%(objectname) %(refname)", "refs/heads/"+c.Branch)
	if err != nil {
		return false, err
	}
	if refs != "" && refs != c.Head+" refs/heads/"+c.Branch {
		return false, fmt.Errorf("candidate branch changed during cleanup")
	}
	entries, err := publicationGit(project, "worktree", "list", "--porcelain")
	if err != nil {
		return false, err
	}
	registered := false
	for _, block := range strings.Split(entries, "\n\n") {
		lines := strings.Split(block, "\n")
		if len(lines) > 0 && lines[0] == "worktree "+c.WorktreePath {
			registered = true
			if !strings.Contains("\n"+block+"\n", "\nHEAD "+c.Head+"\n") || !strings.Contains("\n"+block+"\n", "\nbranch refs/heads/"+c.Branch+"\n") {
				return false, fmt.Errorf("registered worktree identity changed")
			}
		} else if strings.Contains("\n"+block+"\n", "\nbranch refs/heads/"+c.Branch+"\n") {
			return false, fmt.Errorf("candidate branch acquired by a foreign worktree")
		}
	}
	info, err := os.Lstat(c.WorktreePath)
	if os.IsNotExist(err) {
		return registered || refs != "", nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || !registered || refs == "" {
		return false, fmt.Errorf("worktree residue has ambiguous custody")
	}
	top, err := publicationGit(c.WorktreePath, "rev-parse", "--show-toplevel")
	if err != nil {
		return false, err
	}
	if top != c.WorktreePath {
		return false, fmt.Errorf("cleanup worktree root changed")
	}
	common, err := publicationGit(c.WorktreePath, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return false, err
	}
	owner, err := publicationGit(project, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return false, err
	}
	if common != owner {
		return false, fmt.Errorf("cleanup worktree belongs to another repository")
	}
	dirty, err := publicationGit(c.WorktreePath, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return false, err
	}
	if dirty != "" {
		return false, fmt.Errorf("cleanup candidate is dirty")
	}
	return true, nil
}

func stoppedReviewProjection(dir string, after []byte, write bool) error {
	var s reducer.DurableSnapshot
	if err := decodeStoppedReview(after, &s); err != nil {
		return err
	}
	if write {
		l := &Loop{runtimeDir: dir, canonical: s.State, canonicalLoaded: true}
		if err := l.writeProjectionState(); err != nil {
			return err
		}
		if err := l.writePendingReview(); err != nil {
			return err
		}
	}
	if _, _, err := readAdmissionEvidence(dir); err != nil {
		return err
	}
	reviews, err := ReadPendingReview(dir)
	if err != nil {
		return err
	}
	if len(reviews) != 0 {
		return fmt.Errorf("pending review residue remains")
	}
	return nil
}
