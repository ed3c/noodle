package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
)

type dispatchEvent struct {
	Action        string
	Repository    string
	Sender        string
	ClientPayload DispatchPayload
}

func parseDispatchEvent(data []byte) (dispatchEvent, error) {
	var envelope struct {
		Action        string          `json:"action"`
		Repository    json.RawMessage `json:"repository"`
		Sender        json.RawMessage `json:"sender"`
		ClientPayload json.RawMessage `json:"client_payload"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return dispatchEvent{}, fmt.Errorf("decode repository_dispatch event: %w", err)
	}
	var repository struct {
		FullName string `json:"full_name"`
	}
	var sender struct {
		Login string `json:"login"`
	}
	if err := json.Unmarshal(envelope.Repository, &repository); err != nil {
		return dispatchEvent{}, fmt.Errorf("decode event repository: %w", err)
	}
	if err := json.Unmarshal(envelope.Sender, &sender); err != nil {
		return dispatchEvent{}, fmt.Errorf("decode event sender: %w", err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(envelope.ClientPayload, &fields); err != nil {
		return dispatchEvent{}, fmt.Errorf("decode client_payload shape: %w", err)
	}
	want := []string{"base_sha", "evidence", "runtime", "source_repository", "subject", "subject_body_sha256", "target", "write_boundary"}
	if len(fields) != len(want) {
		return dispatchEvent{}, fmt.Errorf("client_payload must declare exactly %s", strings.Join(want, ", "))
	}
	for _, key := range want {
		if _, ok := fields[key]; !ok {
			return dispatchEvent{}, fmt.Errorf("client_payload must declare exactly %s", strings.Join(want, ", "))
		}
	}
	var payload DispatchPayload
	decoder := json.NewDecoder(bytes.NewReader(envelope.ClientPayload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil {
		return dispatchEvent{}, fmt.Errorf("decode client_payload: %w", err)
	}
	return dispatchEvent{Action: envelope.Action, Repository: repository.FullName, Sender: sender.Login, ClientPayload: payload}, nil
}

func Receive(ctx context.Context, client *GitHubClient, policy Policy, capabilities Capabilities, eventBytes []byte, workflowSHA string) (AdmissionResult, error) {
	if err := validatePolicy(policy); err != nil {
		return AdmissionResult{}, err
	}
	if err := validateCapabilities(capabilities); err != nil {
		return AdmissionResult{}, err
	}
	event, err := parseDispatchEvent(eventBytes)
	if err != nil {
		return AdmissionResult{}, err
	}
	payload := event.ClientPayload
	if event.Action != dispatchEventType || event.Repository != targetRepository {
		return AdmissionResult{}, fmt.Errorf("event action/target is %q/%q, want %q/%q", event.Action, event.Repository, dispatchEventType, targetRepository)
	}
	if event.Sender != policy.RepositoryDispatchSender || payload.SourceRepository != sourceRepository {
		return AdmissionResult{}, fmt.Errorf("event sender/source is not target-authorized")
	}
	if payload.Target != targetRepository {
		return AdmissionResult{}, fmt.Errorf("payload target %q is not %q", payload.Target, targetRepository)
	}
	number, err := parseSubject(payload.Subject)
	if err != nil {
		return AdmissionResult{}, err
	}
	if !sha40Pattern.MatchString(payload.BaseSHA) || !sha64Pattern.MatchString(payload.SubjectBodySHA256) {
		return AdmissionResult{}, fmt.Errorf("payload digest or base head has invalid shape")
	}
	repository, err := client.Repository(ctx)
	if err != nil {
		return AdmissionResult{}, err
	}
	if repository.FullName != targetRepository || repository.DefaultBranch != policy.DefaultBranch {
		return AdmissionResult{}, fmt.Errorf("provider repository/default branch drifted")
	}
	baseHead, err := client.DefaultBranchHead(ctx, policy.DefaultBranch)
	if err != nil {
		return AdmissionResult{}, err
	}
	if payload.BaseSHA != baseHead || workflowSHA != baseHead {
		return AdmissionResult{}, fmt.Errorf("payload/workflow base head %q/%q does not match current provider head %q", payload.BaseSHA, workflowSHA, baseHead)
	}
	issue, err := client.Issue(ctx, number)
	if err != nil {
		return AdmissionResult{}, err
	}
	if issue.Number != number || issue.State != "open" || len(issue.PullRequest) != 0 {
		return AdmissionResult{}, fmt.Errorf("subject is not the current open provider Issue")
	}
	contract, err := parseIssueContract(issue.Body, number)
	if err != nil {
		return AdmissionResult{}, err
	}
	sum := sha256.Sum256([]byte(issue.Body))
	if payload.SubjectBodySHA256 != hex.EncodeToString(sum[:]) {
		return AdmissionResult{}, fmt.Errorf("payload body digest is stale")
	}
	if payload.Runtime != contract.Runtime || payload.Evidence != contract.Evidence || !reflect.DeepEqual(payload.WriteBoundary, contract.WriteBoundary) {
		return AdmissionResult{}, fmt.Errorf("payload declaration drifted from current Issue markers")
	}
	identity, err := DispatchIdentity(payload)
	if err != nil {
		return AdmissionResult{}, err
	}
	receipt := Authorization{SchemaVersion: 1, DispatchIdentity: identity, Sender: event.Sender, Declaration: payload}
	comments, err := client.Comments(ctx, number)
	if err != nil {
		return AdmissionResult{}, err
	}
	matches, malformed := authorizationMatches(comments, receipt, policy.AuthorizationCommentAuthor)
	if malformed != nil {
		return AdmissionResult{}, malformed
	}
	if len(matches) > 1 {
		return AdmissionResult{}, fmt.Errorf("duplicate authorization receipts for dispatch %s", identity)
	}
	if len(matches) == 1 {
		return AdmissionResult{Status: "reused", DispatchIdentity: identity, CommentID: matches[0]}, nil
	}
	latestHead, err := client.DefaultBranchHead(ctx, policy.DefaultBranch)
	if err != nil {
		return AdmissionResult{}, err
	}
	latestIssue, err := client.Issue(ctx, number)
	if err != nil {
		return AdmissionResult{}, err
	}
	latestBodySum := sha256.Sum256([]byte(latestIssue.Body))
	if latestHead != baseHead || latestIssue.State != "open" || len(latestIssue.PullRequest) != 0 ||
		hex.EncodeToString(latestBodySum[:]) != payload.SubjectBodySHA256 {
		return AdmissionResult{}, fmt.Errorf("provider Issue or base head changed before authorization write")
	}
	created, err := client.CreateComment(ctx, number, FormatAuthorization(receipt))
	if err != nil {
		return AdmissionResult{}, err
	}
	if created.User.Login != policy.AuthorizationCommentAuthor {
		return AdmissionResult{}, fmt.Errorf("authorization provider returned comment author %q, want %q", created.User.Login, policy.AuthorizationCommentAuthor)
	}
	comments, err = client.Comments(ctx, number)
	if err != nil {
		return AdmissionResult{}, fmt.Errorf("authorization written but provider readback failed: %w", err)
	}
	matches, malformed = authorizationMatches(comments, receipt, policy.AuthorizationCommentAuthor)
	if malformed != nil || len(matches) != 1 || matches[0] != created.ID {
		return AdmissionResult{}, fmt.Errorf("authorization provider readback did not return one exact stored receipt")
	}
	return AdmissionResult{Status: "authorized", DispatchIdentity: identity, CommentID: created.ID}, nil
}

func authorizationMatches(comments []Comment, want Authorization, trustedAuthor string) ([]int64, error) {
	var matches []int64
	for _, comment := range comments {
		if !strings.HasPrefix(comment.Body, authorizationMarker) {
			continue
		}
		if comment.User.Login != trustedAuthor {
			continue
		}
		receipt, err := ParseAuthorization(comment.Body)
		if err != nil {
			return nil, fmt.Errorf("malformed authorization comment %d: %w", comment.ID, err)
		}
		if reflect.DeepEqual(receipt, want) {
			matches = append(matches, comment.ID)
		}
	}
	return matches, nil
}
