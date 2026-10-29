package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/dispatcher"
	"github.com/poteto/noodle/internal/execution"
	"github.com/poteto/noodle/internal/reducer"
	"github.com/poteto/noodle/loop"
	"github.com/spf13/cobra"
)

// Only path values and source references survive the process. Credential values
// must come from the caller's environment or the original native home.
type hostContextInputs struct {
	unknown               map[string]json.RawMessage
	StorageRoot           string            `json:"storage_root,omitempty"`
	Home                  string            `json:"home"`
	NativeHome            string            `json:"native_home"`
	Tools                 map[string]string `json:"tools"`
	Environment           map[string]string `json:"environment_sha256"`
	CredentialEnvironment []string          `json:"credential_environment"`
	NativeAuth            bool              `json:"native_auth"`
	NativeConfigSHA       string            `json:"native_config_sha256,omitempty"`
}

func captureHostContextInputs(req hostRequest) (hostContextInputs, error) {
	inputs := hostContextInputs{Tools: map[string]string{}, Environment: map[string]string{}}
	e, err := execution.New(req.ProjectDir, req.Environment)
	if err != nil {
		return inputs, err
	}
	inputs.Home, _ = e.Lookup("HOME")
	if path, readErr := filepath.EvalSymlinks(filepath.Join(req.ProjectDir, ".noodle")); readErr == nil && path != filepath.Join(req.ProjectDir, ".noodle") {
		inputs.StorageRoot = filepath.Dir(path)
	}
	if !filepath.IsAbs(inputs.Home) {
		return inputs, errors.New("context HOME source is missing or relative")
	}
	data, err := readHostRequestConfig(req)
	if err != nil {
		return inputs, err
	}
	if hostDigest(data) != req.ConfigSHA256 {
		return inputs, errors.New("context config source changed")
	}
	cfg, err := config.Parse(data)
	if err != nil {
		return inputs, err
	}
	for _, key := range cfg.Runtime.Process.RebuildEnvironment {
		value, ok := e.Lookup(key)
		if !ok || value == "" || strings.ContainsAny(key, "=\x00") {
			return inputs, fmt.Errorf("required context environment source %q is missing or invalid", key)
		}
		inputs.Environment[key] = hostDigest([]byte(value))
	}
	for _, key := range cfg.Runtime.Process.CredentialEnvironment {
		value, ok := e.Lookup(key)
		if !ok || value == "" || strings.ContainsAny(key, "=\x00") {
			return inputs, fmt.Errorf("credential environment source %q is missing or invalid", key)
		}
		inputs.CredentialEnvironment = append(inputs.CredentialEnvironment, key)
	}
	for name, provider := range map[string]config.ProviderConfig{"codex": cfg.Agents.Codex, "claude": cfg.Agents.Claude} {
		if provider.Path == "" && !(name == "codex" && cfg.Runtime.Default == dispatcher.CodexNativeRuntime) {
			continue
		}
		binary := name
		if provider.Path != "" {
			path, pathErr := e.ResolvePath(provider.Path)
			if pathErr != nil {
				return inputs, pathErr
			}
			candidate := filepath.Join(path, name)
			if _, statErr := os.Stat(candidate); statErr == nil {
				binary = candidate
			} else if !os.IsNotExist(statErr) {
				return inputs, statErr
			}
		}
		cmd := e.Command(context.Background(), req.ProjectDir, binary)
		if cmd.Err != nil {
			return inputs, fmt.Errorf("context tool %s: %w", name, cmd.Err)
		}
		inputs.Tools[binary] = cmd.Path
	}
	if cfg.Runtime.Default == dispatcher.CodexNativeRuntime {
		inputs.NativeHome, _ = e.Lookup("CODEX_HOME")
		if inputs.NativeHome == "" {
			inputs.NativeHome = filepath.Join(inputs.Home, ".codex")
		}
		if !filepath.IsAbs(inputs.NativeHome) {
			return inputs, errors.New("native home source is relative")
		}
		_, err = os.Stat(filepath.Join(inputs.NativeHome, "auth.json"))
		inputs.NativeAuth = err == nil
		if err != nil && !os.IsNotExist(err) {
			return inputs, err
		}
		data, err := os.ReadFile(filepath.Join(inputs.NativeHome, "config.toml"))
		if err == nil {
			inputs.NativeConfigSHA = hostDigest(data)
		} else if !os.IsNotExist(err) {
			return inputs, err
		}
	}
	return inputs, nil
}

func (inputs hostContextInputs) rebuild(root string, environ []string) ([]string, error) {
	if !filepath.IsAbs(inputs.Home) {
		return nil, errors.New("original task home source is unavailable")
	}
	e, err := execution.New(root, environ)
	if err != nil {
		return nil, err
	}
	// Restore known path sources rather than inheriting the replacement Host's home.
	values := map[string]string{"HOME": inputs.Home}
	if inputs.NativeHome != "" {
		values["CODEX_HOME"] = inputs.NativeHome
	}
	for _, entry := range environ {
		key, value, _ := strings.Cut(entry, "=")
		if _, fixed := values[key]; !fixed {
			values[key] = value
		}
	}
	rebuilt := make([]string, 0, len(values))
	for key, value := range values {
		rebuilt = append(rebuilt, key+"="+value)
	}
	e, err = execution.New(root, rebuilt)
	if err != nil {
		return nil, err
	}
	if err = validateHostEnvironment(e); err != nil {
		return nil, err
	}
	for key, expected := range inputs.Environment {
		value, ok := e.Lookup(key)
		if !ok || hostDigest([]byte(value)) != expected {
			return nil, fmt.Errorf("required context environment source %q is missing or changed", key)
		}
	}
	for _, key := range inputs.CredentialEnvironment {
		value, ok := e.Lookup(key)
		if !ok || value == "" {
			return nil, fmt.Errorf("credential environment source %q is missing", key)
		}
	}
	for binary, path := range inputs.Tools {
		cmd := e.Command(context.Background(), root, binary)
		if cmd.Err != nil || cmd.Path != path {
			return nil, fmt.Errorf("context tool %q source is missing or changed", binary)
		}
	}
	if inputs.NativeAuth {
		info, err := os.Stat(filepath.Join(inputs.NativeHome, "auth.json"))
		if err != nil || !info.Mode().IsRegular() {
			return nil, errors.New("original native credential source is unavailable")
		}
	}
	if inputs.NativeHome != "" {
		info, err := os.Stat(inputs.NativeHome)
		if err != nil || !info.IsDir() {
			return nil, errors.New("original native home source is unavailable")
		}
	}
	if inputs.NativeConfigSHA != "" {
		data, err := os.ReadFile(filepath.Join(inputs.NativeHome, "config.toml"))
		if err != nil || hostDigest(data) != inputs.NativeConfigSHA {
			return nil, errors.New("original native config source is missing or changed")
		}
	}
	return e.Environ(), nil
}

func newHostRebuildCmd() *cobra.Command {
	var requestFile string
	var adoptProfile bool
	cmd := &cobra.Command{Use: "rebuild", Short: "Rebuild a released round from its original task inputs", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		data, err := os.ReadFile(requestFile)
		if err != nil {
			return err
		}
		var binding hostRequestBinding
		if err = json.Unmarshal(data, &binding); err != nil {
			return err
		}
		if binding.Schema != 1 || binding.Status != "accepted" || binding.Operation != "attach" || !filepath.IsAbs(binding.HostDir) {
			return errors.New("original accepted attach binding is unavailable")
		}
		p, err := readHostPool(binding.HostDir)
		if err != nil {
			return err
		}
		owner, ok := p.Owners[binding.RequestID]
		req := hostRequest{Operation: "round_rebuild", ProjectDir: binding.ProjectDir, ConfigPath: binding.ConfigPath, ConfigSHA256: binding.ConfigSHA256, Mode: binding.Mode, RequestID: binding.RequestID, CleanupCompleted: binding.CleanupCompleted, Environment: os.Environ(), AdmissionSHA: binding.BinarySHA256}
		if !ok || owner.Original.BinarySHA256 != binding.BinarySHA256 || owner.EnvironmentSHA != binding.EnvironmentSHA256 || !hostReceiptMatches(req, owner.Original) {
			return errors.New("original task request binding changed")
		}
		req.AdoptCurrentProfile = adoptProfile
		target, ok := p.Generations[p.Active]
		if !ok || target.State != "active" {
			return errors.New("selected native Host is unavailable")
		}
		if owner.State == "owned" && (owner.Current.HostInstanceID != p.Active || (adoptProfile && owner.ControllerOnly)) {
			source, exists := p.Generations[owner.Current.HostInstanceID]
			if !exists {
				return errors.New("original native Host generation is unavailable")
			}
			req.Operation = "round_prepare"
			raw, err := nativeHostRPC(cmd.Context(), source.Directory, req)
			if err != nil {
				return fmt.Errorf("original round release unknown; read original owner: %w", err)
			}
			var reply hostResponse
			if err = json.Unmarshal(raw, &reply); err != nil {
				return err
			}
			if reply.Status != "ok" {
				if _, err = cmd.OutOrStdout().Write(append(raw, '\n')); err != nil {
					return err
				}
				return errors.New(reply.Error)
			}
			req.Operation = "round_rebuild"
		}
		req.BinarySHA256, req.RoundSource, req.RoundState = target.SHA, owner.Current.HostInstanceID, owner.RoundState
		raw, err := nativeHostRPC(cmd.Context(), target.Directory, req)
		if err != nil {
			return fmt.Errorf("context rebuild effect unknown; read original owner: %w", err)
		}
		var reply hostResponse
		if err = json.Unmarshal(raw, &reply); err != nil {
			return err
		}
		if _, err = cmd.OutOrStdout().Write(append(raw, '\n')); err != nil {
			return err
		}
		if reply.Status != "ok" {
			return errors.New(reply.Error)
		}
		return nil
	}}
	cmd.Flags().BoolVar(&adoptProfile, "adopt-current-profile", false, "Select current explicit profile after proving original native thread history")
	cmd.Flags().StringVar(&requestFile, "request-file", "", "Original immutable native attach request")
	_ = cmd.MarkFlagRequired("request-file")
	return cmd
}

func (h *noodleHost) prepareRollingRebuild(ctx context.Context, req hostRequest) hostResponse {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if h.rolling == nil {
		return hostError("refused", errors.New("original rolling Host is unavailable"))
	}
	p, err := readHostPool(h.rolling.root)
	if err != nil {
		return hostError("unknown_effect", err)
	}
	owner, ok := p.Owners[req.RequestID]
	if !ok || (p.Active == h.instanceID && !(req.AdoptCurrentProfile && owner.ControllerOnly)) || owner.Current.HostInstanceID != h.instanceID || !hostReceiptMatches(req, owner.Original) || req.AdmissionSHA != owner.Original.BinarySHA256 {
		return hostError("refused", errors.New("original round or selected successor differs"))
	}
	if owner.State == "ready" {
		return hostResponse{owner.Current, "ok", ""}
	}
	if owner.State != "owned" {
		return hostError("unknown_effect", errors.New("original round ownership is unresolved"))
	}
	c := h.contexts[owner.Current.ContextID]
	if c == nil || c.failed || c.done == nil || h.blocked {
		return hostError("unknown_effect", errors.New("original runtime handle is unavailable"))
	}
	release, ok := c.runtime.(interface {
		ReleaseForSwitch(context.Context) (loop.HostContinuation, error)
	})
	if !ok {
		return hostError("refused", errors.New("original runtime has no round boundary producer"))
	}
	if p.Active != h.instanceID {
		h.rolling.retiring = true
		h.budget.HoldForSwitch()
	}
	_, err = release.ReleaseForSwitch(ctx)
	if errors.Is(err, loop.ErrReleaseBusy) {
		return hostResponse{c.receipt, "busy", err.Error()}
	}
	if err != nil {
		return hostError("unknown_effect", err)
	}
	select {
	case runErr := <-c.done:
		h.finish(c, runErr)
	case <-ctx.Done():
		return hostError("unknown_effect", ctx.Err())
	}
	if !c.failed && !h.blocked && !h.rolling.retiring {
		if err = h.recordRollingBoundary(c); err != nil {
			h.rolling.failure = err.Error()
		}
	}
	if c.failed || h.blocked || h.rolling.failure != "" {
		return hostError("unknown_effect", errors.New("original round release persistence is unresolved"))
	}
	return hostResponse{c.receipt, "ok", ""}
}

func (h *noodleHost) rebuildRollingContext(ctx context.Context, req hostRequest) hostResponse {
	if h.rolling == nil {
		return hostError("refused", errors.New("original native version owner is unavailable"))
	}
	p, err := readHostPool(h.rolling.root)
	if err != nil {
		return hostError("unknown_effect", err)
	}
	owner, ok := p.Owners[req.RequestID]
	if ok && owner.MaintenanceHold != "" {
		return hostError("busy", errors.New("original task is held by restricted maintenance; no rebuild was attempted"))
	}
	if !ok || p.Active != h.instanceID || !hostReceiptMatches(req, owner.Original) || req.AdmissionSHA != owner.Original.BinarySHA256 {
		return hostError("refused", errors.New("original task or selected native owner differs"))
	}
	// A typed refusal without a context is known nonacceptance. A later explicit
	// rebuild may consume the repaired source. Unknown admission never resets.
	if owner.State == "admitting" && owner.LastAdmission != nil && owner.LastAdmission.ContextID == "" && (owner.LastAdmission.Status == "refused" || owner.LastAdmission.Status == "source_drift") && owner.Current.ContextState == "detached" && owner.Current.TerminalCause == loop.TerminalMigrationDetach {
		for _, receipt := range h.receipts {
			if receipt.RequestID != req.RequestID {
				continue
			}
			if !historicalHostContext(owner, receipt.ContextID) {
				return h.reconcileRollingRound(req)
			}
			if receipt.ContextState != "detached" {
				return hostError("unknown_effect", errors.New("historical context release contradicts refused admission"))
			}
		}
		if h.blocked {
			return h.reconcileRollingRound(req)
		}
		if err = updateHostPool(h.rolling.root, func(p *hostPool) error {
			current := p.Owners[req.RequestID]
			if current.State != "admitting" || current.Current != owner.Current || current.LastAdmission == nil || *current.LastAdmission != *owner.LastAdmission {
				return errors.New("original refused admission changed")
			}
			current.State = "ready"
			p.Owners[req.RequestID] = current
			return nil
		}); err != nil {
			return hostError("unknown_effect", err)
		}
		owner.State = "ready"
	}
	// A duplicate or lost reply reads the actual owner; it never creates an attempt.
	if (owner.Current.HostInstanceID == h.instanceID && owner.State != "ready") || owner.State == "admitting" {
		return h.reconcileRollingRound(req)
	}
	if owner.State != "ready" || owner.Current.ContextState != "detached" || owner.Current.TerminalCause != loop.TerminalMigrationDetach || owner.RoundState == nil || owner.Current.HostInstanceID != req.RoundSource {
		return hostError("unknown_effect", errors.New("original round release is not confirmed; no replay attempted"))
	}
	data, err := readHostRequestConfig(req)
	if err != nil {
		return hostError("refused", err)
	}
	if hostDigest(data) != req.ConfigSHA256 {
		return hostError("source_drift", errors.New("original task config changed"))
	}
	if owner.ContextInputs != nil && !owner.ControllerOnly && !req.AdoptCurrentProfile {
		req.Environment, err = owner.ContextInputs.rebuild(req.ProjectDir, req.Environment)
		if err != nil {
			return hostError("refused", err)
		}
	} else {
		req.ControllerOnly = true
		req.DispatchBlocked = owner.RebuildError
		if req.DispatchBlocked == "" {
			req.DispatchBlocked = "original worker execution profile has not been selected"
		}
	}
	e, err := execution.New(req.ProjectDir, req.Environment)
	if err != nil {
		return hostError("refused", err)
	}
	if err = validateHostEnvironment(e); err != nil {
		return hostError("refused", err)
	}
	gitRoot, err := e.Command(ctx, req.ProjectDir, "git", "rev-parse", "--show-toplevel").Output()
	if err != nil || strings.TrimSpace(string(gitRoot)) != e.Root() {
		return hostError("refused", errors.New("original task Git worktree is unavailable or changed"))
	}
	snapshot, err := reducer.ReadSnapshot(filepath.Join(req.ProjectDir, ".noodle", "state.snapshot.json"))
	if err != nil {
		return hostError("refused", fmt.Errorf("original task snapshot is unavailable: %w", err))
	}
	if snapshot.State.Orders == nil {
		return hostError("refused", errors.New("original task snapshot has no order ownership"))
	}
	for _, order := range snapshot.State.Orders {
		for _, stage := range order.Stages {
			for _, attempt := range stage.Attempts {
				if stage.Runtime != dispatcher.CodexNativeRuntime {
					if err = verifyControllerProcessAttempt(filepath.Join(req.ProjectDir, ".noodle"), attempt.SessionID); err != nil {
						return hostError("unknown_effect", err)
					}
					continue
				}
				native, readErr := dispatcher.ReadReleasedNativeSession(ctx, filepath.Join(req.ProjectDir, ".noodle"), attempt.SessionID)
				if readErr != nil {
					return hostError("unknown_effect", fmt.Errorf("original attempt %s readback: %w", attempt.SessionID, readErr))
				}
				if owner.ContextInputs != nil && !req.AdoptCurrentProfile && native.NativeHome != owner.ContextInputs.NativeHome {
					return hostError("refused", errors.New("original native home binding changed"))
				}
				if stage.TaskKey == "local-task" {
					if info, pathErr := os.Stat(native.Worktree); pathErr != nil || !info.IsDir() {
						return hostError("refused", errors.New("original local-task worktree custody is unavailable"))
					}
				}
			}
		}
	}
	if err = hostRecoveryPreflight(filepath.Join(req.ProjectDir, ".noodle")); err != nil {
		return hostError("unknown_effect", err)
	}
	if req.AdoptCurrentProfile {
		inputs, profile, profileErr := selectHostExecutionProfile(ctx, req, snapshot)
		if profileErr != nil {
			return hostError("refused", profileErr)
		}
		if err = updateHostPool(h.rolling.root, func(p *hostPool) error {
			current := p.Owners[req.RequestID]
			if current.State != "ready" || current.Current != owner.Current {
				return errors.New("original controller changed during profile selection")
			}
			current.ContextInputs = &inputs
			current.ExecutionProfile = &profile
			current.ControllerOnly = false
			current.RebuildError = ""
			p.Owners[req.RequestID] = current
			return nil
		}); err != nil {
			return hostError("unknown_effect", err)
		}
		owner.ContextInputs = &inputs
		owner.ControllerOnly = false
		req.ControllerOnly = false
		req.DispatchBlocked = ""
		req.Environment, err = inputs.rebuild(req.ProjectDir, req.Environment)
		if err != nil {
			return hostError("refused", err)
		}
	}
	if req.ControllerOnly {
		if !owner.ControllerOnly && len(snapshot.State.PendingReviews) == 0 {
			return hostError("refused", errors.New("missing worker profile permits only an existing candidate review controller"))
		}
		req.RoundState = &loop.HostContinuation{Mode: "manual", DispatchState: owner.RoundState.DispatchState}
	} else if req.AdoptCurrentProfile {
		req.RoundState = &loop.HostContinuation{Mode: "manual", DispatchState: owner.RoundState.DispatchState}
	} else {
		req.RoundState = owner.RoundState
	}
	req.Operation, req.BinarySHA256 = "attach", h.binarySHA
	req.AdmissionPath = owner.Original.BinaryPath
	return h.attachRolling(ctx, req, req.RoundState)
}

func verifyControllerProcessAttempt(runtimeDir, id string) error {
	if id == "" || filepath.Base(id) != id || id == "." || id == ".." {
		return errors.New("original process session identity is invalid")
	}
	data, err := os.ReadFile(filepath.Join(runtimeDir, "sessions", id, "meta.json"))
	if err != nil {
		return err
	}
	var meta struct {
		SessionID string `json:"session_id"`
		Runtime   string `json:"runtime"`
		Status    string `json:"status"`
		Alive     *bool  `json:"alive"`
	}
	if err = json.Unmarshal(data, &meta); err != nil {
		return err
	}
	if meta.SessionID != id || meta.Runtime != "process" || meta.Alive == nil || *meta.Alive || meta.Status != "exited" {
		return errors.New("original process attempt has no terminal owner readback")
	}
	return nil
}

// Normal live migration carries the original environment. Only a confirmed dead
// producer needs reconstruction; a missing source blocks this task alone.
func (h *noodleHost) rebuildReleasedRounds(ctx context.Context, p hostPool) {
	for id, owner := range p.Owners {
		if owner.MaintenanceHold != "" || owner.State != "ready" || owner.RebuildError != "" || owner.Current.HostInstanceID == h.instanceID {
			continue
		}
		source, ok := p.Generations[owner.Current.HostInstanceID]
		if !ok {
			continue
		}
		absent, err := source.Process.GroupAbsent()
		if err != nil || !absent {
			continue
		}
		r := owner.Original
		req := hostRequest{Operation: "round_rebuild", RequestID: id, ProjectDir: r.ProjectDir, ConfigPath: r.ConfigPath, ConfigSHA256: r.ConfigSHA256, Mode: r.Mode, CleanupCompleted: r.CleanupCompleted, AdmissionSHA: r.BinarySHA256, RoundSource: owner.Current.HostInstanceID, Environment: os.Environ()}
		reply := h.rebuildRollingContext(ctx, req)
		if reply.Status == "ok" {
			continue
		}
		if err := updateHostPool(h.rolling.root, func(current *hostPool) error {
			o := current.Owners[id]
			if o.State == "ready" && o.Current.HostInstanceID == owner.Current.HostInstanceID {
				o.RebuildError = reply.Error
				current.Owners[id] = o
			}
			return nil
		}); err != nil {
			h.rolling.failure = err.Error()
			return
		}
	}
}
