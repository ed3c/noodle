//go:build darwin

package runtime

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	dm "github.com/NVIDIA/OpenShell/sdk/go/proto/datamodelv1"
	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"github.com/poteto/noodle/internal/execution"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/worktree"
	"github.com/poteto/noodle/worktree/archive"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestArchiveRecoveryAfterExitReadsLostDeleteAcknowledgement(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	const name, id, main = "owned-vm", "native-vm", "original-main"
	const contextID, attemptID, sessionID = "original-context", "original-attempt", "original-session"
	task := OpenShellTask{"order", "native-attempt", sessionID}
	session := filepath.Join(root, "sessions", sessionID)
	custodyRoot, captureDir, payload := filepath.Join(root, "custody"), filepath.Join(root, "capture"), filepath.Join(root, "payload")
	vmRoot := filepath.Join(root, "vm")
	disk := filepath.Join(vmRoot, "sandboxes", id)
	for _, dir := range []string{session, custodyRoot, captureDir, payload, disk} {
		if err = os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	ref := func(path string) SessionReadbackFileRef {
		t.Helper()
		value, err := ReadSessionReadbackFile(path, nil)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	write := func(dir, name string, value any) SessionReadbackFileRef {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := maintenanceWrite(path, value); err != nil {
			t.Fatal(err)
		}
		return ref(path)
	}
	config := filepath.Join(root, "gateway.toml")
	if err = os.WriteFile(config, []byte(fmt.Sprintf("[openshell.gateway]\ncompute_driver='vm'\n[openshell.drivers.vm]\nstate_dir=%q\n", vmRoot)), 0600); err != nil {
		t.Fatal(err)
	}
	custodyPath := filepath.Join(custodyRoot, "owner.json")
	var stopped, absent atomic.Bool
	var stopCalls, deleteCalls atomic.Int32
	sandbox := func() *pb.Sandbox {
		phase := pb.SandboxPhase_SANDBOX_PHASE_READY
		if stopped.Load() {
			phase = pb.SandboxPhase_SANDBOX_PHASE_STOPPED
		}
		vm := custodySandbox(name, id, phase)
		vm.Metadata.ResourceVersion, vm.Status.MainProcessInstanceId = 11, main
		return vm
	}
	server := &openShellNativeServer{
		list: func(context.Context, *pb.ListSandboxesRequest) (*pb.ListSandboxesResponse, error) {
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
			var ledger OpenShellCustodyState
			_, readErr := ReadSessionReadbackFile(custodyPath, &ledger)
			if req.GetName() != name || req.GetExpectedSandboxId() != id || req.GetExpectedResourceVersion() != 11 ||
				req.GetWorkspaceScope().GetWorkspace() != "owned-workspace" || req.GetRequestId() == "" ||
				readErr != nil || len(ledger.Records) != 1 || ledger.Records[0].Retirement == nil ||
				ledger.Records[0].Retirement.RequestID != req.GetRequestId() {
				t.Error("conditional delete lacks the exact original VM or durable intent")
				return nil, status.Error(codes.Aborted, "wrong fixture delete")
			}
			absent.Store(true)
			return nil, status.Error(codes.Unavailable, "delete acknowledged only by later native readback")
		},
	}
	target, tls := openShellTestGateway(t, server)
	binding := OpenShellCustodyBinding{StateRoot: custodyRoot, Gateway: target.Sandbox.Gateway,
		Workspace: target.Sandbox.Workspace, ConfigSource: config}
	profile := OpenShellProfile{Gateway: OpenShellGatewaySpec{Binding: binding, TLS: tls, ComputeDriver: "vm"},
		PayloadDirectory: payload, sourceFiles: map[string]string{config: ref(config).SHA256}}
	profile.PayloadSHA256, err = openShellPayloadHash(payload)
	if err != nil {
		t.Fatal(err)
	}
	profileDigest, err := OpenShellDispatchSHA256(profile)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := openShellBytes(sandbox().GetSpec())
	if err != nil {
		t.Fatal(err)
	}
	create := &pb.CreateSandboxRequest{Name: name, RequestId: "original-create", Spec: sandbox().GetSpec(),
		WorkspaceScope: &dm.WorkspaceSelector{Selection: &dm.WorkspaceSelector_Workspace{Workspace: binding.Workspace}}}
	intent, err := openShellBytes(create)
	if err != nil {
		t.Fatal(err)
	}
	execution := OpenShellExecResult{RequestID: "original-exec", StartAttempted: true, ExitObserved: true, ExitCode: 74}
	record := OpenShellCustodyRecord{Task: task, Name: name, Spec: spec, CreateAcknowledged: true, MainProcessInstanceID: main,
		CreateRequestID: create.GetRequestId(), CreateIntent: intent,
		Worker: OpenShellWorkerRecord{Identity: OpenShellIdentity{OrderID: task.OrderID, AttemptID: task.AttemptID, SessionID: sessionID,
			Sandbox: target.Sandbox, SandboxName: name, ConfigSource: config}, ExecResult: execution},
		Exec: &OpenShellCustodyExec{AttemptID: task.AttemptID, Command: OpenShellCommand{RequestID: execution.RequestID}, Result: execution}}
	state := OpenShellCustodyState{Version: openShellCustodyVersion, Binding: binding, Records: []OpenShellCustodyRecord{record}}
	custody := write(custodyRoot, "owner.json", state)

	checkout, tree := archiveRecoveryTestCheckout(t, root)
	dispatch := write(session, "openshell-dispatch.json", openShellDispatchReceipt{Version: 1, ProfileSHA256: profileDigest,
		ContextID: contextID, OrderID: task.OrderID, AttemptID: task.AttemptID, SessionID: sessionID, Target: checkout.Identity.Path,
		Worker: &record.Worker, Phase: "progress-unknown", Error: "original archive failed", Outcome: SessionOutcome{Status: StatusFailed}})
	request := write(root, "request.json", "original request")
	workspace := filepath.Join(captureDir, "workspace.tar")
	if err = os.WriteFile(workspace, progressFixtureArchive(t, "noodle-result.tar"), 0600); err != nil {
		t.Fatal(err)
	}
	capture := write(captureDir, "workspace-capture.json", OpenShellWorkspaceCapture{Schema: 1, Status: "workspace_archive_verified",
		Scope: "repository", OriginalReturn: "UNKNOWN", RestorationVerified: true, OrderID: task.OrderID, NativeAttemptID: task.AttemptID,
		SessionID: sessionID, SandboxID: id, MainProcessInstanceID: main, BaselineHead: checkout.Identity.Head,
		Archive: ref(workspace), BaselineSource: dispatch,
		Sources: map[string]SessionReadbackFileRef{"request": request, "dispatch": dispatch, "custody": custody,
			"profile": ref(config), "baseline": dispatch, "before": write(captureDir, "before.json", "captured before"),
			"after": write(captureDir, "after.json", "captured after"), "workspace_manifest": write(captureDir, "workspace-manifest.json", "restored")}})
	review := write(root, "review.json", "original review")
	hold := ArchiveRecoveryDispatchHold{PauseCommandID: "original-pause", Acknowledgements: write(root, "hold.json", "original pause")}
	conn, err := newOpenShellConnection(binding.Gateway, tls)
	if err != nil {
		t.Fatal(err)
	}
	stop, stopErr := stopOpenShellArchiveRecovery(t.Context(), pb.NewOpenShellClient(conn), storageFailureInputs{state: state, record: record},
		ProgressMaintenance{SessionDirectory: session, Request: request}, review, capture, hold,
		filepath.Join(session, "archive-recovery-stop"), func() error { return nil })
	if err = conn.Close(); err != nil || stopErr != nil {
		t.Fatal(err, stopErr)
	}
	applied := archiveRecoveryApplyRecord{archiveRecoveryApplyIntent: archiveRecoveryApplyIntent{Schema: 1, OriginalReturn: "UNKNOWN",
		Request: request, Capture: capture, Review: review, Stop: stop, OrderID: task.OrderID, AttemptID: attemptID, NativeAttemptID: task.AttemptID,
		SessionID: sessionID, MainProcessInstanceID: main, SandboxID: id, BaselineHead: checkout.Identity.Head, CandidateTree: tree}}
	applyIntent := write(session, "archive-recovery-apply-intent.json", applied)
	candidate := write(session, "archive-recovery-candidate.json", archiveRecoveryCandidate{archiveRecoveryApplyRecord: applied,
		Kind: "archive_recovery_candidate", CandidateHead: checkout.Identity.Head, Intent: applyIntent})
	disposition := write(session, "archive-recovery-cleanup-disposition.json", archiveRecoveryRetirementDisposition{
		Schema: 1, Kind: "archive_recovery_cleanup_disposition", OriginalReturn: "UNKNOWN", Candidate: candidate, Request: request,
		LandingProof: write(root, "archive-landing-proof.json", "original candidate landed"), Stop: stop, Hold: hold, Archive: ref(workspace),
		Checkout: checkout.Identity, ContextID: contextID, OrderID: task.OrderID, AttemptID: attemptID, NativeAttemptID: task.AttemptID,
		SessionID: sessionID, SandboxID: id, MainProcessInstanceID: main, Head: checkout.Identity.Head, Tree: tree})

	retire := func(lock *lockfile.Lock) (ArchiveRecoveryVMRetirementReadback, error) {
		return RetireOpenShellArchiveRecoveryAfterExit(t.Context(), root, contextID, profile, disposition, ref(custodyPath), checkout, lock)
	}
	lockPath := filepath.Join(custodyRoot, "owner.lock")
	shared, err := lockfile.TryLockShared(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = retire(shared); err == nil || ref(custodyPath) != custody || deleteCalls.Load() != 0 {
		t.Fatalf("shared custody lock authorized retirement: %v", err)
	}
	if err = shared.Close(); err != nil {
		t.Fatal(err)
	}
	owner, err := lockfile.TryLockExisting(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if err = ObserveOpenShellArchiveRecoveryAfterExit(t.Context(), root, contextID, profile, disposition, custody, &checkout); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		result, err := retire(owner)
		if err == nil || result.Retirement.State == CleanupSessionRetired || deleteCalls.Load() != 1 {
			t.Fatalf("held VM disk or unacknowledged delete was replayed or accepted: %+v %v", result, err)
		}
	}
	if err = os.Remove(disk); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		result, err := retire(owner)
		if err != nil || result.OriginalReturn != "UNKNOWN" || result.Retirement.State != CleanupSessionRetired ||
			result.Retirement.NativeDeletionOutcome != "unknown" || deleteCalls.Load() != 1 || stopCalls.Load() != 1 {
			t.Fatalf("same VM retirement was not read back exactly once: %+v %v", result, err)
		}
	}
	if ref(dispatch.Path) != dispatch || ref(stop.Path) != stop {
		t.Fatal("post-exit retirement changed the original attempt or Stop receipt")
	}
}

func archiveRecoveryTestCheckout(t *testing.T, root string) (archive.CheckoutReclamationEvidence, string) {
	t.Helper()
	app, identity, tree, selection := prepareArchiveRecoveryTestCheckout(t, root, filepath.Join(root, "repo"))
	const operation = "archive-recovery-checkout/original"
	if err := app.ReclaimArchiveCheckout(t.Context(), filepath.Base(identity.Path), operation, worktree.CheckoutCandidate{Head: identity.Head,
		Tree: tree, Branch: identity.Ref}, selection, identity); err != nil {
		t.Fatal(err)
	}
	packet, err := archive.ReadCheckoutPacket(selection, operation)
	if err != nil {
		t.Fatal(err)
	}
	original := packet.Tree.Entries["result.txt"]
	restored := packet.VerificationCopy.Entries[filepath.Join("checkout", "result.txt")]
	if original.Hash == "" || original.Hash != restored.Hash {
		t.Fatal("signed checkout packet cannot verify restored result bytes")
	}
	evidence, err := archive.ReadCheckoutReclamation(packet)
	if err != nil {
		t.Fatal(err)
	}
	return evidence, tree
}

func prepareArchiveRecoveryTestCheckout(t *testing.T, root, repo string) (*worktree.App, archive.CheckoutIdentity, string, archive.FileRef) {
	t.Helper()
	base := filepath.Join(repo, ".worktrees")
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	environ := slices.DeleteFunc(os.Environ(), func(entry string) bool { return strings.HasPrefix(entry, "GIT_") })
	environ = append(environ, "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	fixture, err := execution.New(repo, environ)
	if err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) string {
		t.Helper()
		cmd := fixture.Command(t.Context(), repo, "git", args...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %q: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "--quiet", "--template=", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(repo, "result.txt"), []byte("original archived result\n"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", "result.txt")
	git("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-m", "fixture")
	const name = "retained-attempt"
	checkout := filepath.Join(base, name)
	git("worktree", "add", "--quiet", "-b", name, checkout)
	registration := git("-C", checkout, "rev-parse", "--absolute-git-dir")
	if err := maintenanceWrite(filepath.Join(registration, "noodle-incarnation.json"), strings.Repeat("a", 64)); err != nil {
		t.Fatal(err)
	}
	app := &worktree.App{Root: repo, Quiet: true, Execution: fixture}
	identity, err := app.ReadCheckoutIdentity(checkout)
	if err != nil {
		t.Fatal(err)
	}
	codec, err := exec.LookPath("zstd")
	if err != nil {
		t.Fatal(err)
	}
	policy := archive.Config{Roots: []string{base}, ArchiveRoot: filepath.Join(root, "archive"),
		BlobRoot: filepath.Join(root, "blobs"), StateDir: filepath.Join(root, "archive-owner"), Codec: codec, MaxItems: 1, ItemTimeoutSeconds: 30}
	for _, path := range []string{policy.ArchiveRoot, policy.BlobRoot, policy.StateDir} {
		if err = os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.WriteFile(filepath.Join(policy.StateDir, "owner.key"), []byte(strings.Repeat("k", 32)), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "policy.json")
	if err = maintenanceWrite(path, policy); err != nil {
		t.Fatal(err)
	}
	digest, err := archive.FileHash(path)
	if err != nil {
		t.Fatal(err)
	}
	selection := archive.FileRef{Path: path, SHA256: digest}
	tree := git("-C", checkout, "rev-parse", "HEAD^{tree}")
	return app, identity, tree, selection
}
