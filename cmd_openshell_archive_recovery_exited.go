package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/execution"
	"github.com/poteto/noodle/loop"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/poteto/noodle/worktree"
	"github.com/poteto/noodle/worktree/archive"
)

type exitedArchiveReclamation struct {
	app       *worktree.App
	budget    *admission.Budget
	profile   loopruntime.OpenShellProfile
	policy    archive.FileRef
	name      string
	operation string
	checkout  *archive.CheckoutReclamationEvidence
}

func prepareExitedArchiveReclamation(ctx context.Context, directory string, fence *archiveRecoveryHostExitFence) (*exitedArchiveReclamation, error) {
	d := fence.Scope
	project := fence.Request.ProjectDir
	var control worktree.TaskControl
	if _, err := loopruntime.ReadSessionReadbackFile(filepath.Join(filepath.Dir(project), "control.json"), &control); err != nil {
		return nil, err
	}
	observed, err := admission.ReadMachinePreparations(directory, control.Preparation)
	if err != nil {
		return nil, err
	}
	original := observed[control.Preparation]
	source := original.Source.Bindings
	if control.Schema != 2 || control.Status != "prepared" || control.ControlRoot != project || control.Preparation.ID != d.Lease.ID ||
		original.Reference != control.Preparation || !original.Promoted || !original.SessionBound || !original.EffectsPossible || original.Closed ||
		original.Owner != d.Lease.Owner || original.ContextID != d.ContextID || original.OrderID != d.OrderID || original.Runtime != "openshell" ||
		original.Source.Purpose != "task-preparation" || !filepath.IsAbs(control.Repository) ||
		source["control_root"] != project || source["host_root"] != fence.Owner.HostRoot || source["repository"] != control.Repository ||
		source["task_path"] != control.Task.Path || source["task_sha256"] != control.Task.SHA256 ||
		source["execution_binary"] != fence.Owner.ExecutionBinary.Path+"/"+fence.Owner.ExecutionBinary.SHA256 {
		return nil, fmt.Errorf("exited archive recovery original promoted preparation differs")
	}
	profile, err := loopruntime.LoadOpenShellProfile(fence.Profile.Path)
	if err != nil {
		return nil, err
	}
	if profile.SourceSHA256 != source["profile"] || loopruntime.VerifySessionReadbackFiles(map[string]loopruntime.SessionReadbackFileRef{"profile": fence.Profile}) != nil {
		return nil, fmt.Errorf("exited archive recovery original preparation profile changed")
	}
	var roots []string
	if err = json.Unmarshal([]byte(source["planned_roots"]), &roots); err != nil || len(roots) == 0 {
		return nil, fmt.Errorf("exited archive recovery original resource paths are unavailable")
	}
	policy, err := exitedArchivePolicy(source, roots)
	if err != nil {
		return nil, err
	}
	quota := filepath.Join(fence.Owner.HostRoot, "quota")
	if canonical, err := filepath.EvalSymlinks(quota); err != nil || canonical != quota {
		return nil, fmt.Errorf("exited archive recovery original quota root is unavailable or aliased")
	}
	for _, path := range []string{filepath.Join(directory, "capacity.lock"), filepath.Join(quota, "quota.lock"), filepath.Join(quota, "quota.json")} {
		if info, statErr := os.Lstat(path); statErr != nil || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("exited archive recovery original accounting file unavailable: %s", path)
		}
	}
	budget := admission.NewBudget(admission.DefaultWorkerLimit)
	if err = budget.BindMachineCapacity(directory, d.Lease.Owner); err != nil {
		return nil, err
	}
	if err = budget.BindSharedQuota(quota, fence.Generation.ID); err != nil {
		return nil, err
	}
	budget.BindResourceSource(d.ContextID, "openshell", func(string) (admission.ResourceClaim, error) {
		return loopruntime.OpenShellPreparedWorkerResourceClaim(profile, roots...)
	})
	if err = budget.ReconcileExitedArchiveWorkerQuota(d.Lease, admission.MachineSettlementFile(fence.Disposition), fence.Generation.ID, false); err != nil {
		return nil, err
	}
	e, err := execution.New(project, hostBaselineEnvironment(fence.Owner.HostRoot))
	if err != nil {
		return nil, err
	}
	app, err := worktree.NewAppWithExecution(e)
	if err != nil {
		return nil, err
	}
	name := strings.TrimPrefix(d.Checkout.Ref, "refs/heads/")
	if app.Root != project || app.CommonDir != d.Checkout.Repo || d.Checkout.Ref != "refs/heads/"+name || name == "." || name == ".." || filepath.Base(name) != name {
		return nil, fmt.Errorf("exited archive recovery checkout root or branch differs")
	}
	operation := "archive-recovery-checkout/" + fence.Disposition.SHA256
	checkout, err := observeExitedArchiveCheckout(ctx, app, e, d.Checkout, d.Tree, name, operation, policy)
	if err != nil {
		return nil, err
	}
	if err = loopruntime.ObserveOpenShellArchiveRecoveryAfterExit(ctx, fence.RuntimeDir, d.ContextID, profile, fence.Disposition, fence.Custody, checkout); err != nil {
		return nil, err
	}
	return &exitedArchiveReclamation{app: app, budget: budget, profile: profile, policy: policy, name: name, operation: operation, checkout: checkout}, nil
}

func exitedArchivePolicy(binding map[string]string, roots []string) (archive.FileRef, error) {
	original := binding["archive"]
	digest, nativeDefault := strings.CutPrefix(original, "native-default/")
	var policy loopruntime.SessionReadbackFileRef
	var err error
	if nativeDefault {
		// The original task preparation stores the storage root and ordered
		// archive roots. Its frozen plan digest includes the existing policy path.
		if len(roots) != 9 || !filepath.IsAbs(roots[3]) || !filepath.IsAbs(roots[8]) {
			return archive.FileRef{}, fmt.Errorf("exited archive recovery native-default archive roots are unavailable")
		}
		base := filepath.Join(roots[3], ".worktrees")
		path := filepath.Join(roots[8], hostDigest([]byte(base))+".json")
		policy, err = loopruntime.ReadSessionReadbackFile(path, nil)
	} else {
		policy, err = archivePreparationFile(original)
	}
	if err != nil {
		return archive.FileRef{}, fmt.Errorf("exited archive recovery requires the original bound archive policy: %w", err)
	}
	codec, err := archivePreparationFile(binding["codec"])
	if err != nil {
		return archive.FileRef{}, err
	}
	selected, err := archive.LoadConfig(policy.Path)
	if err != nil {
		return archive.FileRef{}, err
	}
	if nativeDefault {
		frozen := hostArchivePlan{Config: selected, Path: policy.Path}
		raw, err := json.Marshal(frozen)
		if err != nil || hostDigest(raw) != digest {
			return archive.FileRef{}, fmt.Errorf("exited archive recovery native-default archive plan differs from original preparation")
		}
	}
	actual, err := filepath.EvalSymlinks(selected.Codec)
	if err != nil || actual != codec.Path {
		return archive.FileRef{}, fmt.Errorf("exited archive recovery original archive codec differs")
	}
	return archive.FileRef(policy), nil
}

func archivePreparationFile(value string) (loopruntime.SessionReadbackFileRef, error) {
	i := strings.LastIndex(value, "/")
	if i < 1 || !filepath.IsAbs(value[:i]) || len(value[i+1:]) != 64 {
		return loopruntime.SessionReadbackFileRef{}, fmt.Errorf("original preparation lacks a bound native FileRef")
	}
	ref := loopruntime.SessionReadbackFileRef{Path: value[:i], SHA256: value[i+1:]}
	return ref, loopruntime.VerifySessionReadbackFiles(map[string]loopruntime.SessionReadbackFileRef{"original_preparation": ref})
}

func observeExitedArchiveCheckout(ctx context.Context, app *worktree.App, e *execution.Context, original archive.CheckoutIdentity, tree, name, operation string, policy archive.FileRef) (*archive.CheckoutReclamationEvidence, error) {
	logical := worktree.WorktreePath(app.Root, name)
	parent, err := filepath.EvalSymlinks(filepath.Dir(logical))
	if err != nil || filepath.Join(parent, filepath.Base(logical)) != original.Path {
		return nil, fmt.Errorf("exited archive recovery checkout is outside the original Creator root")
	}
	if _, err = os.Lstat(logical); os.IsNotExist(err) {
		packet, err := archive.ReadCheckoutPacket(policy, operation)
		if err != nil || packet.Request.Identity != original {
			return nil, fmt.Errorf("absent checkout lacks its original preservation: %v", err)
		}
		checkout, err := archive.ReadCheckoutReclamation(packet)
		if errors.Is(err, os.ErrNotExist) {
			// An existing witnessed absence only needs receipt readback.
			return nil, archive.ObserveCheckoutRemovalBeforeReceipt(packet)
		}
		return &checkout, err
	} else if err != nil {
		return nil, err
	}
	id, err := app.ReadCheckoutIdentity(original.Path)
	if err != nil || id != original {
		return nil, fmt.Errorf("exited archive recovery checkout incarnation differs: %v", err)
	}
	observedTree, err := e.Command(ctx, original.Path, "git", "rev-parse", "HEAD^{tree}").Output()
	if err != nil || strings.TrimSpace(string(observedTree)) != tree {
		return nil, fmt.Errorf("exited archive recovery checkout tree differs: %v", err)
	}
	config, err := archive.LoadConfig(policy.Path)
	if err != nil {
		return nil, err
	}
	if _, err = archive.InspectSource(config, original.Path); err != nil {
		return nil, err
	}
	if _, err = os.Lstat(filepath.Join(original.Registration, "soodles-test-requests.noindex")); !os.IsNotExist(err) {
		return nil, fmt.Errorf("exited archive recovery checkout has a foreign Test namespace")
	}
	packet, err := archive.ReadCheckoutPacket(policy, operation)
	if os.IsNotExist(err) {
		return nil, archive.VerifyCheckoutUnreserved(archive.CheckoutRequest{Operation: operation, Identity: original, Config: policy})
	}
	if err != nil || packet.Request.Identity != original {
		return nil, fmt.Errorf("exited archive recovery checkout packet differs: %v", err)
	}
	pending, err := archive.CheckoutRemovalPending(packet)
	if err != nil || pending {
		return nil, fmt.Errorf("exited archive recovery checkout removal remains UNKNOWN: %v", err)
	}
	_, err = archive.InspectCheckout(packet)
	return nil, err
}

func callExitedArchiveRecoveryReclaim(ctx context.Context, ref loopruntime.SessionReadbackFileRef) (hostArchiveRecoveryReclamation, error) {
	d, request, err := readOriginalArchiveRecoveryDisposition(ref)
	if err != nil {
		return hostArchiveRecoveryReclamation{}, err
	}
	result := archiveRecoveryReclaimWaiting(hostRequest{InstanceID: d.HostInstanceID, ContextID: d.ContextID, RequestID: request.RequestID, ArchiveRecoveryDisposition: &ref}, "")
	directory, err := admission.DefaultMachineCapacityDirectory()
	if err != nil {
		return result, err
	}
	finish, err := readOriginalArchiveRecoveryReclamation(ref, d)
	if err != nil && !os.IsNotExist(err) {
		return result, err
	}
	if finish.Path != "" {
		var saved struct {
			Checkout archive.CheckoutReclamationEvidence `json:"checkout"`
		}
		if err = readArchiveCleanupRef(finish, &saved); err != nil {
			return result, err
		}
		result.Status, result.Reclamation, result.Checkout = "archive_recovery_reclaimed", &finish, &saved.Checkout
		return result, nil
	}
	err = withOriginalArchiveHostExitFence(ctx, ref, func(fence *archiveRecoveryHostExitFence) error {
		exited, err := prepareExitedArchiveReclamation(ctx, directory, fence)
		if err != nil {
			return err
		}
		if exited.checkout == nil {
			expected := worktree.CheckoutCandidate{Head: d.Head, Tree: d.Tree, Branch: d.Checkout.Ref}
			if err = exited.app.ReclaimArchiveCheckout(ctx, exited.name, exited.operation, expected, exited.policy, d.Checkout); err != nil {
				return err
			}
			packet, err := archive.ReadCheckoutPacket(exited.policy, exited.operation)
			if err != nil {
				return err
			}
			checkout, err := archive.ReadCheckoutReclamation(packet)
			if err != nil {
				return err
			}
			exited.checkout = &checkout
		}
		result.Checkout = exited.checkout
		vm, err := loopruntime.RetireOpenShellArchiveRecoveryAfterExit(ctx, fence.RuntimeDir, d.ContextID, exited.profile, ref, fence.Custody, *exited.checkout, fence.CustodyLock)
		if vm.Retirement.SessionID != "" {
			result.VM = &vm
		}
		if err != nil {
			return err
		}
		if err = exited.budget.ReconcileExitedArchiveWorkerQuota(d.Lease, admission.MachineSettlementFile(ref), fence.Generation.ID, true); err != nil {
			return err
		}
		result.ArchiveRecoveryReclamationResult, err = loop.SettleExitedArchiveRecovery(exited.budget, loop.ArchiveRecoveryReclamation{Disposition: ref, Lease: d.Lease,
			Checkout: d.Checkout, Hold: d.Hold, Head: d.Head, Tree: d.Tree}, *exited.checkout, vm)
		return err
	})
	if err != nil {
		result.Error = err.Error()
	}
	return result, nil
}
