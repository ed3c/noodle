package runtime

import (
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	dm "github.com/NVIDIA/OpenShell/sdk/go/proto/datamodelv1"
	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"github.com/poteto/noodle/internal/procx"
	"github.com/poteto/noodle/internal/workspaceartifacts"
	"google.golang.org/protobuf/proto"
)

type storageFailureFixture struct {
	plan     StorageFailureStopPlan
	ref      SessionReadbackFileRef
	profile  OpenShellProfile
	state    OpenShellCustodyState
	dispatch openShellDispatchReceipt
	gateway  *custodyGateway
	server   *openShellNativeServer
}

func storageFailureWrite(t *testing.T, name string, value any) SessionReadbackFileRef {
	t.Helper()
	data, err := json.Marshal(value)
	if raw, ok := value.([]byte); ok {
		data, err = raw, nil
	}
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, data, 0600); err != nil {
		t.Fatal(err)
	}
	ref, err := ReadSessionReadbackFile(name, nil)
	if err != nil {
		t.Fatal(err)
	}
	return ref
}

func newStorageFailureFixture(t *testing.T, history int) *storageFailureFixture {
	t.Helper()
	filename, config := profileFixture(t)
	root, err := filepath.EvalSymlinks(filepath.Dir(filename))
	if err != nil {
		t.Fatal(err)
	}
	filename = filepath.Join(root, "profile.json")
	g, server := custodyServer(t)
	target, security := openShellTestGateway(t, server)
	config.Gateway.Binding.Gateway = target.Sandbox.Gateway
	config.Gateway.Binding.Workspace = "owned-workspace"
	writeProfileFixture(t, filename, config)
	key, err := x509.MarshalPKCS8PrivateKey(security.Certificate.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: security.Certificate.Certificate[0]})
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: security.Certificate.Certificate[1]})
	private := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})
	client := filepath.Join(root, "client", "openshell", "gateways", "dedicated")
	for name, data := range map[string][]byte{filepath.Join(root, "ca.pem"): ca, filepath.Join(root, "client.pem"): cert, filepath.Join(root, "client.key"): private, filepath.Join(client, "mtls", "ca.crt"): ca, filepath.Join(client, "mtls", "tls.crt"): cert, filepath.Join(client, "mtls", "tls.key"): private} {
		storageFailureWrite(t, name, data)
	}
	storageFailureWrite(t, filepath.Join(client, "metadata.json"), map[string]string{"name": "dedicated", "gateway_endpoint": target.Sandbox.Gateway, "auth_mode": "mtls"})
	profile, err := LoadOpenShellProfile(filename)
	if err != nil {
		t.Fatal(err)
	}
	project := filepath.Join(root, "project")
	directory := filepath.Join(project, ".noodle", "sessions", "storage-session")
	if err := os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	command := openShellTestCommand()
	command.Argv = []string{profile.Routes["codex"].Binary}
	command.Workdir = "/sandbox/repo"
	command.Env = map[string]string{"NOODLE_SESSION_DIR": "/sandbox/noodle-sessions/storage-session"}
	command, err = wrapOpenShellProgress(command, profile.ProgressSupervisor, "storage-session", strings.Repeat("a", 64))
	if err != nil {
		t.Fatal(err)
	}
	control, err := progressStopCommand(command, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	if err != nil {
		t.Fatal(err)
	}
	create := &pb.CreateSandboxRequest{Name: "original", RequestId: "cccccccc-cccc-4ccc-8ccc-cccccccccccc", WorkspaceScope: &dm.WorkspaceSelector{Selection: &dm.WorkspaceSelector_Workspace{Workspace: "owned-workspace"}}, Spec: profile.Routes["codex"].Spec}
	intent, err := openShellBytes(create)
	if err != nil {
		t.Fatal(err)
	}
	spec, err := openShellBytes(create.Spec)
	if err != nil {
		t.Fatal(err)
	}
	worker := OpenShellWorkerRecord{Identity: OpenShellIdentity{OrderID: "order", AttemptID: "attempt", SessionID: "storage-session", Sandbox: OpenShellSandboxRef{Gateway: profile.Gateway.Binding.Gateway, Workspace: "owned-workspace", SandboxID: "original-id"}, SandboxName: "original", ConfigSource: profile.Gateway.Binding.ConfigSource}, Boundary: OpenShellBoundaryReady}
	record := OpenShellCustodyRecord{Task: OpenShellTask{"order", "attempt", "storage-session"}, Name: "original", CreateRequestID: create.RequestId, CreateIntent: intent, CreateAcknowledged: true, Spec: spec, MainProcessInstanceID: "original-main", Worker: worker, Exec: &OpenShellCustodyExec{AttemptID: "attempt", Command: command, Result: OpenShellExecResult{RequestID: command.RequestID, StartAttempted: true, Reason: "exec acknowledgement unresolved; replay prohibited"}}, ProgressStop: &OpenShellCustodyExec{AttemptID: "attempt/progress-stop", Command: control, Result: OpenShellExecResult{RequestID: control.RequestID, StartAttempted: true, Reason: "exec acknowledgement unresolved; replay prohibited"}}}
	state := OpenShellCustodyState{Version: 3, Binding: profile.Gateway.Binding, Records: []OpenShellCustodyRecord{record}}
	for i := range history {
		r := OpenShellCustodyRecord{Name: fmt.Sprintf("retained-%03d", i), Retained: true, Spec: spec, MainProcessInstanceID: fmt.Sprintf("main-%03d", i), Stopped: &OpenShellStoppedObservation{}}
		r.Worker.Identity = OpenShellIdentity{Sandbox: OpenShellSandboxRef{Gateway: profile.Gateway.Binding.Gateway, Workspace: "owned-workspace", SandboxID: fmt.Sprintf("retained-id-%03d", i)}, SandboxName: r.Name, ConfigSource: profile.Gateway.Binding.ConfigSource}
		state.Records = append(state.Records, r)
	}
	for _, r := range state.Records {
		phase := pb.SandboxPhase_SANDBOX_PHASE_STOPPED
		if r.Name == "original" {
			phase = pb.SandboxPhase_SANDBOX_PHASE_READY
		}
		sandbox := custodySandbox(r.Name, r.Worker.Identity.Sandbox.SandboxID, phase)
		sandbox.Status.MainProcessInstanceId = r.MainProcessInstanceID
		sandbox.Spec = proto.Clone(create.Spec).(*pb.SandboxSpec)
		g.sandboxes[r.Name] = sandbox
	}
	digest, err := OpenShellDispatchSHA256(profile)
	if err != nil {
		t.Fatal(err)
	}
	dispatch := openShellDispatchReceipt{Version: 1, ProfileSHA256: digest, ContextID: "context", OrderID: "order", AttemptID: "attempt", SessionID: "storage-session", Phase: "executing", Baseline: &workspaceartifacts.Baseline{}, Worker: &worker}
	p := StorageFailureStopPlan{Schema: 1, ContextID: "context", HostInstance: "original-host", HostPID: 12345, SessionDirectory: directory}
	p.Profile, err = ReadSessionReadbackFile(filename, nil)
	if err != nil {
		t.Fatal(err)
	}
	p.Dispatch = storageFailureWrite(t, filepath.Join(directory, "openshell-dispatch.json"), dispatch)
	p.Custody = storageFailureWrite(t, filepath.Join(profile.Gateway.Binding.StateRoot, "owner.json"), state)
	p.Failure = storageFailureWrite(t, filepath.Join(directory, "progress-stop-output.log"), []byte("progress helper could not be executed\n"))
	p.NativeImplementation = storageFailureWrite(t, filepath.Join(root, "reviewed-native-source.md"), []byte("Reviewed pinned native Stop preserves the backing disk; not a live disk attestation.\n"))
	p.Request = storageFailureWrite(t, filepath.Join(root, "request.json"), map[string]any{"schema": 1, "operation": "attach", "request_id": "original-request", "required_host_instance_id": p.HostInstance, "project_dir": project, "config_path": filepath.Join(root, "config.toml"), "config_sha256": "config-digest", "binary_sha256": "binary-digest", "status": "accepted"})
	p.Spawn = storageFailureWrite(t, filepath.Join(directory, "spawn.json"), map[string]any{"session_id": "storage-session", "order_id": "order", "provider": "codex", "model": "fixture", "runtime": "openshell", "worktree_path": dispatch.Target})
	f := &storageFailureFixture{plan: p, profile: profile, state: state, dispatch: dispatch, gateway: g, server: server}
	f.freeze(t)
	return f
}

func (f *storageFailureFixture) freeze(t *testing.T) {
	t.Helper()
	f.plan.Custody = storageFailureWrite(t, f.plan.Custody.Path, f.state)
	f.plan.Dispatch = storageFailureWrite(t, f.plan.Dispatch.Path, f.dispatch)
	f.plan.Originals = map[string]SessionReadbackFileRef{}
	before := t.TempDir()
	for key, ref := range f.plan.sources() {
		data, err := os.ReadFile(ref.Path)
		if err != nil {
			t.Fatal(err)
		}
		f.plan.Originals[key] = storageFailureWrite(t, filepath.Join(before, "original-"+key), data)
	}
	f.ref = storageFailureWrite(t, filepath.Join(t.TempDir(), "plan.json"), f.plan)
}

func coldStopFixture(t *testing.T, f *storageFailureFixture) SessionReadbackFileRef {
	t.Helper()
	conn, err := newOpenShellConnection(f.profile.Gateway.Binding.Gateway, f.profile.Gateway.TLS)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	client := pb.NewOpenShellClient(conn)
	in, err := readStorageFailureInputs(f.plan, f.profile, false)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Dir(f.ref.Path)
	before, err := observeStorageFailureInventory(t.Context(), client, in, directory, "before", pb.SandboxPhase_SANDBOX_PHASE_READY)
	if err != nil {
		t.Fatal(err)
	}
	before.Plan = f.ref
	beforeRef, err := writeStorageFailureInventory(directory, "before", before)
	if err != nil {
		t.Fatal(err)
	}
	f.gateway.sandboxes["original"].Status.Phase = pb.SandboxPhase_SANDBOX_PHASE_STOPPED
	sandbox := proto.Clone(f.gateway.sandboxes["original"]).(*pb.Sandbox)
	ack := filepath.Join(directory, "storage-failure-native-stop-ack.json")
	stopped := filepath.Join(directory, "storage-failure-native-stopped.json")
	for _, path := range []string{ack, stopped} {
		if err := maintenanceWriteNative(path, &pb.SandboxResponse{Sandbox: sandbox}); err != nil {
			t.Fatal(err)
		}
	}
	ackRef, _ := ReadSessionReadbackFile(ack, nil)
	stoppedRef, _ := ReadSessionReadbackFile(stopped, nil)
	after, err := observeStorageFailureInventory(t.Context(), client, in, directory, "after", pb.SandboxPhase_SANDBOX_PHASE_STOPPED)
	if err != nil {
		t.Fatal(err)
	}
	after.Plan = f.ref
	afterRef, err := writeStorageFailureInventory(directory, "after", after)
	if err != nil {
		t.Fatal(err)
	}
	requestID := "dddddddd-dddd-4ddd-8ddd-dddddddddddd"
	intent := ProgressMaintenanceStop{Plan: f.ref, RequestID: requestID}
	receipt := StorageFailureStopReceipt{Schema: 1, Plan: f.ref, RequestID: requestID, Acknowledgement: ackRef, Stopped: stoppedRef, BeforeInventory: beforeRef, AfterInventory: afterRef, BeforeImageState: in.before, ComputeStopped: true, WorkspaceReturnAbandoned: true, TaskOutcome: "failed", UnresolvedOriginalExecutions: true, OriginalExecutions: in.unresolved}
	receipt.OperationIntent = storageFailureWrite(t, filepath.Join(directory, "storage-failure-stop-intent.json"), intent)
	receipt.SessionIntent = storageFailureWrite(t, filepath.Join(f.plan.SessionDirectory, "storage-failure-stop-intent.json"), intent)
	return storageFailureWrite(t, filepath.Join(directory, "storage-failure-stop-complete.json"), receipt)
}

func coldReleaseFixture(t *testing.T) (*storageFailureFixture, OpenShellHostRelease, SessionReadbackFileRef) {
	t.Helper()
	f := newStorageFailureFixture(t, 2)
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	source, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	native := procx.NativeIdentity{PID: 2147483601, UID: os.Getuid(), Group: 2147483601, StartSec: 1, Version: 1, Token: []byte{1}}
	gateway := native
	gateway.PID++
	gateway.ParentPID = native.PID
	// Fixtures establish receipt semantics only; no process controls are sent.
	actors := []procx.NativeIdentity{native, gateway}
	plan := map[string]any{"host_dir": source, "process_generation": native, "native_source": map[string]any{"host_instance_id": f.plan.HostInstance}, "handover": map[string]any{"SharedRoot": root, "Cold": map[string]any{"StopPlan": f.ref, "Profile": f.plan.Profile, "Actors": actors}}}
	planRef := storageFailureWrite(t, filepath.Join(source, "plan.json"), plan)
	resources := storageFailureWrite(t, filepath.Join(source, "old-resources.json"), map[string]any{"host_instance_id": f.plan.HostInstance, "host_pid": native.PID, "contexts": 3, "resources": map[string]int{"active_workers": 1, "active_residents": 1, "active_allocations": 2, "unresolved_residents": 0}, "residents": map[string]any{"error": "retained storage failure"}})
	// Bind the historical Stop plan to the same source identity.
	f.plan.HostPID = native.PID
	f.freeze(t)
	coldStopFixture(t, f)
	inventory, err := ObserveStorageFailureStopped(t.Context(), f.ref, f.profile)
	if err != nil {
		t.Fatal(err)
	}
	plan["handover"].(map[string]any)["Cold"].(map[string]any)["StopPlan"] = f.ref
	planRef = storageFailureWrite(t, planRef.Path, plan)
	intent := time.Now().UTC().Add(time.Second)
	retirement := OpenShellColdRetirement{Schema: 1, Plan: planRef, Custody: f.plan.Originals["custody"], Resources: resources, BeforeInventory: inventory, FinalInventory: inventory, Actors: []OpenShellRetiredActor{{Process: native, IntentAt: intent, ExitedAt: intent.Add(time.Second)}, {Process: gateway, IntentAt: intent.Add(3 * time.Second), ExitedAt: intent.Add(4 * time.Second)}}}
	// A post-Host inventory brackets the gateway control, while before inventory
	// remains the original observation before any signal.
	var final StorageFailureInventory
	if err := maintenanceVerify(inventory, &final); err != nil {
		t.Fatal(err)
	}
	final.ObservedAt = intent.Add(2 * time.Second)
	retirement.FinalInventory = storageFailureWrite(t, filepath.Join(source, "final.json"), final)
	retired := storageFailureWrite(t, filepath.Join(source, "retirement.json"), retirement)
	release := OpenShellHostRelease{Schema: 3, SourceRoot: source, SourceInstance: f.plan.HostInstance, ControlRoot: root, Process: native, Plan: planRef, EmptyResources: resources, Cold: &OpenShellColdRelease{StopPlan: f.ref, Profile: f.plan.Profile, Inventory: retirement.FinalInventory, Retirement: retired}}
	return f, release, storageFailureWrite(t, filepath.Join(source, "release.json"), release)
}

func TestColdReleasePreservesFailedTaskAndRejectsUnboundProof(t *testing.T) {
	if !procx.NativeFixtureControlAvailable() {
		t.Skip("cold retirement requires platform generation-bound control")
	}
	f, release, ref := coldReleaseFixture(t)
	original, err := os.ReadFile(f.plan.Custody.Path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = verifyOpenShellHostRelease(ref); err != nil {
		t.Fatal(err)
	}
	proof, err := UpgradeOpenShellCustodyV3(f.state.Binding, release.ControlRoot, f.plan.Originals["custody"], ref, filepath.Join(release.SourceRoot, "handover.json"))
	if err != nil {
		t.Fatal(err)
	}
	var current OpenShellCustodyState
	if _, err = ReadSessionReadbackFile(f.plan.Custody.Path, &current); err != nil {
		t.Fatal(err)
	}
	if current.Version != 4 || current.Handover == nil || *current.Handover != proof || current.Records[0].Exec.Result.ExitObserved || current.Records[0].Stopped != nil {
		t.Fatal("resource transfer rewrote task outcome")
	}
	current.Version, current.ControlRoot, current.Handover = 3, "", nil
	bytes, _ := json.Marshal(current)
	if string(bytes) != string(original) {
		t.Fatal("historical ledger changed")
	}
	c, err := OpenOpenShellCustody(t.Context(), f.state.Binding, f.profile.Gateway.TLS, nil, release.ControlRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err = c.Create(t.Context(), OpenShellTask{"order", "new-attempt", "new-session"}, f.profile.Routes["codex"].Spec); err == nil {
		t.Fatal("cold resource settlement authorized old task retry")
	}
	if err = c.Quiesce(t.Context()); err != nil {
		t.Fatal("qualified stopped compute still blocked resource release", err)
	}
	release.Cold.Inventory = release.Plan
	invalid := storageFailureWrite(t, filepath.Join(release.SourceRoot, "invalid.json"), release)
	if _, err = verifyOpenShellHostRelease(invalid); err == nil {
		t.Fatal("unbound native inventory accepted")
	}
}
