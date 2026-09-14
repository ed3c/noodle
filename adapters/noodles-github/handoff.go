package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
)

func Handoff(ctx context.Context, client *GitHubClient, policy Policy, capabilities Capabilities, subject, worktree string) (HandoffResult, error) {
	if err := validatePolicy(policy); err != nil {
		return HandoffResult{}, err
	}
	if err := validateCapabilities(capabilities); err != nil {
		return HandoffResult{}, err
	}
	number, err := parseSubject(subject)
	if err != nil {
		return HandoffResult{}, err
	}
	if client.token == "" {
		return HandoffResult{}, fmt.Errorf("GITHUB_TOKEN is required for handoff")
	}
	branch, err := gitValue(worktree, "branch", "--show-current")
	if err != nil || branch == "" {
		return HandoffResult{}, fmt.Errorf("read worktree branch: %w", err)
	}
	if branch == policy.DefaultBranch {
		return HandoffResult{}, fmt.Errorf("handoff refuses default branch %q", branch)
	}
	status, err := gitValue(worktree, "status", "--porcelain")
	if err != nil {
		return HandoffResult{}, err
	}
	if status != "" {
		return HandoffResult{}, fmt.Errorf("handoff requires a clean worktree")
	}
	head, err := gitValue(worktree, "rev-parse", "HEAD")
	if err != nil || !sha40Pattern.MatchString(head) {
		return HandoffResult{}, fmt.Errorf("candidate HEAD is not exact")
	}
	if _, err := gitValue(worktree, "remote", "get-url", "--push", policy.PushRemote); err != nil {
		return HandoffResult{}, fmt.Errorf("policy/github.json push_remote %q is unavailable: %w", policy.PushRemote, err)
	}

	repository, err := client.Repository(ctx)
	if err != nil {
		return HandoffResult{}, err
	}
	if repository.FullName != targetRepository || repository.DefaultBranch != policy.DefaultBranch {
		return HandoffResult{}, fmt.Errorf("provider repository/default branch drifted")
	}
	base, err := client.DefaultBranchHead(ctx, policy.DefaultBranch)
	if err != nil {
		return HandoffResult{}, err
	}
	issue, err := client.Issue(ctx, number)
	if err != nil {
		return HandoffResult{}, err
	}
	readyBody := issue.Body
	state := "ready"
	if strings.Contains(readyBody, "<!-- noodles-state: awaiting_land -->") {
		state = "awaiting_land"
		readyBody = strings.Replace(readyBody, "<!-- noodles-state: awaiting_land -->", "<!-- noodles-state: ready -->", 1)
	}
	contract, err := parseIssueContract(readyBody, number)
	if err != nil {
		return HandoffResult{}, err
	}
	if issue.State != "open" || len(issue.PullRequest) != 0 {
		return HandoffResult{}, fmt.Errorf("subject is not an open provider Issue")
	}
	comments, err := client.Comments(ctx, number)
	if err != nil {
		return HandoffResult{}, err
	}
	receipt, err := currentAuthorization(comments, policy, subject, readyBody, base, contract)
	if err != nil {
		return HandoffResult{}, err
	}
	if gitCommand(worktree, "merge-base", "--is-ancestor", receipt.Declaration.BaseSHA, head) != nil {
		return HandoffResult{}, fmt.Errorf("candidate HEAD is not descended from authorization base")
	}
	changed, err := gitValue(worktree, "diff", "--name-only", receipt.Declaration.BaseSHA+".."+head)
	if err != nil {
		return HandoffResult{}, err
	}
	if changed == "" {
		return HandoffResult{}, fmt.Errorf("candidate contains no committed changes")
	}
	for _, path := range strings.Split(changed, "\n") {
		if !pathAdmitted(filepath.ToSlash(path), receipt.Declaration.WriteBoundary) {
			return HandoffResult{}, fmt.Errorf("changed path %q escapes authorization boundary", path)
		}
	}

	handoffBranch := fmt.Sprintf("noodles/issue-%d-%s", number, head[:12])
	ref, exists, err := client.Branch(ctx, handoffBranch)
	if err != nil {
		return HandoffResult{}, err
	}
	if exists && ref.Object.SHA != head {
		return HandoffResult{}, fmt.Errorf("provider branch %q names %s, want %s", handoffBranch, ref.Object.SHA, head)
	}
	if !exists {
		if err := gitCommand(worktree, "push", policy.PushRemote, head+":refs/heads/"+handoffBranch); err != nil {
			return HandoffResult{}, fmt.Errorf("push handoff branch: %w", err)
		}
		ref, exists, err = client.Branch(ctx, handoffBranch)
		if err != nil || !exists || ref.Object.SHA != head {
			return HandoffResult{}, fmt.Errorf("provider branch readback did not return exact candidate head")
		}
	}
	wantBody := "Refs " + subject
	pulls, err := client.OpenPullRequests(ctx)
	if err != nil {
		return HandoffResult{}, err
	}
	var pull PullRequest
	for _, candidate := range pulls {
		competes := candidate.Head.Ref == handoffBranch || candidate.Head.SHA == head
		if !competes {
			continue
		}
		if candidate.State != "open" || candidate.Head.Ref != handoffBranch || candidate.Head.SHA != head || candidate.Base.Ref != policy.DefaultBranch || candidate.Body != wantBody {
			return HandoffResult{}, fmt.Errorf("competing provider PR %d has a non-exact shape", candidate.Number)
		}
		if pull.Number != 0 {
			return HandoffResult{}, fmt.Errorf("multiple exact provider PRs exist")
		}
		pull = candidate
	}
	created := false
	if pull.Number == 0 {
		pull, err = client.CreatePullRequest(ctx, issue.Title, handoffBranch, wantBody, policy.DefaultBranch)
		if err != nil {
			return HandoffResult{}, err
		}
		created = true
	}
	pull, err = client.PullRequest(ctx, pull.Number)
	if err != nil || pull.State != "open" || pull.Head.Ref != handoffBranch || pull.Head.SHA != head || pull.Base.Ref != policy.DefaultBranch || pull.Body != wantBody {
		return HandoffResult{}, fmt.Errorf("provider PR readback was not exact")
	}
	if state == "ready" {
		latest, err := client.Issue(ctx, number)
		if err != nil {
			return HandoffResult{}, err
		}
		wantDigest := sha256.Sum256([]byte(readyBody))
		gotDigest := sha256.Sum256([]byte(latest.Body))
		if hex.EncodeToString(gotDigest[:]) != hex.EncodeToString(wantDigest[:]) || latest.State != "open" {
			return HandoffResult{}, fmt.Errorf("provider Issue changed before lifecycle write")
		}
		awaiting := strings.Replace(readyBody, "<!-- noodles-state: ready -->", "<!-- noodles-state: awaiting_land -->", 1)
		if awaiting == readyBody {
			return HandoffResult{}, fmt.Errorf("ready lifecycle marker is absent")
		}
		if _, err := client.UpdateIssueBody(ctx, number, awaiting); err != nil {
			return HandoffResult{}, err
		}
		latest, err = client.Issue(ctx, number)
		if err != nil || latest.Body != awaiting || latest.State != "open" {
			return HandoffResult{}, fmt.Errorf("provider Issue lifecycle readback mismatched")
		}
	}
	statusName := "reused"
	if created {
		statusName = "created"
	}
	return HandoffResult{Status: statusName, Branch: handoffBranch, Head: head, PR: pull.Number}, nil
}

func currentAuthorization(comments []Comment, policy Policy, subject, body, base string, contract issueContract) (Authorization, error) {
	digest := sha256.Sum256([]byte(body))
	var matches []Authorization
	for _, comment := range comments {
		if comment.User.Login != policy.AuthorizationCommentAuthor || !strings.HasPrefix(comment.Body, authorizationMarker) {
			continue
		}
		receipt, err := ParseAuthorization(comment.Body)
		if err != nil {
			return Authorization{}, err
		}
		d := receipt.Declaration
		if receipt.Sender == policy.RepositoryDispatchSender && d.SourceRepository == sourceRepository && d.Target == targetRepository && d.Subject == subject && d.SubjectBodySHA256 == hex.EncodeToString(digest[:]) && d.BaseSHA == base && d.Runtime == contract.Runtime && d.Evidence == contract.Evidence && reflect.DeepEqual(d.WriteBoundary, contract.WriteBoundary) {
			matches = append(matches, receipt)
		}
	}
	if len(matches) != 1 {
		return Authorization{}, fmt.Errorf("handoff requires one current authorization, got %d", len(matches))
	}
	return matches[0], nil
}

func pathAdmitted(path string, boundaries []string) bool {
	for _, boundary := range boundaries {
		boundary = strings.TrimSuffix(filepath.ToSlash(boundary), "/")
		if path == boundary || strings.HasPrefix(path, boundary+"/") {
			return true
		}
	}
	return false
}

func gitValue(dir string, args ...string) (string, error) {
	command := exec.Command("git", args...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return strings.TrimSpace(string(output)), nil
}
func gitCommand(dir string, args ...string) error { _, err := gitValue(dir, args...); return err }
