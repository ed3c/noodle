package main

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/workspaceartifacts"
	"github.com/poteto/noodle/loop"
	loopruntime "github.com/poteto/noodle/runtime"
)

func prepareArtifactCompaction(ctx context.Context, original, session, pause, directory string) (loopruntime.SessionReadbackFileRef, error) {
	var zero loopruntime.SessionReadbackFileRef
	request, err := artifactCompactionFile(original, nil)
	if err != nil {
		return zero, err
	}
	binding, host, refs, err := artifactCompactionHost(request)
	if err != nil {
		return zero, err
	}
	if err := artifactCompactionExternal(directory, host); err != nil {
		return zero, err
	}
	executable, err := os.Executable()
	if err != nil {
		return zero, err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return zero, err
	}
	tool, err := artifactCompactionFile(executable, nil)
	if err != nil {
		return zero, err
	}
	p := artifactCompactionRequest{Schema: 1, ToolVersion: currentVersion(), Tool: tool, OriginalRequest: request, OriginalBinary: refs["original_binary"], OriginalHost: host, PauseID: pause, Directory: directory}
	cfg, _, err := config.LoadRuntime(binding.ProjectDir)
	if err != nil {
		return zero, err
	}
	p.Owner, err = loop.ReadArtifactCompactionOwner(binding.ProjectDir, cfg, hostOrderID(binding.ProjectDir, session), session, host.ContextID, pause)
	if err != nil {
		return zero, err
	}
	// These calls must complete before taking control.lock: cleanup is serviced
	// by the original Loop, which also needs that lock. Prove the pause first:
	// cleanup inspection rearms UNKNOWN and must never be issued to a running actor.
	observed, err := artifactCompactionReadHost(ctx, p)
	if err != nil {
		return zero, err
	}
	lock, err := artifactCompactionLock(binding.ProjectDir)
	if err != nil {
		return zero, err
	}
	defer lock.Close()
	if err := artifactCompactionObserveOwner(p); err != nil {
		return zero, err
	}
	p.Manifest, err = artifactCompactionFile(filepath.Join(filepath.Dir(p.Owner.Result.PatchPath), "compaction.json"), nil)
	if err != nil {
		return zero, err
	}
	if err := workspaceartifacts.VerifyCompactedPublication(ctx, host.ProjectDir, p.Owner.Baseline, p.Owner.Result, p.Owner.Head); err != nil {
		return zero, err
	}
	ack, err := artifactCompactionFile(filepath.Join(host.ProjectDir, ".noodle", "control-ack.ndjson"), nil)
	if err != nil {
		return zero, err
	}
	refs["pause_ack"], refs["continuation_tool"], refs["compaction_manifest"] = ack, tool, p.Manifest
	maps.Copy(refs, p.Owner.Sources)
	if err := artifactCompactionEvidence(p, refs); err != nil {
		return zero, err
	}
	if err := os.Mkdir(directory, 0700); err != nil {
		return zero, err
	}
	if err := filex.SyncDir(filepath.Dir(directory)); err != nil {
		return zero, err
	}
	for operation, data := range observed {
		path := filepath.Join(directory, "original-"+operation+".json")
		if err := filex.WriteFileAtomicDurable(path, data); err != nil {
			return zero, err
		}
		ref, err := artifactCompactionFile(path, nil)
		if err != nil {
			return zero, err
		}
		refs["original_"+operation] = ref
	}
	p.Evidence = refs
	if err := artifactCompactionSync(refs); err != nil {
		return zero, err
	}
	return artifactCompactionWrite(filepath.Join(directory, "request.json"), p)
}

// Discover the canonical order only by exact session membership, never a name
// prefix or newest task. The owner reader subsequently validates the full state.
func hostOrderID(project, session string) string {
	var snapshot struct {
		State struct {
			Orders map[string]struct {
				Stages []struct {
					Attempts []struct {
						SessionID string `json:"session_id"`
					} `json:"attempts"`
				} `json:"stages"`
			} `json:"orders"`
		} `json:"state"`
	}
	if _, err := artifactCompactionFile(filepath.Join(project, ".noodle", "state.snapshot.json"), &snapshot); err != nil {
		return ""
	}
	order, count := "", 0
	for id, value := range snapshot.State.Orders {
		for _, stage := range value.Stages {
			for _, attempt := range stage.Attempts {
				if attempt.SessionID == session {
					order = id
					count++
				}
			}
		}
	}
	if count != 1 {
		return ""
	}
	return order
}

func validateArtifactCompactionRequest(p artifactCompactionRequest, ref loopruntime.SessionReadbackFileRef) error {
	if p.Schema != 1 || p.ToolVersion != currentVersion() || p.Directory == "" || ref.Path != filepath.Join(p.Directory, "request.json") || p.Owner.Baseline == nil || p.Owner.Result == nil || p.Owner.Binding.OrderID == "" || p.Owner.Binding.SessionID == "" || p.PauseID == "" || p.Manifest.Path != filepath.Join(filepath.Dir(p.Owner.Result.PatchPath), "compaction.json") || len(p.Evidence) == 0 {
		return fmt.Errorf("artifact continuation request is incomplete")
	}
	if err := artifactCompactionExternal(p.Directory, p.OriginalHost); err != nil {
		return err
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return err
	}
	tool, err := artifactCompactionFile(executable, nil)
	if err != nil || tool != p.Tool {
		return fmt.Errorf("artifact continuation tool version or binary changed")
	}
	_, host, refs, err := artifactCompactionHost(p.OriginalRequest)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(host, p.OriginalHost) || refs["original_binary"] != p.OriginalBinary {
		return fmt.Errorf("artifact continuation original Host generation changed")
	}
	for name, source := range refs {
		if p.Evidence[name] != source {
			return fmt.Errorf("artifact continuation frozen Host source differs: %s", name)
		}
	}
	for name, source := range p.Owner.Sources {
		if p.Evidence[name] != source {
			return fmt.Errorf("artifact continuation frozen owner source differs: %s", name)
		}
	}
	if p.Evidence["continuation_tool"] != p.Tool || p.Evidence["compaction_manifest"] != p.Manifest {
		return fmt.Errorf("artifact continuation frozen implementation or intent differs")
	}
	if p.Evidence["pause_ack"].Path != filepath.Join(p.OriginalHost.ProjectDir, ".noodle", "control-ack.ndjson") {
		return fmt.Errorf("artifact continuation original pause source is missing")
	}
	for _, operation := range []string{"status", "cleanup"} {
		ref := p.Evidence["original_"+operation]
		var response hostResponse
		if ref.Path != filepath.Join(p.Directory, "original-"+operation+".json") {
			return fmt.Errorf("artifact continuation original CLI evidence is missing")
		}
		actual, err := artifactCompactionFile(ref.Path, &response)
		want := "ok"
		if operation == "cleanup" {
			want = "eligible"
		}
		if err != nil || actual != ref || response.Status != want || response.Error != "" || !reflect.DeepEqual(response.hostReceipt, p.OriginalHost) {
			return fmt.Errorf("artifact continuation frozen original CLI evidence differs")
		}
	}
	return artifactCompactionEvidence(p, p.Evidence)
}

func artifactCompactionObserveOwner(p artifactCompactionRequest) error {
	cfg, _, err := config.LoadRuntime(p.OriginalHost.ProjectDir)
	if err != nil {
		return err
	}
	owner, err := loop.ReadArtifactCompactionOwner(p.OriginalHost.ProjectDir, cfg, p.Owner.Binding.OrderID, p.Owner.Binding.SessionID, p.OriginalHost.ContextID, p.PauseID)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(owner, p.Owner) {
		return fmt.Errorf("artifact continuation canonical, metadata or publication binding changed")
	}
	return nil
}

func resumeArtifactCompaction(ctx context.Context, path, expected string, apply bool) (loopruntime.SessionReadbackFileRef, error) {
	var p artifactCompactionRequest
	ref, err := artifactCompactionFile(path, &p)
	if err != nil {
		return ref, err
	}
	if expected == "" || ref.SHA256 != expected {
		return ref, fmt.Errorf("artifact continuation request digest differs")
	}
	if err := validateArtifactCompactionRequest(p, ref); err != nil {
		return ref, err
	}
	if err := artifactCompactionObserveOwner(p); err != nil {
		return ref, err
	}
	observed, err := artifactCompactionReadHost(ctx, p)
	if err != nil {
		return ref, err
	}
	lock, err := artifactCompactionLock(p.OriginalHost.ProjectDir)
	if err != nil {
		return ref, err
	}
	defer lock.Close()
	journalLock, err := lockfile.TryLock(filepath.Join(p.Directory, "continuation.lock"))
	if err != nil {
		return ref, err
	}
	defer journalLock.Close()
	if err := validateArtifactCompactionRequest(p, ref); err != nil {
		return ref, err
	}
	if err := artifactCompactionObserveOwner(p); err != nil {
		return ref, err
	}
	if err := workspaceartifacts.VerifyCompactedPublication(ctx, p.OriginalHost.ProjectDir, p.Owner.Baseline, p.Owner.Result, p.Owner.Head); err != nil {
		return ref, err
	}
	if !apply {
		return ref, nil
	}
	return applyArtifactCompaction(ctx, p, ref, observed)
}

type artifactCompactionReceipt struct {
	Schema               int                                           `json:"schema"`
	Status               string                                        `json:"status"`
	Request              loopruntime.SessionReadbackFileRef            `json:"request"`
	ToolVersion          string                                        `json:"tool_version"`
	Tool                 loopruntime.SessionReadbackFileRef            `json:"tool"`
	Manifest             loopruntime.SessionReadbackFileRef            `json:"manifest"`
	OriginalContextID    string                                        `json:"original_context_id"`
	OriginalHostInstance string                                        `json:"original_host_instance"`
	NativeReclamation    string                                        `json:"native_reclamation"`
	Sources              map[string]loopruntime.SessionReadbackFileRef `json:"sources"`
	Error                string                                        `json:"error"`
}

func applyArtifactCompaction(ctx context.Context, p artifactCompactionRequest, request loopruntime.SessionReadbackFileRef, observed map[string][]byte) (loopruntime.SessionReadbackFileRef, error) {
	var zero loopruntime.SessionReadbackFileRef
	completion := filepath.Join(p.Directory, "completion.json")
	if _, err := os.Lstat(completion); err == nil {
		var receipt artifactCompactionReceipt
		ref, err := artifactCompactionFile(completion, &receipt)
		if err != nil {
			return zero, err
		}
		if receipt.Schema != 1 || receipt.Status != "artifact_copies_compacted" || receipt.Request != request || receipt.Tool != p.Tool || receipt.ToolVersion != p.ToolVersion || receipt.Manifest != p.Manifest || receipt.OriginalContextID != p.OriginalHost.ContextID || receipt.OriginalHostInstance != p.OriginalHost.HostInstanceID || receipt.NativeReclamation != "not_performed" || receipt.Error != "" {
			return zero, fmt.Errorf("artifact continuation completion receipt differs")
		}
		for name, ref := range p.Evidence {
			if receipt.Sources[name] != ref {
				return zero, fmt.Errorf("artifact continuation completion source coverage differs")
			}
		}
		if receipt.Sources["continuation_request"] != request || receipt.Sources["continuation_attempt"].SHA256 == "" || receipt.Sources["fresh_status"].SHA256 == "" || receipt.Sources["fresh_cleanup"].SHA256 == "" {
			return zero, fmt.Errorf("artifact continuation completion attempt evidence is incomplete")
		}
		if err := artifactCompactionCheckRefs(receipt.Sources); err != nil {
			return zero, err
		}
		for _, root := range artifactCompactionRoots(p) {
			if _, err := os.Lstat(root); !os.IsNotExist(err) {
				return zero, fmt.Errorf("artifact continuation completed copy reappeared or is unresolved")
			}
		}
		return ref, nil
	} else if !os.IsNotExist(err) {
		return zero, err
	}
	id, err := hostID()
	if err != nil {
		return zero, err
	}
	sources := maps.Clone(p.Evidence)
	sources["continuation_request"] = request
	for operation, data := range observed {
		path := filepath.Join(p.Directory, id+"-original-"+operation+".json")
		if err := filex.WriteFileAtomicDurable(path, data); err != nil {
			return zero, err
		}
		ref, err := artifactCompactionFile(path, nil)
		if err != nil {
			return zero, err
		}
		sources["fresh_"+operation] = ref
	}
	receipt := artifactCompactionReceipt{Schema: 1, Status: "continuing_original_intent", Request: request, ToolVersion: p.ToolVersion, Tool: p.Tool, Manifest: p.Manifest, OriginalContextID: p.OriginalHost.ContextID, OriginalHostInstance: p.OriginalHost.HostInstanceID, NativeReclamation: "not_performed", Sources: sources}
	intent, err := artifactCompactionWrite(filepath.Join(p.Directory, id+"-intent.json"), receipt)
	if err != nil {
		return zero, err
	}
	sources["continuation_attempt"] = intent
	if err := artifactCompactionEvidence(p, sources); err != nil {
		return zero, err
	}
	if err := artifactCompactionSync(sources); err != nil {
		return zero, err
	}
	err = workspaceartifacts.ResumePinnedRetainedPublication(ctx, p.OriginalHost.ProjectDir, p.Owner.Baseline, p.Owner.Result, p.Owner.Head, p.Manifest.SHA256)
	if err == nil {
		err = artifactCompactionCheckRefs(sources)
	}
	if err == nil {
		for _, root := range artifactCompactionRoots(p) {
			if _, statErr := os.Lstat(root); !os.IsNotExist(statErr) {
				err = fmt.Errorf("artifact continuation copy absence is unresolved")
				break
			}
		}
	}
	if err != nil {
		receipt.Status, receipt.Error = "UNKNOWN", err.Error()
		ref, writeErr := artifactCompactionWrite(filepath.Join(p.Directory, id+"-unknown.json"), receipt)
		if writeErr != nil {
			return ref, fmt.Errorf("artifact continuation unresolved and receipt persistence failed: %w", writeErr)
		}
		return ref, err
	}
	receipt.Status = "artifact_copies_compacted"
	return artifactCompactionWrite(completion, receipt)
}
