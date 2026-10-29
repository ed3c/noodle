package runtime

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	dm "github.com/NVIDIA/OpenShell/sdk/go/proto/datamodelv1"
	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/workspaceartifacts"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
	state := OpenShellCustodyState{Version: openShellCustodyVersion, Binding: profile.Gateway.Binding, Records: []OpenShellCustodyRecord{record}}
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

func TestStorageFailureStopRetainsOriginalDebtAndAllSources(t *testing.T) {
	for _, persisted := range []bool{false, true} {
		t.Run(fmt.Sprintf("progress-persisted=%v", persisted), func(t *testing.T) {
			f := newStorageFailureFixture(t, 31)
			if persisted {
				f.dispatch.Phase, f.dispatch.Error = "progress-unknown", "helper failed"
				f.state.Records[0].ProgressStop.Result.ExitObserved = true
				f.state.Records[0].ProgressStop.Result.ExitCode = 126
				f.freeze(t)
			}
			owner, err := lockfile.TryLock(filepath.Join(f.profile.Gateway.Binding.StateRoot, "owner.lock"))
			if err != nil {
				t.Fatal(err)
			}
			defer owner.Close()
			ref, err := StopStorageFailure(t.Context(), f.ref, f.profile)
			if err != nil {
				t.Fatal(err)
			}
			var receipt StorageFailureStopReceipt
			if err := maintenanceVerify(ref, &receipt); err != nil {
				t.Fatal(err)
			}
			wantDebt := 2
			if persisted {
				wantDebt = 1
			}
			if !receipt.ComputeStopped || !receipt.WorkspaceReturnAbandoned || !receipt.UnresolvedOriginalExecutions || receipt.TaskOutcome != "failed" || len(receipt.OriginalExecutions) != wantDebt {
				t.Fatal("compute stop erased original execution debt or changed the failed outcome")
			}
			for _, source := range f.plan.sources() {
				if err := maintenanceVerify(source, nil); err != nil {
					t.Fatal("stop modified an original source:", err)
				}
			}
			if observed, err := ValidateStorageFailureStopReceipt(f.ref, f.profile); err != nil || observed != ref {
				t.Fatal("saved stop proof did not verify", err)
			}
			fresh, err := ObserveStorageFailureStopped(t.Context(), f.ref, f.profile)
			if err != nil {
				t.Fatal(err)
			}
			var inventory StorageFailureInventory
			if err := maintenanceVerify(fresh, &inventory); err != nil || len(inventory.Gets) != 32 || len(inventory.Lists) != 32 || len(inventory.Sandboxes) != 32 {
				t.Fatal("fresh inventory omitted original records or pages", err)
			}
			if _, err := StopStorageFailure(t.Context(), f.ref, f.profile); err == nil {
				t.Fatal("same plan replayed Stop")
			}
			alternate := storageFailureWrite(t, filepath.Join(t.TempDir(), "plan.json"), f.plan)
			if _, err := StopStorageFailure(t.Context(), alternate, f.profile); err == nil {
				t.Fatal("another plan replayed the original session's Stop")
			}
			creates, stops, execs := f.gateway.counts()
			if creates != 0 || stops != 1 || execs != 0 {
				t.Fatalf("unexpected native effects: create=%d stop=%d exec=%d", creates, stops, execs)
			}
		})
	}
}

func TestStorageFailureStopAllowsOriginalOwnerUpdatesAfterEffect(t *testing.T) {
	f := newStorageFailureFixture(t, 1)
	f.gateway.afterStop = func() {
		storageFailureWrite(t, f.plan.Custody.Path, []byte("original owner legitimately observed its exec exit"))
		storageFailureWrite(t, f.plan.Dispatch.Path, []byte("original owner updated its dispatch"))
	}
	ref, err := StopStorageFailure(t.Context(), f.ref, f.profile)
	if err != nil {
		t.Fatal("post-effect live source changes discarded the native ACK:", err)
	}
	if got, err := ValidateStorageFailureStopReceipt(f.ref, f.profile); err != nil || got != ref {
		t.Fatal("immutable before-image proof rejected legitimate live updates:", err)
	}
	if _, err := ObserveStorageFailureInventory(t.Context(), f.ref, f.profile); err != nil {
		t.Fatal(err)
	}
	if err := ValidateStorageFailureStop(f.plan, f.profile); err == nil {
		t.Fatal("pre-effect validation ignored live source drift")
	}
}

func TestStorageFailureStopLostACKNeverReplays(t *testing.T) {
	f := newStorageFailureFixture(t, 0)
	f.gateway.stopErr = status.Error(codes.Unavailable, "ACK lost after stop")
	if _, err := StopStorageFailure(t.Context(), f.ref, f.profile); err == nil {
		t.Fatal("lost stop ACK produced completion")
	}
	for range 2 {
		alternate := storageFailureWrite(t, filepath.Join(t.TempDir(), "plan.json"), f.plan)
		if _, err := StopStorageFailure(t.Context(), alternate, f.profile); err == nil {
			t.Fatal("unknown stop was replayed")
		}
	}
	if _, err := ValidateStorageFailureStopReceipt(f.ref, f.profile); err == nil {
		t.Fatal("Stopped substituted for the missing ACK")
	}
	_, stops, _ := f.gateway.counts()
	if stops != 1 {
		t.Fatal("stop repeated after unknown acknowledgement")
	}
}

func TestStorageFailureStopRefusesInvalidBeforeImages(t *testing.T) {
	tests := []struct {
		name string
		edit func(*testing.T, *storageFailureFixture)
	}{
		{"blocked-custody", func(_ *testing.T, f *storageFailureFixture) { f.state.Blocked = "original failure" }},
		{"other-active", func(_ *testing.T, f *storageFailureFixture) { f.state.Records[1].Stopped = nil }},
		{"other-pending-stop", func(_ *testing.T, f *storageFailureFixture) { f.state.Records[1].Worker.StopPending = true }},
		{"target-stop", func(_ *testing.T, f *storageFailureFixture) { f.state.Records[0].StopRequestID = "prior-stop" }},
		{"wrong-task", func(_ *testing.T, f *storageFailureFixture) { f.state.Records[0].Task.AttemptID = "foreign" }},
		{"wrong-request", func(_ *testing.T, f *storageFailureFixture) { f.state.Records[0].Exec.Result.RequestID = "foreign" }},
		{"wrong-purpose", func(_ *testing.T, f *storageFailureFixture) { f.state.Records[0].ProgressStop.AttemptID = "foreign" }},
		{"missing-progress-stop", func(_ *testing.T, f *storageFailureFixture) { f.state.Records[0].ProgressStop = nil }},
		{"successful-helper", func(_ *testing.T, f *storageFailureFixture) {
			f.state.Records[0].ProgressStop.Result.ExitObserved = true
		}},
		{"wrong-main", func(_ *testing.T, f *storageFailureFixture) { f.state.Records[0].MainProcessInstanceID = "" }},
		{"returned-progress", func(_ *testing.T, f *storageFailureFixture) { f.dispatch.ProgressHead = "published" }},
		{"wrong-phase", func(_ *testing.T, f *storageFailureFixture) { f.dispatch.Phase = "settled" }},
		{"archived-output", func(t *testing.T, f *storageFailureFixture) {
			f.plan.Failure = storageFailureWrite(t, f.plan.Failure.Path, []byte(`{"status":"archived","archives":[]}`))
		}},
		{"empty-output", func(t *testing.T, f *storageFailureFixture) {
			f.plan.Failure = storageFailureWrite(t, f.plan.Failure.Path, []byte(" \n"))
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := newStorageFailureFixture(t, 1)
			test.edit(t, f)
			f.freeze(t)
			if _, err := StopStorageFailure(t.Context(), f.ref, f.profile); err == nil {
				t.Fatal("invalid original sources permitted native Stop")
			}
			_, stops, _ := f.gateway.counts()
			if stops != 0 {
				t.Fatal("refusal followed a native mutation")
			}
		})
	}
}

func TestStorageFailureStopRefusesNativeInventoryDrift(t *testing.T) {
	for _, kind := range []string{"id", "main", "spec", "workspace", "other-ready", "unknown", "missing", "duplicate", "cycle", "get-drift", "source-drift"} {
		t.Run(kind, func(t *testing.T) {
			f := newStorageFailureFixture(t, 1)
			switch kind {
			case "id":
				f.gateway.sandboxes["original"].Metadata.Id = "replacement"
			case "main":
				f.gateway.sandboxes["original"].Status.MainProcessInstanceId = "replacement"
			case "spec":
				f.gateway.sandboxes["original"].Spec.Command = []string{"replacement"}
			case "workspace":
				f.gateway.sandboxes["original"].Metadata.Workspace = "foreign"
			case "other-ready":
				f.gateway.sandboxes["retained-000"].Status.Phase = pb.SandboxPhase_SANDBOX_PHASE_READY
			case "unknown":
				f.gateway.sandboxes["foreign"] = custodySandbox("foreign", "foreign-id", pb.SandboxPhase_SANDBOX_PHASE_STOPPED)
			case "missing":
				delete(f.gateway.sandboxes, "retained-000")
			case "duplicate", "cycle":
				original := f.server.list
				f.server.list = func(ctx context.Context, req *pb.ListSandboxesRequest) (*pb.ListSandboxesResponse, error) {
					response, err := original(ctx, req)
					if kind == "duplicate" {
						response.Sandboxes = append(response.Sandboxes, proto.Clone(response.Sandboxes[0]).(*pb.Sandbox))
					} else {
						response.NextPageToken = "1"
					}
					return response, err
				}
			case "get-drift":
				f.gateway.afterList = func() {
					if f.gateway.lists == 2 {
						f.gateway.sandboxes["original"].Status.MainProcessInstanceId = "changed-after-list"
					}
				}
			case "source-drift":
				f.gateway.afterGet = func() { storageFailureWrite(t, f.plan.Custody.Path, []byte("owner changed before stop")) }
			}
			if _, err := StopStorageFailure(t.Context(), f.ref, f.profile); err == nil {
				t.Fatal("native/source drift permitted Stop")
			}
			_, stops, _ := f.gateway.counts()
			if stops != 0 {
				t.Fatal("drift refusal followed native Stop")
			}
		})
	}
}

func TestStorageFailureStopPreservesACKButRefusesPostStopDrift(t *testing.T) {
	for _, kind := range []string{"ack-main", "after-main", "newborn"} {
		t.Run(kind, func(t *testing.T) {
			f := newStorageFailureFixture(t, 1)
			switch kind {
			case "ack-main":
				f.gateway.afterStop = func() { f.gateway.sandboxes["original"].Status.MainProcessInstanceId = "foreign" }
			case "after-main":
				f.gateway.afterGet = func() {
					if f.gateway.stops != 0 {
						f.gateway.sandboxes["original"].Status.MainProcessInstanceId = "foreign"
					}
				}
			case "newborn":
				f.gateway.afterStop = func() {
					f.gateway.sandboxes["newborn"] = custodySandbox("newborn", "new-id", pb.SandboxPhase_SANDBOX_PHASE_READY)
				}
			}
			if _, err := StopStorageFailure(t.Context(), f.ref, f.profile); err == nil {
				t.Fatal("post-stop drift produced completion")
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(f.ref.Path), "storage-failure-native-stop-ack.json")); err != nil {
				t.Fatal("actual ACK was discarded:", err)
			}
			if _, err := StopStorageFailure(t.Context(), f.ref, f.profile); err == nil {
				t.Fatal("post-stop drift replayed Stop")
			}
		})
	}
}

func TestStorageFailureStopReceiptTamperingAndFreshReadback(t *testing.T) {
	f := newStorageFailureFixture(t, 2)
	ref, err := StopStorageFailure(t.Context(), f.ref, f.profile)
	if err != nil {
		t.Fatal(err)
	}
	var receipt StorageFailureStopReceipt
	if err := maintenanceVerify(ref, &receipt); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(ref.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, edit := range []func(*StorageFailureStopReceipt){func(r *StorageFailureStopReceipt) { r.TaskOutcome = "completed" }, func(r *StorageFailureStopReceipt) { r.OriginalExecutions = nil }, func(r *StorageFailureStopReceipt) { r.RequestID = "foreign" }} {
		tampered := receipt
		edit(&tampered)
		storageFailureWrite(t, ref.Path, tampered)
		if _, err := ValidateStorageFailureStopReceipt(f.ref, f.profile); err == nil {
			t.Fatal("tampered completion verified")
		}
	}
	storageFailureWrite(t, ref.Path, original)
	f.gateway.mutate(func() { f.gateway.sandboxes["retained-000"].Status.Phase = pb.SandboxPhase_SANDBOX_PHASE_READY })
	if _, err := ValidateStorageFailureStopReceipt(f.ref, f.profile); err != nil {
		t.Fatal("historical proof failed without changing its sources:", err)
	}
	if _, err := ObserveStorageFailureInventory(t.Context(), f.ref, f.profile); err == nil {
		t.Fatal("historical proof substituted for fresh full inventory")
	}
	data, err := os.ReadFile(f.plan.Originals["custody"].Path)
	if err != nil {
		t.Fatal(err)
	}
	storageFailureWrite(t, f.plan.Originals["custody"].Path, append(bytes.Clone(data), '\n'))
	if _, err := ValidateStorageFailureStopReceipt(f.ref, f.profile); err == nil {
		t.Fatal("changed immutable before-image verified")
	}
}

func TestStorageFailureStopBindsExactIntentBytes(t *testing.T) {
	f := newStorageFailureFixture(t, 0)
	if _, err := StopStorageFailure(t.Context(), f.ref, f.profile); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(f.plan.SessionDirectory, "storage-failure-stop-intent.json")
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	storageFailureWrite(t, name, append(data, '\n'))
	if _, err := ValidateStorageFailureStopReceipt(f.ref, f.profile); err == nil {
		t.Fatal("changed durable intent bytes verified")
	}
}

func TestStorageFailureInventoryOfflineValidation(t *testing.T) {
	f := newStorageFailureFixture(t, 2)
	if _, err := StopStorageFailure(t.Context(), f.ref, f.profile); err != nil {
		t.Fatal(err)
	}
	inventory, err := ObserveStorageFailureInventory(t.Context(), f.ref, f.profile)
	if err != nil {
		t.Fatal(err)
	}
	f.gateway.mu.Lock()
	lists, gets := f.gateway.lists, f.gateway.gets
	f.gateway.mu.Unlock()
	if err := ValidateStorageFailureInventory(f.ref, f.profile, inventory); err != nil {
		t.Fatal(err)
	}
	for _, edit := range []func(*StorageFailureInventory){
		func(i *StorageFailureInventory) { i.Plan.SHA256 = "foreign" },
		func(i *StorageFailureInventory) { i.Stop.SHA256 = "foreign" },
		func(i *StorageFailureInventory) { i.Lists = i.Lists[1:] },
		func(i *StorageFailureInventory) { i.FinalLists = i.FinalLists[:len(i.FinalLists)-1] },
		func(i *StorageFailureInventory) { i.Gets = i.Gets[1:] },
		func(i *StorageFailureInventory) { i.Lists = []SessionReadbackFileRef{i.Lists[0], i.Lists[0]} },
		func(i *StorageFailureInventory) { i.ObservedAt = time.Time{} },
	} {
		changed := inventory
		edit(&changed)
		if err := ValidateStorageFailureInventory(f.ref, f.profile, changed); err == nil {
			t.Fatal("incomplete, duplicate, or foreign saved inventory verified")
		}
	}
	data, err := os.ReadFile(inventory.Gets[0].Path)
	if err != nil {
		t.Fatal(err)
	}
	storageFailureWrite(t, inventory.Gets[0].Path, append(data, '\n'))
	if err := ValidateStorageFailureInventory(f.ref, f.profile, inventory); err == nil {
		t.Fatal("modified native Get bytes verified")
	}
	f.gateway.mu.Lock()
	defer f.gateway.mu.Unlock()
	if f.gateway.lists != lists || f.gateway.gets != gets {
		t.Fatal("offline verification performed native RPCs")
	}
}

func TestStorageFailureStopRefusesNewbornDuringPreflightGets(t *testing.T) {
	f := newStorageFailureFixture(t, 1)
	f.gateway.afterGet = func() {
		f.gateway.sandboxes["newborn"] = custodySandbox("newborn", "new-id", pb.SandboxPhase_SANDBOX_PHASE_READY)
	}
	if _, err := StopStorageFailure(t.Context(), f.ref, f.profile); err == nil {
		t.Fatal("newborn during preflight produced completion")
	}
	_, stops, _ := f.gateway.counts()
	if stops != 0 {
		t.Fatal("preflight Get failed to fence observed inventory growth before Stop")
	}
}

func TestStorageFailureStopRejectsAliasedSnapshots(t *testing.T) {
	for _, hardlink := range []bool{false, true} {
		t.Run(fmt.Sprintf("hardlink=%v", hardlink), func(t *testing.T) {
			f := newStorageFailureFixture(t, 0)
			ref := f.plan.Custody
			if hardlink {
				name := filepath.Join(t.TempDir(), "linked-custody")
				if err := os.Link(ref.Path, name); err != nil {
					t.Fatal(err)
				}
				ref, _ = ReadSessionReadbackFile(name, nil)
			}
			f.plan.Originals["custody"] = ref
			if err := ValidateStorageFailureStop(f.plan, f.profile); err == nil {
				t.Fatal("mutable snapshot alias accepted")
			}
		})
	}
}

func TestStorageFailureStopDoesNotReplayPendingObservation(t *testing.T) {
	f := newStorageFailureFixture(t, 0)
	f.gateway.stopPhase = pb.SandboxPhase_SANDBOX_PHASE_READY
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if _, err := StopStorageFailure(ctx, f.ref, f.profile); err == nil {
		t.Fatal("Ready after acknowledged Stop produced completion")
	}
	if _, err := ValidateStorageFailureStopReceipt(f.ref, f.profile); err == nil {
		t.Fatal("pending Stopped observation produced a verified receipt")
	}
	var before OpenShellCustodyState
	if err := maintenanceVerify(f.plan.Custody, &before); err != nil || !reflect.DeepEqual(before, f.state) {
		t.Fatal("pending observation rewrote original debt", err)
	}
	if _, err := StopStorageFailure(t.Context(), f.ref, f.profile); err == nil {
		t.Fatal("pending observation retried Stop")
	}
}

func TestStorageFailureSerializesWithOriginalProgressMaintenance(t *testing.T) {
	f := newStorageFailureFixture(t, 0)
	owner, err := lockfile.TryLock(filepath.Join(f.plan.SessionDirectory, "maintenance.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	if _, err = StopStorageFailure(t.Context(), f.ref, f.profile); err == nil {
		t.Fatal("storage failure Stop crossed the original maintenance owner")
	}
	f.gateway.mu.Lock()
	defer f.gateway.mu.Unlock()
	if f.gateway.stops != 0 {
		t.Fatal("busy maintenance owner allowed a native stop")
	}
}
