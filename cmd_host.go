package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/execution"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/loopview"
	"github.com/poteto/noodle/internal/procx"
	"github.com/poteto/noodle/internal/state"
	"github.com/poteto/noodle/internal/statever"
	"github.com/poteto/noodle/loop"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/spf13/cobra"
)

const hostProtocol = "noodle-host-v1"

type hostReceipt struct {
	Schema           int                `json:"schema"`
	Protocol         string             `json:"protocol"`
	HostPID          int                `json:"host_pid"`
	HostInstanceID   string             `json:"host_instance_id"`
	ContextID        string             `json:"context_id"`
	ProjectDir       string             `json:"project_dir"`
	ContextState     string             `json:"context_state"`
	Mode             string             `json:"mode"`
	ConfigPath       string             `json:"config_path"`
	ConfigSHA256     string             `json:"config_sha256"`
	BinaryPath       string             `json:"binary_path"`
	BinarySHA256     string             `json:"binary_sha256"`
	RequestID        string             `json:"request_id"`
	HostDir          string             `json:"host_dir"`
	CleanupCompleted bool               `json:"cleanup_completed,omitempty"`
	TerminalCause    loop.TerminalCause `json:"terminal_cause,omitempty"`
	AdmissionRefusal string             `json:"admission_refusal"`
	OwnerBinding     *hostOwnerBinding  `json:"owner_binding,omitempty"`
}

type hostResponse struct {
	hostReceipt
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

type hostRequest struct {
	ExpectedVersion        string                             `json:"expected_version"`
	ReadbackOrderID        string                             `json:"readback_order_id"`
	ReadbackSessionID      string                             `json:"readback_session_id"`
	ReadbackRequest        loopruntime.SessionReadbackFileRef `json:"readback_request"`
	Operation              string                             `json:"operation"`
	WarningOrderID         string                             `json:"warning_order_id"`
	GatewayWarning         bool                               `json:"gateway_warning"`
	WarningRevision        uint64                             `json:"warning_revision"`
	ProjectDir             string                             `json:"project_dir"`
	ContextID              string                             `json:"context_id"`
	RequiredHostInstanceID string                             `json:"required_host_instance_id"`
	InstanceID             string                             `json:"instance_id"`
	ConfigPath             string                             `json:"config_path"`
	ConfigSHA256           string                             `json:"config_sha256"`
	Mode                   string                             `json:"mode"`
	Environment            []string                           `json:"environment,omitempty"`
	BinarySHA256           string                             `json:"binary_sha256"`
	RequestID              string                             `json:"request_id"`
	ResidentSpec           *admission.ResidentSpec            `json:"resident_spec,omitempty"`
	ResidentID             string                             `json:"resident_id,omitempty"`
	Apply                  bool                               `json:"apply,omitempty"`
	CleanupCompleted       bool                               `json:"cleanup_completed,omitempty"`
	RequestFile            string                             `json:"-"`
	HostDir                string                             `json:"-"`
	AdoptCurrentProfile    bool                               `json:"adopt_current_profile,omitempty"`
	ControllerOnly         bool                               `json:"controller_only,omitempty"`
	DispatchBlocked        string                             `json:"dispatch_blocked,omitempty"`
	AdmissionSHA           string                             `json:"admission_binary_sha256,omitempty"`
	AdmissionPath          string                             `json:"admission_binary_path,omitempty"`
	RoundSource            string                             `json:"round_source,omitempty"`
	RoundState             *loop.HostContinuation             `json:"round_state,omitempty"`
	FixtureManifest        *admission.FixtureManifest         `json:"fixture_manifest,omitempty"`
	NativeConsumer         bool                               `json:"native_consumer,omitempty"`
	ConsumerLease          string                             `json:"consumer_lease,omitempty"`
	peerPID                int
	peerError              error
}

type hostRuntime interface {
	Run(context.Context) error
	Cycle(context.Context) error
	Release(context.Context) error
	Cleanup(context.Context, bool) error
	CleanupAutomatic(context.Context) error
	Ready() <-chan struct{}
	Shutdown()
	FinishOnce() error
	TerminalCause() loop.TerminalCause
}

type hostContext struct {
	receipt        hostReceipt
	request        hostRequest
	runtime        hostRuntime
	done           chan error
	cancel         context.CancelFunc
	lock           *lockfile.Lock
	failed         bool
	finalizing     bool
	finalized      bool
	releaseErr     error
	receiptSaved   bool
	openShell      bool
	environmentSHA string
	execution      *execution.Context
}

type noodleHost struct {
	dir              string
	instanceID       string
	binaryPath       string
	binarySHA        string
	webURL           string
	receipts         []hostReceipt
	contexts         map[string]*hostContext
	completed        chan string
	budget           *admission.Budget
	residents        *admission.Residents
	openShell        *loopruntime.OpenShellGateway
	openShellProfile *loopruntime.OpenShellProfile
	blocked          bool
	shutdown         *hostShutdown
	newRuntime       func(string, string, config.Config, loop.Dependencies) hostRuntime
	rolling          *hostRolling
}

func newHostCmd(app *App) *cobra.Command {
	cmd := &cobra.Command{Use: "host", Short: "Manage the persistent Noodle Host and its rolling versions"}
	cmd.AddCommand(newHostResourcesCmd(app))
	cmd.AddCommand(newHostRollingCmd())
	cmd.AddCommand(newHostRestrictedCmd())
	cmd.AddCommand(newHostRebuildCmd())
	cmd.AddCommand(newHostCleanupCmd(app))
	cmd.AddCommand(newHostRecoverCmd(app))
	cmd.AddCommand(newHostViewsCmd())
	cmd.AddCommand(newHostContextsCmd())
	task := &cobra.Command{Use: "task", Short: "Read native task dispatch capabilities"}
	task.AddCommand(&cobra.Command{Use: "capabilities", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"schema_version": 2, "runtime_admitted_orders": true, "config_key": "runtime.admitted_orders", "empty_unrestricted": true, "scope": "runtime_dispatch"})
	}})
	task.AddCommand(&cobra.Command{Use: "review-capabilities", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"schema_version": 1, "local_task_review_recovery": true, "candidate_bound_merge": true, "local_task_merge_recovery": true, "local_task_completion_recovery": true, "local_task_completion_recovery_version": 2, "local_task_roles": 1, "local_task_directory_scope": 1, "openshell_local_task_review": 1, "openshell_instruction_materialization": 1, "openshell_role_tool_payload": 1, "openshell_saved_progress_attempt": 1, "local_task_worktree_custody": 1, "openshell_progress_maintenance": 1})
	}})
	task.AddCommand(newHostSessionReadbackCmd(app), newHostSavedProgressCmd(app, false), newHostSavedProgressCmd(app, true))
	task.AddCommand(newHostCandidateReadbackCmd())
	cmd.AddCommand(task)
	cmd.AddCommand(&cobra.Command{Use: "capabilities", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return json.NewEncoder(cmd.OutOrStdout()).Encode(hostCapabilities(runtime.GOOS))
	}})
	cmd.AddCommand(&cobra.Command{Use: "serve", Hidden: true, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, cancel := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer cancel()
		return serveHost(ctx)
	}})
	for _, operation := range []string{"attach", "once", "status", "detach"} {
		var mode, contextID, instanceID, requiredInstanceID, configSHA, requestFile, configPath, warningOrderID string
		var cleanupCompleted bool
		child := &cobra.Command{Use: operation, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			if cmd.Flags().Changed("require-existing-instance") && requiredInstanceID == "" {
				return errors.New("required original Host instance is empty")
			}
			req := hostRequest{Operation: operation, ProjectDir: app.projectDir, Mode: mode, ContextID: contextID, InstanceID: instanceID, RequiredHostInstanceID: requiredInstanceID, ConfigPath: configPath, ConfigSHA256: configSHA, RequestFile: requestFile, CleanupCompleted: cleanupCompleted, WarningOrderID: warningOrderID}
			response := callHost(cmd.Context(), req)
			if err := json.NewEncoder(cmd.OutOrStdout()).Encode(response); err != nil {
				return err
			}
			if response.Error != "" {
				return errors.New(response.Error)
			}
			return nil
		}}
		child.Flags().StringVar(&requestFile, "request-file", "", "Native request binding file")
		if operation == "attach" || operation == "once" {
			child.Flags().StringVar(&requiredInstanceID, "require-existing-instance", "", "Require this original running Host; never start a replacement")
			child.Flags().StringVar(&configPath, "config-file", "", "Exact absolute task config source (repository config remains unchanged)")
		}
		if operation == "attach" {
			child.Flags().StringVar(&warningOrderID, "warning-order-id", "", "Authorize one selected OpenShell gateway and worker launch under stable warning pressure")
			child.Flags().BoolVar(&cleanupCompleted, "cleanup-completed", false, "Release completed task runtime automatically; preserve manual migration holds")
		}
		if operation != "detach" {
			child.Flags().StringVar(&mode, "mode", "", "Run mode")
		}
		if operation == "status" || operation == "detach" {
			child.Flags().StringVar(&contextID, "context-id", "", "Native context identity")
			child.Flags().StringVar(&instanceID, "instance-id", "", "Native host instance identity")
		}
		if operation == "status" {
			child.Flags().StringVar(&configSHA, "config-sha256", "", "Original config digest for lost response lookup")
		}
		cmd.AddCommand(child)
	}
	return cmd
}

func hostCapabilities(platform string) map[string]any {
	force := 1
	if platform == "windows" {
		force = 0
	}
	rolling := 0
	if platform == "darwin" || platform == "linux" {
		rolling = 1
	}
	return map[string]any{"schema": 1, "protocol": hostProtocol, "stateless_fixture_recovery": 1, "resident_request_binding": 1, "restricted_host_maintenance": 1, "existing_instance_admission": 1, "openshell_warning_grant": 1, "native_consumer_leases": 1, "native_consumer_worktrees": 1, "shared_service_adapter": "stateless_fixtures_only", "completed_context_cleanup": 1, "external_task_config": 1, "resident_force_after_grace": force, "local_task_interrupt_custody": 1, "codex_native_runtime": force, "safe_host_switch": 0, "rolling_host_version": rolling, "shared_worker_quota": rolling, "openshell_pool_owner": rolling, "openshell_custody_version": 4, "gateway_host_handover": 1, "gateway_custody_release": 3, "worker_admission_policy": admission.WorkerAdmissionPolicy, "task_warning_grant": 1}
}

func hostDirectory() (string, error) {
	dir := os.Getenv("NOODLE_HOST_DIR")
	if dir == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(cache, "noodle", "host")
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("host directory is not a real directory")
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return "", err
	}
	return dir, nil
}

func hostDigest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func hostID() (string, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(id[:]), nil
}
func hostError(status string, err error) hostResponse {
	return hostResponse{hostReceipt: hostReceipt{Schema: 1, Protocol: hostProtocol}, Status: status, Error: err.Error()}
}

func readHostConfig(dir string) ([]byte, error) {
	data, err := os.ReadFile(filepath.Join(dir, config.DefaultConfigPath))
	if os.IsNotExist(err) {
		return nil, nil
	}
	return data, err
}

func readHostRequestConfig(req hostRequest) ([]byte, error) {
	if req.ConfigPath != "" {
		return config.ReadExplicitSource(req.ConfigPath)
	}
	return readHostConfig(req.ProjectDir)
}

func callHost(ctx context.Context, req hostRequest) hostResponse {
	managedOpenShell := false
	dir, err := hostDirectory()
	if err != nil {
		return hostError("refused", err)
	}
	path, err := os.Executable()
	if err != nil {
		return hostError("refused", err)
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return hostError("refused", err)
	}
	binary, err := os.ReadFile(path)
	if err != nil {
		return hostError("refused", err)
	}
	req.BinarySHA256 = hostDigest(binary)
	req.AdmissionSHA = req.BinarySHA256
	req.AdmissionPath = path
	req.ProjectDir, err = filepath.EvalSymlinks(req.ProjectDir)
	if err != nil {
		return hostError("refused", err)
	}
	if req.Operation == "attach" || req.Operation == "once" || (req.Operation == "status" && req.ContextID == "" && req.RequestFile == "") {
		data, err := readHostRequestConfig(req)
		if err != nil {
			return hostError("refused", err)
		}
		cfg, err := config.Parse(data)
		if err != nil {
			return hostError("refused", err)
		}
		if req.Operation == "once" {
			req.Mode = "manual"
		}
		cfg, err = startConfig(cfg, req.Mode)
		if err != nil {
			return hostError("refused", err)
		}
		managedOpenShell = cfg.Runtime.OpenShell.Profile != ""
		req.Mode = cfg.Mode
		if req.Operation != "status" {
			req.ConfigSHA256 = hostDigest(data)
			req.Environment = os.Environ()
			slices.Sort(req.Environment)
		}
	}
	req.HostDir = dir
	var requestBinding *hostRequestBinding
	if req.RequestFile != "" || req.Operation == "attach" || req.Operation == "once" {
		var unlock func()
		requestBinding, unlock, err = bindHostRequest(&req)
		if err != nil {
			return hostError("refused", err)
		}
		defer unlock()
		dir = req.HostDir
	}
	if managedOpenShell && (req.Operation == "attach" || req.Operation == "once") {
		if err = bindSharedOpenShellAdmission(ctx, &req, requestBinding); err != nil {
			if requestBinding != nil {
				requestBinding.Status = "busy"
				if saveErr := saveHostRequest(req.RequestFile, *requestBinding); saveErr != nil {
					return hostError("unknown_effect", saveErr)
				}
			}
			return hostError("busy", err)
		}
	}
	if (req.Operation == "detach" || (req.Operation == "status" && req.ContextID != "")) && (req.ContextID == "" || req.InstanceID == "") {
		return hostError("refused", errors.New("context-id and instance-id are required together"))
	}
	if req.Operation == "status" && req.ContextID == "" && req.ConfigSHA256 == "" {
		return hostError("refused", errors.New("status needs context identity or config-sha256"))
	}
	if req.Operation == "attach" || req.Operation == "once" {
		if waiting := maintenanceWaitResponse(dir); waiting != nil {
			if requestBinding != nil && waiting.Status == "busy" {
				requestBinding.Status = "busy"
				if err := saveHostRequest(req.RequestFile, *requestBinding); err != nil {
					return hostError("unknown_effect", err)
				}
			}
			return *waiting
		}
	}
	if _, statErr := os.Stat(filepath.Join(dir, "pool.json")); os.IsNotExist(statErr) && req.RequiredHostInstanceID == "" && (req.Operation == "attach" || req.Operation == "once") {
		if !sameUnpooledHost(ctx, dir, req.BinarySHA256) {
			if err := ensureHostVersion(ctx, path, dir); err != nil {
				if requestBinding != nil {
					requestBinding.Status = "busy"
					if saveErr := saveHostRequest(req.RequestFile, *requestBinding); saveErr != nil {
						return hostError("unknown_effect", saveErr)
					}
				}
				return hostError("busy", err)
			}
		}
	}
	rootDir := dir
	dir, err = routeBoundHost(rootDir, &req)
	if err != nil {
		return hostError("refused", err)
	}
	conn, err := net.DialTimeout("unix", filepath.Join(dir, "host.sock"), time.Second)
	if err != nil && req.RequiredHostInstanceID == "" && (req.Operation == "attach" || req.Operation == "once") {
		if err = ensureHostVersion(ctx, path, rootDir); err == nil {
			conn, err = net.DialTimeout("unix", filepath.Join(dir, "host.sock"), time.Second)
		}
	}
	if err != nil {
		if req.Operation == "status" {
			return offlineHostStatus(dir, req, err)
		}
		// No request bytes have been sent. This is known nonacceptance,
		// including another client still launching the shared host.
		if requestBinding != nil && (req.Operation == "attach" || req.Operation == "once") {
			requestBinding.Status = "busy"
			if saveErr := saveHostRequest(req.RequestFile, *requestBinding); saveErr != nil {
				return hostError("unknown_effect", saveErr)
			}
			return hostError("busy", err)
		}
		return hostError("host_unavailable", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
	if err = json.NewEncoder(conn).Encode(req); err != nil {
		return hostError("unknown_effect", err)
	}
	var response hostResponse
	if err = json.NewDecoder(conn).Decode(&response); err != nil {
		return hostError("unknown_effect", err)
	}
	if req.Operation == "status" && req.RequestFile != "" && response.Status == "ok" {
		if _, statErr := os.Stat(filepath.Join(rootDir, "pool.json")); statErr == nil {
			bound, bindErr := readHostOwnerBinding(req.RequestFile)
			if bindErr != nil {
				return hostError("unknown_effect", bindErr)
			}
			if bound.Current.ContextID != response.ContextID || bound.Current.HostInstanceID != response.HostInstanceID {
				return hostError("unknown_effect", errors.New("current owner changed during status readback"))
			}
			response.OwnerBinding = bound
		} else if !os.IsNotExist(statErr) {
			return hostError("unknown_effect", statErr)
		}
	}
	if requestBinding != nil && req.Operation != "status" && req.Operation != "cleanup" {
		requestBinding.Status = "pending"
		if response.Status == "busy" || response.Status == "deferred" {
			requestBinding.Status = response.Status
		}
		if response.Status == "ok" {
			requestBinding.Status = "accepted"
		}
		if err := saveHostRequest(req.RequestFile, *requestBinding); err != nil {
			return hostError("unknown_effect", err)
		}
	}
	return response
}

func ensureHostVersion(ctx context.Context, binary, root string) error {
	if hostCapabilities(runtime.GOOS)["rolling_host_version"] == 0 {
		return startHostProcess(ctx, binary, root)
	}
	return ensureRollingHost(ctx, binary, root)
}

// Explicit standalone serve remains controllable by its exact original binary.
// This does not migrate a legacy Host or start an independent second budget.
func sameUnpooledHost(ctx context.Context, root, digest string) bool {
	raw, err := nativeHostRPC(ctx, root, hostRequest{Operation: "resources"})
	var current struct {
		Protocol string `json:"protocol"`
		Binary   string `json:"binary_sha256"`
		Rolling  int    `json:"rolling_host_version"`
	}
	return err == nil && json.Unmarshal(raw, &current) == nil && current.Protocol == hostProtocol && current.Binary == digest && current.Rolling == 0
}

func startHostProcess(ctx context.Context, path, dir string) error {
	lock, err := lockfile.TryLock(filepath.Join(dir, "launch.lock"))
	if err != nil {
		return fmt.Errorf("host launch is already in progress: %w", err)
	}
	defer lock.Close()
	if conn, err := net.DialTimeout("unix", filepath.Join(dir, "host.sock"), time.Second); err == nil {
		conn.Close()
		return nil
	}
	probe, err := lockfile.TryLock(filepath.Join(dir, "host.lock"))
	if err != nil {
		return fmt.Errorf("host lock is held but socket is unavailable: %w", err)
	}
	probe.Close()
	log, err := os.OpenFile(filepath.Join(dir, "host.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer log.Close()
	cmd := exec.Command(path, "host", "serve")
	cmd.Stdout = log
	cmd.Stderr = log
	cmd.Env = hostBaselineEnvironment(dir)
	cmd.Dir = dir
	detachHostProcess(cmd)
	if err = cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return errors.New("host startup timed out; inspect host.log before retry")
		case <-ticker.C:
			conn, err := net.DialTimeout("unix", filepath.Join(dir, "host.sock"), 100*time.Millisecond)
			if err == nil {
				conn.Close()
				return nil
			}
		}
	}
}

func serveHost(shutdownRequested context.Context) error {
	return serveHostWithSampler(shutdownRequested, admission.NewSampler())
}

func serveHostWithSampler(shutdownRequested context.Context, sampler admission.Sampler) error {
	ctx, cancel := context.WithCancel(context.WithoutCancel(shutdownRequested))
	defer cancel()
	dir, err := hostDirectory()
	if err != nil {
		return err
	}
	lock, err := hostGenerationFence(dir)
	if err != nil {
		return err
	}
	defer lock.Close()
	path, err := os.Executable()
	if err != nil {
		return err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	if err := verifyProgressMaintenanceTransactions(dir); err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	id, err := hostID()
	if err != nil {
		return err
	}
	if generation := os.Getenv("NOODLE_HOST_GENERATION"); generation != "" {
		if !validPoolID(generation) {
			return errors.New("invalid native Host generation")
		}
		id = generation
	}
	budget, err := newHostBudget()
	if err != nil {
		return err
	}
	samplerDone := make(chan struct{})
	go func() { defer close(samplerDone); budget.Run(ctx, sampler) }()
	defer func() { cancel(); <-samplerDone }()
	h := &noodleHost{budget: budget, contexts: make(map[string]*hostContext), completed: make(chan string, 16), dir: dir, instanceID: id, binaryPath: path, binarySHA: hostDigest(data), newRuntime: func(dir, bin string, cfg config.Config, deps loop.Dependencies) hostRuntime {
		return loop.New(dir, bin, cfg, deps)
	}}
	h.residents, err = admission.OpenResidents(dir)
	if err != nil {
		return err
	}
	h.residents.BindBudget(budget)
	if err := h.loadReceipts(); err != nil {
		return err
	}
	if err := h.joinRolling(); err != nil {
		return err
	}
	if h.rolling != nil {
		defer h.rolling.fence.Close()
	}
	socket := filepath.Join(dir, "host.sock")
	if err = os.Remove(socket); err != nil && !os.IsNotExist(err) {
		return err
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return err
	}
	defer listener.Close()
	defer os.Remove(socket)
	if err = os.Chmod(socket, 0600); err != nil {
		return err
	}
	type call struct {
		request hostRequest
		reply   chan any
	}
	calls := make(chan call)
	web, url, webDone, err := startHostWeb(dir, contextOwnerIdentity{h.instanceID, h.binarySHA, h.dir}, func(requestCtx context.Context, view *loopview.View) error {
		requestCtx, stop := context.WithTimeout(requestCtx, 2*time.Second)
		defer stop()
		read := call{hostRequest{Operation: "view_resources"}, make(chan any, 1)}
		select {
		case calls <- read:
		case <-requestCtx.Done():
			return requestCtx.Err()
		case <-ctx.Done():
			return ctx.Err()
		}
		select {
		case value := <-read.reply:
			observation, ok := value.(hostViewObservation)
			if !ok {
				return errors.New("Host view observation type invalid")
			}
			return enrichHostView(dir, view, observation)
		case <-requestCtx.Done():
			return requestCtx.Err()
		case <-ctx.Done():
			return ctx.Err()
		}
	})
	if err != nil {
		return fmt.Errorf("start Host readonly web: %w", err)
	}
	h.webURL = url
	defer web.Close()
	go func() { <-ctx.Done(); listener.Close() }()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
				var req hostRequest
				if err := json.NewDecoder(io.LimitReader(conn, 4<<20)).Decode(&req); err != nil {
					return
				}
				req.peerPID, req.peerError = procx.PeerPID(conn)
				c := call{req, make(chan any, 1)}
				select {
				case calls <- c:
				case <-ctx.Done():
					return
				}
				select {
				case response := <-c.reply:
					_ = json.NewEncoder(conn).Encode(response)
				case <-ctx.Done():
				}
			}()
		}
	}()
	shutdownSignal := shutdownRequested.Done()
	var finalized <-chan hostShutdownResult
	switchTick := time.NewTicker(time.Second)
	defer switchTick.Stop()
	for {
		if h.shutdown != nil {
			if err := h.shutdownReady(); err == nil {
				h.releaseShutdownLocks()
				return nil
			} else {
				h.shutdown.diagnostic = err.Error()
			}
		}
		select {
		case <-shutdownSignal:
			h.beginShutdown(ctx)
			shutdownSignal = nil
			finalized = h.shutdown.results
		case result := <-finalized:
			h.applyShutdownResult(result)
		case <-switchTick.C:
			h.reconcileFixtureLifecycle(ctx)
			if h.shutdown == nil && h.rolling != nil && h.advanceRolling(ctx) {
				return nil
			}
		case err := <-webDone:
			if err != nil {
				fmt.Fprintf(os.Stderr, "Host readonly web stopped: %v\n", err)
			}
			h.webURL = ""
			webDone = nil
		case <-h.residents.Released():
			if h.shutdown != nil {
				continue
			}
			if err := h.closeIdleOpenShell(); err != nil {
				fmt.Fprintln(os.Stderr, "OpenShell gateway custody retained:", err)
			}
			h.cleanupAutomaticContexts(ctx)
		case id := <-h.completed:
			if c := h.contexts[id]; c != nil && c.done != nil {
				select {
				case err := <-c.done:
					h.completeContext(c, err)
				default:
				}
			}
		case call := <-calls:
			if h.shutdown != nil && h.handleShutdownCall(call.request, call.reply) {
				continue
			}
			if call.request.Operation == "view_resources" {
				receipts := make([]hostReceipt, 0, len(h.contexts))
				for _, context := range h.contexts {
					receipts = append(receipts, context.receipt)
				}
				call.reply <- hostViewObservation{Resources: h.viewResources(), Receipts: receipts}
				continue
			}
			if call.request.Operation == "task_interruption_inspect" || call.request.Operation == "task_new_attempt" {
				call.reply <- h.savedProgress(ctx, call.request)
				continue
			}
			if call.request.Operation == "task_session_readback" {
				call.reply <- h.sessionReadback(ctx, call.request)
				continue
			}
			for _, c := range h.contexts {
				if c.done == nil {
					continue
				}
				select {
				case err := <-c.done:
					h.completeContext(c, err)
				default:
				}
			}
			if forwarded, ok := h.forwardRolling(ctx, call.request); ok {
				call.reply <- forwarded
				continue
			}
			if call.request.Operation == "round_reconcile" {
				call.reply <- h.reconcileRollingRound(call.request)
			} else if call.request.Operation == "round_prepare" {
				call.reply <- h.prepareRollingRebuild(ctx, call.request)
			} else if call.request.Operation == "round_rebuild" {
				call.reply <- h.rebuildRollingContext(ctx, call.request)
			} else if call.request.Operation == "round_admit" {
				call.reply <- h.rollingRound(ctx, call.request)
			} else if call.request.Operation == "cleanup_all" {
				call.reply <- h.cleanupContexts(ctx, call.request)
			} else if strings.HasPrefix(call.request.Operation, "resident_") {
				call.reply <- h.residentOperation(ctx, call.request)
			} else if call.request.Operation == "grant_warning" {
				call.reply <- h.grantWarning(call.request)
			} else if call.request.Operation == "resources" {
				call.reply <- h.resources()
			} else {
				call.reply <- h.handle(ctx, call.request)
			}
		}
	}
}

func (h *noodleHost) save() error {
	data, err := json.MarshalIndent(h.receipts, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(h.dir, "receipts.*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(h.dir, "receipts.json"))
}

func offlineHostStatus(dir string, req hostRequest, dialErr error) hostResponse {
	data, err := os.ReadFile(filepath.Join(dir, "receipts.json"))
	if err != nil {
		return hostError("host_unavailable", dialErr)
	}
	var receipts []hostReceipt
	if err := json.Unmarshal(data, &receipts); err != nil {
		return hostError("unknown_effect", err)
	}
	if ambiguousHostLookup(req, receipts) {
		return hostError("ambiguous", errors.New("multiple contexts match; use request-file or context identity"))
	}
	for i := len(receipts) - 1; i >= 0; i-- {
		r := receipts[i]
		if !hostReceiptMatches(req, r) {
			continue
		}
		if r.ContextState == "detached" {
			return hostReceiptResult(r)
		}
		return hostResponse{r, "unknown_effect", "host is unavailable; the persisted context was not detached"}
	}
	return hostError("not_found", errors.New("context identity was not found"))
}

func (h *noodleHost) handle(ctx context.Context, req hostRequest) hostResponse {
	if forwarded, ok := h.forwardRolling(ctx, req); ok {
		if response, ok := forwarded.(hostResponse); ok {
			return response
		}
		raw, err := json.Marshal(forwarded)
		var response hostResponse
		if err != nil || json.Unmarshal(raw, &response) != nil {
			return hostError("unknown_effect", errors.New("original forwarded Host response unavailable"))
		}
		return response
	}
	if req.RequiredHostInstanceID != "" && req.RequiredHostInstanceID != h.instanceID {
		if response, ok := h.historicalHostDeferral(req); ok {
			return response
		}
		return hostError("refused", errors.New("required original Host instance differs; no admission attempted"))
	}
	if h.rolling != nil && req.RequestID != "" {
		p, err := readHostPool(h.rolling.root)
		if err != nil {
			return hostError("unknown_effect", err)
		}
		if owner, ok := p.Owners[req.RequestID]; ok && owner.Current.HostInstanceID == h.instanceID && hostReceiptMatches(req, owner.Original) {
			req.BinarySHA256 = h.binarySHA
		}
	}

	if req.Operation == "cleanup" && req.Apply && req.InstanceID != h.instanceID {
		return hostError("refused", errors.New("cleanup Host instance changed; inspect before applying"))
	}
	if req.Operation == "cleanup" && req.RequestID == "" {
		return hostError("refused", errors.New("cleanup requires the original request binding"))
	}
	if req.Operation == "once" {
		req.Mode = "manual"
	}
	if req.BinarySHA256 != h.binarySHA && req.Operation != "status" && req.Operation != "detach" && req.Operation != "cleanup" {
		return hostError("binary_mismatch", errors.New("client and host binaries differ"))
	}
	if req.Operation == "status" || req.Operation == "detach" || req.Operation == "cleanup" {
		if ambiguousHostLookup(req, h.receipts) {
			return hostError("ambiguous", errors.New("multiple contexts match; use request-file or context identity"))
		}
		for i := len(h.receipts) - 1; i >= 0; i-- {
			r := h.receipts[i]
			if !hostReceiptMatches(req, r) {
				continue
			}
			// finish retains the current context when saving its detached receipt
			// fails. Only earlier detached contexts are already durable.
			c := h.contexts[r.ContextID]
			if r.ContextState == "detached" && c == nil {
				if req.BinarySHA256 != r.BinarySHA256 || (req.BinarySHA256 != h.binarySHA && req.RequestID == "" && req.ContextID == "") {
					return hostError("binary_mismatch", errors.New("historical readback requires the original binary and exact context or request identity"))
				}
				return hostReceiptResult(r)
			}
			if req.BinarySHA256 != h.binarySHA {
				return hostError("binary_mismatch", errors.New("client and host binaries differ"))
			}
			if h.blocked || (c != nil && c.failed) || (c == nil && r.ContextState != "detached") {
				return hostResponse{r, "unknown_effect", "prior context is unresolved; no replay was attempted"}
			}
			if req.Operation == "status" || r.ContextState == "detached" {
				return hostReceiptResult(r)
			}
			if c == nil {
				return hostResponse{r, "stale_context", "context is no longer owned by this host"}
			}
			releaseCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			var releaseErr error
			if req.Operation == "cleanup" {
				releaseErr = c.runtime.Cleanup(releaseCtx, req.Apply)
			} else {
				releaseErr = c.runtime.Release(releaseCtx)
			}
			if err := releaseErr; err != nil {
				status := "unknown_effect"
				if errors.Is(err, loop.ErrReleaseBusy) {
					status = "busy"
				}
				return hostResponse{r, status, err.Error()}
			}
			if req.Operation == "cleanup" && !req.Apply {
				return hostResponse{r, "eligible", ""}
			}
			select {
			case err := <-c.done:
				h.finish(c, err)
			case <-releaseCtx.Done():
				return hostResponse{r, "unknown_effect", releaseCtx.Err().Error()}
			}
			if h.blocked || c.failed {
				return hostResponse{h.receipts[i], "unknown_effect", "context cleanup or receipt persistence failed"}
			}
			return hostResponse{h.receipts[i], "ok", ""}
		}
		return hostError("not_found", errors.New("context identity was not found"))
	}
	if h.rolling != nil && h.rolling.retiring && req.Operation != "status" {
		return hostError("busy", errors.New("Host switch is draining; original contexts remain queryable"))
	}
	if req.Operation != "attach" && req.Operation != "once" {
		return hostError("refused", errors.New("unsupported host operation"))
	}
	if req.RequestID == "" {
		return hostError("refused", errors.New("native request identity is missing"))
	}
	for _, r := range h.receipts {
		if r.RequestID != req.RequestID {
			continue
		}
		if r.ProjectDir != req.ProjectDir || r.ConfigSHA256 != req.ConfigSHA256 || r.ConfigPath != req.ConfigPath || r.Mode != req.Mode || r.CleanupCompleted != req.CleanupCompleted {
			return hostError("refused", errors.New("request identity has conflicting bindings"))
		}
		c := h.contexts[r.ContextID]
		if c != nil && hostDigest([]byte(strings.Join(req.Environment, "\x00"))) != c.environmentSHA {
			return hostResponse{r, "busy", "request environment differs from active context"}
		}
		if h.blocked || (c != nil && c.failed) || (c == nil && r.ContextState != "detached") {
			return hostResponse{r, "unknown_effect", "original request has an unresolved context"}
		}
		return hostReceiptResult(r)
	}
	if h.blocked {
		return hostError("unknown_effect", errors.New("prior active or failed context requires readback; attach refused"))
	}
	if len(h.contexts) >= 16 {
		return hostError("busy", errors.New("host context limit reached (16)"))
	}
	for _, c := range h.contexts {
		if c.receipt.ProjectDir == req.ProjectDir {
			return hostResponse{c.receipt, "busy", "project already has an attached context"}
		}
	}

	return h.attachRolling(ctx, req, nil)
}

func (h *noodleHost) attach(ctx context.Context, req hostRequest) hostResponse {
	return h.attachContinuation(ctx, req, nil)
}

func (h *noodleHost) attachContinuation(ctx context.Context, req hostRequest, continuation *loop.HostContinuation) hostResponse {
	if err := verifyProgressMaintenanceTransactions(h.dir); err != nil {
		return hostError("unknown_effect", err)
	}
	data, err := readHostRequestConfig(req)
	if err != nil {
		return hostError("refused", err)
	}
	if hostDigest(data) != req.ConfigSHA256 {
		return hostError("source_drift", errors.New("config changed before host admission"))
	}
	if req.Operation == "once" {
		req.Mode = "manual"
	}
	if _, err = prepareHostStorage(ctx, req, data); err != nil {
		return hostError("refused", err)
	}
	runtimeDir := filepath.Join(req.ProjectDir, ".noodle")
	if err = os.MkdirAll(runtimeDir, 0755); err != nil {
		return hostError("refused", err)
	}
	lock, err := lockfile.TryLock(filepath.Join(runtimeDir, "noodle.lock"))
	if err != nil {
		return hostError("busy", err)
	}
	keepLock := false
	defer func() {
		if !keepLock {
			lock.Close()
		}
	}()
	if err = statever.CheckCompatibility(filepath.Join(runtimeDir, "state.json")); err != nil {
		return hostError("refused", err)
	}
	if req.Operation == "once" {
		entries, err := os.ReadDir(filepath.Join(runtimeDir, "sessions"))
		if err != nil && !os.IsNotExist(err) {
			return hostError("refused", err)
		}
		if len(entries) != 0 {
			return hostError("busy", errors.New("host once requires a bootstrap context without existing sessions"))
		}
		for _, name := range []string{"control.ndjson", "orders-next.json"} {
			info, err := os.Stat(filepath.Join(runtimeDir, name))
			if err != nil && !os.IsNotExist(err) {
				return hostError("refused", err)
			}
			if err == nil && info.Size() != 0 {
				return hostError("busy", errors.New("host once has pending runtime inputs"))
			}
		}
	}
	e, err := execution.New(req.ProjectDir, req.Environment)
	if err != nil {
		return hostError("refused", err)
	}
	if err = validateHostEnvironment(e); err != nil {
		return hostError("refused", err)
	}
	if err = hostRecoveryPreflight(runtimeDir); err != nil {
		return hostError("unknown_effect", err)
	}
	cfg, err := config.Parse(data)
	if err != nil {
		return hostError("refused", err)
	}
	cfg, err = config.Bind(cfg, e)
	if err != nil {
		return hostError("refused", err)
	}
	validation := config.Validate(cfg)
	if len(validation.Fatals()) != 0 {
		return hostError("refused", errors.New("fatal config diagnostics prevent host attach"))
	}
	cfg, err = startConfig(cfg, req.Mode)
	if err != nil {
		return hostError("refused", err)
	}
	if req.WarningOrderID != "" && (req.Operation != "attach" || cfg.Mode != "manual" || req.RequiredHostInstanceID != h.instanceID || cfg.Runtime.OpenShell.Profile == "" || len(cfg.Runtime.AdmittedOrders) != 1 || cfg.Runtime.AdmittedOrders[0] != req.WarningOrderID) {
		return hostError("refused", errors.New("gateway warning requires the existing Host and one explicitly admitted manual OpenShell order"))
	}
	if shouldStartServer(cfg.Server) {
		return hostError("refused", errors.New("host contexts require server.enabled=false"))
	}
	remote := slices.Contains(cfg.AvailableRuntimes(), "sprites") || slices.Contains(cfg.AvailableRuntimes(), "cursor") || cfg.Runtime.Default == "sprites" || cfg.Runtime.Default == "cursor"
	if remote {
		return hostError("refused", errors.New("shared host does not support remote runtimes without confirmed remote completion"))
	}
	var selectedOpenShell *loopruntime.OpenShellProfile
	if cfg.Runtime.OpenShell.Profile != "" {
		selectedOpenShell, err = h.prepareOpenShell(cfg.Runtime.OpenShell.Profile)
		if err != nil {
			return hostError("refused", err)
		}
	}
	id, err := hostID()
	if err != nil {
		return hostError("refused", err)
	}
	r := hostReceipt{Schema: 1, Protocol: hostProtocol, HostPID: os.Getpid(), HostInstanceID: h.instanceID,
		ContextID: id, ProjectDir: req.ProjectDir, ContextState: "active", Mode: cfg.Mode,
		ConfigPath: req.ConfigPath, ConfigSHA256: req.ConfigSHA256, BinaryPath: h.binaryPath,
		BinarySHA256: h.binarySHA, RequestID: req.RequestID, HostDir: h.controlRoot(), CleanupCompleted: req.CleanupCompleted}
	source := config.RuntimeSource{Path: req.ConfigPath, SHA256: req.ConfigSHA256}
	created, err := config.SaveRuntimeSource(req.ProjectDir, source)
	failBeforeRuntime := func(cause error) hostResponse {
		if created {
			if rollback := config.RollbackRuntimeSource(req.ProjectDir, source); rollback != nil {
				h.blocked = true
				return hostError("unknown_effect", fmt.Errorf("config binding rollback failed after %v: %w", cause, rollback))
			}
		}
		return hostError("refused", cause)
	}
	if err != nil {
		return failBeforeRuntime(err)
	}
	h.receipts = append(h.receipts, r)
	if err = h.save(); err != nil {
		h.receipts = h.receipts[:len(h.receipts)-1]
		return failBeforeRuntime(err)
	}
	runCtx, cancel := context.WithCancel(ctx)
	deps := loop.Dependencies{ModeOverride: state.RunMode(req.Mode), Execution: e, Admission: h.budget, AdmissionContext: id, CleanupCompleted: req.CleanupCompleted, HostContinuation: continuation, DispatchBlocked: req.DispatchBlocked}
	if cfg.Runtime.OpenShell.Profile != "" {
		deps.OpenShellGateway, deps.OpenShellProfile = h.openShell, selectedOpenShell
	}
	rt := h.newRuntime(req.ProjectDir, h.binaryPath, cfg, deps)
	environmentSHA := hostDigest([]byte(strings.Join(req.Environment, "\x00")))
	req.Environment = nil
	c := &hostContext{receipt: r, request: req, runtime: rt, done: make(chan error, 1), cancel: cancel, lock: lock, openShell: cfg.Runtime.OpenShell.Profile != "", environmentSHA: environmentSHA, execution: e}
	if h.contexts == nil {
		h.contexts = make(map[string]*hostContext)
	}
	h.contexts[id] = c
	if h.budget != nil {
		h.budget.Register(id)
	}
	keepLock = true
	if c.openShell {
		if req.WarningOrderID != "" {
			_, err = h.openShell.GrantWarning(id, req.WarningOrderID, 0)
			if err != nil {
				err = fmt.Errorf("%w: %v", admission.ErrDeferred, err)
			}
		}
		if err == nil {
			_, err = h.openShell.Acquire(ctx, id)
		}
		if err != nil {
			if response, settled := h.finishOpenShellDeferral(c, err); settled {
				return response
			}
			h.finish(c, err)
			return hostResponse{c.receipt, "unknown_effect", fmt.Sprintf("OpenShell gateway acquisition failed; original custody retained: %v", err)}
		}
	}
	if req.Operation == "once" {
		err = rt.Cycle(runCtx)
		if err == nil {
			err = rt.FinishOnce()
		}
		cancel()
		h.finish(c, err)
		if err != nil {
			return hostResponse{c.receipt, "failed", err.Error()}
		}
		if h.blocked || c.failed {
			return hostResponse{c.receipt, "unknown_effect", "context cleanup or receipt persistence failed"}
		}
		return hostResponse{c.receipt, "ok", ""}
	}
	go func() {
		err := rt.Run(runCtx)
		if err == nil && runCtx.Err() != nil {
			err = runCtx.Err()
		}
		c.done <- err
		if h.completed != nil {
			select {
			case h.completed <- id:
			case <-ctx.Done():
			}
		}
	}()
	select {
	case <-rt.Ready():
		return hostResponse{r, "ok", ""}
	case err := <-c.done:
		if err == nil {
			err = errors.New("loop exited before becoming ready")
		}
		h.finish(c, err)
		return hostResponse{c.receipt, "failed", err.Error()}
	case <-time.After(30 * time.Second):
		return hostResponse{r, "unknown_effect", "loop startup has not completed; use status"}
	}
}

const hostSwapUsedLimitEnv = "NOODLE_HOST_SWAP_USED_LIMIT_BYTES"

func hostSwapUsedLimit() (uint64, error) {
	value, present := os.LookupEnv(hostSwapUsedLimitEnv)
	if !present {
		return admission.DefaultSwapUsedLimit, nil
	}
	limit, err := strconv.ParseUint(value, 10, 53)
	if err != nil {
		return 0, fmt.Errorf("%s must be an unsigned integer byte limit (0..9007199254740991): %w", hostSwapUsedLimitEnv, err)
	}
	return limit, nil
}

func hostBaselineEnvironment(dir string) []string {
	result := []string{"NOODLE_HOST_DIR=" + dir}
	for _, key := range []string{hostSwapUsedLimitEnv, "HOME", "PATH", "TMPDIR", "TMP", "TEMP", "LANG", "LC_ALL", "USER", "LOGNAME", "SystemRoot", "WINDIR"} {
		if value, ok := os.LookupEnv(key); ok {
			result = append(result, key+"="+value)
		}
	}
	return result
}
