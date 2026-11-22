//go:build darwin && cgo

package runtime

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	dm "github.com/NVIDIA/OpenShell/sdk/go/proto/datamodelv1"
	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/procx"
	"github.com/poteto/noodle/internal/workspaceartifacts"
	"github.com/poteto/noodle/worktree/archive"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestArchiveRecoveryIsolatedCrossOwnerLoop(t *testing.T) {
	driver, resultPath := os.Getenv("NOODLE_CROSS_OWNER_DRIVER"), os.Getenv("NOODLE_CROSS_OWNER_RESULT")
	mode := os.Getenv("NOODLE_CROSS_OWNER_LIVE_ACTIVATION")
	rollback := mode == "rollback"
	packedStash := mode == "packed"
	liveActivation := mode == "1" || rollback || packedStash
	if driver == "" || resultPath == "" {
		t.Skip("select the isolated Soodles fixture driver and compact result path")
	}
	parent, err := filepath.EvalSymlinks(filepath.Dir(resultPath))
	if err != nil || !filepath.IsAbs(resultPath) || parent != filepath.Dir(resultPath) ||
		(parent != "/private/tmp" && !strings.HasPrefix(parent, "/private/tmp/")) {
		t.Fatal("compact result must be an exclusive path in /private/tmp", err)
	}
	root, err := os.MkdirTemp("/private/tmp", "TestArchiveRecoveryCrossOwnerInactiveHost-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	// /private/tmp's group is inherited by new directories on macOS. Set the
	// disposable root's group before Git creates its original registration.
	if err = os.Chown(root, -1, os.Getgid()); err != nil {
		t.Fatal(err)
	}
	const name, id, main = "owned-vm", "native-vm", "original-main"
	const attemptID, sessionID = "fixture-attempt", "fixture-session"
	contextID := strings.Repeat("c1", 16)
	task := OpenShellTask{"fixture-order", "native-attempt", sessionID}
	host := filepath.Join(root, "host")
	repo := filepath.Join(host, "controls", strings.Repeat("c0", 16), "checkout")
	runtimeDir := filepath.Join(repo, ".noodle")
	session := filepath.Join(runtimeDir, "sessions", sessionID)
	custodyRoot := filepath.Join(host, "openshell", "custody")
	custodyPath := filepath.Join(custodyRoot, "owner.json")
	vmRoot := filepath.Join(root, "vm")
	disk := filepath.Join(vmRoot, "sandboxes", id)
	captureDir, ownerDir := filepath.Join(root, "capture"), filepath.Join(root, "soodles-owner")
	for _, path := range []string{session, custodyRoot, disk, captureDir, ownerDir} {
		if err = os.MkdirAll(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.WriteFile(filepath.Join(custodyRoot, "owner.lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	environment := slices.DeleteFunc(os.Environ(), func(entry string) bool {
		return strings.HasPrefix(entry, "GIT_") || strings.HasPrefix(entry, "GH_") || strings.HasPrefix(entry, "GITHUB_") ||
			strings.HasPrefix(entry, "OPENAI_") || strings.HasPrefix(entry, "NOODLE_")
	})
	environment = append(environment, "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	// Initialize the fixture repository before writing any durable task input.
	// Checkout content, signed restoration, and native disk evidence are real
	// local effects. Provider and historical dispatch records belong to the fixture.
	app, checkout, _, policy := prepareArchiveRecoveryTestCheckout(t, root, repo)
	ref := func(path string) SessionReadbackFileRef {
		t.Helper()
		result, err := ReadSessionReadbackFile(path, nil)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	write := func(path string, value any) SessionReadbackFileRef {
		t.Helper()
		if err := maintenanceWrite(path, value); err != nil {
			t.Fatal(err)
		}
		return ref(path)
	}
	var disabledEnvironment SessionReadbackFileRef
	var sharedStashSources []SessionReadbackFileRef
	if liveActivation {
		git := func(args ...string) {
			t.Helper()
			command := app.Execution.Command(t.Context(), repo, "git", args...)
			if out, err := command.CombinedOutput(); err != nil {
				t.Fatalf("fixture Git %q: %v\n%s", args, err, out)
			}
		}
		git("config", "--local", "extensions.worktreeConfig", "true")
		config := filepath.Join(checkout.Registration, "config.worktree")
		if err = os.WriteFile(config, []byte("[codex]\n\tlocalEnvironmentConfigPath = __none__\n"), 0600); err != nil {
			t.Fatal(err)
		}
		disabledEnvironment = ref(config)
		if err = os.WriteFile(filepath.Join(repo, "result.txt"), []byte("unrelated primary-worktree stash\n"), 0600); err != nil {
			t.Fatal(err)
		}
		git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "stash", "push", "-m", "unrelated shared stash")
		stashSource := "refs/stash"
		if packedStash {
			git("pack-refs", "--all")
			if _, err = os.Lstat(filepath.Join(checkout.Repo, stashSource)); !os.IsNotExist(err) {
				t.Fatal("original checkout shape must retain the stash only in packed-refs", err)
			}
			stashSource = "packed-refs"
		}
		for _, name := range []string{stashSource, "logs/refs/stash"} {
			sharedStashSources = append(sharedStashSources, ref(filepath.Join(checkout.Repo, filepath.FromSlash(name))))
		}
	}
	var stopped, absent atomic.Bool
	var deleteCalls, stopCalls atomic.Int32
	sandbox := func() *pb.Sandbox {
		phase := pb.SandboxPhase_SANDBOX_PHASE_READY
		if stopped.Load() {
			phase = pb.SandboxPhase_SANDBOX_PHASE_STOPPED
		}
		result := custodySandbox(name, id, phase)
		result.Metadata.ResourceVersion, result.Status.MainProcessInstanceId = 11, main
		return result
	}
	server := &openShellNativeServer{
		list: func(context.Context, *pb.ListSandboxesRequest) (*pb.ListSandboxesResponse, error) {
			if _, err := os.Stat(filepath.Join(root, "fixture-pending-gateway")); err == nil {
				return nil, status.Error(codes.Unavailable, "fixture Gateway effect awaiting readback")
			}
			if absent.Load() {
				return &pb.ListSandboxesResponse{}, nil
			}
			return &pb.ListSandboxesResponse{Sandboxes: []*pb.Sandbox{sandbox()}}, nil
		},
		get: func(context.Context, *pb.GetSandboxRequest) (*pb.SandboxResponse, error) {
			if absent.Load() {
				return nil, status.Error(codes.NotFound, "original fixture VM absent")
			}
			return &pb.SandboxResponse{Sandbox: sandbox()}, nil
		},
		stop: func(context.Context, *pb.StopSandboxRequest) (*pb.SandboxResponse, error) {
			stopCalls.Add(1)
			stopped.Store(true)
			return &pb.SandboxResponse{Sandbox: sandbox()}, nil
		},
		deleteMatches: func(_ context.Context, req *pb.DeleteStoppedSandboxIfMatchesRequest) (*pb.DeleteStoppedSandboxIfMatchesResponse, error) {
			deleteCalls.Add(1)
			var custody OpenShellCustodyState
			_, readErr := ReadSessionReadbackFile(custodyPath, &custody)
			if req.GetName() != name || req.GetExpectedSandboxId() != id || req.GetExpectedResourceVersion() != 11 ||
				req.GetWorkspaceScope().GetWorkspace() != "owned-workspace" || req.GetRequestId() == "" || !stopped.Load() ||
				readErr != nil || len(custody.Records) != 1 || custody.Records[0].Retirement == nil ||
				custody.Records[0].Retirement.RequestID != req.GetRequestId() {
				t.Error("conditional delete escaped the retired fixture VM or lacked durable intent")
				return nil, status.Error(codes.Aborted, "wrong fixture VM")
			}
			if err := os.Remove(disk); err != nil {
				return nil, status.Error(codes.FailedPrecondition, "fixture VM disk removal failed")
			}
			absent.Store(true)
			return &pb.DeleteStoppedSandboxIfMatchesResponse{SandboxId: id, Outcome: pb.DeletionOutcome_DELETION_OUTCOME_COMPLETED}, nil
		},
	}
	target, security := openShellTestGateway(t, server)
	profilePath := filepath.Join(host, "openshell", "profile.json")
	profile := writeCrossOwnerProfile(t, profilePath, custodyRoot, vmRoot, target, security)
	profileDigest, err := OpenShellDispatchSHA256(profile)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := openShellBytes(sandbox().GetSpec())
	if err != nil {
		t.Fatal(err)
	}
	create := &pb.CreateSandboxRequest{Name: name, RequestId: "original-create", Spec: sandbox().GetSpec(),
		WorkspaceScope: &dm.WorkspaceSelector{Selection: &dm.WorkspaceSelector_Workspace{Workspace: target.Sandbox.Workspace}}}
	intent, err := openShellBytes(create)
	if err != nil {
		t.Fatal(err)
	}
	failed := OpenShellExecResult{RequestID: "original-exec", StartAttempted: true, ExitObserved: true, ExitCode: 74}
	record := OpenShellCustodyRecord{Task: task, Name: name, Spec: spec, CreateAcknowledged: true, MainProcessInstanceID: main,
		CreateRequestID: create.GetRequestId(), CreateIntent: intent,
		Worker: OpenShellWorkerRecord{Identity: OpenShellIdentity{OrderID: task.OrderID, AttemptID: task.AttemptID, SessionID: sessionID,
			Sandbox: target.Sandbox, SandboxName: name, ConfigSource: profile.Gateway.Binding.ConfigSource}, ExecResult: failed},
		Exec: &OpenShellCustodyExec{AttemptID: task.AttemptID, Command: OpenShellCommand{RequestID: failed.RequestID}, Result: failed}}
	state := OpenShellCustodyState{Version: openShellCustodyVersion, Binding: profile.Gateway.Binding, ControlRoot: host,
		Records: []OpenShellCustodyRecord{record}}
	custody := write(custodyPath, state)

	nativeRequest := filepath.Join(ownerDir, "host-request.json")
	candidatePath := filepath.Join(session, "archive-recovery-candidate.json")
	dispositionPath := filepath.Join(session, "archive-recovery-cleanup-disposition.json")
	landingPath := filepath.Join(ownerDir, "archive-landing-proof.json")
	manifestInput := map[string]any{
		"root": root, "host": host, "noodle": filepath.Join(root, "noodle"), "profile": profilePath,
		"fixture_repo": repo, "native_request": nativeRequest, "session_dir": session,
		"order_id": task.OrderID, "attempt_id": attemptID, "context_id": contextID,
		"head": checkout.Head, "checkout": checkout, "policy": policy.Path,
		"candidate": candidatePath, "disposition": dispositionPath, "landing_proof": landingPath,
	}
	var originalActor procx.NativeIdentity
	var exited <-chan error
	if liveActivation {
		originalActor, exited = startArchiveCrossOwnerController(t, root, host, runtimeDir, environment)
		manifestInput["live_pid"] = originalActor.PID
		manifestInput["live_binary"] = filepath.Join(root, "original-controller")
		manifestInput["activation_rollback"] = rollback
	}
	manifest := write(filepath.Join(root, "fixture.json"), manifestInput)
	command := exec.CommandContext(t.Context(), "go", "test", ".", "-run", "^TestSeedExitedArchiveCrossOwnerHost$", "-count=1", "-v")
	command.Dir = ".."
	command.Env = append(environment, "NOODLE_CROSS_OWNER_FIXTURE="+manifest.Path, "GOPROXY=off", "GOSUMDB=off")
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("disposable native Host seed: %v\n%s", err, out)
	}
	var seed struct {
		Request        SessionReadbackFileRef        `json:"request"`
		Lease          admission.MachineSessionLease `json:"lease"`
		Hold           ArchiveRecoveryDispatchHold   `json:"hold"`
		HostInstanceID string                        `json:"host_instance_id"`
	}
	if _, err = ReadSessionReadbackFile(filepath.Join(root, "host-seed.json"), &seed); err != nil {
		t.Fatal(err)
	}
	if seed.Request != ref(nativeRequest) || seed.Lease.Binding == nil || seed.Lease.Binding.WorktreePath != checkout.Path ||
		seed.Lease.Binding.AttemptID != attemptID || seed.HostInstanceID == "" {
		t.Fatal("native Host did not seed the same request, checkout, and attempt")
	}
	baseline, err := workspaceartifacts.Capture(t.Context(), checkout.Path, filepath.Join(session, "baseline"))
	if err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(captureDir, "workspace.tar")
	if err = os.WriteFile(archivePath, progressFixtureArchive(t, "noodle-result.tar"), 0600); err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(root, "review-restore")
	extracted, err := workspaceartifacts.ExtractWithManifest(t.Context(), archivePath, restored)
	if err != nil {
		t.Fatal(err)
	}
	scope := workspaceartifacts.LocalTaskScope{IntegrationBranch: "main", WritePaths: []string{"result.txt"}}
	tree, err := workspaceartifacts.InspectArchiveCandidate(t.Context(), checkout.Path, filepath.Join(root, "scratch"), restored, baseline, scope)
	if err != nil {
		t.Fatal(err)
	}
	dispatch := write(filepath.Join(session, "openshell-dispatch.json"), openShellDispatchReceipt{Version: 1, ProfileSHA256: profileDigest,
		ContextID: contextID, OrderID: task.OrderID, AttemptID: task.AttemptID, SessionID: sessionID, Target: checkout.Path,
		Worker: &record.Worker, Phase: "progress-unknown", Error: "original archive failed", Outcome: SessionOutcome{Status: StatusFailed}})
	capture := OpenShellWorkspaceCapture{Schema: 1, Status: "workspace_archive_verified", Scope: "repository",
		OriginalReturn: "UNKNOWN", RestorationVerified: true, OrderID: task.OrderID, NativeAttemptID: task.AttemptID,
		SessionID: sessionID, SandboxID: id, MainProcessInstanceID: main, BaselineHead: baseline.Head,
		Archive: ref(archivePath), BaselineSource: dispatch, Sources: map[string]SessionReadbackFileRef{
			"request": seed.Request, "dispatch": dispatch, "custody": custody, "profile": ref(profilePath), "baseline": dispatch,
			"before":             write(filepath.Join(captureDir, "before.json"), "fixture capture before"),
			"after":              write(filepath.Join(captureDir, "after.json"), "fixture capture after"),
			"workspace_manifest": write(filepath.Join(captureDir, "workspace-manifest.json"), extracted)}}
	captureRef := write(filepath.Join(captureDir, "workspace-capture.json"), capture)
	review := write(filepath.Join(root, "archive-review.json"), map[string]any{"schema": 1, "status": "archive_recovery_review_evidence",
		"original_return": "UNKNOWN", "attempt_id": attemptID, "capture": captureRef, "tree": tree, "authorizes_landing": false})
	conn, err := newOpenShellConnection(profile.Gateway.Binding.Gateway, profile.Gateway.TLS)
	if err != nil {
		t.Fatal(err)
	}
	client := pb.NewOpenShellClient(conn)
	input := storageFailureInputs{state: state, record: record}
	original := ProgressMaintenance{SessionDirectory: session, Request: seed.Request}
	verifyFixture := func() error { return nil }
	stop, stopErr := stopOpenShellArchiveRecovery(t.Context(), client, input, original, review, captureRef, seed.Hold,
		filepath.Join(session, "archive-recovery-stop"), verifyFixture)
	if stopErr != nil {
		conn.Close()
		t.Fatal(stopErr)
	}
	apply := OpenShellArchiveRecoveryApplyInput{Review: review, Capture: captureRef, Stop: stop, AttemptID: attemptID,
		DispatchHold: seed.Hold, Tree: tree, Scope: scope, Target: checkout.Path}
	candidate, applyErr := applyOpenShellArchiveRecovery(t.Context(), original, apply, capture, baseline, input, custody, verifyFixture,
		func(ctx context.Context) (*pb.SandboxResponse, error) {
			return client.GetSandbox(ctx, &pb.GetSandboxRequest{Name: name, WorkspaceScope: create.WorkspaceScope})
		})
	if err = conn.Close(); applyErr != nil || err != nil || candidate.Path != candidatePath {
		t.Fatal("original fixture Apply did not produce the reviewed candidate", applyErr, err)
	}
	checkout, err = app.ReadCheckoutIdentity(checkout.Path)
	if err != nil || checkout.Head == baseline.Head {
		t.Fatal("applied checkout does not identify a new original candidate", err)
	}

	python := func(verb string) map[string]any {
		t.Helper()
		command := exec.CommandContext(t.Context(), "python3", driver, verb, manifest.Path)
		command.Env = append(environment, "PYTHONDONTWRITEBYTECODE=1")
		out, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("isolated Soodles %s: %v\n%s", verb, err, out)
		}
		var result map[string]any
		if err = json.Unmarshal(out, &result); err != nil {
			t.Fatalf("decode isolated Soodles %s result: %v\n%s", verb, err, out)
		}
		return result
	}
	python("seed")
	disposition := write(dispositionPath, struct {
		archiveRecoveryRetirementDisposition
		HostInstanceID string `json:"host_instance_id"`
	}{archiveRecoveryRetirementDisposition: archiveRecoveryRetirementDisposition{
		Schema: 1, Kind: "archive_recovery_cleanup_disposition", Candidate: candidate, LandingProof: ref(landingPath),
		Request: seed.Request, Stop: stop, Archive: capture.Archive, Hold: seed.Hold, Checkout: checkout, Lease: seed.Lease,
		ContextID: contextID, OrderID: task.OrderID, AttemptID: attemptID, SessionID: sessionID, NativeAttemptID: task.AttemptID,
		SandboxID: id, MainProcessInstanceID: main, Head: checkout.Head, Tree: tree.CandidateTree, OriginalReturn: "UNKNOWN"},
		HostInstanceID: seed.HostInstanceID})
	if liveActivation {
		python("held")
		current, err := procx.ObserveNativeIdentity(originalActor.PID)
		if err != nil || !originalActor.SameGeneration(current) || current.Stopped || current.Zombie {
			t.Fatal("original actor must still be alive before the successor takes control", err)
		}
		activation := exec.CommandContext(t.Context(), "go", "test", ".", "-run", "^TestActivateIsolatedArchiveCrossOwnerHost$", "-count=1", "-v")
		activation.Dir = ".."
		activation.Env = append(environment, "NOODLE_CROSS_OWNER_FIXTURE="+manifest.Path, "GOPROXY=off", "GOSUMDB=off")
		if out, err := activation.CombinedOutput(); err != nil {
			t.Fatalf("successor-led activation of disposable native Host: %v\n%s", err, out)
		}
		if rollback {
			current, err := procx.ObserveNativeIdentity(originalActor.PID)
			if err != nil || !originalActor.SameGeneration(current) || current.Stopped || current.Zombie {
				t.Fatal("explicit rollback did not resume the same original actor", err)
			}
			if python("held")["live_original_controller_rejected"] != true || !stopped.Load() || deleteCalls.Load() != 0 {
				t.Fatal("rollback changed the original VM or allowed a settlement effect")
			}
			return
		}
		select {
		case err := <-exited:
			killed, ok := err.(*exec.ExitError)
			if !ok || killed.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
				t.Fatal("successor did not retire only the original isolated controller", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("original isolated controller stayed present after handoff")
		}
		for _, role := range []string{"gateway", "driver"} {
			bound, err := os.ReadFile(filepath.Join(root, role+"-pid"))
			pid, parseErr := strconv.Atoi(string(bound))
			actor, observeErr := procx.ObserveNativeIdentity(pid)
			if err != nil || parseErr != nil || observeErr != nil || actor.Stopped || actor.Zombie {
				t.Fatal("handoff affected a surviving Gateway-side actor", role, err, parseErr, observeErr)
			}
		}
		conn, err := newOpenShellConnection(profile.Gateway.Binding.Gateway, profile.Gateway.TLS)
		if err != nil {
			t.Fatal("successor stopped the original gateway", err)
		}
		after, err := pb.NewOpenShellClient(conn).GetSandbox(t.Context(), &pb.GetSandboxRequest{Name: name, WorkspaceScope: create.WorkspaceScope})
		if closeErr := conn.Close(); err != nil || closeErr != nil || after.GetSandbox().GetStatus().GetPhase() != pb.SandboxPhase_SANDBOX_PHASE_STOPPED ||
			after.GetSandbox().GetMetadata().GetId() != id || deleteCalls.Load() != 0 {
			t.Fatal("handoff changed original Gateway or VM before LoopManager settlement", err, closeErr)
		}
	}
	evidence := python("run")
	_, checkoutErr := os.Lstat(checkout.Path)
	_, diskErr := os.Lstat(disk)
	if !os.IsNotExist(checkoutErr) || !os.IsNotExist(diskErr) || !absent.Load() ||
		deleteCalls.Load() != 1 || stopCalls.Load() != 1 || ref(dispatch.Path) != dispatch || ref(stop.Path) != stop {
		t.Fatal("isolated original checkout/VM effects or retained failure evidence differs")
	}
	packet, err := archive.ReadCheckoutPacket(policy, "archive-recovery-checkout/"+disposition.SHA256)
	if err != nil || packet.Request.Identity != checkout {
		t.Fatal("original checkout preservation proof differs", err)
	}
	before := packet.Tree.Entries["result.txt"].Hash
	after := packet.VerificationCopy.Entries[filepath.Join("checkout", "result.txt")].Hash
	if before == "" || before != after {
		t.Fatal("archived checkout result did not restore")
	}
	if liveActivation {
		entry := packet.Source.AdminTree.Entries["config.worktree"]
		restored := filepath.Join(root, "confirmed-checkout-restore")
		if err = archive.RestoreCheckoutRecovery(packet, restored); err != nil {
			t.Fatal("original checkout could not be restored after its verified removal", err)
		}
		restoredConfig := ref(filepath.Join(restored, "registration", "config.worktree"))
		if entry.Hash != disabledEnvironment.SHA256 || restoredConfig.SHA256 != disabledEnvironment.SHA256 {
			t.Fatal("same checkout's disabled Codex environment did not restore from the preserved bytes")
		}
		for _, saved := range sharedStashSources {
			if ref(saved.Path) != saved {
				t.Fatal("unrelated shared stash or reflog changed during same-attempt settlement", saved.Path)
			}
		}
		evidence["archived_codex_config_preserved"] = true
		evidence["shared_stash_preserved"] = true
		evidence["shared_stash_packed"] = packedStash
	}
	checkoutProof, err := archive.ReadCheckoutReclamation(packet)
	if err != nil {
		t.Fatal(err)
	}
	settled, err := admission.ReadArchivedArchiveRecoverySettlement(filepath.Join(root, "machine"), seed.Lease, admission.MachineSettlementFile(disposition))
	if err != nil || settled == nil || settled.RuntimeState != CleanupSessionRetired || settled.SandboxID != id ||
		settled.Sources["checkout_packet"] != admission.MachineSettlementFile(checkoutProof.Packet) {
		t.Fatal("original machine ledger did not confirm the same archived settlement", err)
	}
	evidence["disposition_sha256"] = disposition.SHA256
	evidence["native_request_sha256"] = seed.Request.SHA256
	evidence["original_failure_sha256"] = dispatch.SHA256
	evidence["archive_stop_sha256"] = stop.SHA256
	evidence["checkout_removed"] = true
	evidence["restored_result_sha256"] = after
	evidence["original_machine_lease_settled"] = true
	evidence["vm_disk_removed"] = true
	evidence["gateway_conditional_delete_calls"] = deleteCalls.Load()
	evidence["gateway_stop_calls"] = stopCalls.Load()
	evidence["loop_entry"] = "soodles loop inspect/run/inspect/run --request-file <isolated-request>"
	evidence["live_original_controller"] = liveActivation
	// Temporary paths refer to material that will be discarded. The compact
	// artifact preserves digests and effects only.
	for _, key := range []string{"request", "noodle", "disposition", "reclamation"} {
		if item, ok := evidence[key].(map[string]any); ok {
			evidence[key+"_sha256"] = item["sha256"]
		}
		delete(evidence, key)
	}
	bytes, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	output, err := os.OpenFile(resultPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := output.Write(append(bytes, '\n'))
	if closeErr := output.Close(); writeErr != nil || closeErr != nil {
		t.Fatal(writeErr, closeErr)
	}
}

func TestArchiveCrossOwnerControllerChild(t *testing.T) {
	root := os.Getenv("NOODLE_CROSS_OWNER_CONTROLLER_ROOT")
	if root == "" {
		return
	}
	if filepath.Dir(root) != "/private/tmp" || !strings.HasPrefix(filepath.Base(root), "TestArchiveRecoveryCrossOwnerInactiveHost-") {
		t.Fatal("controller child must be isolated")
	}
	host, runtimeDir := os.Getenv("NOODLE_CROSS_OWNER_CONTROLLER_HOST"), os.Getenv("NOODLE_CROSS_OWNER_CONTROLLER_RUNTIME")
	for _, directory := range []string{host, runtimeDir} {
		canonical, err := filepath.EvalSymlinks(directory)
		if err != nil || canonical != directory || !strings.HasPrefix(directory, root+string(filepath.Separator)) {
			t.Fatal("controller child state escapes fixture", err)
		}
	}
	role := os.Getenv("NOODLE_CROSS_OWNER_CONTROLLER_ROLE")
	spawn := func(role string) *exec.Cmd {
		t.Helper()
		child := exec.Command(os.Args[0], "-test.run=^TestArchiveCrossOwnerControllerChild$")
		env := slices.DeleteFunc(os.Environ(), func(entry string) bool {
			return strings.HasPrefix(entry, "NOODLE_CROSS_OWNER_CONTROLLER_ROLE=")
		})
		child.Env = append(env, "NOODLE_CROSS_OWNER_CONTROLLER_ROLE="+role)
		if role == "gateway" {
			child.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		}
		if err := child.Start(); err != nil {
			t.Fatal(err)
		}
		return child
	}
	if role == "gateway" || role == "driver" {
		// Real actors retain the original kernel ancestry for activation
		// controls. The runtime test owns the separate loopback RPC fixture.
		var child *exec.Cmd
		if role == "gateway" {
			child = spawn("driver")
			defer func() { _ = child.Wait() }()
		}
		if err := os.WriteFile(filepath.Join(root, role+"-pid"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
			t.Fatal(err)
		}
		for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(25 * time.Millisecond) {
			if _, err := os.Stat(filepath.Join(root, "fixture-finish")); err == nil {
				return
			}
		}
		return
	}
	if role != "controller" {
		t.Fatal("unknown isolated actor", role)
	}
	hostLock, err := lockfile.TryLockShared(filepath.Join(host, "host.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer hostLock.Close()
	runtimeLock, err := lockfile.TryLock(filepath.Join(runtimeDir, "noodle.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer runtimeLock.Close()
	// Model only the known frozen-controller property: normal shutdown
	// cannot clear the runtime hold. A real generation holds these locks.
	signal.Ignore(syscall.SIGTERM)
	gateway := spawn("gateway")
	go func() { _ = gateway.Wait() }()
	for _, role := range []string{"gateway", "driver"} {
		for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
			if _, err = os.Stat(filepath.Join(root, role+"-pid")); err == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("isolated actor did not start", role, err)
			}
		}
	}
	if err = os.WriteFile(filepath.Join(root, "controller-ready"), []byte("locked\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for {
		time.Sleep(time.Second)
	}
}

func startArchiveCrossOwnerController(t *testing.T, root, host, runtimeDir string, environment []string) (procx.NativeIdentity, <-chan error) {
	t.Helper()
	image, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(image)
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root, "original-controller")
	if err = os.WriteFile(binary, data, 0700); err != nil {
		t.Fatal(err)
	}
	child := exec.Command(binary, "-test.run=^TestArchiveCrossOwnerControllerChild$")
	child.Env = append(environment, "NOODLE_CROSS_OWNER_CONTROLLER_ROOT="+root,
		"NOODLE_CROSS_OWNER_CONTROLLER_HOST="+host, "NOODLE_CROSS_OWNER_CONTROLLER_RUNTIME="+runtimeDir,
		"NOODLE_CROSS_OWNER_CONTROLLER_ROLE=controller")
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err = child.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		exited <- child.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		_ = os.WriteFile(filepath.Join(root, "fixture-finish"), nil, 0600)
		select {
		case <-done:
		default:
			_ = child.Process.Kill()
			<-done
		}
		for _, role := range []string{"driver", "gateway"} {
			bound, err := os.ReadFile(filepath.Join(root, role+"-pid"))
			pid, parseErr := strconv.Atoi(string(bound))
			if err != nil || parseErr != nil {
				continue
			}
			for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
				if _, err := procx.ObserveNativeIdentity(pid); err != nil {
					break
				}
			}
		}
	})
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		if _, err = os.Stat(filepath.Join(root, "controller-ready")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("isolated controller did not claim its locks", err)
		}
	}
	identity, err := procx.ObserveNativeIdentity(child.Process.Pid)
	if err != nil || identity.ParentPID != os.Getpid() || identity.Group != identity.PID || !identity.SameGeneration(identity) {
		t.Fatal("isolated controller identity changed", err)
	}
	return identity, exited
}

func writeCrossOwnerProfile(t *testing.T, path, custody, vmRoot string, gateway OpenShellTarget, security OpenShellTLS) OpenShellProfile {
	t.Helper()
	root := filepath.Dir(path)
	write := func(name string, data []byte, mode os.FileMode) string {
		t.Helper()
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, mode); err != nil {
			t.Fatal(err)
		}
		return path
	}
	private, err := x509.MarshalPKCS8PrivateKey(security.Certificate.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: security.Certificate.Certificate[1]})
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: security.Certificate.Certificate[0]})
	key := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private})
	const client = "client/openshell/gateways/isolated/"
	caFile := write(client+"mtls/ca.crt", ca, 0600)
	certFile := write(client+"mtls/tls.crt", cert, 0600)
	keyFile := write(client+"mtls/tls.key", key, 0600)
	metadata, err := json.Marshal(map[string]string{"name": "isolated", "gateway_endpoint": gateway.Sandbox.Gateway, "auth_mode": "mtls"})
	if err != nil {
		t.Fatal(err)
	}
	write(client+"metadata.json", metadata, 0600)
	stub := []byte("#!/bin/sh\nexit 88\n")
	cli := write("gateway-stub", stub, 0700)
	write("payload/codex", stub, 0700)
	write("payload/noodle-progress", stub, 0700)
	spec := write("spec.json", []byte(`{"template":{"image":"fixture@sha256:isolated"},"providers":["noodle-codex"],"command":["sleep","infinity"]}`), 0600)
	config := write("gateway.toml", []byte(fmt.Sprintf(`[openshell.gateway]
compute_driver = "vm"
[openshell.drivers.vm]
state_dir=%q
mem_mib=4
vcpus=2
overlay_disk_mib=4
[noodle_worker_resources]
schema=1
memory_peak_mib=6
disk_peak_mib=10
checkout_peak_mib=1
dependency_peak_mib=0
return_tar_peak_mib=1
restore_peak_mib=1
retention_peak_mib=3
memory_budget_mib=64
cpu_budget=8
disk_safety_mib=1
max_swap_used_mib=0
max_swapout_bytes_per_second=0
recovery_samples=2
[noodle_preparation_resources]
schema=1
memory_peak_mib=2
cpus=1
disk_peak_mib=3
[noodle_resident_resources]
schema=1
memory_peak_mib=3
cpus=1
disk_peak_mib=2
`, vmRoot)), 0600)
	var profile openShellProfileFile
	profile.SchemaVersion = 1
	profile.CLI, profile.GatewayName, profile.PayloadDirectory = cli, "isolated", "payload"
	profile.ProgressSupervisor = "/sandbox/payload/noodle-progress"
	profile.Routes = map[string]struct {
		Binary   string `json:"binary"`
		SpecFile string `json:"spec_file"`
	}{"codex": {Binary: "/sandbox/payload/codex", SpecFile: spec}}
	profile.Gateway.Resident = admission.ResidentSpec{Name: "isolated-openshell", Argv: []string{cli}, Directory: root, Sharing: "shared", Transport: "network"}
	profile.Gateway.Binding.StateRoot = custody
	profile.Gateway.Binding.Gateway = gateway.Sandbox.Gateway
	profile.Gateway.Binding.Workspace = gateway.Sandbox.Workspace
	profile.Gateway.Binding.Owner = "noodle"
	profile.Gateway.TLS.CAFile, profile.Gateway.TLS.CertificateFile, profile.Gateway.TLS.KeyFile = caFile, certFile, keyFile
	profile.Gateway.TLS.ServerName = security.ServerName
	profile.Gateway.Version, profile.Gateway.ComputeDriver, profile.Gateway.ReadinessTimeoutSeconds = "0.1.5", "vm", 10
	profile.Gateway.IdentitySource, profile.Gateway.ConfigFiles = config, []string{config}
	profile.Environment = []string{"XDG_CONFIG_HOME=" + filepath.Join(root, "client")}
	writeProfileFixture(t, path, profile)
	loaded, err := LoadOpenShellProfile(path)
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}
