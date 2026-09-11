package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"reflect"
	"sort"
	"strings"
)

func Sync(ctx context.Context, client *GitHubClient, policy Policy, capabilities Capabilities) ([]BacklogItem, []Diagnostic, error) {
	if err := validatePolicy(policy); err != nil {
		return nil, nil, err
	}
	if err := validateCapabilities(capabilities); err != nil {
		return nil, nil, err
	}
	repository, err := client.Repository(ctx)
	if err != nil {
		return nil, nil, err
	}
	if repository.FullName != targetRepository || repository.DefaultBranch != policy.DefaultBranch {
		return nil, nil, fmt.Errorf("provider repository/default branch drifted")
	}
	baseHead, err := client.DefaultBranchHead(ctx, policy.DefaultBranch)
	if err != nil {
		return nil, nil, err
	}
	if !sha40Pattern.MatchString(baseHead) {
		return nil, nil, fmt.Errorf("provider default branch head %q is not exact", baseHead)
	}
	issues, err := client.OpenIssues(ctx)
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(issues, func(i, j int) bool { return issues[i].Number < issues[j].Number })
	items := make([]BacklogItem, 0)
	diagnostics := make([]Diagnostic, 0)
	for _, issue := range issues {
		subject := fmt.Sprintf("%s#%d", targetRepository, issue.Number)
		if issue.State != "open" || len(issue.PullRequest) != 0 {
			diagnostics = append(diagnostics, diagnostic(subject, "not_open_issue", "provider row is not an open Issue"))
			continue
		}
		issue, issueErr := client.Issue(ctx, issue.Number)
		if issueErr != nil {
			diagnostics = append(diagnostics, diagnostic(subject, "provider_unreadable", issueErr.Error()))
			continue
		}
		if issue.State != "open" || len(issue.PullRequest) != 0 {
			diagnostics = append(diagnostics, diagnostic(subject, "not_open_issue", "provider Issue changed while synchronizing"))
			continue
		}
		contract, contractErr := parseIssueContract(issue.Body, issue.Number)
		if contractErr != nil {
			diagnostics = append(diagnostics, diagnostic(subject, "malformed_issue", contractErr.Error()))
			continue
		}
		comments, commentsErr := client.Comments(ctx, issue.Number)
		if commentsErr != nil {
			diagnostics = append(diagnostics, diagnostic(subject, "provider_unreadable", commentsErr.Error()))
			continue
		}
		valid := make([]Authorization, 0)
		invalidCode := ""
		sawUntrustedReceipt := false
		for _, comment := range comments {
			if !strings.HasPrefix(comment.Body, authorizationMarker) {
				continue
			}
			if comment.User.Login != policy.AuthorizationCommentAuthor {
				sawUntrustedReceipt = true
				continue
			}
			receipt, parseErr := ParseAuthorization(comment.Body)
			if parseErr != nil {
				invalidCode = "malformed_receipt"
				break
			}
			if receipt.Declaration.Target != targetRepository || receipt.Declaration.Subject != subject {
				invalidCode = "foreign_subject"
				break
			}
			valid = append(valid, receipt)
		}
		if invalidCode != "" {
			diagnostics = append(diagnostics, diagnostic(subject, invalidCode, "authorization history contains a non-target receipt"))
			continue
		}
		current := make([]Authorization, 0)
		bodySum := sha256.Sum256([]byte(issue.Body))
		bodyDigest := hex.EncodeToString(bodySum[:])
		for _, receipt := range valid {
			declaration := receipt.Declaration
			if receipt.Sender == policy.RepositoryDispatchSender &&
				declaration.SourceRepository == sourceRepository &&
				declaration.SubjectBodySHA256 == bodyDigest && declaration.BaseSHA == baseHead &&
				declaration.Runtime == contract.Runtime && declaration.Evidence == contract.Evidence &&
				reflect.DeepEqual(declaration.WriteBoundary, contract.WriteBoundary) {
				current = append(current, receipt)
			}
		}
		switch {
		case len(current) == 0 && len(valid) == 0 && sawUntrustedReceipt:
			diagnostics = append(diagnostics, diagnostic(subject, "untrusted_receipt_author", "only untrusted authorization comments exist"))
		case len(current) == 0 && len(valid) == 0:
			diagnostics = append(diagnostics, diagnostic(subject, "missing_receipt", "no execution authorization receipt exists"))
		case len(current) == 0:
			diagnostics = append(diagnostics, diagnostic(subject, "stale_receipt", "authorization does not match the current Issue body or base head"))
		case len(current) > 1:
			diagnostics = append(diagnostics, diagnostic(subject, "duplicate_receipt", "more than one current authorization receipt exists"))
		default:
			latestIssue, latestErr := client.Issue(ctx, issue.Number)
			if latestErr != nil {
				diagnostics = append(diagnostics, diagnostic(subject, "provider_unreadable", latestErr.Error()))
				continue
			}
			latestBodySum := sha256.Sum256([]byte(latestIssue.Body))
			if latestIssue.State != "open" || len(latestIssue.PullRequest) != 0 ||
				hex.EncodeToString(latestBodySum[:]) != bodyDigest {
				diagnostics = append(diagnostics, diagnostic(subject, "provider_changed", "provider Issue changed before backlog emission"))
				continue
			}
			items = append(items, BacklogItem{
				ID: subject, Title: latestIssue.Title, Status: "open", Body: latestIssue.Body,
				Repository: targetRepository, IssueNumber: issue.Number,
				Authorization: current[0], ExecutionSkill: "execute",
			})
		}
	}
	latestHead, err := client.DefaultBranchHead(ctx, policy.DefaultBranch)
	if err != nil {
		return nil, diagnostics, err
	}
	if latestHead != baseHead {
		return nil, diagnostics, fmt.Errorf("provider default branch changed while synchronizing")
	}
	return items, diagnostics, nil
}

func diagnostic(subject, code, message string) Diagnostic {
	return Diagnostic{Subject: subject, Code: code, Message: message}
}
