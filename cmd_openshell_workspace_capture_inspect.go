package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/execution"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/loop"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/poteto/noodle/worktree"
	"github.com/spf13/cobra"
)

func newInspectOpenShellWorkspaceCaptureCmd() *cobra.Command {
	var capture, expected string
	cmd := &cobra.Command{Use: "inspect-workspace-capture", Short: "Verify original archive recovery evidence without contacting the Host or VM", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
		defer cancel()
		result, err := inspectOriginalWorkspaceCapture(ctx, loopruntime.SessionReadbackFileRef{Path: capture, SHA256: expected})
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(result)
	}}
	cmd.Flags().StringVar(&capture, "capture-file", "", "Original workspace-capture.json path")
	cmd.Flags().StringVar(&expected, "expected-sha256", "", "Reviewed capture FileRef SHA256")
	_ = cmd.MarkFlagRequired("capture-file")
	_ = cmd.MarkFlagRequired("expected-sha256")
	return cmd
}

func inspectOriginalWorkspaceCapture(ctx context.Context, ref loopruntime.SessionReadbackFileRef) (map[string]any, error) {
	proof, err := loopruntime.ReadOpenShellWorkspaceCapture(ref)
	if err != nil {
		return nil, err
	}
	request := proof.Sources["request"]
	var binding hostRequestBinding
	actual, err := loopruntime.ReadSessionReadbackFile(request.Path, &binding)
	if err != nil {
		return nil, err
	}
	if actual != request || binding.Schema != 1 || binding.Operation != "attach" || binding.Status != "accepted" {
		return nil, fmt.Errorf("archive inspection original accepted request differs")
	}
	owner, err := readHostOwnerBinding(request.Path)
	if err != nil {
		return nil, err
	}
	if owner.Request != request || binding.RequiredHostInstanceID != owner.Original.HostInstanceID || owner.Current.HostInstanceID != owner.Original.HostInstanceID || owner.Current.ContextState != "active" || owner.Current.ContextID != owner.Original.ContextID {
		return nil, fmt.Errorf("archive inspection original Host binding differs")
	}
	data, err := readWorkspaceCaptureConfig(binding)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Parse(data)
	if err != nil {
		return nil, err
	}
	profileRef := proof.Sources["profile"]
	profile, err := loadSessionObserverProfile(cfg.Runtime.OpenShell.Profile, profileRef)
	if err != nil {
		return nil, err
	}
	runtimeDir := filepath.Join(binding.ProjectDir, ".noodle")
	var snapshot reducer.DurableSnapshot
	snapshotRef, err := loopruntime.ReadSessionReadbackFile(filepath.Join(runtimeDir, "state.snapshot.json"), &snapshot)
	if err != nil {
		return nil, err
	}
	scope, attempt, err := loop.ArchiveRecoveryScope(cfg, snapshot, proof.OrderID, proof.SessionID)
	if err != nil {
		return nil, err
	}
	var integration struct {
		Branch string `json:"branch"`
		Root   string `json:"root"`
	}
	integrationRef, err := loopruntime.ReadSessionReadbackFile(filepath.Join(runtimeDir, "integration-branch.json"), &integration)
	if err != nil {
		return nil, err
	}
	if integration.Branch == "" || integration.Root != binding.ProjectDir {
		return nil, fmt.Errorf("archive inspection original integration binding differs")
	}
	scope.IntegrationBranch = integration.Branch
	e, err := execution.New(binding.ProjectDir, os.Environ())
	if err != nil {
		return nil, err
	}
	if err = validateHostEnvironment(e); err != nil {
		return nil, err
	}
	custody, err := worktree.ReadCustody(ctx, e, filepath.Join(binding.ProjectDir, ".worktrees", attempt.WorktreeName))
	if err != nil {
		return nil, err
	}
	session, err := filepath.EvalSymlinks(filepath.Join(runtimeDir, "sessions", attempt.SessionID))
	if err != nil {
		return nil, err
	}
	p := loopruntime.ProgressMaintenance{Request: request, Profile: profileRef, HostDir: owner.HostRoot, ContextID: owner.Current.ContextID, SessionDirectory: session}
	tree, err := loopruntime.InspectOpenShellWorkspaceCapture(ctx, ref, p, profile, binding.ProjectDir, custody.Physical, custody.CommonDir, scope)
	if err != nil {
		return nil, err
	}
	sources := owner.Sources
	sources["canonical_snapshot"], sources["integration_binding"] = snapshotRef, integrationRef
	if err = loopruntime.VerifySessionReadbackFiles(sources); err != nil {
		return nil, err
	}
	if _, err = readWorkspaceCaptureConfig(binding); err != nil {
		return nil, err
	}
	return map[string]any{
		"schema": 1, "status": "archive_recovery_review_evidence", "review_eligibility": "pending_owner_review", "original_return": "UNKNOWN",
		"order_id": proof.OrderID, "attempt_id": attempt.AttemptID, "native_attempt_id": proof.NativeAttemptID, "session_id": attempt.SessionID,
		"sandbox_id": proof.SandboxID, "main_process_instance_id": proof.MainProcessInstanceID, "capture": ref, "archive": proof.Archive,
		"baseline_head": proof.BaselineHead, "baseline_index_sha256": proof.BaselineIndexSHA256, "baseline_source": proof.BaselineSource,
		"tree": tree, "write_paths": scope.WritePaths, "sources": sources, "authorizes_landing": false, "effects": []string{}, "next": nil,
	}, nil
}
