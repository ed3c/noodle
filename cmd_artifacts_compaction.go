package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/loop"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/spf13/cobra"
)

type artifactCompactionRequest struct {
	Schema          int                                           `json:"schema"`
	ToolVersion     string                                        `json:"tool_version"`
	Tool            loopruntime.SessionReadbackFileRef            `json:"tool"`
	OriginalRequest loopruntime.SessionReadbackFileRef            `json:"original_request"`
	OriginalBinary  loopruntime.SessionReadbackFileRef            `json:"original_binary"`
	OriginalHost    hostReceipt                                   `json:"original_host"`
	PauseID         string                                        `json:"pause_id"`
	Directory       string                                        `json:"directory"`
	Owner           loop.ArtifactCompactionOwner                  `json:"owner"`
	Manifest        loopruntime.SessionReadbackFileRef            `json:"manifest"`
	Evidence        map[string]loopruntime.SessionReadbackFileRef `json:"evidence"`
}

// Tests replace only this original CLI read boundary. Production always executes
// the original immutable binary; no IPC packet impersonates its binary digest.
var artifactNativeRead = artifactOriginalCLI

func newArtifactsCompactionCmd() *cobra.Command {
	root := &cobra.Command{Use: "artifacts", Short: "Continue previously authorized local artifact compaction", PersistentPreRunE: func(*cobra.Command, []string) error { return nil }}
	var original, session, pause, output string
	prepare := &cobra.Command{Use: "prepare-compaction", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		ref, err := prepareArtifactCompaction(cmd.Context(), original, session, pause, output)
		return artifactCompactionOutput(cmd, ref, "prepared", err)
	}}
	for name, ptr := range map[string]*string{"original-request-file": &original, "session-id": &session, "pause-id": &pause, "output": &output} {
		prepare.Flags().StringVar(ptr, name, "", "Exact original identity or new external evidence directory")
		_ = prepare.MarkFlagRequired(name)
	}
	var request, expected string
	var apply bool
	resume := &cobra.Command{Use: "resume-compaction", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		ref, err := resumeArtifactCompaction(cmd.Context(), request, expected, apply)
		status := "eligible"
		if apply {
			status = "artifact_copies_compacted"
		}
		return artifactCompactionOutput(cmd, ref, status, err)
	}}
	resume.Flags().StringVar(&request, "request-file", "", "Immutable artifact continuation request")
	resume.Flags().StringVar(&expected, "expected-sha256", "", "Frozen continuation request digest")
	resume.Flags().BoolVar(&apply, "apply", false, "Continue only the original five-copy deletion intent")
	_ = resume.MarkFlagRequired("request-file")
	_ = resume.MarkFlagRequired("expected-sha256")
	root.AddCommand(prepare, resume)
	return root
}

func artifactCompactionOutput(cmd *cobra.Command, ref loopruntime.SessionReadbackFileRef, status string, err error) error {
	if err != nil {
		status = "UNKNOWN"
	}
	result := map[string]any{"schema": 1, "owner": "noodle.artifacts", "status": status, "receipt": ref, "native_reclamation": "not_performed", "original_task_outcome": "unchanged"}
	if err != nil {
		result["error"] = err.Error()
	}
	if outputErr := json.NewEncoder(cmd.OutOrStdout()).Encode(result); outputErr != nil {
		return outputErr
	}
	return err
}

func artifactOriginalCLI(ctx context.Context, binary, project, hostDir, request, operation string) ([]byte, error) {
	if operation != "status" && operation != "cleanup" {
		return nil, fmt.Errorf("artifact continuation original operation is not read-only")
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "--project-dir", project, "host", operation, "--request-file", request)
	cmd.Dir = project
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "NOODLE_HOST_DIR=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "NOODLE_HOST_DIR="+hostDir)
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("original Host %s readback failed; no artifact deletion authorized", operation)
	}
	if len(data) > 4<<20 {
		return nil, fmt.Errorf("original Host readback exceeds evidence bound")
	}
	return data, nil
}

func artifactCompactionHost(request loopruntime.SessionReadbackFileRef) (binding hostRequestBinding, receipt hostReceipt, refs map[string]loopruntime.SessionReadbackFileRef, err error) {
	actual, err := loopruntime.ReadSessionReadbackFile(request.Path, &binding)
	if err != nil {
		return binding, receipt, nil, err
	}
	if actual != request || binding.Schema != 1 || binding.Operation != "attach" || binding.Status != "ok" || !binding.CleanupCompleted || binding.RequestID == "" || binding.EnvironmentSHA256 == "" || !filepath.IsAbs(binding.HostDir) || !filepath.IsAbs(binding.ProjectDir) || !filepath.IsAbs(binding.ConfigPath) {
		return binding, receipt, nil, fmt.Errorf("artifact continuation original Host request is incomplete")
	}
	refs = map[string]loopruntime.SessionReadbackFileRef{"original_request": request}
	var receipts []hostReceipt
	ref, err := loopruntime.ReadSessionReadbackFile(filepath.Join(binding.HostDir, "receipts.json"), &receipts)
	if err != nil {
		return binding, receipt, nil, err
	}
	refs["host_receipts"] = ref
	count := 0
	for _, row := range receipts {
		if row.RequestID != binding.RequestID {
			continue
		}
		if row.ProjectDir != binding.ProjectDir || row.HostDir != binding.HostDir || row.BinarySHA256 != binding.BinarySHA256 || row.ConfigPath != binding.ConfigPath || row.ConfigSHA256 != binding.ConfigSHA256 || row.Mode != binding.Mode || row.CleanupCompleted != binding.CleanupCompleted || row.ContextState != "active" || row.HostInstanceID == "" || row.ContextID == "" || row.HostPID <= 1 || !filepath.IsAbs(row.BinaryPath) || (binding.RequiredHostInstanceID != "" && binding.RequiredHostInstanceID != row.HostInstanceID) {
			return binding, receipt, nil, fmt.Errorf("artifact continuation original Host receipt differs")
		}
		receipt = row
		count++
	}
	if count != 1 {
		return binding, receipt, nil, fmt.Errorf("artifact continuation original active Host is missing or ambiguous")
	}
	ref, err = artifactCompactionFile(receipt.BinaryPath, nil)
	if err != nil || ref.SHA256 != receipt.BinarySHA256 {
		return binding, receipt, nil, fmt.Errorf("artifact continuation original binary changed")
	}
	refs["original_binary"] = ref
	ref, err = artifactCompactionFile(binding.ConfigPath, nil)
	if err != nil || ref.SHA256 != binding.ConfigSHA256 {
		return binding, receipt, nil, fmt.Errorf("artifact continuation original config changed")
	}
	refs["config"] = ref
	var source config.RuntimeSource
	ref, err = artifactCompactionFile(filepath.Join(binding.ProjectDir, ".noodle", "host-config.json"), &source)
	if err != nil || source.Path != binding.ConfigPath || source.SHA256 != binding.ConfigSHA256 {
		return binding, receipt, nil, fmt.Errorf("artifact continuation original config reference differs")
	}
	refs["host_config"] = ref
	return binding, receipt, refs, nil
}

func artifactCompactionReadHost(ctx context.Context, p artifactCompactionRequest) (map[string][]byte, error) {
	if _, _, _, err := artifactCompactionHost(p.OriginalRequest); err != nil {
		return nil, err
	}
	observed := map[string][]byte{}
	for _, operation := range []string{"status", "cleanup"} {
		data, err := artifactNativeRead(ctx, p.OriginalBinary.Path, p.OriginalHost.ProjectDir, p.OriginalHost.HostDir, p.OriginalRequest.Path, operation)
		if err != nil {
			return nil, err
		}
		var response hostResponse
		if err := json.Unmarshal(data, &response); err != nil {
			return nil, err
		}
		want := "ok"
		if operation == "cleanup" {
			want = "eligible"
		}
		if response.Status != want || response.Error != "" || !reflect.DeepEqual(response.hostReceipt, p.OriginalHost) {
			return nil, fmt.Errorf("original Host %s is not bound eligible readback", operation)
		}
		observed[operation] = data
	}
	return observed, nil
}
