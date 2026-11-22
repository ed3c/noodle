package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/procx"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/poteto/noodle/worktree/archive"
	"github.com/spf13/cobra"
)

const archiveRecoveryActivationOperation = "activate-workspace-capture-cleanup"
const archiveRecoveryActivationFile = "archive-recovery-activation.json"
const archiveRecoveryActivationPhase = "archive-recovery-activation"

type archiveRecoveryActivationPlan struct {
	Schema      int                                `json:"schema"`
	Disposition loopruntime.SessionReadbackFileRef `json:"disposition"`
	Request     loopruntime.SessionReadbackFileRef `json:"request"`
	Successor   loopruntime.SessionReadbackFileRef `json:"successor"`
	Scope       hostOperatorNativeScope            `json:"native_scope"`
}

type archiveRecoveryActivationResult struct {
	Status         string                              `json:"status"`
	Disposition    loopruntime.SessionReadbackFileRef  `json:"disposition"`
	Request        loopruntime.SessionReadbackFileRef  `json:"request"`
	AttemptID      string                              `json:"attempt_id"`
	Successor      loopruntime.SessionReadbackFileRef  `json:"successor"`
	HostInstanceID string                              `json:"host_instance_id"`
	OriginalReturn string                              `json:"original_return"`
	Effects        []string                            `json:"effects"`
	Next           *archiveRecoveryNext                `json:"next"`
	ControllerPID  int                                 `json:"controller_pid"`
	GatewayPID     int                                 `json:"gateway_pid"`
	DriverPID      int                                 `json:"driver_pid"`
	Activation     *loopruntime.SessionReadbackFileRef `json:"activation,omitempty"`
}

// Project the bound activation separately from the reclaim continuation. A
// saved control intent alone cannot prove that the original generation exited.
func observeArchiveRecoveryActivation(ref loopruntime.SessionReadbackFileRef, d archiveRecoveryCleanupDisposition, request hostRequestBinding) *loopruntime.SessionReadbackFileRef {
	var plan archiveRecoveryActivationPlan
	session := filepath.Dir(ref.Path)
	bound, err := loopruntime.ReadSessionReadbackFile(filepath.Join(session, archiveRecoveryActivationFile), &plan)
	if err != nil || plan.Schema != 1 || plan.Disposition != ref || plan.Request != d.Request {
		return nil
	}
	retire, err := loopruntime.ReadSessionReadbackFile((archiveRecoveryActivation{session: session}).controlPath("retire"), nil)
	if err != nil || retire.SHA256 != bound.SHA256 {
		return nil
	}
	owner, _, generation, err := readArchiveRecoveryExitOwner(d, request)
	if err != nil || !filepath.IsAbs(plan.Successor.Path) || len(plan.Successor.SHA256) != 64 || plan.Successor == owner.ExecutionBinary ||
		archiveRecoveryActivationScope(plan.Scope, generation) != nil || verifyOriginalArchiveHostKernelExit(generation.Process) != nil {
		return nil
	}
	return &bound
}

func newArchiveRecoveryActivationCmd() *cobra.Command {
	var path, sha string
	var planOnly, retire, resume bool
	cmd := &cobra.Command{Use: archiveRecoveryActivationOperation, Short: "Qualify and fence the original archive recovery controller from its successor", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if planOnly && (retire || resume) || retire && resume {
				return errors.New("archive activation observation, retirement and rollback are separate operations")
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), archiveRecoveryReclaimTimeout)
			defer cancel()
			result, err := activateOriginalArchiveRecovery(ctx, loopruntime.SessionReadbackFileRef{Path: path, SHA256: sha}, planOnly, retire, resume)
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
		}}
	cmd.Flags().StringVar(&path, "disposition-file", "", "Original archive recovery disposition")
	cmd.Flags().StringVar(&sha, "expected-disposition-sha256", "", "Exact original disposition digest")
	cmd.Flags().BoolVar(&planOnly, "plan-only", false, "Read the original Owner's activation prerequisites without effects")
	cmd.Flags().BoolVar(&retire, "allow-controller-retirement", false, "Allow retirement of only the exact fenced controller after fresh proof")
	cmd.Flags().BoolVar(&resume, "resume-original", false, "Undo a reversible suspension before any retirement intent")
	_ = cmd.MarkFlagRequired("disposition-file")
	_ = cmd.MarkFlagRequired("expected-disposition-sha256")
	return cmd
}

func activateOriginalArchiveRecovery(ctx context.Context, ref loopruntime.SessionReadbackFileRef, planOnly, retire, resume bool) (archiveRecoveryActivationResult, error) {
	result := archiveRecoveryActivationResult{Disposition: ref, OriginalReturn: "UNKNOWN", Effects: []string{}}
	if resume {
		return revertOriginalArchiveRecovery(ctx, result)
	}
	in, err := readArchiveRecoveryExitInputs(ref)
	if err != nil {
		return result, err
	}
	fence, err := readVerifiedArchiveRecoveryFence(ref, in)
	if err != nil {
		return result, err
	}
	result.Request, result.AttemptID = fence.Scope.Request, fence.Scope.AttemptID
	result.HostInstanceID = fence.Generation.ID
	executable, err := os.Executable()
	if err != nil {
		return result, err
	}
	result.Successor, err = hostReadbackBinaryRef(executable)
	if err != nil || result.Successor == fence.Owner.ExecutionBinary {
		return result, errors.New("archive activation requires a distinct fixed successor executable")
	}
	var plan archiveRecoveryActivationPlan
	planPath := filepath.Join(in.session, archiveRecoveryActivationFile)
	activation, err := loopruntime.ReadSessionReadbackFile(planPath, &plan)
	if os.IsNotExist(err) {
		owner, err := planHostOperatorMaintenance(ctx, fence.Scope.Request.Path)
		if err != nil {
			return result, err
		}
		scope, err := observeHostOperatorNativeScope(ctx, owner)
		if err != nil {
			return result, err
		}
		if owner.Request != fence.Scope.Request || owner.Custody != fence.Custody || owner.Profile != fence.Profile || len(owner.Contexts) != 1 ||
			owner.Contexts[0].ContextID != fence.Scope.ContextID || len(owner.Resident.Contexts) != 1 {
			return result, errors.New("archive activation original Host and Gateway must serve only this attempt")
		}
		plan = archiveRecoveryActivationPlan{Schema: 1, Disposition: ref, Request: fence.Scope.Request, Successor: result.Successor, Scope: *scope}
	} else if err != nil {
		return result, err
	} else {
		result.Activation = &activation
	}
	if plan.Schema != 1 || plan.Disposition != ref || plan.Request != fence.Scope.Request || plan.Successor != result.Successor ||
		plan.Scope.Host.Binary != fence.Owner.ExecutionBinary || plan.Scope.Host.Process.PID != fence.Generation.Process.PID {
		return result, errors.New("archive activation successor or original Owner changed")
	}
	if err = archiveRecoveryActivationScope(plan.Scope, fence.Generation); err != nil {
		return result, err
	}
	if result.Activation != nil {
		reclaimed, err := readOriginalArchiveRecoveryReclamation(ref, fence.Scope)
		if err != nil {
			return result, err
		}
		if reclaimed.Path != "" {
			result.Status = "archive_recovery_reclaimed"
			return result, nil
		}
	}
	// Before a signal, acknowledge only read-only evidence. The producer will
	// collect the same proof again after the old controller is suspended.
	directory, err := admission.DefaultMachineCapacityDirectory()
	if err != nil {
		return result, err
	}
	qualify := func(ctx context.Context) error {
		fresh, err := readVerifiedArchiveRecoveryFence(ref, in)
		if err != nil || !reflect.DeepEqual(fresh.Owner, fence.Owner) || fresh.Custody != fence.Custody {
			return fmt.Errorf("archive activation Original Owner or custody changed: %v", err)
		}
		if _, err = prepareExitedArchiveReclamation(ctx, directory, fresh); err != nil {
			return err
		}
		exited := verifyOriginalArchiveHostKernelExit(fresh.Generation.Process) == nil
		if err = verifyArchiveRecoveryActivationGateway(plan.Scope, exited); err != nil {
			return err
		}
		return observeArchiveRecoveryCheckoutActors(ctx, fresh.Scope, &plan.Scope, exited)
	}
	result.ControllerPID, result.GatewayPID, result.DriverPID = plan.Scope.Host.Process.PID, plan.Scope.Gateway.Process.PID, plan.Scope.Driver.Process.PID
	if planOnly {
		if _, err := archiveRecoveryActivationMarker(in.request.HostDir, fence.Generation, plan); err != nil {
			return result, err
		}
		if err = qualify(ctx); err != nil {
			return result, err
		}
		result.Status = "archive_recovery_activation_qualified"
		return result, nil
	}
	steps := archiveRecoveryActivation{
		root: in.request.HostDir, session: in.session,
		plan: plan, generation: fence.Generation,
		locks: []string{filepath.Join(in.runtimeDir, "control.lock"), filepath.Join(in.session, "maintenance.lock")},
		validateOwner: func() error {
			fresh, err := readVerifiedArchiveRecoveryFence(ref, in)
			if err != nil || !reflect.DeepEqual(fresh.Owner, fence.Owner) || fresh.Custody != fence.Custody {
				return fmt.Errorf("archive activation original attempt identity or custody changed: %v", err)
			}
			return nil
		},
		qualify: qualify,
	}
	result.Status, result.Effects, err = steps.run(ctx, retire, false)
	if err != nil {
		return result, err
	}
	activation, err = loopruntime.ReadSessionReadbackFile(planPath, nil)
	if err != nil {
		return result, err
	}
	result.Activation = &activation
	return result, nil
}

func revertOriginalArchiveRecovery(ctx context.Context, result archiveRecoveryActivationResult) (archiveRecoveryActivationResult, error) {
	d, request, err := readOriginalArchiveRecoveryDisposition(result.Disposition)
	if err != nil {
		return result, err
	}
	session := filepath.Dir(result.Disposition.Path)
	runtimeDir := filepath.Dir(filepath.Dir(session))
	var plan archiveRecoveryActivationPlan
	ref, err := loopruntime.ReadSessionReadbackFile(filepath.Join(session, archiveRecoveryActivationFile), &plan)
	if err != nil {
		return result, err
	}
	executable, err := os.Executable()
	if err != nil {
		return result, err
	}
	successor, err := hostReadbackBinaryRef(executable)
	if err != nil || plan.Schema != 1 || plan.Disposition != result.Disposition || plan.Request != d.Request ||
		plan.Successor != successor {
		return result, errors.New("archive activation original reversible Owner plan changed")
	}
	owner, _, generation, err := readArchiveRecoveryExitOwner(d, request)
	if err != nil {
		return result, err
	}
	if owner.ExecutionBinary != plan.Scope.Host.Binary {
		return result, errors.New("archive activation original rollback executor changed")
	}
	steps := archiveRecoveryActivation{
		root: request.HostDir, session: session, plan: plan, generation: generation,
		locks: []string{filepath.Join(runtimeDir, "control.lock"), filepath.Join(session, "maintenance.lock")},
		validateOwner: func() error {
			fresh, _, current, err := readArchiveRecoveryExitOwner(d, request)
			if err != nil || !reflect.DeepEqual(fresh, owner) || !reflect.DeepEqual(current, generation) {
				return fmt.Errorf("archive activation original rollback Owner changed: %v", err)
			}
			return nil
		},
	}
	result.Request, result.AttemptID, result.HostInstanceID = d.Request, d.AttemptID, d.HostInstanceID
	result.Successor, result.Activation = successor, &ref
	result.ControllerPID, result.GatewayPID, result.DriverPID = plan.Scope.Host.Process.PID, plan.Scope.Gateway.Process.PID, plan.Scope.Driver.Process.PID
	result.Status, result.Effects, err = steps.run(ctx, false, true)
	return result, err
}

type archiveRecoveryActivation struct {
	root, session string
	plan          archiveRecoveryActivationPlan
	generation    hostGeneration
	locks         []string
	validateOwner func() error
	qualify       func(context.Context) error
}

func (a archiveRecoveryActivation) run(ctx context.Context, retire, resume bool) (string, []string, error) {
	effects := []string{}
	// Old clients check bootstrap.json before admission. The exact generation
	// socket can bypass that marker, so keep the original pool lock through the
	// kernel suspension and check for in-flight admissions under that same lock.
	epoch, err := lockfile.TryLock(filepath.Join(a.root, "maintenance.lock"))
	if err != nil {
		return "", effects, err
	}
	defer epoch.Close()
	for _, path := range append([]string{filepath.Join(a.root, "pool.lock")}, a.locks...) {
		lock, err := lockfile.TryLockExisting(path)
		if err != nil {
			return "", effects, err
		}
		defer lock.Close()
	}
	if err = ctx.Err(); err != nil {
		return "", effects, err
	}
	if err = a.validateOwner(); err != nil {
		return "", effects, err
	}
	planRaw, err := json.Marshal(a.plan)
	if err != nil {
		return "", effects, err
	}
	path := filepath.Join(a.session, archiveRecoveryActivationFile)
	bound, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return "", effects, err
	}
	if err == nil && string(bound) != string(planRaw) || os.IsNotExist(err) && resume {
		return "", effects, errors.New("archive activation saved Owner plan differs")
	}
	markerPath := filepath.Join(a.root, "bootstrap.json")
	markerRaw, err := archiveRecoveryActivationMarker(a.root, a.generation, a.plan)
	if err != nil {
		return "", effects, err
	}
	kernel := a.plan.Scope.Host.Process
	if err = archiveRecoveryActivationScope(a.plan.Scope, a.generation); err != nil {
		return "", effects, err
	}
	if resume {
		_, err := os.Lstat(markerPath)
		return a.rollback(ctx, markerPath, err == nil)
	}
	if _, err = os.Lstat(a.controlPath("resume")); !os.IsNotExist(err) {
		return "", effects, errors.New("archive activation already has a rollback intent")
	}
	if err = saveHostStorageBytes(path, planRaw); err != nil {
		return "", effects, err
	}
	if err = saveHostStorageBytes(markerPath, markerRaw); err != nil {
		return "", effects, err
	}
	if err = verifyOriginalArchiveHostKernelExit(a.generation.Process); err == nil {
		// On re-entry, independently observed exit can be reused. This does
		// not claim an acknowledgement for any earlier control operation.
		if err = a.qualify(ctx); err != nil {
			return "", effects, err
		}
		return "archive_recovery_controller_exited", effects, nil
	}
	stopped, err := a.control(ctx, "suspend", kernel, true, false)
	if err != nil {
		return "", effects, err
	}
	if stopped {
		effects = append(effects, "original_archive_controller_suspended")
	}
	if err = verifyArchiveRecoveryActivationActors(ctx, a.plan.Scope); err != nil {
		return "", effects, err
	}
	if err = a.qualify(ctx); err != nil {
		return "", effects, err
	}
	if err = verifyArchiveRecoveryActivationActors(ctx, a.plan.Scope); err != nil {
		return "", effects, err
	}
	if !retire {
		return "archive_recovery_controller_fenced", effects, nil
	}
	if err = a.validateOwner(); err != nil {
		return "", effects, err
	}
	offered, err := a.control(ctx, "retire", kernel, false, true)
	if err != nil {
		return "", effects, err
	}
	if offered {
		effects = append(effects, "original_archive_controller_retirement_offered")
	}
	for {
		if err = verifyOriginalArchiveHostKernelExit(a.generation.Process); err == nil {
			break
		}
		if err = waitArchiveActivationObservation(ctx); err != nil {
			return "", effects, err
		}
	}
	if err = a.qualify(ctx); err != nil {
		return "", effects, err
	}
	return "archive_recovery_controller_exited", effects, nil
}

func archiveRecoveryActivationScope(scope hostOperatorNativeScope, generation hostGeneration) error {
	host, gateway, driver := scope.Host.Process, scope.Gateway.Process, scope.Driver.Process
	// The rolling launcher records its own observation immediately after
	// cmd.Start. The kernel start precedes that observation, rather than
	// sharing the same clock sample. Later controls match the audit identity.
	kernelStart := time.Unix(int64(host.StartSec), int64(host.StartUsec)*1000)
	startDelay := generation.Process.StartedAt.Sub(kernelStart)
	if scope.Schema != 1 || !host.SameGeneration(host) || !gateway.SameGeneration(gateway) || !driver.SameGeneration(driver) ||
		host.UID != os.Getuid() || gateway.UID != host.UID || driver.UID != host.UID || host.PID != generation.Process.PID ||
		host.Group != host.PID || gateway.ParentPID != host.PID || gateway.Group == host.Group ||
		driver.ParentPID != gateway.PID || driver.Group != gateway.Group || generation.Binary != scope.Host.Binary.Path ||
		generation.SHA != scope.Host.Binary.SHA256 ||
		startDelay < 0 || startDelay > time.Second {
		return errors.New("archive activation exact independent Host and Gateway process scope differs")
	}
	return nil
}

func archiveRecoveryActivationMarker(root string, generation hostGeneration, plan archiveRecoveryActivationPlan) ([]byte, error) {
	raw, err := json.Marshal(plan)
	if err != nil {
		return nil, err
	}
	marker := hostBootstrap{Schema: 1, Phase: archiveRecoveryActivationPhase, Source: generation,
		Target: plan.Successor.Path, TargetSHA: plan.Successor.SHA256, ProcessIdentity: hostDigest(raw)}
	bound, err := json.MarshalIndent(marker, "", "  ")
	if err != nil {
		return nil, err
	}
	before, err := os.ReadFile(filepath.Join(root, "bootstrap.json"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	if err == nil && string(before) != string(bound) {
		return nil, errors.New("archive activation found different Host maintenance ownership")
	}
	return bound, nil
}

func verifyArchiveRecoveryActivationGateway(scope hostOperatorNativeScope, controllerExited bool) error {
	for i, actor := range []hostOperatorActor{scope.Gateway, scope.Driver} {
		p := actor.Process
		current, err := procx.ObserveNativeIdentity(p.PID)
		if err != nil || !current.SameGeneration(p) || current.Zombie || current.Stopped || current.Group != p.Group ||
			(current.ParentPID != p.ParentPID && (!controllerExited || i != 0)) {
			return errors.New("archive activation original Gateway and driver identities changed")
		}
		if err = verifyArchiveRecoveryActivationImage(current, actor.Binary); err != nil {
			return err
		}
	}
	return nil
}

func verifyArchiveRecoveryActivationActors(ctx context.Context, scope hostOperatorNativeScope) error {
	actor := scope.Host.Process
	current, err := procx.ObserveNativeIdentity(actor.PID)
	if err != nil || !current.SameGeneration(actor) || current.Zombie || !current.Stopped || current.Group != actor.Group || current.ParentPID != actor.ParentPID {
		return errors.New("archive activation original controller suspension changed")
	}
	if err = verifyArchiveRecoveryActivationImage(current, scope.Host.Binary); err != nil {
		return err
	}
	if err = verifyArchiveRecoveryActivationGateway(scope, false); err != nil {
		return err
	}
	actors := []procx.NativeIdentity{scope.Host.Process, scope.Gateway.Process, scope.Driver.Process}
	rows, err := admission.ObserveVisibleProcesses(ctx)
	if err != nil {
		return err
	}
	for _, row := range rows {
		known := slices.ContainsFunc(actors, func(p procx.NativeIdentity) bool { return row.PID == p.PID })
		if !known && slices.ContainsFunc(actors, func(p procx.NativeIdentity) bool {
			return row.ProcessGroup == p.Group || row.ParentPID == p.PID || slices.Contains(row.AncestorPIDs, p.PID)
		}) {
			return fmt.Errorf("archive activation additional process may hold an in-flight effect: %d", row.PID)
		}
	}
	return nil
}

func verifyArchiveRecoveryActivationImage(process procx.NativeIdentity, expected loopruntime.SessionReadbackFileRef) error {
	image, err := procx.ObserveNativeImage(process)
	if err != nil || image != expected.Path {
		return errors.New("archive activation original process executable changed")
	}
	return verifyArchiveRecoveryActivationBinary(expected)
}

func verifyArchiveRecoveryActivationBinary(expected loopruntime.SessionReadbackFileRef) error {
	canonical, err := filepath.EvalSymlinks(expected.Path)
	if err != nil || !filepath.IsAbs(expected.Path) || canonical != expected.Path {
		return errors.New("archive activation original executable path is unavailable or indirect")
	}
	before, err := os.Lstat(expected.Path)
	if err != nil || !before.Mode().IsRegular() {
		return errors.New("archive activation original executable is not a regular file")
	}
	// Gateway images can exceed the size limit for structured JSON evidence.
	digest, err := archive.FileHash(expected.Path)
	if err != nil {
		return err
	}
	after, err := os.Lstat(expected.Path)
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() ||
		before.ModTime() != after.ModTime() || before.Mode() != after.Mode() || digest != expected.SHA256 {
		return errors.New("archive activation original executable changed")
	}
	return nil
}

func observeArchiveRecoveryCheckoutActors(ctx context.Context, d archiveRecoveryCleanupDisposition, scope *hostOperatorNativeScope, controllerExited bool) error {
	// The original archive inspector verifies the clean checkout and Git locks.
	// Detect surviving checkout descriptors held by the Gateway side as well.
	pids := []string{fmt.Sprint(scope.Gateway.Process.PID), fmt.Sprint(scope.Driver.Process.PID)}
	if !controllerExited {
		pids = append(pids, fmt.Sprint(scope.Host.Process.PID))
	}
	out, err := exec.CommandContext(ctx, "/usr/sbin/lsof", "-p", strings.Join(pids, ","), "-Fpn").Output()
	if err != nil {
		return fmt.Errorf("archive activation cannot observe original Gateway checkout descriptors: %w", err)
	}
	seen := map[string]bool{}
	for line := range strings.Lines(string(out)) {
		switch {
		case strings.HasPrefix(line, "p"):
			seen[strings.TrimSpace(line[1:])] = true
		case strings.HasPrefix(line, "n"):
			name := strings.TrimSpace(line[1:])
			for _, path := range []string{d.Checkout.Path, d.Checkout.Registration} {
				if name == path || strings.HasPrefix(name, path+string(filepath.Separator)) {
					return errors.New("archive activation original Gateway still holds a checkout descriptor")
				}
			}
		}
	}
	for _, pid := range pids {
		if !seen[pid] {
			return errors.New("archive activation original process descriptor observation is incomplete")
		}
	}
	return nil
}

func (a archiveRecoveryActivation) controlPath(operation string) string {
	return filepath.Join(a.session, "archive-recovery-activation-"+operation+".json")
}

func (a archiveRecoveryActivation) control(ctx context.Context, operation string, actor procx.NativeIdentity, suspend, retire bool) (bool, error) {
	path := a.controlPath(operation)
	want, err := json.Marshal(a.plan)
	if err != nil {
		return false, err
	}
	prior, err := os.ReadFile(path)
	existing := err == nil
	if err != nil && !os.IsNotExist(err) || existing && string(prior) != string(want) {
		return false, errors.New("archive activation control intent changed")
	}
	if retire && existing {
		// The kernel exit observer is the only reconciliation for a lost
		// retirement acknowledgement. Do not signal again.
		return false, nil
	}
	current, err := procx.ObserveNativeIdentity(actor.PID)
	if err != nil || !current.SameGeneration(actor) || current.Zombie || current.ParentPID != actor.ParentPID || current.Group != actor.Group {
		return false, errors.New("archive activation exact original controller changed")
	}
	if !retire && current.Stopped == suspend {
		if existing {
			return false, nil
		}
		return false, errors.New("archive activation controller already changed outside this intent")
	}
	if existing || retire && !current.Stopped {
		return false, errors.New("archive activation must reconcile original control intent before proceeding")
	}
	if err = saveHostStorageBytes(path, want); err != nil {
		return false, err
	}
	if retire {
		return true, procx.SignalNativeIdentity(actor, true)
	}
	if err = procx.SetNativeSuspended(actor, suspend); err != nil {
		return false, err
	}
	for {
		current, observed := procx.ObserveNativeIdentity(actor.PID)
		if observed != nil || !current.SameGeneration(actor) || current.Zombie ||
			current.ParentPID != actor.ParentPID || current.Group != actor.Group {
			return false, errors.New("archive activation controller changed after control offer")
		}
		if current.Stopped == suspend {
			return true, nil
		}
		if err = waitArchiveActivationObservation(ctx); err != nil {
			return false, err
		}
	}
}

func waitArchiveActivationObservation(ctx context.Context) error {
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return fmt.Errorf("archive activation control effect awaits native readback: %w", ctx.Err())
	case <-timer.C:
		return nil
	}
}

func (a archiveRecoveryActivation) rollback(ctx context.Context, marker string, markerPresent bool) (string, []string, error) {
	effects := []string{}
	if _, err := os.Lstat(a.controlPath("retire")); !os.IsNotExist(err) {
		return "", effects, errors.New("archive activation retirement was offered; rollback refused")
	}
	if _, err := os.Lstat(a.controlPath("suspend")); err != nil {
		return "", effects, errors.New("archive activation has no original suspension intent")
	}
	if !markerPresent {
		if _, err := os.Lstat(a.controlPath("resume")); err != nil {
			return "", effects, errors.New("archive activation original maintenance marker is missing")
		}
	}
	resumed, err := a.control(ctx, "resume", a.plan.Scope.Host.Process, false, false)
	if err != nil {
		return "", effects, err
	}
	if resumed {
		effects = append(effects, "original_archive_controller_resumed")
	}
	if markerPresent {
		if err = os.Remove(marker); err != nil {
			return "", effects, err
		}
		if err = filex.SyncDir(a.root); err != nil {
			return "", effects, err
		}
	}
	return "archive_recovery_activation_reverted", effects, nil
}
