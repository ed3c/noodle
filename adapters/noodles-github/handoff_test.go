package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNoodlesGitHubHandoff(t *testing.T) {
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	repo := filepath.Join(root, "candidate")
	runGit(t, root, "init", "--bare", "--initial-branch=main", remote)
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "init", "--initial-branch=main")
	runGit(t, repo, "config", "user.name", "Test")
	runGit(t, repo, "config", "user.email", "test@example.com")
	if err := os.MkdirAll(filepath.Join(repo, "adapters", "noodles-github"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(repo, "adapters", "noodles-github", "fixture.txt")
	if err := os.WriteFile(path, []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-m", "base")
	base := gitOutput(t, repo, "rev-parse", "HEAD")
	runGit(t, repo, "remote", "add", "provider", remote)
	runGit(t, repo, "push", "provider", "main")
	runGit(t, repo, "switch", "-c", "candidate")
	if err := os.WriteFile(path, []byte("candidate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-m", "fix: candidate")
	head := gitOutput(t, repo, "rev-parse", "HEAD")

	body := issueBody(17)
	fake := newFakeGitHub(t, Issue{Number: 17, Title: "Hand off candidate", State: "open", Body: body})
	fake.baseSHA = base
	payload := testPayload(17, body)
	payload.BaseSHA = base
	receipt := Authorization{SchemaVersion: 1, Sender: testSender, Declaration: payload}
	receipt.DispatchIdentity, _ = DispatchIdentity(payload)
	fake.comments[17] = []Comment{trustedComment(1, FormatAuthorization(receipt))}
	branch := "noodles/issue-17-" + head[:12]
	fake.branches[branch] = head

	result, err := Handoff(context.Background(), fake.client(), testPolicy(), testCapabilities(), "ed3c/noodle#17", "provider", repo)
	if err != nil {
		t.Fatal(err)
	}
	if result.Status != "created" || result.Branch != branch || result.Head != head || result.PR != 1 {
		t.Fatalf("result=%#v", result)
	}
	if fake.pullPosts != 1 || fake.issueEdits != 1 || !strings.Contains(fake.issues[17].Body, "noodles-state: awaiting_land") {
		t.Fatalf("provider residue: pulls=%d edits=%d issue=%q", fake.pullPosts, fake.issueEdits, fake.issues[17].Body)
	}
	items, _, err := Sync(context.Background(), fake.client(), testPolicy(), testCapabilities())
	if err != nil || len(items) != 0 {
		t.Fatalf("sync items=%#v err=%v", items, err)
	}
	result, err = Handoff(context.Background(), fake.client(), testPolicy(), testCapabilities(), "ed3c/noodle#17", "provider", repo)
	if err != nil || result.Status != "reused" || fake.pullPosts != 1 || fake.issueEdits != 1 {
		t.Fatalf("retry result=%#v err=%v pulls=%d edits=%d", result, err, fake.pullPosts, fake.issueEdits)
	}
}

func TestNoodlesGitHubHandoffRejectsDirtyCandidateBeforeMutation(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init", "--initial-branch=candidate")
	runGit(t, repo, "config", "user.name", "Test")
	runGit(t, repo, "config", "user.email", "test@example.com")
	if err := os.WriteFile(filepath.Join(repo, "tracked"), []byte("base"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(t, repo, "add", ".")
	runGit(t, repo, "commit", "-m", "base")
	runGit(t, repo, "remote", "add", "provider", repo)
	if err := os.WriteFile(filepath.Join(repo, "dirty"), []byte("dirty"), 0o644); err != nil {
		t.Fatal(err)
	}
	fake := newFakeGitHub(t)
	_, err := Handoff(context.Background(), fake.client(), testPolicy(), testCapabilities(), "ed3c/noodle#17", "provider", repo)
	if err == nil || fake.pullPosts != 0 || fake.issueEdits != 0 {
		t.Fatalf("err=%v pulls=%d edits=%d", err, fake.pullPosts, fake.issueEdits)
	}
}
