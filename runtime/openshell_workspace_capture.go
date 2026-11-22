package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	dm "github.com/NVIDIA/OpenShell/sdk/go/proto/datamodelv1"
	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/workspaceartifacts"
)

// CaptureOpenShellWorkspace retains the original repository when the supervisor
// reached archive creation but could not write the archive to the guest disk.
// The calling Host owner supplies a live, locked and recheckable admission.
func CaptureOpenShellWorkspace(ctx context.Context, p ProgressMaintenance, profile OpenShellProfile, order, output string, verifyOwner func() error) (SessionReadbackFileRef, error) {
	lock, err := lockfile.TryLock(filepath.Join(p.SessionDirectory, "maintenance.lock"))
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	defer lock.Close()
	record, baseline, sources, err := workspaceCaptureInputs(p, profile, order, false)
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	if err = workspaceartifacts.VerifyCapturedBaseline(ctx, baseline); err != nil {
		return SessionReadbackFileRef{}, err
	}
	if err = verifyOwner(); err != nil {
		return SessionReadbackFileRef{}, err
	}
	conn, err := newOpenShellConnection(profile.Gateway.Binding.Gateway, profile.Gateway.TLS)
	if err != nil {
		return SessionReadbackFileRef{}, err
	}
	defer conn.Close()
	sources["request"] = p.Request
	return captureWorkspaceArchive(ctx, pb.NewOpenShellClient(conn), record, baseline, output, sources, verifyOwner)
}

func workspaceCaptureInputs(p ProgressMaintenance, profile OpenShellProfile, order string, afterArchiveStop bool) (record OpenShellCustodyRecord, baseline *workspaceartifacts.Baseline, sources map[string]SessionReadbackFileRef, err error) {
	if err = VerifyOpenShellProfile(profile); err != nil {
		return record, nil, nil, err
	}
	dir := p.SessionDirectory
	session := filepath.Base(dir)
	var d openShellDispatchReceipt
	dispatch, err := ReadSessionReadbackFile(filepath.Join(dir, "openshell-dispatch.json"), &d)
	if err != nil {
		return record, nil, nil, err
	}
	profileDigest, err := OpenShellDispatchSHA256(profile)
	if err != nil {
		return record, nil, nil, err
	}
	if d.Version != 1 || d.ProfileSHA256 != profileDigest || d.ContextID != p.ContextID || d.OrderID != order || order == "" || d.SessionID != session || d.AttemptID == "" || d.Worker == nil || d.Baseline == nil || d.Phase != "progress-unknown" || d.Error == "" || d.Progress != nil || d.ProgressSource != nil || d.Delivery != nil || d.CandidateHead != "" || d.ProgressHead != "" || d.Maintenance != nil {
		return record, nil, nil, fmt.Errorf("workspace capture requires the original unreturned OpenShell attempt")
	}
	for _, name := range []string{"maintenance-stop-intent.json", "storage-failure-stop-intent.json"} {
		if _, statErr := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(statErr) {
			return record, nil, nil, fmt.Errorf("workspace capture conflicts with original stop intent")
		}
	}
	var custody OpenShellCustodyState
	owner, err := ReadSessionReadbackFile(filepath.Join(profile.Gateway.Binding.StateRoot, "owner.json"), &custody)
	if err != nil {
		return record, nil, nil, err
	}
	if custody.Binding != profile.Gateway.Binding || custody.Blocked != "" || len(custody.PendingRetained) != 0 {
		return record, nil, nil, fmt.Errorf("workspace capture custody differs from the original profile")
	}
	if err = validateOpenShellCustodyLedger(custody); err != nil {
		return record, nil, nil, err
	}
	for _, row := range custody.Records {
		if row.Worker.Identity == d.Worker.Identity {
			record = row
		} else if row.Stopped == nil {
			return record, nil, nil, fmt.Errorf("workspace capture requires exclusive original worker custody")
		}
	}
	if record.Task != (OpenShellTask{order, d.AttemptID, session}) || !record.CreateAcknowledged || record.Retained || archiveCaptureHasConflictingStop(record, afterArchiveStop) || record.Exec == nil || record.MainProcessInstanceID == "" {
		return record, nil, nil, fmt.Errorf("workspace capture original worker or execution binding differs")
	}
	e := record.Exec
	if e.AttemptID != d.AttemptID || e.Result.RequestID != e.Command.RequestID || !e.Result.StartAttempted || !e.Result.ExitObserved || e.Result.ExitCode != 74 || e.Result.InputError != "" || e.Result.StreamError != "" || e.Result != d.Worker.ExecResult {
		return record, nil, nil, fmt.Errorf("workspace capture original supervisor exit is unresolved or differs")
	}
	base, binding, err := unwrapOpenShellProgress(e.Command)
	if err != nil || base.Workdir != "/sandbox/repo" || binding.SessionID != session || binding.Binary != profile.ProgressSupervisor {
		return record, nil, nil, fmt.Errorf("workspace capture original repository or supervisor differs")
	}
	stderr, err := ReadSessionReadbackFile(filepath.Join(dir, "stderr.log"), nil)
	if err != nil {
		return record, nil, nil, err
	}
	data, err := os.ReadFile(stderr.Path)
	if err != nil {
		return record, nil, nil, err
	}
	untilFailure := []byte("guest progress unavailable: write " + binding.Directory + "/noodle-result.tar: no space left on device")
	if !bytes.Contains(append([]byte{'\n'}, data...), append(append([]byte{'\n'}, untilFailure...), '\n')) {
		return record, nil, nil, fmt.Errorf("workspace capture requires original supervisor archive ENOSPC evidence")
	}
	return record, d.Baseline, map[string]SessionReadbackFileRef{"dispatch": dispatch, "custody": owner, "stderr": stderr, "profile": p.Profile}, nil
}

// The original custody may observe the archive Stop through Get without its own
// Stop RPC. Only post-Stop callers can use that history with the separate ACK.
func archiveCaptureHasConflictingStop(record OpenShellCustodyRecord, afterArchiveStop bool) bool {
	return (!afterArchiveStop && record.Stopped != nil) || record.StopRequestID != "" ||
		record.StopAcknowledged || record.Worker.StopPending || record.ProgressStop != nil ||
		record.Retirement != nil
}

func captureWorkspaceArchive(ctx context.Context, client pb.OpenShellClient, record OpenShellCustodyRecord, baseline *workspaceartifacts.Baseline, output string, sources map[string]SessionReadbackFileRef, verifyOwner func() error) (ref SessionReadbackFileRef, err error) {
	identity := record.Worker.Identity
	scope := &dm.WorkspaceSelector{Selection: &dm.WorkspaceSelector_Workspace{Workspace: identity.Sandbox.Workspace}}
	check := func(s *pb.Sandbox) error {
		if err := maintenanceNative(record, s); err != nil {
			return err
		}
		if s.GetStatus().GetPhase() != pb.SandboxPhase_SANDBOX_PHASE_READY {
			return fmt.Errorf("workspace capture requires the original Ready sandbox")
		}
		return nil
	}
	observe := func() (*pb.SandboxResponse, error) {
		result, err := client.GetSandbox(ctx, &pb.GetSandboxRequest{Name: record.Name, WorkspaceScope: scope})
		if err != nil {
			return nil, err
		}
		return result, check(result.GetSandbox())
	}
	before, err := observe()
	if err != nil {
		return ref, err
	}
	if err = os.Mkdir(output, 0700); err != nil {
		return ref, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, os.RemoveAll(output))
		}
	}()
	archive := filepath.Join(output, "workspace.tar")
	file, err := os.OpenFile(archive, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return ref, err
	}
	transport := &OpenShellTransport{client: client, ctx: ctx, target: OpenShellTarget{Sandbox: identity.Sandbox, Name: record.Name, ConfigSource: identity.ConfigSource}, observeSandbox: check}
	execID, captureErr := streamWorkspaceArchive(ctx, transport, file)
	if err = errors.Join(captureErr, file.Sync(), file.Close()); err != nil {
		return ref, err
	}
	// Extract enforces the original workspace path and archive rules, including EOF.
	restored := filepath.Join(output, "restoration-check")
	manifest, err := workspaceartifacts.ExtractWithManifest(ctx, archive, restored)
	if err != nil {
		return ref, err
	}
	if err = os.RemoveAll(restored); err != nil {
		return ref, err
	}
	after, err := observe()
	if err != nil {
		return ref, err
	}
	if err = VerifySessionReadbackFiles(sources); err != nil {
		return ref, err
	}
	if err = verifyOwner(); err != nil {
		return ref, err
	}
	manifestPath := filepath.Join(output, "workspace-manifest.json")
	if err = maintenanceWrite(manifestPath, manifest); err != nil {
		return ref, err
	}
	sources["workspace_manifest"], err = ReadSessionReadbackFile(manifestPath, nil)
	if err != nil {
		return ref, err
	}
	for name, observation := range map[string]*pb.SandboxResponse{"before": before, "after": after} {
		path := filepath.Join(output, name+".json")
		if err = maintenanceWriteNative(path, observation); err != nil {
			return ref, err
		}
		sources[name], err = ReadSessionReadbackFile(path, nil)
		if err != nil {
			return ref, err
		}
	}
	receipt := filepath.Join(output, "workspace-capture.json")
	if err = maintenanceWrite(receipt, map[string]any{
		"schema": 1, "status": "workspace_archive_verified", "original_return": "UNKNOWN", "scope": "repository", "restoration_verified": true,
		"order_id": record.Task.OrderID, "native_attempt_id": record.Task.AttemptID, "session_id": record.Task.SessionID, "sandbox_id": identity.Sandbox.SandboxID,
		"main_process_instance_id": record.MainProcessInstanceID, "capture_exec_request_id": execID, "archive": SessionReadbackFileRef{Path: archive, SHA256: manifest.SHA256}, "sources": sources,
		"baseline_head": baseline.Head, "baseline_index_sha256": baseline.IndexSHA256, "baseline_source": sources["dispatch"],
	}); err != nil {
		return ref, err
	}
	return ReadSessionReadbackFile(receipt, nil)
}

func streamWorkspaceArchive(ctx context.Context, transport *OpenShellTransport, output io.Writer) (string, error) {
	id, err := openShellUUID()
	if err != nil {
		return "", err
	}
	command := OpenShellCommand{RequestID: id, Workdir: "/sandbox/repo", Env: map[string]string{}, Argv: []string{"tar", "--hard-dereference", "-cf", "-", "-C", "/sandbox/repo", "--", "."}}
	exec, err := transport.Start(ctx, command)
	if exec != nil {
		defer exec.Close()
	}
	if err != nil {
		return id, err
	}
	if err = exec.CloseWrite(); err != nil {
		return id, err
	}
	var size int64
	for chunk := range exec.Output() {
		if chunk.Stream != OpenShellStdout || size+int64(len(chunk.Data)) > 9<<30 {
			return id, fmt.Errorf("workspace capture stream reported an error or exceeded its size bound")
		}
		n, writeErr := output.Write(chunk.Data)
		if writeErr != nil {
			return id, writeErr
		}
		if n != len(chunk.Data) {
			return id, io.ErrShortWrite
		}
		size += int64(n)
	}
	result, err := exec.Wait()
	if err != nil || !result.ExitObserved || result.ExitCode != 0 || result.InputError != "" || result.StreamError != "" || size == 0 {
		return id, errors.Join(err, fmt.Errorf("workspace capture tar exit or output incomplete"))
	}
	return id, nil
}
