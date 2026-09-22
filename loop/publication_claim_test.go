package loop

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/poteto/noodle/event"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
	"github.com/poteto/noodle/internal/statever"
)

func TestInspectPublicationClaimBindsCompletedSupervisedWorktree(t *testing.T) {
	project, worktree, head, tree, base := publicationFixture(t, event.StageOutcomeCompleted, false)
	claim, err := InspectPublicationClaim(project, "order-1", "example/project#7")
	if err != nil {
		t.Fatalf("inspect publication claim: %v", err)
	}
	if claim.Repository != "example/project" || claim.Subject != "example/project#7" {
		t.Fatalf("provider identity = %q %q", claim.Repository, claim.Subject)
	}
	if claim.WorktreePath != worktree || claim.Branch != "claim-1" {
		t.Fatalf("worktree identity = %q %q", claim.WorktreePath, claim.Branch)
	}
	if claim.Head != head || claim.Tree != tree || claim.BaseHead != base || claim.BaseBranch != "main" {
		t.Fatalf("git identity = head %q tree %q base %q/%q", claim.Head, claim.Tree, claim.BaseBranch, claim.BaseHead)
	}
	if claim.AuthorizesProviderWrite || claim.AuthorizesLanding {
		t.Fatal("publication claim granted authority")
	}
	if len(claim.Evidence.CanonicalSnapshotSHA256) != 64 || len(claim.Evidence.SessionEventsSHA256) != 64 {
		t.Fatalf("evidence digests = %#v", claim.Evidence)
	}
}

func TestInspectPublicationClaimRefusesNonCompletedAndDirtyCandidates(t *testing.T) {
	for _, test := range []struct {
		name     string
		outcome  event.StageOutcome
		blocking bool
		dirty    bool
		want     string
	}{
		{name: "blocked", outcome: event.StageOutcomeBlocked, blocking: true, want: "requires a non-blocking completed"},
		{name: "dirty", outcome: event.StageOutcomeCompleted, dirty: true, want: "clean worktree"},
	} {
		t.Run(test.name, func(t *testing.T) {
			project, worktree, _, _, _ := publicationFixture(t, test.outcome, test.blocking)
			if test.dirty {
				if err := os.WriteFile(filepath.Join(worktree, "dirty.txt"), []byte("dirty\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			_, err := InspectPublicationClaim(project, "order-1", "example/project#7")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestInspectPublicationClaimRefusesForeignSubjectAndRootWorktree(t *testing.T) {
	project, _, _, _, _ := publicationFixture(t, event.StageOutcomeCompleted, false)
	if _, err := InspectPublicationClaim(project, "order-1", "other/project#7"); err == nil || !strings.Contains(err.Error(), "does not match remote") {
		t.Fatalf("foreign subject error = %v", err)
	}

	snapshotPath := filepath.Join(project, ".noodle", "state.snapshot.json")
	snapshot, err := reducer.ReadSnapshot(snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	review := snapshot.State.PendingReviews["order-1"]
	review.WorktreePath = project
	snapshot.State.PendingReviews["order-1"] = review
	if err := reducer.WriteSnapshotAtomic(snapshotPath, snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectPublicationClaim(project, "order-1", "example/project#7"); err == nil || !strings.Contains(err.Error(), "Noodle linked worktree") {
		t.Fatalf("root worktree error = %v", err)
	}
}

func publicationFixture(t *testing.T, outcome event.StageOutcome, blocking bool) (string, string, string, string, string) {
	t.Helper()
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	project := filepath.Join(root, "project")
	publicationRun(t, root, "git", "init", "--bare", "-b", "main", remote)
	publicationRun(t, root, "git", "init", "-b", "main", project)
	publicationRun(t, project, "git", "config", "user.name", "Publication Test")
	publicationRun(t, project, "git", "config", "user.email", "publication@example.invalid")
	if err := os.WriteFile(filepath.Join(project, ".gitignore"), []byte(".worktrees/\n.noodle/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	publicationRun(t, project, "git", "add", ".gitignore")
	publicationRun(t, project, "git", "commit", "-m", "base")
	publicationRun(t, project, "git", "remote", "add", "origin", "git@github.com:example/project.git")
	// Push through a temporary local URL, then restore the exact provider URL.
	publicationRun(t, project, "git", "remote", "set-url", "--push", "origin", remote)
	publicationRun(t, project, "git", "push", "-u", "origin", "main")
	publicationRun(t, project, "git", "remote", "set-url", "--push", "origin", "git@github.com:example/project.git")
	publicationRun(t, project, "git", "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")
	base := publicationOutput(t, project, "git", "rev-parse", "HEAD")

	worktree := filepath.Join(project, ".worktrees", "claim-1")
	publicationRun(t, project, "git", "worktree", "add", "-b", "claim-1", worktree, base)
	if err := os.WriteFile(filepath.Join(worktree, "candidate.txt"), []byte("candidate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	publicationRun(t, worktree, "git", "add", "candidate.txt")
	publicationRun(t, worktree, "git", "commit", "-m", "candidate")
	head := publicationOutput(t, worktree, "git", "rev-parse", "HEAD")
	tree := publicationOutput(t, worktree, "git", "rev-parse", "HEAD^{tree}")

	runtimeDir := filepath.Join(project, ".noodle")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	canonical := state.State{
		Orders: map[string]state.OrderNode{"order-1": {
			OrderID: "order-1", Status: state.OrderActive,
			Stages: []state.StageNode{{StageIndex: 0, TaskKey: "execute", Status: state.StageReview,
				Attempts: []state.AttemptNode{{AttemptID: "order-1:0:0", SessionID: "session-1", Status: state.AttemptCompleted, WorktreeName: "claim-1"}}}},
		}},
		PendingReviews: map[string]state.PendingReviewNode{"order-1": {
			OrderID: "order-1", StageIndex: 0, TaskKey: "execute", WorktreeName: "claim-1", WorktreePath: worktree, SessionID: "session-1",
		}},
		Mode: state.RunModeSupervised, SchemaVersion: statever.Current,
	}
	if err := reducer.WriteSnapshotAtomic(filepath.Join(runtimeDir, "state.snapshot.json"), reducer.BuildSnapshot(canonical, nil, now)); err != nil {
		t.Fatal(err)
	}
	writer, err := event.NewEventWriter(runtimeDir, "session-1")
	if err != nil {
		t.Fatal(err)
	}
	stageIndex := 0
	payload, _ := json.Marshal(event.StageMessagePayload{Message: "candidate ready", Blocking: &blocking, Outcome: outcome, OrderID: "order-1", StageIndex: &stageIndex})
	if err := writer.Append(context.Background(), event.Event{Type: event.EventStageMessage, Payload: payload, SessionID: "session-1"}); err != nil {
		t.Fatal(err)
	}
	return project, worktree, head, tree, base
}

func publicationRun(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	command := exec.Command(name, args...)
	command.Dir = dir
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, output)
	}
}

func publicationOutput(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	command := exec.Command(name, args...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v: %s", name, args, err, output)
	}
	return strings.TrimSpace(string(output))
}
