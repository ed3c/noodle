package runtime

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	dm "github.com/NVIDIA/OpenShell/sdk/go/proto/datamodelv1"
	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/workspaceartifacts"
)

// OpenShellArchiveRecoveryApplyInput is derived by the original paused controller
// from the admitted scope and physical checkout, while holding its control lock.
type OpenShellArchiveRecoveryApplyInput struct {
	Review, Capture, Stop SessionReadbackFileRef
	DispatchHold          ArchiveRecoveryDispatchHold
	AttemptID             string
	Tree                  workspaceartifacts.ArchiveCandidateTree
	Scope                 workspaceartifacts.LocalTaskScope
	Target                string
}

type archiveRecoveryApplyIntent struct {
	Schema                int                    `json:"schema"`
	OriginalReturn        string                 `json:"original_return"`
	Request               SessionReadbackFileRef `json:"request"`
	Capture               SessionReadbackFileRef `json:"capture"`
	Review                SessionReadbackFileRef `json:"review"`
	Stop                  SessionReadbackFileRef `json:"stop"`
	OrderID               string                 `json:"order_id"`
	AttemptID             string                 `json:"attempt_id"`
	NativeAttemptID       string                 `json:"native_attempt_id"`
	SessionID             string                 `json:"session_id"`
	MainProcessInstanceID string                 `json:"main_process_instance_id"`
	SandboxID             string                 `json:"sandbox_id"`
	BaselineHead          string                 `json:"baseline_head"`
	CandidateTree         string                 `json:"candidate_tree"`
	AuthorizesLanding     bool                   `json:"authorizes_landing"`
}

type archiveRecoveryApplyRecord struct {
	archiveRecoveryApplyIntent
	Stopped SessionReadbackFileRef `json:"stopped"`
}

type archiveRecoveryCandidate struct {
	archiveRecoveryApplyRecord
	Kind          string                 `json:"kind"`
	CandidateHead string                 `json:"candidate_head"`
	Intent        SessionReadbackFileRef `json:"intent"`
	Delivery      SessionReadbackFileRef `json:"delivery"`
	Publication   SessionReadbackFileRef `json:"publication"`
}

// ApplyOpenShellArchiveRecovery retains an archive as a local review candidate.
// It leaves the original dispatch, custody history, and return outcome intact.
func ApplyOpenShellArchiveRecovery(ctx context.Context, p ProgressMaintenance, profile OpenShellProfile, input OpenShellArchiveRecoveryApplyInput, checkOwner func() error) (SessionReadbackFileRef, error) {
	var empty SessionReadbackFileRef
	if checkOwner == nil {
		return empty, fmt.Errorf("archive recovery original control owner is missing")
	}
	lock, err := lockfile.TryLock(filepath.Join(p.SessionDirectory, "maintenance.lock"))
	if err != nil {
		return empty, err
	}
	defer lock.Close()
	capture, err := ReadOpenShellWorkspaceCapture(input.Capture)
	if err != nil {
		return empty, err
	}
	record, baseline, sources, err := workspaceCaptureInputs(p, profile, capture.OrderID, true)
	if err != nil {
		return empty, err
	}
	sources["request"] = p.Request
	currentCustody := sources["custody"]
	for name, source := range sources {
		if name == "custody" && capture.Sources[name].Path == source.Path && len(capture.Sources[name].SHA256) == 64 {
			// The confirmed original Stop witnesses historical custody. The
			// current source remains bound independently until Apply reserves.
			continue
		}
		if capture.Sources[name] != source {
			return empty, fmt.Errorf("archive recovery original source differs: %s", name)
		}
	}
	if capture.SessionID != record.Task.SessionID || capture.NativeAttemptID != record.Task.AttemptID || capture.SandboxID != record.Worker.Identity.Sandbox.SandboxID || capture.MainProcessInstanceID != record.MainProcessInstanceID || capture.BaselineHead != baseline.Head || capture.BaselineIndexSHA256 != baseline.IndexSHA256 {
		return empty, fmt.Errorf("archive recovery captured identity differs")
	}
	var dispatch openShellDispatchReceipt
	if err = maintenanceVerify(sources["dispatch"], &dispatch); err != nil {
		return empty, err
	}
	physical, err := filepath.EvalSymlinks(dispatch.Target)
	if err != nil || physical != input.Target || !filepath.IsAbs(physical) {
		return empty, fmt.Errorf("archive recovery original checkout differs")
	}
	var custody OpenShellCustodyState
	if err = maintenanceVerify(sources["custody"], &custody); err != nil {
		return empty, err
	}
	in := storageFailureInputs{state: custody, record: record}
	return applyOpenShellArchiveRecovery(ctx, p, input, capture, baseline, in, currentCustody, checkOwner, func(ctx context.Context) (*pb.SandboxResponse, error) {
		conn, err := newOpenShellConnection(profile.Gateway.Binding.Gateway, profile.Gateway.TLS)
		if err != nil {
			return nil, err
		}
		defer conn.Close()
		scope := &dm.WorkspaceSelector{Selection: &dm.WorkspaceSelector_Workspace{Workspace: record.Worker.Identity.Sandbox.Workspace}}
		return pb.NewOpenShellClient(conn).GetSandbox(ctx, &pb.GetSandboxRequest{Name: record.Name, WorkspaceScope: scope})
	})
}

func applyOpenShellArchiveRecovery(ctx context.Context, p ProgressMaintenance, input OpenShellArchiveRecoveryApplyInput, capture OpenShellWorkspaceCapture, baseline *workspaceartifacts.Baseline, in storageFailureInputs, currentCustody SessionReadbackFileRef, checkOwner func() error, observe func(context.Context) (*pb.SandboxResponse, error)) (SessionReadbackFileRef, error) {
	var empty SessionReadbackFileRef
	var reviewed struct {
		Schema            int                                     `json:"schema"`
		Status            string                                  `json:"status"`
		OriginalReturn    string                                  `json:"original_return"`
		AttemptID         string                                  `json:"attempt_id"`
		Capture           SessionReadbackFileRef                  `json:"capture"`
		Tree              workspaceartifacts.ArchiveCandidateTree `json:"tree"`
		AuthorizesLanding bool                                    `json:"authorizes_landing"`
	}
	if err := maintenanceVerify(input.Review, &reviewed); err != nil {
		return empty, err
	}
	if reviewed.Schema != 1 || reviewed.Status != "archive_recovery_review_evidence" || reviewed.OriginalReturn != "UNKNOWN" || reviewed.AuthorizesLanding || reviewed.Capture != input.Capture || reviewed.AttemptID == "" || reviewed.AttemptID != input.AttemptID || !reflect.DeepEqual(reviewed.Tree, input.Tree) || len(reviewed.Tree.ChangedPaths) == 0 {
		return empty, fmt.Errorf("archive recovery original reviewed candidate differs")
	}
	if err := verifyArchiveRecoveryApplyStop(p, input, in); err != nil {
		return empty, err
	}
	identity := archiveRecoveryApplyIntent{Schema: 1, OriginalReturn: "UNKNOWN", Request: p.Request, Capture: input.Capture, Review: input.Review, Stop: input.Stop, OrderID: capture.OrderID, AttemptID: input.AttemptID, NativeAttemptID: capture.NativeAttemptID, SessionID: capture.SessionID, MainProcessInstanceID: capture.MainProcessInstanceID, SandboxID: capture.SandboxID, BaselineHead: baseline.Head, CandidateTree: input.Tree.CandidateTree}
	intentPath := filepath.Join(p.SessionDirectory, "archive-recovery-apply-intent.json")
	candidatePath := filepath.Join(p.SessionDirectory, "archive-recovery-candidate.json")
	var saved archiveRecoveryApplyRecord
	intentRef, err := ReadSessionReadbackFile(intentPath, &saved)
	if err == nil {
		if saved.archiveRecoveryApplyIntent != identity {
			return empty, fmt.Errorf("archive recovery original Apply intent differs")
		}
		return readArchiveRecoveryCandidate(ctx, candidatePath, intentRef, saved, input.Target, baseline, in.record)
	}
	if !os.IsNotExist(err) {
		return empty, err
	}
	if _, err = os.Lstat(candidatePath); !os.IsNotExist(err) {
		return empty, fmt.Errorf("archive recovery candidate exists without original Apply intent")
	}
	if err = checkOwner(); err != nil {
		return empty, err
	}

	stage, err := os.MkdirTemp(p.SessionDirectory, "archive-recovery-apply-")
	if err != nil {
		return empty, err
	}
	reserved := false
	defer func() {
		if !reserved {
			_ = os.RemoveAll(stage)
		}
	}()
	restored := filepath.Join(stage, "restored")
	var manifest workspaceartifacts.VerifiedArchiveManifest
	if err = maintenanceVerify(capture.Sources["workspace_manifest"], &manifest); err != nil {
		return empty, err
	}
	verified, err := workspaceartifacts.ExtractWithManifest(ctx, capture.Archive.Path, restored)
	if err != nil {
		return empty, err
	}
	if verified.SHA256 != capture.Archive.SHA256 || !reflect.DeepEqual(verified, manifest) {
		return empty, fmt.Errorf("archive recovery original archive or manifest differs")
	}
	scratch := filepath.Join(stage, "review-tree")
	tree, err := workspaceartifacts.InspectArchiveCandidate(ctx, input.Target, scratch, restored, baseline, input.Scope)
	if err != nil {
		return empty, err
	}
	if !reflect.DeepEqual(tree, input.Tree) {
		return empty, fmt.Errorf("archive recovery candidate tree or original Git scope changed")
	}
	if err = os.RemoveAll(scratch); err != nil {
		return empty, err
	}
	if err = verifyArchiveRecoveryApplySources(capture.Sources, currentCustody); err != nil {
		return empty, err
	}
	if err = VerifySessionReadbackFiles(map[string]SessionReadbackFileRef{"capture": input.Capture, "review": input.Review}); err != nil {
		return empty, err
	}
	if err = verifyArchiveRecoveryApplyStop(p, input, in); err != nil {
		return empty, err
	}
	if err = checkOwner(); err != nil {
		return empty, err
	}
	stopped, err := observe(ctx)
	if err != nil {
		return empty, err
	}
	if err = verifyArchiveRecoveryStopped(in.record, stopped); err != nil {
		return empty, err
	}
	stoppedPath := filepath.Join(stage, "native-stopped-before-apply.json")
	if err = maintenanceWriteNative(stoppedPath, stopped); err != nil {
		return empty, err
	}
	stoppedRef, err := ReadSessionReadbackFile(stoppedPath, nil)
	if err != nil {
		return empty, err
	}
	if err = checkOwner(); err != nil {
		return empty, err
	}
	if err := maintenanceVerify(currentCustody, nil); err != nil {
		return empty, err
	}
	record := archiveRecoveryApplyRecord{archiveRecoveryApplyIntent: identity, Stopped: stoppedRef}
	reserved = true
	if err = maintenanceWrite(intentPath, record); err != nil {
		return empty, err
	}
	intentRef, err = ReadSessionReadbackFile(intentPath, nil)
	if err != nil {
		return empty, err
	}
	deliveryPath := filepath.Join(stage, "delivery")
	result, err := workspaceartifacts.Apply(ctx, input.Target, baseline, restored, deliveryPath)
	if err != nil {
		return empty, fmt.Errorf("archive recovery Apply result requires original Owner readback: %w", err)
	}
	if err = checkOwner(); err != nil {
		return empty, err
	}
	message := fmt.Sprintf("chore: retain verified OpenShell archive for original review\n\nRestore the reviewed bytes for original session %s after the same sandbox stopped. Preserve the original return as UNKNOWN.", capture.SessionID)
	head, err := workspaceartifacts.PublishLocalTask(ctx, input.Target, baseline, result, message, &input.Scope, "chore: retain refused OpenShell archive as progress")
	if err != nil {
		return empty, fmt.Errorf("archive recovery local commit requires original Owner readback: %w", err)
	}
	if result.LocalTaskRefused() {
		return empty, fmt.Errorf("archive recovery original scope refused, retained progress requires original Owner readback")
	}
	if err = checkOwner(); err != nil {
		return empty, err
	}
	delivery, err := ReadSessionReadbackFile(filepath.Join(deliveryPath, "result.json"), nil)
	if err != nil {
		return empty, err
	}
	publication, err := ReadSessionReadbackFile(filepath.Join(deliveryPath, "publish.json"), nil)
	if err != nil {
		return empty, err
	}
	candidate := archiveRecoveryCandidate{archiveRecoveryApplyRecord: record, Kind: "archive_recovery_candidate", CandidateHead: head, Intent: intentRef, Delivery: delivery, Publication: publication}
	if err = verifyArchiveRecoveryPublication(ctx, candidate, input.Target, baseline, in.record); err != nil {
		return empty, err
	}
	if err = maintenanceWrite(candidatePath, candidate); err != nil {
		return empty, err
	}
	return ReadSessionReadbackFile(candidatePath, nil)
}

func verifyArchiveRecoveryApplySources(sources map[string]SessionReadbackFileRef, currentCustody SessionReadbackFileRef) error {
	if sources["custody"].Path != currentCustody.Path || currentCustody.Path == "" {
		return fmt.Errorf("archive recovery current custody source differs")
	}
	for name, source := range sources {
		if name != "custody" {
			if err := maintenanceVerify(source, nil); err != nil {
				return err
			}
		}
	}
	return maintenanceVerify(currentCustody, nil)
}

func verifyArchiveRecoveryApplyStop(p ProgressMaintenance, input OpenShellArchiveRecoveryApplyInput, in storageFailureInputs) error {
	stopDir := filepath.Join(p.SessionDirectory, "archive-recovery-stop")
	if input.Stop.Path != filepath.Join(stopDir, "archive-recovery-stop.json") {
		return fmt.Errorf("archive recovery original Stop receipt path differs")
	}
	var intent OpenShellArchiveRecoveryStop
	reservation, err := ReadSessionReadbackFile(filepath.Join(p.SessionDirectory, "archive-recovery-stop-intent.json"), &intent)
	if err != nil {
		return err
	}
	if intent.DispatchHold != input.DispatchHold {
		return fmt.Errorf("archive recovery original dispatch hold differs")
	}
	actual, err := readArchiveRecoveryStop(in, p, input.Review, input.Capture, stopDir, reservation, intent)
	if err != nil {
		return err
	}
	if actual != input.Stop {
		return fmt.Errorf("archive recovery original Stop receipt changed")
	}
	return nil
}

func verifyArchiveRecoveryStopped(record OpenShellCustodyRecord, response *pb.SandboxResponse) error {
	sandbox := response.GetSandbox()
	if err := maintenanceNative(record, sandbox); err != nil {
		return err
	}
	if sandbox.GetStatus().GetPhase() != pb.SandboxPhase_SANDBOX_PHASE_STOPPED {
		return fmt.Errorf("archive recovery fresh original Stopped evidence is missing")
	}
	if observed := openShellStoppedObservation(sandbox); record.Stopped != nil &&
		record.Stopped.ComputeInstanceID != "" && observed.ComputeInstanceID != "" &&
		record.Stopped.ComputeInstanceID != observed.ComputeInstanceID {
		return fmt.Errorf("archive recovery stopped compute instance differs from custody")
	}
	return nil
}

func readArchiveRecoveryCandidate(ctx context.Context, path string, intent SessionReadbackFileRef, recorded archiveRecoveryApplyRecord, target string, baseline *workspaceartifacts.Baseline, record OpenShellCustodyRecord) (SessionReadbackFileRef, error) {
	var candidate archiveRecoveryCandidate
	ref, err := ReadSessionReadbackFile(path, &candidate)
	if err != nil {
		return SessionReadbackFileRef{}, fmt.Errorf("archive recovery Apply is UNKNOWN; original Owner must read back the reserved effect: %w", err)
	}
	if candidate.Kind != "archive_recovery_candidate" || candidate.archiveRecoveryApplyRecord != recorded || candidate.Intent != intent {
		return SessionReadbackFileRef{}, fmt.Errorf("archive recovery original candidate receipt differs")
	}
	if err = verifyArchiveRecoveryPublication(ctx, candidate, target, baseline, record); err != nil {
		return SessionReadbackFileRef{}, err
	}
	return ref, nil
}

func verifyArchiveRecoveryPublication(ctx context.Context, candidate archiveRecoveryCandidate, target string, baseline *workspaceartifacts.Baseline, record OpenShellCustodyRecord) error {
	stage := filepath.Dir(candidate.Stopped.Path)
	session := filepath.Dir(candidate.Intent.Path)
	if filepath.Dir(stage) != session || filepath.Base(candidate.Stopped.Path) != "native-stopped-before-apply.json" || candidate.Delivery.Path != filepath.Join(stage, "delivery", "result.json") || candidate.Publication.Path != filepath.Join(stage, "delivery", "publish.json") {
		return fmt.Errorf("archive recovery retained evidence differs from original session")
	}
	var stopped pb.SandboxResponse
	if err := maintenanceReadNative(candidate.Stopped, &stopped); err != nil {
		return err
	}
	if err := verifyArchiveRecoveryStopped(record, &stopped); err != nil {
		return err
	}
	var publication struct {
		Phase        string `json:"phase"`
		Parent       string `json:"parent"`
		ExpectedTree string `json:"expected_tree"`
		ObservedHead string `json:"observed_head"`
	}
	if err := maintenanceVerify(candidate.Publication, &publication); err != nil {
		return err
	}
	if publication.Phase != "published" || publication.Parent != baseline.Head || publication.ExpectedTree != candidate.CandidateTree || publication.ObservedHead != candidate.CandidateHead {
		return fmt.Errorf("archive recovery retained publication differs from reviewed candidate")
	}
	var result workspaceartifacts.Result
	if err := maintenanceVerify(candidate.Delivery, &result); err != nil {
		return err
	}
	if result.State != "published" || result.LocalTaskRefused() || result.ReturnedDirectory != filepath.Join(stage, "restored") || result.PatchPath != filepath.Join(stage, "delivery", "changes.patch") {
		return fmt.Errorf("archive recovery retained workspace artifact differs")
	}
	return workspaceartifacts.VerifyRetainedPublication(ctx, target, baseline, &result, candidate.CandidateHead)
}
