package loop

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/poteto/noodle/event"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/internal/state"
)

func stoppedReviewFixture(t *testing.T) (string, string) {
	t.Helper()
	project, wt, _, _, _ := publicationFixture(t, event.StageOutcomeCompleted, false)
	dir := filepath.Join(project, ".noodle")
	s, err := reducer.ReadSnapshot(filepath.Join(dir, "state.snapshot.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.OrderRevision = strings.Repeat("a", 32)
	s.State.LastEventID = "1"
	if err := reducer.WriteSnapshotAtomic(filepath.Join(dir, "state.snapshot.json"), s); err != nil {
		t.Fatal(err)
	}
	l := &Loop{runtimeDir: dir, canonical: s.State, canonicalLoaded: true}
	if err := l.writeProjectionState(); err != nil {
		t.Fatal(err)
	}
	if err := l.writePendingReview(); err != nil {
		t.Fatal(err)
	}
	session := filepath.Join(dir, "sessions", "session-1")
	// A reaped, real child PID is absence evidence; never signal a guessed PID.
	cmd := publicationTestProcess(t)
	recoveryJSON(t, filepath.Join(session, "process.json"), map[string]any{"session_id": "session-1", "pid": cmd})
	recoveryJSON(t, filepath.Join(session, "meta.json"), map[string]any{"session_id": "session-1", "status": "exited", "runtime": "process"})
	recoveryJSON(t, filepath.Join(session, "spawn.json"), map[string]any{"session_id": "session-1", "worktree_path": wt, "retry_count": 0})
	recoveryWrite(t, filepath.Join(session, "prompt.txt"), []byte("normal task"))
	return project, wt
}

func TestStoppedReviewExactRejection(t *testing.T) {
	project, wt := stoppedReviewFixture(t)
	dir := filepath.Join(project, ".noodle")
	before, _ := os.ReadFile(filepath.Join(dir, "state.snapshot.json"))
	r := InspectStoppedReview(project, "/exact/noodle", "order-1", "example/project#7")
	if r.Status != "recoverable" || len(r.Next.Argv) != 8 {
		t.Fatalf("inspect: %+v", r)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "state.snapshot.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("inspection wrote canonical state")
	}
	r = RejectStoppedReview(project, "/exact/noodle", "order-1", "example/project#7", r.Digest)
	if r.Status != "rejected" {
		t.Fatalf("reject: %+v", r)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatal("target worktree remains")
	}
	if refs := publicationOutput(t, project, "git", "for-each-ref", "refs/heads/claim-1"); refs != "" {
		t.Fatal("target branch remains")
	}
	publicationRun(t, project, "git", "bundle", "verify", filepath.Join(r.EvidencePath, "candidate.bundle"))
	s, err := reducer.ReadSnapshot(filepath.Join(dir, "state.snapshot.json"))
	if err != nil || s.State.Orders["order-1"].Status != state.OrderCancelled || len(s.State.PendingReviews) != 0 {
		t.Fatalf("canonical: %+v %v", s, err)
	}
	if again := InspectStoppedReview(project, "/exact/noodle", "order-1", "example/project#7"); again.Status != "rejected" || len(again.Next.Argv) != 0 {
		t.Fatalf("readback: %+v", again)
	}
	if again := RejectStoppedReview(project, "/exact/noodle", "order-1", "example/project#7", r.Digest); again.Status != "rejected" {
		t.Fatalf("reentry: %+v", again)
	}
	if read := InspectAdmission(project, "/exact/noodle"); read.Status != "no_proposal" {
		t.Fatalf("admission remains blocked: %+v", read)
	}
}

func TestStoppedReviewRefusesChangedCustody(t *testing.T) {
	for _, name := range []string{"live_lock", "live_process", "dirty", "moved", "foreign", "stale_digest", "nonreview"} {
		t.Run(name, func(t *testing.T) {
			project, wt := stoppedReviewFixture(t)
			r := InspectStoppedReview(project, "/exact/noodle", "order-1", "example/project#7")
			if r.Status != "recoverable" {
				t.Fatalf("setup: %+v", r)
			}
			subject := "example/project#7"
			switch name {
			case "live_lock":
				lock, err := lockfile.TryLock(filepath.Join(project, ".noodle", "noodle.lock"))
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
			case "live_process":
				recoveryJSON(t, filepath.Join(project, ".noodle", "sessions", "session-1", "process.json"), map[string]any{"session_id": "session-1", "pid": os.Getpid()})
			case "dirty":
				recoveryWrite(t, filepath.Join(wt, "dirty"), []byte("keep me"))
			case "moved":
				publicationRun(t, wt, "git", "commit", "--allow-empty", "-m", "later candidate")
			case "foreign":
				subject = "other/project#7"
			case "stale_digest":
				r.Digest = strings.Repeat("0", 64)
			case "nonreview":
				path := filepath.Join(project, ".noodle", "state.snapshot.json")
				s, _ := reducer.ReadSnapshot(path)
				o := s.State.Orders["order-1"]
				o.Stages[0].Status = state.StageCompleted
				s.State.Orders["order-1"] = o
				if err := reducer.WriteSnapshotAtomic(path, s); err != nil {
					t.Fatal(err)
				}
			}
			path := filepath.Join(project, ".noodle", "state.snapshot.json")
			before, _ := os.ReadFile(path)
			got := RejectStoppedReview(project, "/exact/noodle", "order-1", subject, r.Digest)
			if got.Status != "refused" || got.Invalid == "" {
				t.Fatalf("refusal: %+v", got)
			}
			after, _ := os.ReadFile(path)
			if !bytes.Equal(before, after) {
				t.Fatal("refusal mutated owner")
			}
			if _, err := os.Stat(wt); err != nil {
				t.Fatal("refusal removed candidate")
			}
		})
	}
}

func TestStoppedReviewInterruptedReadback(t *testing.T) {
	for _, cut := range []string{"after_intent", "after_canonical", "after_cleanup"} {
		t.Run(cut, func(t *testing.T) {
			project, _ := stoppedReviewFixture(t)
			r := InspectStoppedReview(project, "/exact/noodle", "order-1", "example/project#7")
			func() {
				defer func() {
					if recover() != "interrupt" {
						t.Error("fault hook not reached")
					}
				}()
				recoverStoppedReview(project, "/exact/noodle", "order-1", "example/project#7", r.Digest, func(point string) {
					if point == cut {
						panic("interrupt")
					}
				})
			}()
			read := InspectStoppedReview(project, "/exact/noodle", "order-1", "example/project#7")
			if read.Status != "recoverable" && read.Status != "rejected" {
				t.Fatalf("readback: %+v", read)
			}
			got := RejectStoppedReview(project, "/exact/noodle", "order-1", "example/project#7", r.Digest)
			if got.Status != "rejected" {
				t.Fatalf("resume: %+v", got)
			}
			var receipt map[string]any
			raw, err := os.ReadFile(filepath.Join(got.EvidencePath, "intent.json"))
			if err != nil || json.Unmarshal(raw, &receipt) != nil {
				t.Fatal("missing original intent")
			}
		})
	}
}

// Keep fixture process observation explicit and bounded.
func publicationTestProcess(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("git", "--version")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	return pid
}
