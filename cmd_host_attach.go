package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/execution"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/state"
	"github.com/poteto/noodle/internal/statever"
	"github.com/poteto/noodle/loop"
	loopruntime "github.com/poteto/noodle/runtime"
)

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
