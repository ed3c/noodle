package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxGitHubResponseBytes = 8 << 20

type GitHubClient struct {
	baseURL string
	token   string
	client  *http.Client
}

func NewGitHubClient(baseURL, token string) *GitHubClient {
	return &GitHubClient{
		baseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		token:   strings.TrimSpace(token),
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *GitHubClient) request(ctx context.Context, method, path string, input, output any) error {
	var body io.Reader
	if input != nil {
		data, err := json.Marshal(input)
		if err != nil {
			return fmt.Errorf("encode GitHub request: %w", err)
		}
		body = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, body)
	if err != nil {
		return fmt.Errorf("build GitHub request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("GitHub %s %s: %w", method, path, err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxGitHubResponseBytes+1))
	if err != nil {
		return fmt.Errorf("read GitHub %s %s: %w", method, path, err)
	}
	if len(data) > maxGitHubResponseBytes {
		return fmt.Errorf("GitHub %s %s response exceeds %d bytes", method, path, maxGitHubResponseBytes)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("GitHub %s %s returned %s: %s", method, path, response.Status, strings.TrimSpace(string(data)))
	}
	if output != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, output); err != nil {
			return fmt.Errorf("decode GitHub %s %s: %w", method, path, err)
		}
	}
	return nil
}

func (c *GitHubClient) Repository(ctx context.Context) (Repository, error) {
	var repository Repository
	err := c.request(ctx, http.MethodGet, "/repos/"+targetRepository, nil, &repository)
	return repository, err
}

func (c *GitHubClient) DefaultBranchHead(ctx context.Context, branch string) (string, error) {
	var ref GitRef
	path := "/repos/" + targetRepository + "/git/ref/heads/" + url.PathEscape(branch)
	if err := c.request(ctx, http.MethodGet, path, nil, &ref); err != nil {
		return "", err
	}
	return ref.Object.SHA, nil
}

func (c *GitHubClient) Issue(ctx context.Context, number int) (Issue, error) {
	var issue Issue
	path := "/repos/" + targetRepository + "/issues/" + strconv.Itoa(number)
	err := c.request(ctx, http.MethodGet, path, nil, &issue)
	return issue, err
}

func (c *GitHubClient) OpenIssues(ctx context.Context) ([]Issue, error) {
	var all []Issue
	for page := 1; ; page++ {
		var issues []Issue
		path := fmt.Sprintf("/repos/%s/issues?state=open&per_page=100&page=%d", targetRepository, page)
		if err := c.request(ctx, http.MethodGet, path, nil, &issues); err != nil {
			return nil, err
		}
		all = append(all, issues...)
		if len(issues) < 100 {
			return all, nil
		}
	}
}

func (c *GitHubClient) Comments(ctx context.Context, number int) ([]Comment, error) {
	var all []Comment
	for page := 1; ; page++ {
		var comments []Comment
		path := fmt.Sprintf("/repos/%s/issues/%d/comments?per_page=100&page=%d", targetRepository, number, page)
		if err := c.request(ctx, http.MethodGet, path, nil, &comments); err != nil {
			return nil, err
		}
		all = append(all, comments...)
		if len(comments) < 100 {
			return all, nil
		}
	}
}

func (c *GitHubClient) CreateComment(ctx context.Context, number int, body string) (Comment, error) {
	var comment Comment
	path := fmt.Sprintf("/repos/%s/issues/%d/comments", targetRepository, number)
	err := c.request(ctx, http.MethodPost, path, map[string]string{"body": body}, &comment)
	return comment, err
}

func (c *GitHubClient) Branch(ctx context.Context, branch string) (GitRef, bool, error) {
	var ref GitRef
	path := "/repos/" + targetRepository + "/git/ref/heads/" + url.PathEscape(branch)
	err := c.request(ctx, http.MethodGet, path, nil, &ref)
	if err != nil && strings.Contains(err.Error(), "returned 404") {
		return GitRef{}, false, nil
	}
	return ref, err == nil, err
}

func (c *GitHubClient) OpenPullRequests(ctx context.Context) ([]PullRequest, error) {
	var pulls []PullRequest
	err := c.request(ctx, http.MethodGet, "/repos/"+targetRepository+"/pulls?state=open&per_page=100", nil, &pulls)
	return pulls, err
}

func (c *GitHubClient) CreatePullRequest(ctx context.Context, title, branch, body, base string) (PullRequest, error) {
	var pull PullRequest
	input := map[string]string{"title": title, "head": branch, "body": body, "base": base}
	err := c.request(ctx, http.MethodPost, "/repos/"+targetRepository+"/pulls", input, &pull)
	return pull, err
}

func (c *GitHubClient) PullRequest(ctx context.Context, number int) (PullRequest, error) {
	var pull PullRequest
	err := c.request(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d", targetRepository, number), nil, &pull)
	return pull, err
}

func (c *GitHubClient) UpdateIssueBody(ctx context.Context, number int, body string) (Issue, error) {
	var issue Issue
	err := c.request(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/issues/%d", targetRepository, number), map[string]string{"body": body}, &issue)
	return issue, err
}
