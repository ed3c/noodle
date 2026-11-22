//go:build darwin && cgo

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/procx"
	"github.com/poteto/noodle/loop"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/poteto/noodle/worktree"
	"github.com/poteto/noodle/worktree/archive"
)

// This seed uses the existing native preparation and quota writers. It's only
// driven by the single localhost cross-Owner test, before the VM Stop proof.
func TestSeedExitedArchiveCrossOwnerHost(t *testing.T) {
	manifest := os.Getenv("NOODLE_CROSS_OWNER_FIXTURE")
	if manifest == "" {
		t.Skip("driven by the isolated cross-Owner fixture")
	}
	var input struct {
		Root          string                   `json:"root"`
		Noodle        string                   `json:"noodle"`
		FixtureRepo   string                   `json:"fixture_repo"`
		Profile       string                   `json:"profile"`
		Policy        string                   `json:"policy"`
		Host          string                   `json:"host"`
		NativeRequest string                   `json:"native_request"`
		SessionDir    string                   `json:"session_dir"`
		ContextID     string                   `json:"context_id"`
		OrderID       string                   `json:"order_id"`
		AttemptID     string                   `json:"attempt_id"`
		Checkout      archive.CheckoutIdentity `json:"checkout"`
		LivePID       int                      `json:"live_pid"`
		LiveBinary    string                   `json:"live_binary"`
	}
	if _, err := loopruntime.ReadSessionReadbackFile(manifest, &input); err != nil {
		t.Fatal(err)
	}
	if canonical, err := filepath.EvalSymlinks(input.Root); err != nil || canonical != input.Root ||
		filepath.Dir(input.Root) != "/private/tmp" || !strings.HasPrefix(filepath.Base(input.Root), "TestArchiveRecoveryCrossOwnerInactiveHost-") {
		t.Fatal("cross-Owner driver is outside its isolated temporary root")
	}
	for _, path := range []string{manifest, input.Noodle, input.FixtureRepo, input.Profile, input.Policy, input.Host,
		input.NativeRequest, input.SessionDir, input.Checkout.Path, input.Checkout.Repo, input.Checkout.Registration} {
		canonical, err := hostPlannedPath(path)
		if err != nil || canonical != path || !strings.HasPrefix(path, input.Root+string(filepath.Separator)) {
			t.Fatalf("fixture path is outside its original isolated root: %s", path)
		}
	}
	if !validPoolID(input.ContextID) || input.OrderID == "" || input.AttemptID == "" {
		t.Fatal("missing original fixture attempt identity")
	}
	write := func(path string, data []byte) loopruntime.SessionReadbackFileRef {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		ref, err := loopruntime.ReadSessionReadbackFile(path, nil)
		if err != nil {
			t.Fatal(err)
		}
		return ref
	}
	save := func(path string, object any) loopruntime.SessionReadbackFileRef {
		t.Helper()
		data, err := json.Marshal(object)
		if err != nil {
			t.Fatal(err)
		}
		return write(path, data)
	}

	// The installed CLI deliberately has no per-Host override for the native
	// machine ledger. Build the same source with only its directory replaced.
	machine := filepath.Join(input.Root, "machine")
	overlaySources := buildIsolatedArchiveNoodle(t, input.Root, machine, input.Noodle, write)
	binary, err := hostReadbackBinaryRef(input.Noodle)
	if err != nil {
		t.Fatal(err)
	}
	originalBinary := binary
	profile, err := loopruntime.LoadOpenShellProfile(input.Profile)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := loopruntime.ReadSessionReadbackFile(input.Policy, nil)
	if err != nil {
		t.Fatal(err)
	}
	archiveConfig, err := archive.LoadConfig(policy.Path)
	if err != nil {
		t.Fatal(err)
	}
	codecPath, err := filepath.EvalSymlinks(archiveConfig.Codec)
	if err != nil {
		t.Fatal(err)
	}
	codec, err := loopruntime.ReadSessionReadbackFile(codecPath, nil)
	if err != nil {
		t.Fatal(err)
	}

	const instance = "edededededededededededededededed"
	const requestID = "aeaeaeaeaeaeaeaeaeaeaeaeaeaeaeae"
	runtimeDir := filepath.Join(input.FixtureRepo, ".noodle")
	if filepath.Dir(input.SessionDir) != filepath.Join(runtimeDir, "sessions") {
		t.Fatal("original fixture session is outside the prior runtime directory")
	}
	config := write(filepath.Join(input.FixtureRepo, ".noodle.toml"), []byte(fmt.Sprintf("[runtime]\nadmitted_orders=[%q]\n[runtime.openshell]\nprofile=%q\n", input.OrderID, input.Profile)))
	ack := save(filepath.Join(runtimeDir, "control-ack.ndjson"), loop.ControlAck{ID: "original-pause", At: time.Now().UTC(),
		Action: "pause", Status: "ok", DispatchState: loop.StatePaused})
	hold, err := func() (loopruntime.ArchiveRecoveryDispatchHold, error) {
		save(filepath.Join(runtimeDir, "status.json"), map[string]string{"loop_state": "paused"})
		return archiveRecoveryPaused(runtimeDir)
	}()
	if err != nil || hold.Acknowledgements != ack {
		t.Fatal(hold, err)
	}
	generationDir := filepath.Join(input.Host, "g", instance)
	for _, path := range []string{filepath.Join(input.Host, "host.lock"), filepath.Join(runtimeDir, "noodle.lock"),
		filepath.Join(runtimeDir, "control.lock"), filepath.Join(input.SessionDir, "maintenance.lock")} {
		write(path, []byte("isolated original lock\n"))
	}

	process := procx.ProcessRecord{SessionID: instance, StartedAt: time.Now().UTC()}
	if input.LivePID > 1 {
		if input.LiveBinary != filepath.Join(input.Root, "original-controller") {
			t.Fatal("original live executable escapes the fixture")
		}
		originalBinary, err = hostReadbackBinaryRef(input.LiveBinary)
		if err != nil {
			t.Fatal(err)
		}
		actor, err := observeSessionActor(input.LivePID, originalBinary)
		if err != nil || actor.Process.Group != input.LivePID {
			t.Fatal("original live actor is unavailable or not a dedicated process group", err)
		}
		process.PID = input.LivePID
		process.StartedAt = time.Unix(int64(actor.Process.StartSec), int64(actor.Process.StartUsec)*1000).UTC()
	} else {
		// The retained-archive control uses an actual exited native process.
		leader := exec.CommandContext(t.Context(), binary.Path, "host", "capabilities")
		leader.Dir = input.Root
		detachHostProcess(leader)
		if err = leader.Start(); err != nil {
			t.Fatal(err)
		}
		process.PID = leader.Process.Pid
		if err = leader.Wait(); err != nil {
			t.Fatal(err)
		}
		if err = verifyOriginalArchiveHostKernelExit(process); err != nil {
			t.Fatal(err)
		}
	}
	binding := hostRequestBinding{Schema: 1, RequestID: requestID, Operation: "attach", ProjectDir: input.FixtureRepo,
		ConfigPath: config.Path, ConfigSHA256: config.SHA256, Mode: "manual", BinarySHA256: originalBinary.SHA256,
		EnvironmentSHA256: hostDigest(nil), Status: "accepted", HostDir: input.Host, RequiredHostInstanceID: instance, CleanupCompleted: true}
	request := save(input.NativeRequest, binding)
	receipt := hostReceipt{Schema: 1, Protocol: hostProtocol, HostPID: process.PID, HostInstanceID: instance, ContextID: input.ContextID,
		ProjectDir: input.FixtureRepo, ContextState: "active", Mode: binding.Mode, ConfigPath: config.Path, ConfigSHA256: config.SHA256,
		BinaryPath: originalBinary.Path, BinarySHA256: originalBinary.SHA256, RequestID: requestID, HostDir: input.Host, CleanupCompleted: true}
	save(filepath.Join(generationDir, "receipts.json"), []hostReceipt{receipt})
	if err = updateHostPool(input.Host, func(pool *hostPool) error {
		pool.Active = instance
		pool.Generations[instance] = hostGeneration{ID: instance, Directory: generationDir, Binary: originalBinary.Path, SHA: originalBinary.SHA256,
			State: "active", Process: process}
		pool.Owners[requestID] = hostTaskOwner{State: "owned", Original: receipt, Current: receipt, EnvironmentSHA: binding.EnvironmentSHA256}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err = readHostOwnerBinding(input.NativeRequest); err != nil {
		t.Fatal(err)
	}

	roots := []string{input.Host, input.FixtureRepo, input.Root, archiveConfig.ArchiveRoot, archiveConfig.BlobRoot, archiveConfig.StateDir}
	encodedRoots, err := json.Marshal(roots)
	if err != nil {
		t.Fatal(err)
	}
	task := save(filepath.Join(input.Root, "task.json"), map[string]string{"repository": input.FixtureRepo, "base_ref": "main"})
	prepSource := admission.PreparationSource{Purpose: "task-preparation", Bindings: map[string]string{
		"task_path": task.Path, "task_sha256": task.SHA256, "goal_path": task.Path, "goal_sha256": task.SHA256,
		"repository": input.FixtureRepo, "head": input.Checkout.Head, "control_root": input.FixtureRepo,
		"host_root": input.Host, "profile": profile.SourceSHA256, "execution_binary": originalBinary.Path + "/" + originalBinary.SHA256,
		"storage": "isolated-fixture", "archive": policy.Path + "/" + policy.SHA256, "codec": codec.Path + "/" + codec.SHA256,
		"planned_roots": string(encodedRoots),
	}}
	claim, err := loopruntime.OpenShellLifecycleResourceClaim(profile, "task-preparation", roots...)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := loopruntime.OpenShellPreparedWorkerResourceClaim(profile, roots...)
	if err != nil {
		t.Fatal(err)
	}
	budget := admission.NewBudget(admission.DefaultWorkerLimit)
	if err = budget.BindMachineCapacity(machine, hostMachineGenerationOwner(input.Host, instance)); err != nil {
		t.Fatal(err)
	}
	if err = budget.BindSharedQuota(filepath.Join(input.Host, "quota"), instance); err != nil {
		t.Fatal(err)
	}
	prep, err := budget.PrepareMachineResources(t.Context(), prepSource, claim, hostShutdownTestSampler{})
	if err != nil {
		t.Fatal(err)
	}
	if err = budget.StartPreparationEffects(prep); err != nil {
		t.Fatal(err)
	}
	save(filepath.Join(filepath.Dir(input.FixtureRepo), "control.json"), worktree.TaskControl{Schema: 2, Status: "prepared", Preparation: prep,
		Task: worktree.ControlSource(task), Repository: input.FixtureRepo, ControlRoot: input.FixtureRepo, Head: input.Checkout.Head})
	if err = budget.BindPreparation(prep, input.ContextID, input.OrderID, "openshell"); err != nil {
		t.Fatal(err)
	}
	budget.BindResourceSource(input.ContextID, "openshell", func(string) (admission.ResourceClaim, error) { return worker, nil })
	budget.Register(input.ContextID)
	for index := 3; index >= 0; index-- {
		budget.Observe(admission.Sample{At: time.Now().Add(-time.Duration(index) * time.Second), Pressure: admission.Normal,
			TotalMemoryBytes: 16 << 30, CPUs: 8}, nil)
	}
	if _, err = budget.TryAcquireScopedResident(input.ContextID, "original-gateway"); err != nil {
		t.Fatal(err)
	}
	permit, err := budget.TryAcquireTask(input.ContextID, input.OrderID, "openshell")
	if err != nil {
		t.Fatal(err)
	}
	if err = permit.BindSession(admission.MachineSessionBinding{ContextID: input.ContextID, ProjectDir: input.FixtureRepo,
		OrderID: input.OrderID, Runtime: "openshell", AttemptID: input.AttemptID, SessionID: filepath.Base(input.SessionDir), WorktreePath: input.Checkout.Path}); err != nil {
		t.Fatal(err)
	}
	leases, err := budget.MachineSessionLeases(input.ContextID, input.FixtureRepo)
	if err != nil || len(leases) != 1 || leases[0].ID != prep.ID || leases[0].Binding == nil {
		t.Fatal(leases, err)
	}
	// Preserve native allocations after this seed process exits. The cross-owner
	// path must release only the worker allocation and settle the same nonce.
	save(filepath.Join(input.Root, "host-seed.json"), map[string]any{"request": request, "lease": leases[0], "hold": hold,
		"host_instance_id": instance, "noodle": binary, "capacity_overlay": overlaySources, "original_process": process})
}

func TestActivateIsolatedArchiveCrossOwnerHost(t *testing.T) {
	manifest := os.Getenv("NOODLE_CROSS_OWNER_FIXTURE")
	if manifest == "" {
		t.Skip("driven by the isolated live cross-Owner fixture")
	}
	var input struct {
		Root        string `json:"root"`
		Noodle      string `json:"noodle"`
		Disposition string `json:"disposition"`
		LivePID     int    `json:"live_pid"`
		LiveBinary  string `json:"live_binary"`
		Rollback    bool   `json:"activation_rollback"`
	}
	if _, err := loopruntime.ReadSessionReadbackFile(manifest, &input); err != nil {
		t.Fatal(err)
	}
	if canonical, err := filepath.EvalSymlinks(input.Root); err != nil || canonical != input.Root ||
		filepath.Dir(input.Root) != "/private/tmp" || !strings.HasPrefix(filepath.Base(input.Root), "TestArchiveRecoveryCrossOwnerInactiveHost-") ||
		input.Noodle != filepath.Join(input.Root, "noodle") || input.LiveBinary != filepath.Join(input.Root, "original-controller") ||
		!strings.HasPrefix(input.Disposition, input.Root+string(filepath.Separator)) || input.LivePID <= 1 {
		t.Fatal("successor activation test must use sacrificial fixture actors")
	}
	ref, err := loopruntime.ReadSessionReadbackFile(input.Disposition, nil)
	if err != nil {
		t.Fatal(err)
	}
	in, err := readArchiveRecoveryExitInputs(ref)
	if err != nil {
		t.Fatal(err)
	}
	fence, err := readVerifiedArchiveRecoveryFence(ref, in)
	if err != nil || fence.Generation.Process.PID != input.LivePID {
		t.Fatal("same original live generation unavailable", err)
	}
	original, err := hostReadbackBinaryRef(input.LiveBinary)
	if err != nil {
		t.Fatal(err)
	}
	successor, err := hostReadbackBinaryRef(input.Noodle)
	if err != nil || successor == original {
		t.Fatal("distinct verified successor required", err)
	}
	actor := func(pid int) hostOperatorActor {
		t.Helper()
		result, err := observeSessionActor(pid, original)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	surrogate := func(role string) hostOperatorActor {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(input.Root, role+"-pid"))
		pid, parseErr := strconv.Atoi(string(raw))
		if err != nil || parseErr != nil || pid <= 1 {
			t.Fatal("isolated Gateway actor identity unavailable", role, err, parseErr)
		}
		return actor(pid)
	}
	scope := hostOperatorNativeScope{Schema: 1, Host: actor(input.LivePID), Gateway: surrogate("gateway"), Driver: surrogate("driver")}
	plan := archiveRecoveryActivationPlan{Schema: 1, Disposition: ref, Request: fence.Scope.Request, Successor: successor, Scope: scope}
	fresh := func() (*archiveRecoveryHostExitFence, error) {
		result, err := readVerifiedArchiveRecoveryFence(ref, in)
		if err != nil {
			return nil, err
		}
		if !reflect.DeepEqual(result.Owner, fence.Owner) || result.Custody != fence.Custody {
			return nil, fmt.Errorf("original fixture scope changed")
		}
		return result, nil
	}
	activation := archiveRecoveryActivation{
		root: in.request.HostDir, session: in.session, plan: plan, generation: fence.Generation,
		locks: []string{filepath.Join(in.runtimeDir, "control.lock"), filepath.Join(in.session, "maintenance.lock")},
		validateOwner: func() error {
			_, err := fresh()
			return err
		},
		qualify: func(ctx context.Context) error {
			bound, err := fresh()
			if err != nil {
				return err
			}
			_, err = prepareExitedArchiveReclamation(ctx, filepath.Join(input.Root, "machine"), bound)
			if err != nil {
				return err
			}
			exited := verifyOriginalArchiveHostKernelExit(bound.Generation.Process) == nil
			if err = verifyArchiveRecoveryActivationGateway(scope, exited); err != nil {
				return err
			}
			return observeArchiveRecoveryCheckoutActors(ctx, bound.Scope, &scope, exited)
		},
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	status, effects, err := activation.run(ctx, false, false)
	if err != nil || status != "archive_recovery_controller_fenced" ||
		!reflect.DeepEqual(effects, []string{"original_archive_controller_suspended"}) {
		t.Fatal("successor did not fence the same live generation", status, effects, err)
	}
	pending, err := hostMaintenancePending(in.request.HostDir)
	if err != nil || !pending {
		t.Fatal("fence must survive lock release and protect subsequent entry", err)
	}
	current, err := procx.ObserveNativeIdentity(scope.Host.Process.PID)
	if err != nil || !current.SameGeneration(scope.Host.Process) || !current.Stopped {
		t.Fatal("original controller must remain suspended across successor calls", err)
	}
	status, effects, err = activation.run(ctx, false, false)
	if err != nil || status != "archive_recovery_controller_fenced" || len(effects) != 0 {
		t.Fatal("readback repeated original controller suspension", status, effects, err)
	}
	if observed := observeArchiveRecoveryActivation(ref, fence.Scope, fence.Request); observed != nil {
		t.Fatal("a suspended controller must not project completed activation", observed)
	}
	unsettled := filepath.Join(input.Root, "fixture-pending-gateway")
	if err = os.WriteFile(unsettled, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, effects, err = activation.run(ctx, true, false); err == nil || len(effects) != 0 {
		t.Fatal("unknown Gateway effect did not block original controller retirement", effects, err)
	}
	if _, err = os.Lstat(activation.controlPath("retire")); !os.IsNotExist(err) {
		t.Fatal("successor offered retirement before Gateway qualification", err)
	}
	if input.Rollback {
		command := exec.CommandContext(ctx, input.Noodle, "openshell", "progress-maintenance", archiveRecoveryActivationOperation,
			"--disposition-file", ref.Path, "--expected-disposition-sha256", ref.SHA256, "--resume-original")
		raw, err := command.CombinedOutput()
		var result archiveRecoveryActivationResult
		if err != nil || json.Unmarshal(raw, &result) != nil || result.Status != "archive_recovery_activation_reverted" ||
			result.Disposition != ref || result.AttemptID != fence.Scope.AttemptID || result.Next != nil ||
			!reflect.DeepEqual(result.Effects, []string{"original_archive_controller_resumed"}) {
			t.Fatal("original actor could not be resumed while Gateway readback stayed unavailable", err, string(raw))
		}
		pending, err := hostMaintenancePending(in.request.HostDir)
		if err != nil || pending {
			t.Fatal("explicit rollback retained the temporary maintenance gate", err)
		}
		if _, effects, err = activation.run(ctx, true, false); err == nil || len(effects) != 0 {
			t.Fatal("rolled-back activation epoch unexpectedly offered a retirement", effects, err)
		}
		if err = os.Remove(unsettled); err != nil {
			t.Fatal(err)
		}
		return
	}
	if err = os.Remove(unsettled); err != nil {
		t.Fatal(err)
	}
	status, effects, err = activation.run(ctx, true, false)
	if err != nil || status != "archive_recovery_controller_exited" ||
		!reflect.DeepEqual(effects, []string{"original_archive_controller_retirement_offered"}) {
		t.Fatal("qualified successor did not retire the exact original controller", status, effects, err)
	}
	status, effects, err = activation.run(ctx, true, false)
	if err != nil || status != "archive_recovery_controller_exited" || len(effects) != 0 {
		t.Fatal("successor repeated controller retirement after observed exit", status, effects, err)
	}
	bound, err := loopruntime.ReadSessionReadbackFile(filepath.Join(in.session, archiveRecoveryActivationFile), nil)
	if observed := observeArchiveRecoveryActivation(ref, fence.Scope, fence.Request); err != nil || observed == nil || *observed != bound {
		t.Fatal("original activation was not projected after confirmed exit", observed, err)
	}
}

func buildIsolatedArchiveNoodle(t *testing.T, root, machine, binary string, write func(string, []byte) loopruntime.SessionReadbackFileRef) map[string]loopruntime.SessionReadbackFileRef {
	t.Helper()
	path, err := filepath.Abs("internal/admission/machine_capacity.go")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	original := string(raw)
	start := strings.Index(original, "func DefaultMachineCapacityDirectory()")
	if start < 0 || strings.Count(original, `"os/user"`) != 1 || strings.Count(original, "user.LookupId(") != 1 {
		t.Fatal("original capacity directory selector drifted")
	}
	if tail := strings.Index(original[start:], "\n}\n"); tail < 0 {
		t.Fatal("original capacity directory selector end is unavailable")
	}
	end := strings.Index(original[start:], "\n}\n") + start + len("\n}\n")
	isolated := original[:start] + fmt.Sprintf("func DefaultMachineCapacityDirectory() (string, error) { return %q, nil }\n", machine) + original[end:]
	isolated = strings.Replace(isolated, "\n\t\"os/user\"", "", 1)
	if !strings.Contains(isolated, "runtime.") {
		isolated = strings.Replace(isolated, "\n\t\"runtime\"", "", 1)
	}
	if !strings.Contains(isolated, "strconv.") {
		isolated = strings.Replace(isolated, "\n\t\"strconv\"", "", 1)
	}
	replacement := write(filepath.Join(root, "isolated-capacity.go"), []byte(isolated))
	overlay := []byte(fmt.Sprintf(`{"Replace":{%q:%q}}`, path, replacement.Path))
	overlayFile := write(filepath.Join(root, "capacity-overlay.json"), overlay)
	build := exec.CommandContext(t.Context(), "go", "build", "-overlay="+overlayFile.Path, "-o", binary, ".")
	build.Env = append(os.Environ(), "GOPROXY=off", "GOSUMDB=off")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build isolated native Noodle CLI: %v\n%s", err, out)
	}
	return map[string]loopruntime.SessionReadbackFileRef{"original": {Path: path, SHA256: hostDigest(raw)},
		"replaced": replacement, "map": overlayFile}
}
