package main

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/lockfile"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/spf13/cobra"
)

func newCaptureOpenShellWorkspaceCmd() *cobra.Command {
	var request, order, session, output string
	cmd := &cobra.Command{Use: "capture-workspace", Short: "Verify a private archive of the original Ready OpenShell workspace after archive ENOSPC", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, cancel := context.WithTimeout(cmd.Context(), 5*time.Minute)
		defer cancel()
		ref, err := captureOriginalOpenShellWorkspace(ctx, request, order, session, output)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"status": "workspace_archive_verified", "original_return": "UNKNOWN", "receipt": ref, "effects": []string{"workspace_archive_captured"}})
	}}
	for name, target := range map[string]*string{"request-file": &request, "order-id": &order, "session-id": &session, "output": &output} {
		cmd.Flags().StringVar(target, name, "", "Original binding or fresh private output directory")
		_ = cmd.MarkFlagRequired(name)
	}
	return cmd
}

func captureOriginalOpenShellWorkspace(ctx context.Context, request, order, session, output string) (ref loopruntime.SessionReadbackFileRef, err error) {
	if !filepath.IsAbs(request) || !filepath.IsAbs(output) || filepath.Clean(output) != output || filepath.Base(session) != session || session == "." || session == ".." || order == "" {
		return ref, fmt.Errorf("workspace capture requires absolute paths and the original order and session")
	}
	var binding hostRequestBinding
	requestRef, err := loopruntime.ReadSessionReadbackFile(request, &binding)
	if err != nil {
		return ref, err
	}
	if binding.Status != "accepted" || binding.RequiredHostInstanceID == "" || !filepath.IsAbs(binding.ProjectDir) || (binding.ConfigPath != "" && !filepath.IsAbs(binding.ConfigPath)) {
		return ref, fmt.Errorf("workspace capture requires the original accepted Host request")
	}
	selected, err := readHostOwnerBinding(request)
	if err != nil {
		return ref, err
	}
	if selected.Request != requestRef || selected.Current.ContextState != "active" || selected.Original.HostInstanceID != binding.RequiredHostInstanceID || selected.Current.HostInstanceID != selected.Original.HostInstanceID || selected.Current.ContextID != selected.Original.ContextID {
		return ref, fmt.Errorf("workspace capture requires the same active Host generation")
	}
	data, err := readWorkspaceCaptureConfig(binding)
	if err != nil {
		return ref, err
	}
	cfg, err := config.Parse(data)
	if err != nil {
		return ref, err
	}
	profile, err := loopruntime.LoadOpenShellProfile(cfg.Runtime.OpenShell.Profile)
	if err != nil {
		return ref, err
	}
	parent, err := workspaceCapturePrivateParent(filepath.Dir(output), binding.ProjectDir, profile.Gateway.Binding.StateRoot, binding.HostDir)
	if err != nil {
		return ref, err
	}
	dir, err := filepath.EvalSymlinks(filepath.Join(binding.ProjectDir, ".noodle", "sessions", session))
	if err != nil || filepath.Base(dir) != session {
		return ref, fmt.Errorf("workspace capture original session directory unavailable")
	}
	profileRef, err := loopruntime.ReadSessionReadbackFile(profile.SourcePath, nil)
	if err != nil {
		return ref, err
	}
	lock, err := lockfile.TryLock(filepath.Join(filepath.Dir(filepath.Dir(dir)), "control.lock"))
	if err != nil {
		return ref, err
	}
	defer lock.Close()
	verify := func() error {
		current, err := readHostOwnerBinding(request)
		if err != nil {
			return err
		}
		if current.Request != requestRef || !reflect.DeepEqual(current.Current, selected.Current) || current.ExecutionBinary != selected.ExecutionBinary || current.GenerationDir != selected.GenerationDir {
			return fmt.Errorf("workspace capture original owner changed")
		}
		if _, err := readWorkspaceCaptureConfig(binding); err != nil {
			return err
		}
		return verifyWorkspaceCaptureHost(current)
	}
	p := loopruntime.ProgressMaintenance{Request: requestRef, Profile: profileRef, ContextID: selected.Current.ContextID, SessionDirectory: dir}
	return loopruntime.CaptureOpenShellWorkspace(ctx, p, profile, order, filepath.Join(parent, filepath.Base(output)), verify)
}

func workspaceCapturePrivateParent(parent string, protected ...string) (string, error) {
	parent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", err
	}
	for _, root := range protected {
		rel, err := filepath.Rel(root, parent)
		if err != nil || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
			return "", fmt.Errorf("workspace capture output must be outside original custody and project")
		}
	}
	return parent, nil
}

func readWorkspaceCaptureConfig(binding hostRequestBinding) ([]byte, error) {
	name := binding.ConfigPath
	if name == "" {
		name = filepath.Join(binding.ProjectDir, config.DefaultConfigPath)
	}
	source, err := loopruntime.ReadSessionReadbackFile(name, nil)
	if err != nil {
		return nil, err
	}
	data, err := readHostRequestConfig(hostRequest{ProjectDir: binding.ProjectDir, ConfigPath: binding.ConfigPath})
	if err != nil {
		return nil, err
	}
	if source.SHA256 != binding.ConfigSHA256 || hostDigest(data) != binding.ConfigSHA256 {
		return nil, fmt.Errorf("workspace capture original configuration changed")
	}
	return data, nil
}

func verifyWorkspaceCaptureHost(owner *hostOwnerBinding) error {
	packet, err := progressMaintenanceResources(owner.GenerationDir)
	if err != nil {
		return err
	}
	var fields struct {
		Schema     int    `json:"schema"`
		Protocol   string `json:"protocol"`
		HostDir    string `json:"host_dir"`
		InstanceID string `json:"host_instance_id"`
		PID        int    `json:"host_pid"`
		BinarySHA  string `json:"binary_sha256"`
		Contexts   int    `json:"contexts"`
		Shutdown   struct {
			Requested *bool `json:"requested"`
		} `json:"shutdown"`
	}
	data, err := json.Marshal(packet)
	if err != nil {
		return err
	}
	if err = json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if fields.Schema != 1 || fields.Protocol != hostProtocol || fields.HostDir != owner.GenerationDir || fields.InstanceID != owner.Current.HostInstanceID || fields.PID != owner.Current.HostPID || fields.BinarySHA != owner.ExecutionBinary.SHA256 || fields.Contexts != 1 || fields.Shutdown.Requested == nil || *fields.Shutdown.Requested {
		return fmt.Errorf("workspace capture requires the original live Host with only its original context")
	}
	return nil
}
