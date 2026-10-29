package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"time"

	dm "github.com/NVIDIA/OpenShell/sdk/go/proto/datamodelv1"
	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"github.com/poteto/noodle/internal/lockfile"
	"google.golang.org/protobuf/proto"
)

// StorageFailureStopPlan explicitly abandons workspace return, not original
// execution debt. Originals contains independent before-images of all six refs.
// NativeImplementation is opaque reviewed source evidence, not a disk attestation.
type StorageFailureStopPlan struct {
	Schema                                                             int
	Profile, Dispatch, Custody, Failure, NativeImplementation, Request SessionReadbackFileRef
	SessionDirectory, ContextID, HostInstance                          string
	HostPID                                                            int
	Originals                                                          map[string]SessionReadbackFileRef
}

// StorageFailureExecution retains uncertainty without exposing command/env data.
type StorageFailureExecution struct {
	Purpose, AttemptID, RequestID string
}

// StorageFailureStopReceipt proves only this distinct compute stop. It never
// completes the old session, clears old exec ACK debt, or certifies an archive.
type StorageFailureStopReceipt struct {
	Schema                          int
	Plan                            SessionReadbackFileRef
	RequestID                       string
	Acknowledgement, Stopped        SessionReadbackFileRef
	BeforeInventory, AfterInventory SessionReadbackFileRef
	SessionIntent, OperationIntent  SessionReadbackFileRef
	BeforeImageState                string
	ComputeStopped                  bool   `json:"computeStopped"`
	WorkspaceReturnAbandoned        bool   `json:"workspaceReturnAbandoned"`
	TaskOutcome                     string `json:"taskOutcome"`
	UnresolvedOriginalExecutions    bool   `json:"unresolvedOriginalExecutions"`
	OriginalExecutions              []StorageFailureExecution
}

// StorageFailureInventory binds all-workspace List pages and fresh per-record
// Gets. It remains an observation, not an atomic lock or old exec reconciliation.
type StorageFailureInventory struct {
	Schema                  int
	Plan, Stop              SessionReadbackFileRef
	ObservedAt              time.Time
	Sandboxes               []StorageFailureSandbox
	Lists, Gets, FinalLists []SessionReadbackFileRef
}

type StorageFailureSandbox struct {
	Name, SandboxID, MainProcessInstanceID, Phase string
}

type storageFailureInputs struct {
	state      OpenShellCustodyState
	record     OpenShellCustodyRecord
	before     string
	unresolved []StorageFailureExecution
}

func (p StorageFailureStopPlan) sources() map[string]SessionReadbackFileRef {
	return map[string]SessionReadbackFileRef{"profile": p.Profile, "dispatch": p.Dispatch, "custody": p.Custody, "failure": p.Failure, "native_implementation": p.NativeImplementation, "request": p.Request}
}

// ValidateStorageFailureStop checks live sources immediately before an effect.
func ValidateStorageFailureStop(p StorageFailureStopPlan, profile OpenShellProfile) error {
	_, err := readStorageFailureInputs(p, profile, true)
	return err
}

func readStorageFailureInputs(p StorageFailureStopPlan, profile OpenShellProfile, live bool) (storageFailureInputs, error) {
	var in storageFailureInputs
	fail := func(message string) (storageFailureInputs, error) {
		return in, fmt.Errorf("storage failure %s", message)
	}
	if p.Schema != 1 || p.ContextID == "" || p.HostInstance == "" || p.HostPID <= 1 || !filepath.IsAbs(p.SessionDirectory) {
		return fail("owner identity is incomplete")
	}
	dir, err := filepath.EvalSymlinks(p.SessionDirectory)
	if err != nil || dir != p.SessionDirectory {
		return fail("session directory is not canonical")
	}
	sources := p.sources()
	if len(p.Originals) != len(sources) {
		return fail("before-images are incomplete")
	}
	for key, ref := range sources {
		original, ok := p.Originals[key]
		if !ok || !filepath.IsAbs(ref.Path) || !filepath.IsAbs(original.Path) || original.SHA256 == "" || original.SHA256 != ref.SHA256 || original.Path == ref.Path {
			return fail("before-image binding differs")
		}
		if err := maintenanceVerify(original, nil); err != nil {
			return in, err
		}
		if live {
			if err := maintenanceVerify(ref, nil); err != nil {
				return in, err
			}
			a, err := os.Stat(ref.Path)
			if err != nil {
				return in, err
			}
			b, err := os.Stat(original.Path)
			if err != nil || os.SameFile(a, b) {
				return fail("before-image shares the live source")
			}
		}
	}
	if p.Profile.Path != profile.SourcePath || p.Profile.SHA256 != profile.sourceFiles[profile.SourcePath] || p.Dispatch.Path != filepath.Join(dir, "openshell-dispatch.json") || p.Failure.Path != filepath.Join(dir, "progress-stop-output.log") || p.Custody.Path != filepath.Join(profile.Gateway.Binding.StateRoot, "owner.json") {
		return fail("original source paths differ")
	}
	if err := VerifyOpenShellProfile(profile); err != nil {
		return in, err
	}
	read := func(key string, value any) error {
		data, err := os.ReadFile(p.Originals[key].Path)
		if err != nil {
			return err
		}
		if err := json.Unmarshal(data, value); err != nil {
			return fmt.Errorf("storage failure %s before-image encoding invalid", key)
		}
		return nil
	}
	var d openShellDispatchReceipt
	if err := read("dispatch", &d); err != nil {
		return in, err
	}
	digest, err := OpenShellDispatchSHA256(profile)
	if err != nil {
		return in, err
	}
	if d.Version != 1 || d.ProfileSHA256 != digest || d.ContextID != p.ContextID || d.OrderID == "" || d.AttemptID == "" || d.SessionID != filepath.Base(dir) || d.Baseline == nil || d.Worker == nil || d.Progress != nil || d.ProgressSource != nil || d.ProgressHead != "" || d.CandidateHead != "" || d.Delivery != nil || d.Maintenance != nil {
		return fail("dispatch is not an unreturned original attempt")
	}
	switch {
	case d.Phase == "progress-unknown" && d.Error != "":
		in.before = "progress-unknown"
	case d.Phase == "executing" && d.Error == "":
		in.before = "retained_persistence_failed"
	default:
		return fail("dispatch failure boundary differs")
	}
	var request struct {
		Schema                 int    `json:"schema"`
		RequestID              string `json:"request_id"`
		RequiredHostInstanceID string `json:"required_host_instance_id"`
		Operation              string `json:"operation"`
		ProjectDir             string `json:"project_dir"`
		ConfigPath             string `json:"config_path"`
		ConfigSHA256           string `json:"config_sha256"`
		BinarySHA256           string `json:"binary_sha256"`
		Status                 string `json:"status"`
	}
	if err := read("request", &request); err != nil {
		return in, err
	}
	if request.Schema != 1 || request.RequestID == "" || request.RequiredHostInstanceID != p.HostInstance || (request.Operation != "attach" && request.Operation != "once") || request.Status != "accepted" || !filepath.IsAbs(request.ProjectDir) || !filepath.IsAbs(request.ConfigPath) || request.ConfigSHA256 == "" || request.BinarySHA256 == "" || dir != filepath.Join(request.ProjectDir, ".noodle", "sessions", d.SessionID) {
		return fail("original request binding differs")
	}
	if err := read("custody", &in.state); err != nil {
		return in, err
	}
	if in.state.Version != openShellCustodyVersion || in.state.Binding != profile.Gateway.Binding || in.state.Blocked != "" || len(in.state.PendingRetained) != 0 {
		return fail("original custody is blocked or differs")
	}
	if err := (&OpenShellCustody{state: in.state}).validateLedger(); err != nil {
		return fail("original custody ledger is invalid")
	}
	matches := 0
	for _, record := range in.state.Records {
		if record.Worker.Identity == d.Worker.Identity {
			in.record = record
			matches++
			continue
		}
		if record.Stopped == nil || (!record.Retained && !record.CreateAcknowledged) || record.Worker.StopPending || (record.StopRequestID != "" && !record.StopAcknowledged) || (record.Exec != nil && !record.Exec.Result.ExitObserved) || (record.ProgressStop != nil && !record.ProgressStop.Result.ExitObserved) {
			return fail("another original worker is unsettled")
		}
		for _, exec := range []*OpenShellCustodyExec{record.Exec, record.ProgressStop} {
			if exec != nil && (!exec.Result.StartAttempted || exec.Command.RequestID == "" || exec.Result.RequestID != exec.Command.RequestID || exec.AttemptID == "") {
				return fail("another original execution acknowledgement differs")
			}
		}
	}
	r := in.record
	if matches != 1 || r.Task != (OpenShellTask{d.OrderID, d.AttemptID, d.SessionID}) || r.Worker.Identity.OrderID != d.OrderID || r.Worker.Identity.AttemptID != d.AttemptID || r.Worker.Identity.SessionID != d.SessionID || r.Retained || !r.CreateAcknowledged || r.StopRequestID != "" || r.StopAcknowledged || r.Stopped != nil || r.Worker.StopPending || d.Worker.StopPending || r.Exec == nil || r.ProgressStop == nil || r.MainProcessInstanceID == "" {
		return fail("original worker or stop effects differ")
	}
	_, binding, err := unwrapOpenShellProgress(r.Exec.Command)
	if err != nil || binding.SessionID != d.SessionID || binding.Binary != profile.ProgressSupervisor {
		return fail("original supervisor binding differs")
	}
	control, err := progressStopCommand(r.Exec.Command, r.ProgressStop.Command.RequestID)
	if err != nil || !reflect.DeepEqual(control, r.ProgressStop.Command) || r.Exec.AttemptID != d.AttemptID || r.ProgressStop.AttemptID != d.AttemptID+"/progress-stop" || r.Exec.Command.RequestID == r.ProgressStop.Command.RequestID {
		return fail("original execution purpose differs")
	}
	for i, exec := range []*OpenShellCustodyExec{r.Exec, r.ProgressStop} {
		if exec.Result.RequestID != exec.Command.RequestID || !exec.Result.StartAttempted {
			return fail("original execution request binding differs")
		}
		if !exec.Result.ExitObserved {
			purpose := "model"
			if i == 1 {
				purpose = "progress-stop"
			}
			in.unresolved = append(in.unresolved, StorageFailureExecution{purpose, exec.AttemptID, exec.Command.RequestID})
		}
	}
	if result := r.ProgressStop.Result; result.ExitObserved && result.ExitCode == 0 && result.InputError == "" && result.StreamError == "" {
		return fail("progress-stop has a successful native result")
	}
	var create pb.CreateSandboxRequest
	if err := proto.Unmarshal(r.CreateIntent, &create); err != nil || create.GetSpec() == nil {
		return fail("original create specification is missing")
	}
	base, _, _ := unwrapOpenShellProgress(r.Exec.Command)
	boundRoute := false
	for _, route := range profile.Routes {
		if len(base.Argv) != 0 && base.Argv[0] == route.Binary && proto.Equal(create.GetSpec(), route.Spec) {
			boundRoute = true
		}
	}
	if !boundRoute {
		return fail("original create policy differs from the admitted route")
	}
	failure, err := os.ReadFile(p.Originals["failure"].Path)
	if err != nil {
		return in, err
	}
	if len(bytes.TrimSpace(failure)) == 0 {
		return fail("original stop output is empty")
	}
	var output struct {
		Status string `json:"status"`
	}
	if json.Unmarshal(failure, &output) == nil && output.Status == "archived" {
		return fail("original stop output claims archived progress")
	}
	return in, nil
}

// StopStorageFailure leaves the old custody and dispatch bytes untouched. The
// session reservation survives every failure, including an unknown Stop ACK.
func StopStorageFailure(ctx context.Context, planRef SessionReadbackFileRef, profile OpenShellProfile) (SessionReadbackFileRef, error) {
	var p StorageFailureStopPlan
	if err := maintenanceVerify(planRef, &p); err != nil {
		return SessionReadbackFileRef{}, err
	}
	in, err := readStorageFailureInputs(p, profile, true)
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	lock, err := lockfile.TryLock(filepath.Join(p.SessionDirectory, "maintenance.lock"))
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	defer lock.Close()
	reservation := filepath.Join(p.SessionDirectory, "storage-failure-stop-intent.json")
	for _, path := range []string{reservation, filepath.Join(p.SessionDirectory, "maintenance-stop-intent.json")} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			return SessionReadbackFileRef{}, fmt.Errorf("original attempt already reserved a stop; no replay")
		}
	}
	conn, err := newOpenShellConnection(profile.Gateway.Binding.Gateway, profile.Gateway.TLS)
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	defer conn.Close()
	client := pb.NewOpenShellClient(conn)
	scope := &dm.WorkspaceSelector{Selection: &dm.WorkspaceSelector_Workspace{Workspace: in.state.Binding.Workspace}}
	directory := filepath.Dir(planRef.Path)
	before, err := observeStorageFailureInventory(ctx, client, in, directory, "before", pb.SandboxPhase_SANDBOX_PHASE_READY)
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	before.Plan = planRef
	beforeRef, err := writeStorageFailureInventory(directory, "before", before)
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	if err := maintenanceVerify(planRef, nil); err != nil {
		return SessionReadbackFileRef{}, err
	}
	if err := ValidateStorageFailureStop(p, profile); err != nil {
		return SessionReadbackFileRef{}, err
	}
	if err := ctx.Err(); err != nil {
		return SessionReadbackFileRef{}, err
	}
	id, err := openShellUUID()
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	intent := ProgressMaintenanceStop{Plan: planRef, RequestID: id}
	if err := maintenanceWrite(reservation, intent); err != nil {
		return SessionReadbackFileRef{}, err
	}
	if err := maintenanceWrite(filepath.Join(directory, "storage-failure-stop-intent.json"), intent); err != nil {
		return SessionReadbackFileRef{}, err
	}
	sessionIntent, err := ReadSessionReadbackFile(reservation, nil)
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	operationIntent, err := ReadSessionReadbackFile(filepath.Join(directory, "storage-failure-stop-intent.json"), nil)
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	ack, err := client.StopSandbox(ctx, &pb.StopSandboxRequest{Name: in.record.Name, WorkspaceScope: scope, RequestId: id})
	if err != nil {
		return SessionReadbackFileRef{}, fmt.Errorf("storage failure stop acknowledgement unresolved; never replay: %w", err)
	}
	ackPath := filepath.Join(directory, "storage-failure-native-stop-ack.json")
	if err := maintenanceWriteNative(ackPath, ack); err != nil {
		return SessionReadbackFileRef{}, err
	}
	if err := maintenanceNative(in.record, ack.GetSandbox()); err != nil {
		return SessionReadbackFileRef{}, err
	}
	ackRef, err := ReadSessionReadbackFile(ackPath, nil)
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	for {
		after, err := client.GetSandbox(ctx, &pb.GetSandboxRequest{Name: in.record.Name, WorkspaceScope: scope})
		if err != nil {
			return SessionReadbackFileRef{}, fmt.Errorf("storage failure fresh stop observation unresolved: %w", err)
		}
		if err := maintenanceNative(in.record, after.GetSandbox()); err != nil {
			return SessionReadbackFileRef{}, err
		}
		if after.GetSandbox().GetStatus().GetPhase() == pb.SandboxPhase_SANDBOX_PHASE_STOPPED {
			stoppedPath := filepath.Join(directory, "storage-failure-native-stopped.json")
			if err := maintenanceWriteNative(stoppedPath, after); err != nil {
				return SessionReadbackFileRef{}, err
			}
			stoppedRef, err := ReadSessionReadbackFile(stoppedPath, nil)
			if err != nil {
				return SessionReadbackFileRef{}, err
			}
			afterInventory, err := observeStorageFailureInventory(ctx, client, in, directory, "after", pb.SandboxPhase_SANDBOX_PHASE_STOPPED)
			if err != nil {
				return SessionReadbackFileRef{}, err
			}
			afterInventory.Plan = planRef
			afterRef, err := writeStorageFailureInventory(directory, "after", afterInventory)
			if err != nil {
				return SessionReadbackFileRef{}, err
			}
			receipt := StorageFailureStopReceipt{Schema: 1, Plan: planRef, RequestID: id, SessionIntent: sessionIntent, OperationIntent: operationIntent, Acknowledgement: ackRef, Stopped: stoppedRef, BeforeInventory: beforeRef, AfterInventory: afterRef, BeforeImageState: in.before, ComputeStopped: true, WorkspaceReturnAbandoned: true, TaskOutcome: "failed", UnresolvedOriginalExecutions: len(in.unresolved) != 0, OriginalExecutions: in.unresolved}
			name := filepath.Join(directory, "storage-failure-stop-complete.json")
			if err := maintenanceWrite(name, receipt); err != nil {
				return SessionReadbackFileRef{}, err
			}
			return ReadSessionReadbackFile(name, nil)
		}
		select {
		case <-ctx.Done():
			return SessionReadbackFileRef{}, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// ValidateStorageFailureStopReceipt verifies saved proof against immutable
// before-images. It does not reuse an old Stopped observation as a fresh Get.
func ValidateStorageFailureStopReceipt(planRef SessionReadbackFileRef, profile OpenShellProfile) (SessionReadbackFileRef, error) {
	var p StorageFailureStopPlan
	if err := maintenanceVerify(planRef, &p); err != nil {
		return SessionReadbackFileRef{}, err
	}
	in, err := readStorageFailureInputs(p, profile, false)
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	directory := filepath.Dir(planRef.Path)
	var receipt StorageFailureStopReceipt
	ref, err := ReadSessionReadbackFile(filepath.Join(directory, "storage-failure-stop-complete.json"), &receipt)
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	if receipt.Schema != 1 || receipt.Plan != planRef || receipt.RequestID == "" || !receipt.ComputeStopped || !receipt.WorkspaceReturnAbandoned || receipt.TaskOutcome != "failed" || receipt.BeforeImageState != in.before || receipt.UnresolvedOriginalExecutions != (len(in.unresolved) != 0) || !reflect.DeepEqual(receipt.OriginalExecutions, in.unresolved) || receipt.Acknowledgement.Path != filepath.Join(directory, "storage-failure-native-stop-ack.json") || receipt.Stopped.Path != filepath.Join(directory, "storage-failure-native-stopped.json") {
		return SessionReadbackFileRef{}, fmt.Errorf("storage failure stop completion binding differs")
	}
	if receipt.OperationIntent.Path != filepath.Join(directory, "storage-failure-stop-intent.json") || receipt.SessionIntent.Path != filepath.Join(p.SessionDirectory, "storage-failure-stop-intent.json") {
		return SessionReadbackFileRef{}, fmt.Errorf("storage failure stop intent paths differ")
	}
	for _, intentRef := range []SessionReadbackFileRef{receipt.OperationIntent, receipt.SessionIntent} {
		var intent ProgressMaintenanceStop
		if err := maintenanceVerify(intentRef, &intent); err != nil {
			return SessionReadbackFileRef{}, err
		}
		if intent.Plan != planRef || intent.RequestID != receipt.RequestID || intent.Acknowledgement != (SessionReadbackFileRef{}) || intent.Stopped != (SessionReadbackFileRef{}) {
			return SessionReadbackFileRef{}, fmt.Errorf("storage failure stop reservation differs")
		}
	}
	for i, nativeRef := range []SessionReadbackFileRef{receipt.Acknowledgement, receipt.Stopped} {
		var response pb.SandboxResponse
		if err := maintenanceReadNative(nativeRef, &response); err != nil {
			return SessionReadbackFileRef{}, err
		}
		if err := maintenanceNative(in.record, response.GetSandbox()); err != nil {
			return SessionReadbackFileRef{}, err
		}
		if i == 1 && response.GetSandbox().GetStatus().GetPhase() != pb.SandboxPhase_SANDBOX_PHASE_STOPPED {
			return SessionReadbackFileRef{}, fmt.Errorf("storage failure native stop proof is not Stopped")
		}
	}
	for i, inventoryRef := range []SessionReadbackFileRef{receipt.BeforeInventory, receipt.AfterInventory} {
		prefix, phase := "before", pb.SandboxPhase_SANDBOX_PHASE_READY
		if i == 1 {
			prefix, phase = "after", pb.SandboxPhase_SANDBOX_PHASE_STOPPED
		}
		if inventoryRef.Path != filepath.Join(directory, "storage-failure-"+prefix+"-inventory.json") {
			return SessionReadbackFileRef{}, fmt.Errorf("storage failure inventory source path differs")
		}
		var inventory StorageFailureInventory
		if err := maintenanceVerify(inventoryRef, &inventory); err != nil {
			return SessionReadbackFileRef{}, err
		}
		if inventory.Plan != planRef || inventory.Stop != (SessionReadbackFileRef{}) {
			return SessionReadbackFileRef{}, fmt.Errorf("storage failure inventory plan binding differs")
		}
		if err := validateStorageFailureInventory(in, inventory, phase); err != nil {
			return SessionReadbackFileRef{}, err
		}
	}
	return ref, nil
}

// ObserveStorageFailureInventory saves a new all-workspace inventory. The
// original execution debt stays unknown even after all compute is stopped.
func ObserveStorageFailureInventory(ctx context.Context, planRef SessionReadbackFileRef, profile OpenShellProfile) (StorageFailureInventory, error) {
	var result StorageFailureInventory
	stop, err := ValidateStorageFailureStopReceipt(planRef, profile)
	if err != nil {
		return result, err
	}
	var p StorageFailureStopPlan
	if err := maintenanceVerify(planRef, &p); err != nil {
		return result, err
	}
	in, err := readStorageFailureInputs(p, profile, false)
	if err != nil {
		return result, err
	}
	conn, err := newOpenShellConnection(profile.Gateway.Binding.Gateway, profile.Gateway.TLS)
	if err != nil {
		return result, err
	}
	defer conn.Close()
	client := pb.NewOpenShellClient(conn)
	directory, err := os.MkdirTemp(filepath.Dir(planRef.Path), "storage-failure-observation-")
	if err != nil {
		return result, err
	}
	result, err = observeStorageFailureInventory(ctx, client, in, directory, "fresh", pb.SandboxPhase_SANDBOX_PHASE_STOPPED)
	if err != nil {
		return StorageFailureInventory{}, err
	}
	after, err := ValidateStorageFailureStopReceipt(planRef, profile)
	if err != nil || after != stop {
		return StorageFailureInventory{}, fmt.Errorf("storage failure proof changed during native observation")
	}
	result.Plan, result.Stop, result.ObservedAt = planRef, stop, time.Now().UTC()
	return result, nil
}

// ObserveStorageFailureStopped returns the saved fresh inventory's file ref.
func ObserveStorageFailureStopped(ctx context.Context, planRef SessionReadbackFileRef, profile OpenShellProfile) (SessionReadbackFileRef, error) {
	inventory, err := ObserveStorageFailureInventory(ctx, planRef, profile)
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	return writeStorageFailureInventory(filepath.Dir(inventory.Lists[0].Path), "fresh", inventory)
}

// ValidateStorageFailureInventory verifies a saved full inventory without RPCs.
// The consumer binds ObservedAt to its own retirement intent and source digest.
func ValidateStorageFailureInventory(planRef SessionReadbackFileRef, profile OpenShellProfile, inventory StorageFailureInventory) error {
	stop, err := ValidateStorageFailureStopReceipt(planRef, profile)
	if err != nil {
		return err
	}
	if inventory.Plan != planRef || inventory.Stop != stop {
		return fmt.Errorf("storage failure saved inventory belongs to another plan or stop")
	}
	var p StorageFailureStopPlan
	if err := maintenanceVerify(planRef, &p); err != nil {
		return err
	}
	in, err := readStorageFailureInputs(p, profile, false)
	if err != nil {
		return err
	}
	return validateStorageFailureInventory(in, inventory, pb.SandboxPhase_SANDBOX_PHASE_STOPPED)
}

func writeStorageFailureInventory(directory, prefix string, inventory StorageFailureInventory) (SessionReadbackFileRef, error) {
	name := filepath.Join(directory, "storage-failure-"+prefix+"-inventory.json")
	if err := maintenanceWrite(name, inventory); err != nil {
		return SessionReadbackFileRef{}, err
	}
	return ReadSessionReadbackFile(name, nil)
}

func observeStorageFailureInventory(ctx context.Context, client pb.OpenShellClient, in storageFailureInputs, directory, prefix string, targetPhase pb.SandboxPhase) (StorageFailureInventory, error) {
	result := StorageFailureInventory{Schema: 1}
	var err error
	result.Lists, err = readStorageFailureLists(ctx, client, in, directory, prefix)
	if err != nil {
		return result, err
	}
	scope := &dm.WorkspaceSelector{Selection: &dm.WorkspaceSelector_Workspace{Workspace: in.state.Binding.Workspace}}
	for i, record := range in.state.Records {
		response, err := client.GetSandbox(ctx, &pb.GetSandboxRequest{Name: record.Name, WorkspaceScope: scope})
		if err != nil {
			return result, err
		}
		name := filepath.Join(directory, fmt.Sprintf("storage-failure-%s-get-%06d.json", prefix, i))
		if err := maintenanceWriteNative(name, response); err != nil {
			return result, err
		}
		ref, err := ReadSessionReadbackFile(name, nil)
		if err != nil {
			return result, err
		}
		result.Gets = append(result.Gets, ref)
		phase := "Stopped"
		if record.Name == in.record.Name && targetPhase == pb.SandboxPhase_SANDBOX_PHASE_READY {
			phase = "Ready"
		}
		result.Sandboxes = append(result.Sandboxes, StorageFailureSandbox{record.Name, record.Worker.Identity.Sandbox.SandboxID, record.MainProcessInstanceID, phase})
	}
	result.FinalLists, err = readStorageFailureLists(ctx, client, in, directory, prefix+"-final")
	if err != nil {
		return result, err
	}
	result.ObservedAt = time.Now().UTC()
	return result, validateStorageFailureInventory(in, result, targetPhase)
}

func readStorageFailureLists(ctx context.Context, client pb.OpenShellClient, in storageFailureInputs, directory, prefix string) ([]SessionReadbackFileRef, error) {
	var refs []SessionReadbackFileRef
	tokens := map[string]bool{}
	token := ""
	for {
		response, err := client.ListSandboxes(ctx, &pb.ListSandboxesRequest{WorkspaceScope: &dm.WorkspaceSelector{Selection: &dm.WorkspaceSelector_AllWorkspaces{AllWorkspaces: &dm.AllWorkspaces{}}}, PageSize: 100, PageToken: token})
		if err != nil {
			return nil, err
		}
		name := filepath.Join(directory, fmt.Sprintf("storage-failure-%s-list-%06d.json", prefix, len(refs)))
		if err := maintenanceWriteNative(name, response); err != nil {
			return nil, err
		}
		ref, err := ReadSessionReadbackFile(name, nil)
		if err != nil {
			return nil, err
		}
		refs = append(refs, ref)
		token = response.GetNextPageToken()
		if token == "" {
			break
		}
		if tokens[token] || len(refs) > len(in.state.Records) {
			return nil, fmt.Errorf("storage failure inventory pagination is cyclic or unbounded")
		}
		tokens[token] = true
	}
	return refs, nil
}

func validateStorageFailureInventory(in storageFailureInputs, inventory StorageFailureInventory, targetPhase pb.SandboxPhase) error {
	if inventory.Schema != 1 || inventory.ObservedAt.IsZero() || len(inventory.Lists) == 0 || len(inventory.FinalLists) == 0 || len(inventory.Gets) != len(in.state.Records) || len(inventory.Sandboxes) != len(in.state.Records) {
		return fmt.Errorf("storage failure native inventory is incomplete")
	}
	byName := map[string]OpenShellCustodyRecord{}
	for _, record := range in.state.Records {
		byName[record.Name] = record
	}
	check := func(record OpenShellCustodyRecord, sandbox *pb.Sandbox) error {
		if err := maintenanceNative(record, sandbox); err != nil {
			return err
		}
		phase := pb.SandboxPhase_SANDBOX_PHASE_STOPPED
		if record.Name == in.record.Name {
			phase = targetPhase
		}
		if sandbox.GetStatus().GetPhase() != phase {
			return fmt.Errorf("storage failure inventory native phase differs")
		}
		return nil
	}
	for _, pages := range [][]SessionReadbackFileRef{inventory.Lists, inventory.FinalLists} {
		seen, tokens := map[string]bool{}, map[string]bool{}
		for i, ref := range pages {
			var response pb.ListSandboxesResponse
			if err := maintenanceReadNative(ref, &response); err != nil {
				return err
			}
			for _, sandbox := range response.GetSandboxes() {
				name := sandbox.GetMetadata().GetName()
				record, ok := byName[name]
				if !ok || seen[name] {
					return fmt.Errorf("storage failure inventory has an unknown or duplicate sandbox")
				}
				if err := check(record, sandbox); err != nil {
					return err
				}
				seen[name] = true
			}
			token := response.GetNextPageToken()
			if (i == len(pages)-1) != (token == "") || (token != "" && tokens[token]) {
				return fmt.Errorf("storage failure inventory page chain differs")
			}
			tokens[token] = true
		}
		if len(seen) != len(byName) {
			return fmt.Errorf("storage failure inventory is missing original sandboxes")
		}
	}
	for i, ref := range inventory.Gets {
		var response pb.SandboxResponse
		if err := maintenanceReadNative(ref, &response); err != nil {
			return err
		}
		record := in.state.Records[i]
		if err := check(record, response.GetSandbox()); err != nil {
			return err
		}
		phase := "Stopped"
		if record.Name == in.record.Name && targetPhase == pb.SandboxPhase_SANDBOX_PHASE_READY {
			phase = "Ready"
		}
		if inventory.Sandboxes[i] != (StorageFailureSandbox{record.Name, record.Worker.Identity.Sandbox.SandboxID, record.MainProcessInstanceID, phase}) {
			return fmt.Errorf("storage failure inventory summary differs from native evidence")
		}
	}
	return nil
}
