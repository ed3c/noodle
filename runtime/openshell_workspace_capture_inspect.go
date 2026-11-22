package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"github.com/poteto/noodle/internal/workspaceartifacts"
)

// OpenShellWorkspaceCapture is the existing capture-workspace evidence shape.
type OpenShellWorkspaceCapture struct {
	Schema                int                               `json:"schema"`
	Status                string                            `json:"status"`
	Scope                 string                            `json:"scope"`
	OriginalReturn        string                            `json:"original_return"`
	RestorationVerified   bool                              `json:"restoration_verified"`
	OrderID               string                            `json:"order_id"`
	NativeAttemptID       string                            `json:"native_attempt_id"`
	SessionID             string                            `json:"session_id"`
	SandboxID             string                            `json:"sandbox_id"`
	MainProcessInstanceID string                            `json:"main_process_instance_id"`
	CaptureExecRequestID  string                            `json:"capture_exec_request_id"`
	BaselineHead          string                            `json:"baseline_head"`
	BaselineIndexSHA256   string                            `json:"baseline_index_sha256"`
	BaselineSource        SessionReadbackFileRef            `json:"baseline_source"`
	Archive               SessionReadbackFileRef            `json:"archive"`
	Sources               map[string]SessionReadbackFileRef `json:"sources"`
}

func ReadOpenShellWorkspaceCapture(ref SessionReadbackFileRef) (OpenShellWorkspaceCapture, error) {
	var capture OpenShellWorkspaceCapture
	if err := maintenanceVerify(ref, &capture); err != nil {
		return capture, err
	}
	if filepath.Base(ref.Path) != "workspace-capture.json" || capture.Schema != 1 || capture.Status != "workspace_archive_verified" || capture.Scope != "repository" || capture.OriginalReturn != "UNKNOWN" || !capture.RestorationVerified || len(capture.Sources) != 8 || capture.BaselineSource != capture.Sources["dispatch"] {
		return capture, fmt.Errorf("workspace capture evidence shape differs")
	}
	dir := filepath.Dir(ref.Path)
	paths := map[string]SessionReadbackFileRef{
		"workspace.tar": capture.Archive, "before.json": capture.Sources["before"], "after.json": capture.Sources["after"], "workspace-manifest.json": capture.Sources["workspace_manifest"],
	}
	for name, source := range paths {
		if source.Path != filepath.Join(dir, name) || len(source.SHA256) != 64 {
			return capture, fmt.Errorf("workspace capture evidence path differs: %s", name)
		}
	}
	return capture, nil
}

var ErrWorkspaceCaptureCustodyChanged = errors.New("workspace capture original source differs: custody")

// InspectOpenShellWorkspaceCapture verifies an existing capture and computes a
// review tree in scratch space. It never contacts the original Host or Gateway.
func InspectOpenShellWorkspaceCapture(ctx context.Context, ref SessionReadbackFileRef, p ProgressMaintenance, profile OpenShellProfile, project, target, gitCommonDir string, scope workspaceartifacts.LocalTaskScope) (workspaceartifacts.ArchiveCandidateTree, error) {
	return inspectOpenShellWorkspaceCapture(ctx, ref, p, profile, project, target, gitCommonDir, scope, false, false)
}

// InspectHistoricalOpenShellWorkspaceTree compares retained archive bytes for
// recapture only. It does not verify the old custody or authorize recovery effects.
func InspectHistoricalOpenShellWorkspaceTree(ctx context.Context, ref SessionReadbackFileRef, p ProgressMaintenance, profile OpenShellProfile, project, target, gitCommonDir string, scope workspaceartifacts.LocalTaskScope) (workspaceartifacts.ArchiveCandidateTree, error) {
	return inspectOpenShellWorkspaceCapture(ctx, ref, p, profile, project, target, gitCommonDir, scope, true, false)
}

// InspectStoppedOpenShellWorkspaceTree verifies the retained tree for a planned
// Apply. The original Stop receipt and current VM phase need separate owner readback.
func InspectStoppedOpenShellWorkspaceTree(ctx context.Context, ref SessionReadbackFileRef, p ProgressMaintenance, profile OpenShellProfile, project, target, gitCommonDir string, scope workspaceartifacts.LocalTaskScope) (workspaceartifacts.ArchiveCandidateTree, error) {
	return inspectOpenShellWorkspaceCapture(ctx, ref, p, profile, project, target, gitCommonDir, scope, true, true)
}

func inspectOpenShellWorkspaceCapture(ctx context.Context, ref SessionReadbackFileRef, p ProgressMaintenance, profile OpenShellProfile, project, target, gitCommonDir string, scope workspaceartifacts.LocalTaskScope, historical, afterArchiveStop bool) (workspaceartifacts.ArchiveCandidateTree, error) {
	var result workspaceartifacts.ArchiveCandidateTree
	capture, err := ReadOpenShellWorkspaceCapture(ref)
	if err != nil {
		return result, err
	}
	record, baseline, original, err := workspaceCaptureInputs(p, profile, capture.OrderID, afterArchiveStop)
	if err != nil {
		return result, err
	}
	original["request"] = p.Request
	for name, source := range original {
		if name != "custody" && capture.Sources[name] != source {
			return result, fmt.Errorf("workspace capture original source differs: %s", name)
		}
	}
	oldCustody, currentCustody := capture.Sources["custody"], original["custody"]
	if historical {
		if oldCustody.Path != currentCustody.Path || len(oldCustody.SHA256) != 64 || (!afterArchiveStop && oldCustody == currentCustody) {
			return result, fmt.Errorf("historical archive recapture requires the same changed original custody source")
		}
	} else if oldCustody != currentCustody {
		return result, ErrWorkspaceCaptureCustodyChanged
	}
	if capture.SessionID != record.Task.SessionID || capture.NativeAttemptID != record.Task.AttemptID || capture.SandboxID != record.Worker.Identity.Sandbox.SandboxID || capture.MainProcessInstanceID != record.MainProcessInstanceID || capture.CaptureExecRequestID == "" || capture.CaptureExecRequestID == record.Exec.Command.RequestID || capture.BaselineHead != baseline.Head || capture.BaselineIndexSHA256 != baseline.IndexSHA256 {
		return result, fmt.Errorf("workspace capture attempt, native identity, or baseline differs")
	}
	var dispatch openShellDispatchReceipt
	if err = maintenanceVerify(original["dispatch"], &dispatch); err != nil {
		return result, err
	}
	dispatchedTarget, err := filepath.EvalSymlinks(dispatch.Target)
	if err != nil || dispatchedTarget != target {
		return result, fmt.Errorf("workspace capture original checkout differs")
	}
	for _, side := range []string{"before", "after"} {
		var observation pb.SandboxResponse
		if err = maintenanceReadNative(capture.Sources[side], &observation); err != nil {
			return result, err
		}
		if err = maintenanceNative(record, observation.GetSandbox()); err != nil {
			return result, err
		}
		if observation.GetSandbox().GetStatus().GetPhase() != pb.SandboxPhase_SANDBOX_PHASE_READY {
			return result, fmt.Errorf("workspace capture observation was not the original Ready VM")
		}
	}
	var manifest workspaceartifacts.VerifiedArchiveManifest
	if err = maintenanceVerify(capture.Sources["workspace_manifest"], &manifest); err != nil {
		return result, err
	}
	parent, err := workspaceInspectionTempRoot(os.TempDir(), project, target, gitCommonDir, p.HostDir, filepath.Dir(filepath.Dir(p.SessionDirectory)), profile.Gateway.Binding.StateRoot, filepath.Dir(ref.Path), filepath.Dir(p.Request.Path))
	if err != nil {
		return result, err
	}
	temporary, err := os.MkdirTemp(parent, "noodle-capture-inspect-")
	if err != nil {
		return result, err
	}
	defer os.RemoveAll(temporary)
	before, err := os.Lstat(capture.Archive.Path)
	if err != nil {
		return result, err
	}
	restored := filepath.Join(temporary, "restored")
	verified, err := workspaceartifacts.ExtractWithManifest(ctx, capture.Archive.Path, restored)
	if err != nil {
		return result, err
	}
	if verified.SHA256 != capture.Archive.SHA256 || !reflect.DeepEqual(verified, manifest) {
		return result, fmt.Errorf("workspace capture archive and verified manifest differ")
	}
	result, err = workspaceartifacts.InspectArchiveCandidate(ctx, target, filepath.Join(temporary, "git"), restored, baseline, scope)
	if err != nil {
		return result, err
	}
	after, err := os.Lstat(capture.Archive.Path)
	if err != nil || !os.SameFile(before, after) || after.Size() != before.Size() || after.Mode() != before.Mode() || !after.ModTime().Equal(before.ModTime()) {
		return result, fmt.Errorf("workspace capture archive changed during inspection")
	}
	for name, source := range capture.Sources {
		if historical && name == "custody" {
			continue
		}
		if err = maintenanceVerify(source, nil); err != nil {
			return result, err
		}
	}
	if historical {
		if err = maintenanceVerify(currentCustody, nil); err != nil {
			return result, err
		}
	}
	return result, maintenanceVerify(ref, nil)
}

func workspaceInspectionTempRoot(parent string, protected ...string) (string, error) {
	parent, err := filepath.EvalSymlinks(parent)
	if err != nil || !filepath.IsAbs(parent) {
		return "", fmt.Errorf("archive inspection temporary directory is unavailable")
	}
	for _, name := range protected {
		root, err := filepath.EvalSymlinks(name)
		if err != nil || !filepath.IsAbs(root) {
			return "", fmt.Errorf("archive inspection original evidence directory is unavailable")
		}
		rel, err := filepath.Rel(root, parent)
		if err != nil || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
			return "", fmt.Errorf("archive inspection temporary directory overlaps original owner or capture data")
		}
		original, err := os.Stat(root)
		if err != nil {
			return "", err
		}
		for ancestor := parent; ; ancestor = filepath.Dir(ancestor) {
			current, err := os.Stat(ancestor)
			if err != nil {
				return "", err
			}
			if os.SameFile(original, current) {
				return "", fmt.Errorf("archive inspection temporary directory overlaps original owner or capture data")
			}
			if ancestor == filepath.Dir(ancestor) {
				break
			}
		}
	}
	return parent, nil
}
