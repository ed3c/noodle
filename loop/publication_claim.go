package loop

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/poteto/noodle/event"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
)

const publicationClaimSchema = 1

var publicationSubjectPattern = regexp.MustCompile(`^([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)#([1-9][0-9]*)$`)

// PublicationClaim is a read-only custody receipt for one supervised,
// completed cook. It grants no provider-write or landing authority.
type PublicationClaim struct {
	SchemaVersion           int                      `json:"schema_version"`
	Owner                   string                   `json:"owner"`
	Repository              string                   `json:"repository"`
	Subject                 string                   `json:"subject"`
	OrderID                 string                   `json:"order_id"`
	StageIndex              int                      `json:"stage_index"`
	AttemptID               string                   `json:"attempt_id"`
	SessionID               string                   `json:"session_id"`
	WorktreeName            string                   `json:"worktree_name"`
	WorktreePath            string                   `json:"worktree_path"`
	Branch                  string                   `json:"branch"`
	Head                    string                   `json:"head"`
	Tree                    string                   `json:"tree"`
	BaseBranch              string                   `json:"base_branch"`
	BaseHead                string                   `json:"base_head"`
	PushRemote              string                   `json:"push_remote"`
	RemoteURL               string                   `json:"remote_url"`
	Evidence                PublicationClaimEvidence `json:"evidence"`
	AuthorizesProviderWrite bool                     `json:"authorizes_provider_write"`
	AuthorizesLanding       bool                     `json:"authorizes_landing"`
}

type PublicationClaimEvidence struct {
	CanonicalSnapshotSHA256 string `json:"canonical_snapshot_sha256"`
	SessionEventsSHA256     string `json:"session_events_sha256"`
}

// InspectPublicationClaim binds canonical Noodle custody to exact Git objects.
// It performs no state transition and no provider operation.
func InspectPublicationClaim(projectDir, orderID, subject string) (PublicationClaim, error) {
	claim := PublicationClaim{SchemaVersion: publicationClaimSchema, Owner: "Noodle"}
	projectDir, err := canonicalDirectory(projectDir)
	if err != nil {
		return claim, fmt.Errorf("publication project: %w", err)
	}
	orderID = strings.TrimSpace(orderID)
	if orderID == "" {
		return claim, fmt.Errorf("publication order_id is required")
	}
	subject = strings.TrimSpace(subject)
	match := publicationSubjectPattern.FindStringSubmatch(subject)
	if match == nil {
		return claim, fmt.Errorf("publication subject %q is not OWNER/REPOSITORY#N", subject)
	}

	runtimeDir := filepath.Join(projectDir, ".noodle")
	snapshotPath := filepath.Join(runtimeDir, "state.snapshot.json")
	snapshotBytes, err := os.ReadFile(snapshotPath)
	if err != nil {
		return claim, fmt.Errorf("publication canonical snapshot: %w", err)
	}
	snapshot, err := reducer.ReadSnapshot(snapshotPath)
	if err != nil {
		return claim, fmt.Errorf("publication canonical snapshot: %w", err)
	}
	if snapshot.State.Mode != state.RunModeSupervised {
		return claim, fmt.Errorf("publication requires supervised mode, got %q", snapshot.State.Mode)
	}
	order, ok := snapshot.State.Orders[orderID]
	if !ok || order.OrderID != orderID || order.Status != state.OrderActive {
		return claim, fmt.Errorf("publication active canonical order %q is missing", orderID)
	}
	review, ok := snapshot.State.PendingReviews[orderID]
	if !ok || review.OrderID != orderID {
		return claim, fmt.Errorf("publication pending review %q is missing", orderID)
	}
	if review.StageIndex < 0 || review.StageIndex >= len(order.Stages) {
		return claim, fmt.Errorf("publication review stage %d is invalid", review.StageIndex)
	}
	stage := order.Stages[review.StageIndex]
	if stage.StageIndex != review.StageIndex || stage.Status != state.StageReview {
		return claim, fmt.Errorf("publication stage is not parked for review")
	}
	if len(stage.Attempts) == 0 {
		return claim, fmt.Errorf("publication terminal attempt is missing")
	}
	attempt := stage.Attempts[len(stage.Attempts)-1]
	if attempt.Status != state.AttemptCompleted || attempt.SessionID == "" || attempt.SessionID != review.SessionID || attempt.WorktreeName != review.WorktreeName {
		return claim, fmt.Errorf("publication terminal attempt does not match review custody")
	}
	outcome, err := readRequiredStageOutcomeForIdentity(runtimeDir, review.SessionID, orderID, review.StageIndex)
	if err != nil {
		return claim, fmt.Errorf("publication typed outcome: %w", err)
	}
	if outcome.Outcome != event.StageOutcomeCompleted || outcome.IsBlocking() {
		return claim, fmt.Errorf("publication requires a non-blocking completed typed outcome")
	}

	worktreeName := strings.TrimSpace(review.WorktreeName)
	worktreePath, err := canonicalDirectory(review.WorktreePath)
	if err != nil {
		return claim, fmt.Errorf("publication worktree: %w", err)
	}
	expectedPath := filepath.Join(projectDir, ".worktrees", worktreeName)
	expectedPath, err = canonicalDirectory(expectedPath)
	if err != nil || worktreeName == "" || worktreePath != expectedPath || worktreePath == projectDir {
		return claim, fmt.Errorf("publication worktree is not the claimed Noodle linked worktree")
	}
	top, err := publicationGit(worktreePath, "rev-parse", "--show-toplevel")
	if err != nil || top != worktreePath {
		return claim, fmt.Errorf("publication worktree root mismatch")
	}
	worktreeCommon, err := publicationGit(worktreePath, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return claim, err
	}
	projectCommon, err := publicationGit(projectDir, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || worktreeCommon != projectCommon {
		return claim, fmt.Errorf("publication worktree owner mismatch")
	}
	status, err := publicationGit(worktreePath, "status", "--porcelain", "--untracked-files=all")
	if err != nil || status != "" {
		return claim, fmt.Errorf("publication requires a clean worktree")
	}
	branch, err := publicationGit(worktreePath, "symbolic-ref", "--short", "HEAD")
	if err != nil || branch == "" {
		return claim, fmt.Errorf("publication requires an attached branch")
	}
	head, err := exactPublicationObject(worktreePath, "HEAD")
	if err != nil {
		return claim, err
	}
	tree, err := exactPublicationObject(worktreePath, "HEAD^{tree}")
	if err != nil {
		return claim, err
	}
	remote := "origin"
	remoteURL, err := publicationGit(projectDir, "remote", "get-url", "--push", remote)
	if err != nil {
		return claim, fmt.Errorf("publication push remote: %w", err)
	}
	repository, err := githubRepository(remoteURL)
	if err != nil {
		return claim, err
	}
	if repository != match[1] {
		return claim, fmt.Errorf("publication subject repository %q does not match remote %q", match[1], repository)
	}
	remoteHead, err := publicationGit(projectDir, "symbolic-ref", "--short", "refs/remotes/"+remote+"/HEAD")
	if err != nil || !strings.HasPrefix(remoteHead, remote+"/") {
		return claim, fmt.Errorf("publication remote default branch is unavailable")
	}
	baseBranch := strings.TrimPrefix(remoteHead, remote+"/")
	baseHead, err := exactPublicationObject(projectDir, "refs/remotes/"+remote+"/"+baseBranch)
	if err != nil {
		return claim, err
	}
	if publicationGitRun(worktreePath, "merge-base", "--is-ancestor", baseHead, head) != nil {
		return claim, fmt.Errorf("publication candidate is not descended from remote base %s", baseHead)
	}
	changed, err := publicationGit(worktreePath, "diff", "--name-only", baseHead+".."+head)
	if err != nil || changed == "" {
		return claim, fmt.Errorf("publication candidate has no committed changes")
	}

	eventsPath := filepath.Join(runtimeDir, "sessions", review.SessionID, "events.ndjson")
	eventsBytes, err := os.ReadFile(eventsPath)
	if err != nil {
		return claim, fmt.Errorf("publication session events: %w", err)
	}
	claim.Repository = repository
	claim.Subject = subject
	claim.OrderID = orderID
	claim.StageIndex = review.StageIndex
	claim.AttemptID = attempt.AttemptID
	claim.SessionID = review.SessionID
	claim.WorktreeName = worktreeName
	claim.WorktreePath = worktreePath
	claim.Branch = branch
	claim.Head = head
	claim.Tree = tree
	claim.BaseBranch = baseBranch
	claim.BaseHead = baseHead
	claim.PushRemote = remote
	claim.RemoteURL = remoteURL
	claim.Evidence = PublicationClaimEvidence{
		CanonicalSnapshotSHA256: publicationDigest(snapshotBytes),
		SessionEventsSHA256:     publicationDigest(eventsBytes),
	}
	return claim, nil
}

func canonicalDirectory(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("path is empty")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", resolved)
	}
	return resolved, nil
}

func publicationGit(dir string, args ...string) (string, error) {
	command := exec.Command("git", args...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}

func publicationGitRun(dir string, args ...string) error {
	_, err := publicationGit(dir, args...)
	return err
}

func exactPublicationObject(dir, revision string) (string, error) {
	value, err := publicationGit(dir, "rev-parse", revision)
	if err != nil || !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(value) {
		return "", fmt.Errorf("publication Git object %q is not exact", revision)
	}
	return value, nil
}

func githubRepository(remote string) (string, error) {
	value := strings.TrimSpace(remote)
	var path string
	if strings.HasPrefix(value, "git@github.com:") {
		path = strings.TrimPrefix(value, "git@github.com:")
	} else {
		parsed, err := url.Parse(value)
		if err != nil || parsed.Hostname() != "github.com" || (parsed.Scheme != "https" && parsed.Scheme != "ssh") {
			return "", fmt.Errorf("publication remote is not an exact GitHub repository")
		}
		path = strings.TrimPrefix(parsed.Path, "/")
	}
	path = strings.TrimSuffix(path, ".git")
	if !regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`).MatchString(path) {
		return "", fmt.Errorf("publication remote repository is invalid")
	}
	return path, nil
}

func publicationDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}
