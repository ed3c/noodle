package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/procx"
	"github.com/poteto/noodle/loop"
	loopruntime "github.com/poteto/noodle/runtime"
)

type hostSessionObserver struct {
	Binary          loopruntime.SessionReadbackFileRef `json:"binary"`
	ExecutionBinary loopruntime.SessionReadbackFileRef `json:"execution_binary"`
	InactiveScope   string                             `json:"inactive_scope"`
	Before          hostSessionObserverFrame           `json:"before"`
	After           hostSessionObserverFrame           `json:"after"`
}
type hostSessionObserverFrame struct {
	Status    hostResponse       `json:"status"`
	Resources json.RawMessage    `json:"resources"`
	Host      hostOperatorActor  `json:"host"`
	Gateway   hostOperatorActor  `json:"gateway"`
	Resident  admission.Resident `json:"resident"`
}

type sessionObserverResources struct {
	Protocol     string `json:"protocol"`
	Root         string `json:"host_root"`
	Directory    string `json:"host_dir"`
	Binary       string `json:"binary_path"`
	SHA          string `json:"binary_sha256"`
	PID          int    `json:"host_pid"`
	Instance     string `json:"host_instance_id"`
	RollingError string `json:"rolling_error"`
	Shutdown     *struct {
		Requested *bool `json:"requested"`
	} `json:"shutdown"`
	Residents *struct {
		Records []admission.Resident `json:"records"`
		Error   json.RawMessage      `json:"error"`
	} `json:"residents"`
	Resources *struct {
		Active     *int                           `json:"active_workers"`
		Unresolved *int                           `json:"unresolved_residents"`
		Reason     string                         `json:"reason"`
		Quota      *admission.SharedQuotaSnapshot `json:"shared_quota"`
	} `json:"resources"`
}

// The subject's original executable makes both official read-only calls. The
// Observer never sends a forged current-caller SHA to an older Host actor.
func readOriginalSessionHostCLI(ctx context.Context, owner *hostOwnerBinding, requestFile, operation string, into any) error {
	if operation != "status" && operation != "resources" {
		return fmt.Errorf("session observer operation is not read-only: %s", operation)
	}
	args := []string{"--project-dir", owner.Current.ProjectDir, "host", operation}
	if operation == "status" {
		args = append(args, "--request-file", requestFile)
	}
	cmd := exec.CommandContext(ctx, owner.ExecutionBinary.Path, args...)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "NOODLE_HOST_DIR=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, "NOODLE_HOST_DIR="+owner.GenerationDir)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err = cmd.Start(); err != nil {
		return err
	}
	raw, readErr := io.ReadAll(io.LimitReader(pipe, (1<<20)+1))
	_ = pipe.Close()
	waitErr := cmd.Wait()
	if readErr != nil {
		return readErr
	}
	if waitErr != nil {
		return fmt.Errorf("original Host %s readback unavailable: %w", operation, waitErr)
	}
	if len(raw) > 1<<20 {
		return fmt.Errorf("original Host %s response exceeds bound", operation)
	}
	return json.Unmarshal(raw, into)
}

func observeSessionActor(pid int, binary loopruntime.SessionReadbackFileRef) (hostOperatorActor, error) {
	identity, err := procx.ObserveNativeIdentity(pid)
	if err != nil {
		return hostOperatorActor{}, err
	}
	if identity.UID != os.Getuid() || identity.Zombie || identity.Stopped {
		return hostOperatorActor{}, fmt.Errorf("original session actor lifetime differs")
	}
	image, err := procx.ObserveNativeImage(identity)
	if err != nil {
		return hostOperatorActor{}, err
	}
	current, err := hostReadbackBinaryRef(image)
	if err != nil || current != binary {
		return hostOperatorActor{}, fmt.Errorf("original session actor executable differs")
	}
	return hostOperatorActor{Process: identity, Binary: binary}, nil
}

func readSessionObserverFrame(ctx context.Context, owner *hostOwnerBinding, requestFile string, spec loopruntime.OpenShellGatewaySpec) (frame hostSessionObserverFrame, err error) {
	if err = readOriginalSessionHostCLI(ctx, owner, requestFile, "status", &frame.Status); err != nil {
		return frame, err
	}
	if err = readOriginalSessionHostCLI(ctx, owner, requestFile, "resources", &frame.Resources); err != nil {
		return frame, err
	}
	if err = validateSessionObserverResources(owner, frame.Status, frame.Resources, spec, &frame.Resident); err != nil {
		return frame, err
	}
	frame.Host, err = observeSessionActor(owner.Current.HostPID, owner.ExecutionBinary)
	if err != nil {
		return frame, err
	}
	binary, err := hostReadbackBinaryRef(spec.Resident.Argv[0])
	if err != nil {
		return frame, err
	}
	frame.Gateway, err = observeSessionActor(frame.Resident.PID, binary)
	if err != nil {
		return frame, err
	}
	if frame.Gateway.Process.ParentPID != frame.Host.Process.PID || frame.Gateway.Process.Group != frame.Resident.ProcessGroup {
		return frame, fmt.Errorf("original gateway resident ancestry differs")
	}
	// Read only the named resident, rather than enumerating unrelated processes.
	process := exec.CommandContext(ctx, "ps", "-p", fmt.Sprint(frame.Resident.PID), "-o", "lstart=")
	process.Env = append(os.Environ(), "LC_ALL=C")
	raw, err := process.Output()
	if err != nil || strings.Join(strings.Fields(string(raw)), " ") != frame.Resident.Started {
		return frame, fmt.Errorf("original gateway resident start identity differs")
	}
	return frame, nil
}

func validateSessionObserverResources(owner *hostOwnerBinding, status hostResponse, raw json.RawMessage, spec loopruntime.OpenShellGatewaySpec, resident *admission.Resident) error {
	r := owner.Current
	if status.Schema != 1 || status.Protocol != hostProtocol || status.Mode != r.Mode || status.ConfigPath != r.ConfigPath || status.CleanupCompleted != r.CleanupCompleted || status.Status != "ok" || status.Error != "" || status.ContextState != "active" || status.HostPID != r.HostPID || status.HostInstanceID != r.HostInstanceID || status.ContextID != r.ContextID || status.RequestID != r.RequestID || status.ProjectDir != r.ProjectDir || status.HostDir != owner.HostRoot || status.BinaryPath != owner.ExecutionBinary.Path || status.BinarySHA256 != owner.ExecutionBinary.SHA256 || status.ConfigSHA256 != r.ConfigSHA256 {
		return fmt.Errorf("original session Host status binding differs")
	}
	var resource sessionObserverResources
	if err := json.Unmarshal(raw, &resource); err != nil {
		return err
	}
	if resource.Protocol != hostProtocol || resource.Root != owner.HostRoot || resource.Directory != owner.GenerationDir || resource.Instance != r.HostInstanceID || resource.PID != r.HostPID || resource.Binary != owner.ExecutionBinary.Path || resource.SHA != owner.ExecutionBinary.SHA256 || resource.RollingError != "" || resource.Shutdown == nil || resource.Shutdown.Requested == nil || *resource.Shutdown.Requested {
		return fmt.Errorf("original session Host resource identity unavailable")
	}
	budget := resource.Resources
	if budget == nil || budget.Active == nil || *budget.Active != 0 || budget.Unresolved == nil || *budget.Unresolved != 0 || budget.Quota == nil || budget.Quota.Error != "" || budget.Reason == "shared_quota_unknown" || budget.Quota.Allocations <= 0 || budget.Quota.Residents <= 0 || budget.Quota.Allocations != budget.Quota.Residents {
		return fmt.Errorf("original Host does not prove zero active workers; exact session inactivity is unknown")
	}
	if resource.Residents == nil || string(resource.Residents.Error) != "null" {
		return fmt.Errorf("original Host resident custody is unknown")
	}
	guard, err := loopruntime.OpenShellGatewayGuardID(spec)
	if err != nil {
		return err
	}
	count := 0
	for _, row := range resource.Residents.Records {
		if row.Name != spec.Resident.Name {
			continue
		}
		if row.ID != spec.Resident.Name || row.Sharing != "shared" || row.State != "running" || row.GuardID != guard || row.Fingerprint == "" || row.PID <= 1 || row.ProcessGroup != row.PID || row.Started == "" || row.StopRequested != "" || row.ForceRequested != "" || !slices.Contains(row.Contexts, r.ContextID) {
			return fmt.Errorf("original gateway resident/context lease differs")
		}
		*resident = row
		count++
	}
	if count != 1 {
		return fmt.Errorf("original gateway resident missing or ambiguous")
	}
	return nil
}

func observeHostSession(ctx context.Context, project, requestFile, orderID, sessionID string, owner *hostOwnerBinding, binary loopruntime.SessionReadbackFileRef) (result hostSessionReadback, err error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	result = hostSessionReadback{SchemaVersion: 2, Status: "refused", HostInstanceID: owner.Current.HostInstanceID, ContextID: owner.Current.ContextID, RequestID: owner.Current.RequestID, Sources: owner.Sources, Effects: []string{}}
	defer func() {
		if err != nil {
			result.Error = err.Error()
		}
	}()
	project, err = filepath.EvalSymlinks(project)
	if err != nil {
		return result, err
	}
	if owner.Current.ProjectDir != project || owner.Current.ContextState != "active" || owner.Current.HostPID <= 1 || owner.ExecutionBinary == binary {
		return result, fmt.Errorf("original observer subject binding differs")
	}
	configPath := owner.Current.ConfigPath
	if configPath == "" {
		configPath = filepath.Join(project, ".noodle.toml")
	}
	result.Sources["client_binary"] = binary
	result.Sources["config"], err = loopruntime.ReadSessionReadbackFile(configPath, nil)
	if err != nil {
		return result, err
	}
	if result.Sources["config"].SHA256 != owner.Current.ConfigSHA256 {
		return result, fmt.Errorf("original observer config changed")
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return result, err
	}
	if hostDigest(raw) != owner.Current.ConfigSHA256 {
		return result, fmt.Errorf("original observer config changed during load")
	}
	cfg, err := config.Parse(raw)
	if err != nil {
		return result, err
	}
	if (len(cfg.Runtime.AdmittedOrders) != 0 && !slices.Contains(cfg.Runtime.AdmittedOrders, orderID)) || !filepath.IsAbs(cfg.Runtime.OpenShell.Profile) {
		return result, fmt.Errorf("session is outside original OpenShell task admission")
	}
	result.Sources["profile"], err = loopruntime.ReadSessionReadbackFile(cfg.Runtime.OpenShell.Profile, nil)
	if err != nil {
		return result, err
	}
	profile, err := loadSessionObserverProfile(cfg.Runtime.OpenShell.Profile, result.Sources["profile"])
	if err != nil {
		return result, err
	}
	profile.Gateway.ControlRoot = owner.HostRoot
	spec := profile.Gateway
	spec.Binding.StateRoot, err = filepath.EvalSymlinks(spec.Binding.StateRoot)
	if err != nil {
		return result, err
	}
	var quota struct {
		Schema int `json:"schema"`
		Leases map[string]struct {
			Owner    string `json:"owner"`
			Context  string `json:"context"`
			Resident bool   `json:"resident"`
			Key      string `json:"resident_key"`
		} `json:"leases"`
	}
	result.Sources["quota"], err = loopruntime.ReadSessionReadbackFile(filepath.Join(owner.HostRoot, "quota", "quota.json"), &quota)
	if err != nil {
		return result, err
	}
	if quota.Schema != 3 || len(quota.Leases) == 0 {
		return result, fmt.Errorf("original observer quota custody unavailable")
	}
	residentLease := false
	for _, lease := range quota.Leases {
		if !lease.Resident {
			return result, fmt.Errorf("original Host worker lease remains; inactivity unknown")
		}
		residentLease = residentLease || (lease.Owner == owner.Current.HostInstanceID && lease.Key == profile.Gateway.Resident.Name)
	}
	if !residentLease {
		return result, fmt.Errorf("original gateway quota lease unavailable")
	}
	before, err := readSessionObserverFrame(ctx, owner, requestFile, spec)
	if err != nil {
		return result, err
	}
	var resource sessionObserverResources
	if err = json.Unmarshal(before.Resources, &resource); err != nil {
		return result, err
	}
	if resource.Resources.Quota.Allocations != len(quota.Leases) {
		return result, fmt.Errorf("original quota source differs from authoritative Host snapshot")
	}
	for _, lease := range quota.Leases {
		if lease.Owner == owner.Current.HostInstanceID && lease.Key == profile.Gateway.Resident.Name && !(slices.Contains(before.Resident.Contexts, lease.Context) || slices.Contains(before.Resident.Released, lease.Context)) {
			return result, fmt.Errorf("original gateway allocation context is not leased by resident")
		}
	}
	observer := &hostSessionObserver{Binary: binary, ExecutionBinary: owner.ExecutionBinary, InactiveScope: "host_zero_workers", Before: before}
	result.Observer = observer
	observation, err := loop.ReadOpenShellSessionEvidence(ctx, project, filepath.Join(project, ".noodle"), owner.Current.ContextID, profile, orderID, sessionID)
	if err != nil {
		return result, err
	}
	after, err := readSessionObserverFrame(ctx, owner, requestFile, spec)
	if err != nil {
		return result, err
	}
	observer.After = after
	if err = verifySessionObserverFrames(before, after); err != nil {
		return result, err
	}
	if err = loopruntime.VerifySessionReadbackFiles(result.Sources); err != nil {
		return result, err
	}
	if err = loopruntime.VerifySessionReadbackFiles(observation.Execution.Sources); err != nil {
		return result, err
	}
	afterRoot, rootErr := filepath.EvalSymlinks(profile.Gateway.Binding.StateRoot)
	if rootErr != nil || afterRoot != spec.Binding.StateRoot {
		return result, fmt.Errorf("original observer custody root changed")
	}
	if err = loopruntime.VerifyOpenShellProfile(profile); err != nil {
		return result, err
	}
	result.Status = "observed"
	result.Observation = &observation
	return result, nil
}

func verifySessionObserverFrames(before, after hostSessionObserverFrame) error {
	if !before.Host.Process.SameGeneration(after.Host.Process) || before.Host.Process.ParentPID != after.Host.Process.ParentPID || before.Host.Process.Group != after.Host.Process.Group || !before.Gateway.Process.SameGeneration(after.Gateway.Process) || before.Gateway.Process.ParentPID != after.Gateway.Process.ParentPID || before.Gateway.Process.Group != after.Gateway.Process.Group || before.Host.Binary != after.Host.Binary || before.Gateway.Binary != after.Gateway.Binary || !reflect.DeepEqual(before.Resident, after.Resident) {
		return fmt.Errorf("original session actors or resident changed during observation")
	}
	return nil
}

func loadSessionObserverProfile(source string, ref loopruntime.SessionReadbackFileRef) (loopruntime.OpenShellProfile, error) {
	profile, err := loopruntime.LoadOpenShellProfile(source)
	if err != nil {
		return profile, err
	}
	return profile, loopruntime.VerifyOpenShellProfileSource(profile, ref)
}
