package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"
)

var landBodyPattern = regexp.MustCompile(`^Refs ed3c/noodle#([1-9][0-9]*)$`)

func Land(ctx context.Context, client *GitHubClient, policy Policy, capabilities Capabilities, eventBytes []byte) (LandResult, error) {
	if err := validatePolicy(policy); err != nil {
		return LandResult{}, err
	}
	if err := validateCapabilities(capabilities); err != nil {
		return LandResult{}, err
	}
	if client.token == "" {
		return LandResult{}, fmt.Errorf("GITHUB_TOKEN is required for land")
	}
	var event WorkflowRunEvent
	if err := json.Unmarshal(eventBytes, &event); err != nil {
		return LandResult{}, fmt.Errorf("decode workflow_run event: %w", err)
	}
	run := event.WorkflowRun
	if event.Action != "completed" || event.Repository.FullName != targetRepository || event.Repository.DefaultBranch != policy.DefaultBranch || run.Name != "Test" || run.Event != "pull_request" || run.Status != "completed" || run.Conclusion != "success" || !sha40Pattern.MatchString(run.HeadSHA) {
		return LandResult{}, fmt.Errorf("land requires one completed successful pull_request Test run with an exact head")
	}
	if len(run.PullRequests) != 1 || run.PullRequests[0].Number < 1 {
		return LandResult{}, fmt.Errorf("land requires exactly one associated target PR")
	}
	pull, err := client.PullRequest(ctx, run.PullRequests[0].Number)
	if err != nil {
		return LandResult{}, err
	}
	match := landBodyPattern.FindStringSubmatch(pull.Body)
	if len(match) != 2 {
		return LandResult{}, fmt.Errorf("provider PR body is not an exact target Issue reference")
	}
	number, err := parseSubject(match[0][5:])
	if err != nil {
		return LandResult{}, err
	}
	wantBranch := fmt.Sprintf("noodles/issue-%d-%s", number, run.HeadSHA[:12])
	if pull.Head.SHA != run.HeadSHA || pull.Head.Ref != wantBranch || pull.Base.Ref != policy.DefaultBranch || (pull.State != "open" && !pull.Merged) {
		return LandResult{}, fmt.Errorf("provider PR shape does not match the exact workflow head")
	}
	issue, err := client.Issue(ctx, number)
	if err != nil {
		return LandResult{}, err
	}
	landedMarker := "<!-- noodles-state: landed -->"
	if pull.Merged {
		if issue.State != "open" && issue.State != "closed" {
			return LandResult{}, fmt.Errorf("merged PR Issue state is invalid")
		}
	} else if issue.State != "open" || !strings.Contains(issue.Body, "<!-- noodles-state: awaiting_land -->") {
		return LandResult{}, fmt.Errorf("Issue is not open at awaiting_land")
	}
	readyBody := strings.Replace(issue.Body, "<!-- noodles-state: awaiting_land -->", "<!-- noodles-state: ready -->", 1)
	if strings.Contains(issue.Body, landedMarker) {
		readyBody = strings.Replace(issue.Body, landedMarker, "<!-- noodles-state: ready -->", 1)
	}
	contract, err := parseIssueContract(readyBody, number)
	if err != nil {
		return LandResult{}, err
	}
	comments, err := client.Comments(ctx, number)
	if err != nil {
		return LandResult{}, err
	}
	base, err := client.DefaultBranchHead(ctx, policy.DefaultBranch)
	if err != nil {
		return LandResult{}, err
	}
	authorizationBase := base
	if pull.Merged {
		authorizationBase = ""
	}
	receipt, err := landAuthorization(comments, policy, fmt.Sprintf("%s#%d", targetRepository, number), readyBody, authorizationBase, contract)
	if err != nil {
		return LandResult{}, err
	}
	files, err := client.PullRequestFiles(ctx, pull.Number)
	if err != nil {
		return LandResult{}, err
	}
	if len(files) == 0 {
		return LandResult{}, fmt.Errorf("provider PR has no changed paths")
	}
	for _, file := range files {
		path := file.Filename
		if strings.HasPrefix(path, ".github/workflows/") || !pathAdmitted(path, receipt.Declaration.WriteBoundary) {
			return LandResult{}, fmt.Errorf("changed path %q is not landable", path)
		}
	}
	mergeSHA := pull.MergeCommitSHA
	status := "reconciled"
	if !pull.Merged {
		merged, err := client.MergePullRequest(ctx, pull.Number, run.HeadSHA)
		if err != nil {
			return LandResult{}, err
		}
		if !merged.Merged || !sha40Pattern.MatchString(merged.SHA) {
			return LandResult{}, fmt.Errorf("provider did not merge the exact candidate head")
		}
		mergeSHA = merged.SHA
		status = "merged"
	}
	pull, err = client.PullRequest(ctx, pull.Number)
	if err != nil || !pull.Merged || pull.Head.SHA != run.HeadSHA || pull.MergeCommitSHA != mergeSHA {
		return LandResult{}, fmt.Errorf("provider merge readback was not exact")
	}
	mainHead, err := client.DefaultBranchHead(ctx, policy.DefaultBranch)
	if err != nil || mainHead != mergeSHA {
		return LandResult{}, fmt.Errorf("default branch did not read back the merge commit")
	}
	landedBody := strings.Replace(readyBody, "<!-- noodles-state: ready -->", landedMarker, 1)
	if issue.Body != landedBody {
		if _, err := client.UpdateIssueBody(ctx, number, landedBody); err != nil {
			return LandResult{}, err
		}
	}
	issue, err = client.Issue(ctx, number)
	if err != nil || issue.Body != landedBody {
		return LandResult{}, fmt.Errorf("Issue landed marker readback mismatched")
	}
	if issue.State != "closed" || issue.StateReason != "completed" {
		if _, err := client.CloseIssue(ctx, number); err != nil {
			return LandResult{}, err
		}
	}
	issue, err = client.Issue(ctx, number)
	if err != nil || issue.State != "closed" || issue.StateReason != "completed" || issue.Body != landedBody {
		return LandResult{}, fmt.Errorf("Issue closure readback mismatched")
	}
	return LandResult{Status: status, Head: run.HeadSHA, Merge: mergeSHA, PR: pull.Number, Issue: number}, nil
}

func landAuthorization(comments []Comment, policy Policy, subject, body, base string, contract issueContract) (Authorization, error) {
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
		baseMatches := base == "" || d.BaseSHA == base
		if receipt.Sender == policy.RepositoryDispatchSender && d.SourceRepository == sourceRepository && d.Target == targetRepository && d.Subject == subject && d.SubjectBodySHA256 == hex.EncodeToString(digest[:]) && baseMatches && d.Runtime == contract.Runtime && d.Evidence == contract.Evidence && reflect.DeepEqual(d.WriteBoundary, contract.WriteBoundary) {
			matches = append(matches, receipt)
		}
	}
	if len(matches) != 1 {
		return Authorization{}, fmt.Errorf("land requires one current authorization, got %d", len(matches))
	}
	return matches[0], nil
}
