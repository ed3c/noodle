package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

const testHeadSHA = "3333333333333333333333333333333333333333"

func TestNoodlesGitHubLand(t *testing.T) {
	body := strings.Replace(issueBody(17), "noodles-state: ready", "noodles-state: awaiting_land", 1)
	readyBody := strings.Replace(body, "noodles-state: awaiting_land", "noodles-state: ready", 1)
	fake := newFakeGitHub(t, Issue{Number: 17, Title: "Land candidate", State: "open", Body: body})
	payload := testPayload(17, readyBody)
	receipt := Authorization{SchemaVersion: 1, Sender: testSender, Declaration: payload}
	receipt.DispatchIdentity, _ = DispatchIdentity(payload)
	fake.comments[17] = []Comment{trustedComment(1, FormatAuthorization(receipt))}
	pull := PullRequest{Number: 7, State: "open", Body: "Refs ed3c/noodle#17"}
	pull.Head.Ref, pull.Head.SHA, pull.Base.Ref = "noodles/issue-17-"+testHeadSHA[:12], testHeadSHA, "main"
	fake.pulls[7] = pull
	fake.pullFiles[7] = []PullRequestFile{{Filename: "adapters/noodles-github/land.go"}, {Filename: "policy/repo-capabilities.json"}}

	result, err := Land(context.Background(), fake.client(), testPolicy(), testCapabilities(), workflowEvent(t, testHeadSHA, []int{7}))
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "merged" || result.Head != testHeadSHA || result.Merge == "" || result.PR != 7 || result.Issue != 17 {
		t.Fatalf("result=%#v", result)
	}
	if fake.mergePosts != 1 || fake.mergeHead != testHeadSHA || fake.issueEdits != 1 || fake.closePosts != 1 || fake.closeBeforeMerge || fake.issues[17].State != "closed" || !strings.Contains(fake.issues[17].Body, "noodles-state: landed") {
		t.Fatalf("mutation residue: %#v", fake)
	}

	result, err = Land(context.Background(), fake.client(), testPolicy(), testCapabilities(), workflowEvent(t, testHeadSHA, []int{7}))
	if err != nil || result.Status != "reconciled" || fake.mergePosts != 1 || fake.issueEdits != 1 || fake.closePosts != 1 {
		t.Fatalf("retry result=%#v err=%v mutations=%d/%d/%d", result, err, fake.mergePosts, fake.issueEdits, fake.closePosts)
	}
}

func TestNoodlesGitHubLandPlantedNegativesStopBeforeMutation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*WorkflowRunEvent, *fakeGitHub)
	}{
		{"wrong event", func(e *WorkflowRunEvent, _ *fakeGitHub) { e.WorkflowRun.Event = "push" }},
		{"wrong conclusion", func(e *WorkflowRunEvent, _ *fakeGitHub) { e.WorkflowRun.Conclusion = "failure" }},
		{"wrong head", func(e *WorkflowRunEvent, _ *fakeGitHub) { e.WorkflowRun.HeadSHA = strings.Repeat("4", 40) }},
		{"missing PR", func(e *WorkflowRunEvent, _ *fakeGitHub) { e.WorkflowRun.PullRequests = nil }},
		{"multiple PRs", func(e *WorkflowRunEvent, _ *fakeGitHub) {
			e.WorkflowRun.PullRequests = append(e.WorkflowRun.PullRequests, PullRequestLink{Number: 8})
		}},
		{"stale authorization", func(_ *WorkflowRunEvent, f *fakeGitHub) { f.comments[17] = nil }},
		{"stale Issue body", func(_ *WorkflowRunEvent, f *fakeGitHub) {
			issue := f.issues[17]
			issue.Body += "\nprovider drift\n"
			f.issues[17] = issue
		}},
		{"boundary escape", func(_ *WorkflowRunEvent, f *fakeGitHub) { f.pullFiles[7] = []PullRequestFile{{Filename: "README.md"}} }},
		{"workflow mutation", func(_ *WorkflowRunEvent, f *fakeGitHub) {
			f.pullFiles[7] = []PullRequestFile{{Filename: ".github/workflows/test.yml"}}
		}},
		{"wrong merge head", func(_ *WorkflowRunEvent, f *fakeGitHub) { f.wrongMergeHead = true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := strings.Replace(issueBody(17), "noodles-state: ready", "noodles-state: awaiting_land", 1)
			readyBody := strings.Replace(body, "noodles-state: awaiting_land", "noodles-state: ready", 1)
			fake := newFakeGitHub(t, Issue{Number: 17, State: "open", Body: body})
			payload := testPayload(17, readyBody)
			receipt := Authorization{SchemaVersion: 1, Sender: testSender, Declaration: payload}
			receipt.DispatchIdentity, _ = DispatchIdentity(payload)
			fake.comments[17] = []Comment{trustedComment(1, FormatAuthorization(receipt))}
			pull := PullRequest{Number: 7, State: "open", Body: "Refs ed3c/noodle#17"}
			pull.Head.Ref, pull.Head.SHA, pull.Base.Ref = "noodles/issue-17-"+testHeadSHA[:12], testHeadSHA, "main"
			fake.pulls[7] = pull
			fake.pullFiles[7] = []PullRequestFile{{Filename: "adapters/noodles-github/land.go"}}
			var event WorkflowRunEvent
			_ = json.Unmarshal(workflowEvent(t, testHeadSHA, []int{7}), &event)
			tc.mutate(&event, fake)
			bytes, _ := json.Marshal(event)
			if _, err := Land(context.Background(), fake.client(), testPolicy(), testCapabilities(), bytes); err == nil {
				t.Fatal("expected refusal")
			}
			wantMergePosts := 0
			if tc.name == "wrong merge head" {
				wantMergePosts = 1
			}
			if fake.mergePosts != wantMergePosts || fake.issueEdits != 0 || fake.closePosts != 0 || fake.closeBeforeMerge {
				t.Fatalf("later mutation occurred: merge=%d edit=%d close=%d", fake.mergePosts, fake.issueEdits, fake.closePosts)
			}
		})
	}
}

func workflowEvent(t *testing.T, head string, prs []int) []byte {
	t.Helper()
	links := make([]PullRequestLink, len(prs))
	for i, number := range prs {
		links[i].Number = number
	}
	event := WorkflowRunEvent{Action: "completed", Repository: Repository{FullName: targetRepository, DefaultBranch: defaultBranch}, WorkflowRun: WorkflowRun{Name: "Test", Event: "pull_request", Status: "completed", Conclusion: "success", HeadSHA: head, PullRequests: links}}
	data, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
