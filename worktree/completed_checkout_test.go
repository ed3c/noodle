package worktree

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/poteto/noodle/internal/execution"
	"github.com/poteto/noodle/worktree/archive"
)

func completedCheckoutFixture(t *testing.T) (*App, archive.FileRef) {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("original lossless archive metadata requires macOS")
	}
	root, err := filepath.EvalSymlinks(setupTestRepo(t))
	if err != nil {
		t.Fatal(err)
	}
	app, err := NewAppWithExecution(execution.Capture(root))
	if err != nil {
		t.Fatal(err)
	}
	app.Quiet = true
	if err = app.Create("retained-candidate"); err != nil {
		t.Fatal(err)
	}
	path := WorktreePath(root, "retained-candidate")
	if err = os.WriteFile(filepath.Join(path, "result.txt"), []byte("accepted branch result\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runGitIn(t, path, "add", "result.txt")
	runGitIn(t, path, "commit", "-m", "preserve accepted result on task branch")
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	codec, err := exec.LookPath("zstd")
	if err != nil {
		t.Fatal(err)
	}
	cfg := archive.Config{Roots: []string{filepath.Dir(path)}, ArchiveRoot: filepath.Join(base, "archive"), BlobRoot: filepath.Join(base, "cas"), StateDir: filepath.Join(base, "owner"), Codec: codec, MaxItems: 1, ItemTimeoutSeconds: 30}
	for _, dir := range []string{cfg.ArchiveRoot, cfg.BlobRoot, cfg.StateDir} {
		if err = os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.WriteFile(filepath.Join(cfg.StateDir, "owner.key"), []byte(strings.Repeat("k", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	policy := filepath.Join(base, "policy.json")
	if err = os.WriteFile(policy, raw, 0600); err != nil {
		t.Fatal(err)
	}
	hash, err := archive.FileHash(policy)
	if err != nil {
		t.Fatal(err)
	}
	return app, archive.FileRef{Path: policy, SHA256: hash}
}

func completedCheckoutCandidate(t *testing.T, app *App) CheckoutCandidate {
	t.Helper()
	path := WorktreePath(app.Root, "retained-candidate")
	id, err := app.ReadCheckoutIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := app.gitOutput("-C", path, "rev-parse", "HEAD^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	return CheckoutCandidate{Head: id.Head, Branch: id.Ref, Tree: tree}
}

func TestCompletedCheckoutPreservesBranchAndReleasesVerificationCopy(t *testing.T) {
	app, policy := completedCheckoutFixture(t)
	path := WorktreePath(app.Root, "retained-candidate")
	if err := os.WriteFile(filepath.Join(path, ".gitignore"), []byte("artifact\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runGitIn(t, path, "add", ".gitignore")
	runGitIn(t, path, "commit", "-m", "declare ignored fixture evidence")
	if err := os.WriteFile(filepath.Join(path, "artifact"), []byte("irreplaceable ignored evidence"), 0600); err != nil {
		t.Fatal(err)
	}
	head, err := app.gitOutput("-C", path, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	expected := completedCheckoutCandidate(t, app)
	if err := app.CompletedCheckout(ctx, "retained-candidate", "completed-operation", expected, policy, false); err != nil {
		t.Fatal("read-only readiness", err)
	}
	if _, err := archive.ReadCheckoutPacket(policy, "completed-operation"); !os.IsNotExist(err) {
		t.Fatal("inspect created preservation effects", err)
	}
	if err := app.CompletedCheckout(ctx, "retained-candidate", "completed-operation", expected, policy, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("checkout remains", err)
	}
	if after, err := app.gitOutput("rev-parse", "refs/heads/retained-candidate"); err != nil || after != head {
		t.Fatal("cleanup deleted or changed branch", after, head)
	}
	packet, err := archive.ReadCheckoutPacket(policy, "completed-operation")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Dir(packet.Recovery)); !os.IsNotExist(err) {
		t.Fatal("rebuildable verification copy still consumes space", err)
	}
	if _, err = archive.InspectCheckoutRemoval(packet); err != nil {
		t.Fatal("retained evidence cannot read known removal", err)
	}
	restoreRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(restoreRoot, "fresh-restore")
	if err = archive.RestoreCheckoutRecovery(packet, destination); err != nil {
		t.Fatal("retained pool/CAS no longer restores", err)
	}
	data, err := os.ReadFile(filepath.Join(destination, "checkout", "result.txt"))
	if err != nil || string(data) != "accepted branch result\n" {
		t.Fatal("restored accepted result differs", err)
	}
	data, err = os.ReadFile(filepath.Join(destination, "checkout", "artifact"))
	if err != nil || string(data) != "irreplaceable ignored evidence" {
		t.Fatal("ignored evidence was not restored", err)
	}
	if err = app.CompletedCheckout(ctx, "retained-candidate", "completed-operation", expected, policy, true); err != nil {
		t.Fatal("same operation did not read its known result", err)
	}
}

func TestCompletedCheckoutHoldsUnresolvedOwnersAndNativeConsumers(t *testing.T) {
	for _, reason := range []string{"consumer", "foreign-test", "dirty", "pinned", "legacy"} {
		t.Run(reason, func(t *testing.T) {
			app, policy := completedCheckoutFixture(t)
			path := WorktreePath(app.Root, "retained-candidate")
			expected := completedCheckoutCandidate(t, app)
			id, err := app.ReadCheckoutIdentity(path)
			if err != nil {
				t.Fatal(err)
			}
			switch reason {
			case "consumer":
				guard, err := app.AcquireCheckoutUse(context.Background(), path)
				if err != nil {
					t.Fatal(err)
				}
				defer guard.Close()
			case "foreign-test":
				err = os.Mkdir(filepath.Join(id.Registration, "soodles-test-requests.noindex"), 0700)
			case "dirty":
				err = os.WriteFile(filepath.Join(path, "result.txt"), []byte("unreviewed bytes"), 0600)
			case "pinned":
				runGitIn(t, app.Root, "worktree", "lock", path)
			case "legacy":
				err = os.Remove(filepath.Join(id.Registration, "noodle-incarnation.json"))
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = app.CompletedCheckout(context.Background(), "retained-candidate", "held-operation", expected, policy, true); err == nil {
				t.Fatal("unsafe checkout was removed")
			}
			if _, err = os.Stat(filepath.Join(path, "result.txt")); err != nil {
				t.Fatal("held data was removed", err)
			}
		})
	}
}

func TestCompletedCheckoutRecoversObservedRemovalWithoutReplayingPendingEffect(t *testing.T) {
	app, policy := completedCheckoutFixture(t)
	expected := completedCheckoutCandidate(t, app)
	path := WorktreePath(app.Root, "retained-candidate")
	id, err := app.ReadCheckoutIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	packet, err := archive.PrepareCheckout(archive.CheckoutRequest{Operation: "interrupted-removal", Identity: id, Config: policy})
	if err != nil {
		t.Fatal(err)
	}
	if err = archive.BeginCheckoutRemoval(packet); err != nil {
		t.Fatal(err)
	}
	if err = app.CompletedCheckout(context.Background(), "retained-candidate", "interrupted-removal", expected, policy, true); err == nil {
		t.Fatal("pending effect was replayed")
	}
	if _, err = os.Stat(path); err != nil {
		t.Fatal("unknown effect removed source", err)
	}
	// The original native command succeeds, but its acknowledgement is lost.
	runGitIn(t, app.Root, "worktree", "remove", "--", path)
	if err = app.CompletedCheckout(context.Background(), "retained-candidate", "interrupted-removal", expected, policy, true); err != nil {
		t.Fatal("known native absence could not reconcile original effect", err)
	}
	if _, err = archive.InspectCheckoutRemoval(packet); err != nil {
		t.Fatal("reconciled effect lost readback", err)
	}
}
