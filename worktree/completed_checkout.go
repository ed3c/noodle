package worktree

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/worktree/archive"
)

type CheckoutCandidate struct {
	Head   string
	Tree   string
	Branch string
}

// AcquireCheckoutUse excludes cooperating native commands for one physical
// checkout. Original runtime quiescence remains the Loop owner's obligation.
func (a *App) AcquireCheckoutUse(ctx context.Context, path string) (io.Closer, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	physical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return nil, err
	}
	common, err := a.gitOutput("rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil || (a.CommonDir != "" && common != a.CommonDir) {
		return nil, fmt.Errorf("original checkout owner changed: %v", err)
	}
	dir := filepath.Join(common, "noodle-checkout-use")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	key := sha256.Sum256([]byte(physical))
	lock, err := lockfile.TryLock(filepath.Join(dir, hex.EncodeToString(key[:])+".lock"))
	if err != nil {
		return nil, fmt.Errorf("checkout has another native consumer: %w", err)
	}
	return lock, nil
}

// CompletedCheckout is called by the quiescent original Loop after it validates
// Main's fixed disposition. This entry never commits, merges or deletes a ref.
func (a *App) CompletedCheckout(ctx context.Context, name, operation string, expected CheckoutCandidate, policy archive.FileRef, apply bool) error {
	return a.completedCheckout(ctx, name, operation, expected, policy, apply, nil)
}

// ReclaimArchiveCheckout uses the same reversible checkout protocol with the
// original recovery disposition's stronger incarnation binding.
func (a *App) ReclaimArchiveCheckout(ctx context.Context, name, operation string, expected CheckoutCandidate, policy archive.FileRef, original archive.CheckoutIdentity) error {
	return a.completedCheckout(ctx, name, operation, expected, policy, true, &original)
}

func (a *App) completedCheckout(ctx context.Context, name, operation string, expected CheckoutCandidate, policy archive.FileRef, apply bool, original *archive.CheckoutIdentity) error {
	if expected.Head == "" || expected.Tree == "" || !strings.HasPrefix(expected.Branch, "refs/heads/") {
		return fmt.Errorf("original accepted checkout candidate is unavailable")
	}
	if _, err := PrepareStorageExcludes(ctx, a.execution()); err != nil {
		return err
	}
	if hash, err := archive.FileHash(policy.Path); err != nil || hash != policy.SHA256 {
		return fmt.Errorf("original checkout archive policy changed: %v", err)
	}
	logical := WorktreePath(a.Root, name)
	if _, err := os.Lstat(logical); os.IsNotExist(err) {
		packet, readErr := archive.ReadCheckoutPacket(policy, operation)
		if readErr != nil {
			return fmt.Errorf("checkout missing without original preservation evidence: %w", readErr)
		}
		if packet.Source.Head != expected.Head || packet.Source.Branch != expected.Branch ||
			(original != nil && packet.Request.Identity != *original) {
			return fmt.Errorf("removed checkout differs from original accepted candidate")
		}
		if packet.Request.Identity.Path == logical || packet.Source.Path == logical {
			return a.readRemovedCheckout(packet, apply)
		}
		// A storage alias remains part of the original paired custody source.
		parent, parentErr := filepath.EvalSymlinks(filepath.Dir(logical))
		if parentErr != nil || packet.Source.Path != filepath.Join(parent, filepath.Base(logical)) {
			return fmt.Errorf("removed checkout differs from original logical identity: %v", parentErr)
		}
		return a.readRemovedCheckout(packet, apply)
	} else if err != nil {
		return err
	}
	custody, err := ReadCustody(ctx, a.execution(), logical)
	if err != nil {
		return err
	}
	guard, err := a.AcquireCheckoutUse(ctx, custody.Physical)
	if err != nil {
		return err
	}
	defer guard.Close()
	if err = a.acquireMergeLock(); err != nil {
		return err
	}
	defer a.releaseMergeLock()
	identity, err := a.ReadCheckoutIdentity(custody.Physical)
	if err != nil {
		return err
	}
	if original != nil && identity != *original {
		return fmt.Errorf("original checkout incarnation changed")
	}
	foreign := filepath.Join(identity.Registration, "soodles-test-requests.noindex")
	if _, err := os.Lstat(foreign); !os.IsNotExist(err) {
		return fmt.Errorf("foreign Test namespace HOLD; original owner inventory required: %s (%v)", foreign, err)
	}
	tree, err := a.gitOutput("-C", custody.Physical, "rev-parse", "HEAD^{tree}")
	if err != nil || identity.Head != expected.Head || identity.Ref != expected.Branch || tree != expected.Tree {
		return fmt.Errorf("checkout differs from original accepted HEAD/tree/branch: %v", err)
	}
	packet, err := archive.ReadCheckoutPacket(policy, operation)
	if err == nil {
		if packet.Request.Identity != identity {
			return fmt.Errorf("original checkout incarnation changed")
		}
		if pending, err := archive.CheckoutRemovalPending(packet); err != nil || pending {
			return fmt.Errorf("original checkout removal is pending or UNKNOWN; do not replay: %v", err)
		}
		if _, err = archive.InspectCheckout(packet); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	} else {
		request := archive.CheckoutRequest{Operation: operation, Identity: identity, Config: policy}
		if err = archive.VerifyCheckoutUnreserved(request); err != nil {
			return err
		}
		cfg, loadErr := archive.LoadConfig(policy.Path)
		if loadErr != nil {
			return loadErr
		}
		source, inspectErr := archive.InspectSource(cfg, custody.Physical)
		if inspectErr != nil {
			return inspectErr
		}
		if !apply {
			return nil
		}
		tree, captureErr := archive.Capture(custody.Physical, source.Excluded)
		if captureErr != nil {
			return captureErr
		}
		for rel, entry := range tree.Entries {
			if source.Tracked[rel] {
				continue
			}
			if entry.Kind == "dir" {
				trackedParent := false
				for tracked := range source.Tracked {
					if strings.HasPrefix(tracked, rel+string(filepath.Separator)) {
						trackedParent = true
						break
					}
				}
				if trackedParent {
					continue
				}
			}
			request.Ignored = append(request.Ignored, archive.CheckoutDisposition{Path: rel, Owner: "worktree", Disposition: "PRESERVE", SHA256: entry.Hash})
		}
		packet, err = archive.PrepareCheckout(request)
		if err != nil {
			return err
		}
	}
	if !apply {
		return nil
	}
	current, err := a.ReadCheckoutIdentity(custody.Physical)
	if err != nil || current != identity {
		return fmt.Errorf("checkout identity changed after restoration: %v", err)
	}
	if err = archive.BeginCheckoutRemoval(packet); err != nil {
		return err
	}
	// Git's ordinary non-force entry keeps every branch. Never fall back to the
	// old Cleanup path, force removal, branch deletion or repository-wide prune.
	if err = a.gitRunWithDetails("worktree", "remove", "--", custody.Physical); err != nil {
		return fmt.Errorf("native checkout removal UNKNOWN; retain original attempt: %w", err)
	}
	return a.readRemovedCheckout(packet, true)
}

func (a *App) readRemovedCheckout(packet archive.CheckoutPacket, apply bool) error {
	if apply {
		if _, err := archive.RecordCheckoutRemoval(packet); err != nil {
			return err
		}
		return archive.ReleaseCheckoutVerification(packet)
	}
	_, err := archive.InspectCheckoutRemoval(packet)
	return err
}
