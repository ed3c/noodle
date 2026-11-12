package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/lockfile"
	loopruntime "github.com/poteto/noodle/runtime"
)

type hostRequestBinding struct {
	PriorRequest           *hostAdmissionFileRef `json:"prior_request,omitempty"`
	RequiredHostInstanceID string                `json:"required_host_instance_id"`
	Schema                 int                   `json:"schema"`
	RequestID              string                `json:"request_id"`
	Operation              string                `json:"operation"`
	ProjectDir             string                `json:"project_dir"`
	ConfigPath             string                `json:"config_path"`
	ConfigSHA256           string                `json:"config_sha256"`
	Mode                   string                `json:"mode"`
	BinarySHA256           string                `json:"binary_sha256"`
	EnvironmentSHA256      string                `json:"environment_sha256"`
	Status                 string                `json:"status"`
	HostDir                string                `json:"host_dir"`
	CleanupCompleted       bool                  `json:"cleanup_completed,omitempty"`
}

func bindHostRequest(req *hostRequest) (*hostRequestBinding, func(), error) {
	if req.RequestFile == "" {
		return nil, nil, errors.New("attach and once need --request-file for exact response recovery")
	}
	path, err := filepath.Abs(req.RequestFile)
	if err != nil {
		return nil, nil, err
	}
	req.RequestFile = path
	if req.Operation != "status" && req.Operation != "detach" && req.Operation != "cleanup" {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, nil, err
		}
	}
	lock, err := lockfile.TryLock(path + ".lock")
	if err != nil {
		return nil, nil, err
	}
	unlock := func() { _ = lock.Close() }
	fail := func(err error) (*hostRequestBinding, func(), error) { unlock(); return nil, nil, err }
	data, err := os.ReadFile(path)
	var binding hostRequestBinding
	if err == nil {
		if err = json.Unmarshal(data, &binding); err != nil {
			return fail(err)
		}
		if binding.Schema != 1 || binding.RequestID == "" || binding.HostDir == "" || (binding.Operation != "attach" && binding.Operation != "once") {
			return fail(errors.New("invalid native request binding"))
		}
		if binding.ProjectDir != req.ProjectDir || (binding.BinarySHA256 != req.BinarySHA256 && !poolBindingCompatible(binding.HostDir, binding, req.BinarySHA256)) {
			return fail(errors.New("request project or binary binding changed"))
		}
		if req.RequiredHostInstanceID != "" && req.RequiredHostInstanceID != binding.RequiredHostInstanceID {
			return fail(errors.New("original Host instance requirement changed"))
		}
		req.RequiredHostInstanceID = binding.RequiredHostInstanceID
		req.HostDir = binding.HostDir
		if req.Operation == "status" || req.Operation == "detach" || req.Operation == "cleanup" {
			if req.ConfigSHA256 != "" && req.ConfigSHA256 != binding.ConfigSHA256 {
				return fail(errors.New("request config binding changed"))
			}
			req.RequestID = binding.RequestID
			req.ConfigSHA256 = binding.ConfigSHA256
			req.ConfigPath = binding.ConfigPath
			req.Mode = binding.Mode
			req.CleanupCompleted = binding.CleanupCompleted
			return &binding, unlock, nil
		}
		if binding.Operation != req.Operation || binding.ConfigSHA256 != req.ConfigSHA256 || binding.ConfigPath != req.ConfigPath || binding.Mode != req.Mode || binding.CleanupCompleted != req.CleanupCompleted {
			return fail(errors.New("original native request binding changed"))
		}
		if binding.Status != "busy" {
			req.Operation = "status"
			req.RequestID = binding.RequestID
			req.Environment = nil
			return &binding, unlock, nil
		}
		if binding.PriorRequest != nil {
			if err := validateTransferredAdmission(path, binding); err != nil {
				return fail(err)
			}
		}
		if binding.BinarySHA256 != req.BinarySHA256 {
			p, readErr := readHostPool(binding.HostDir)
			if readErr != nil {
				return fail(readErr)
			}
			path, ok := waitingPoolAdmission(binding.HostDir, binding, p)
			if !ok {
				return fail(errors.New("original waiting admission source is unavailable"))
			}
			req.AdmissionSHA, req.AdmissionPath = binding.BinarySHA256, path
		}
	} else if !os.IsNotExist(err) || req.Operation == "status" || req.Operation == "detach" || req.Operation == "cleanup" {
		return fail(err)
	} else {
		id, err := hostID()
		if err != nil {
			return fail(err)
		}
		binding = hostRequestBinding{RequiredHostInstanceID: req.RequiredHostInstanceID, Schema: 1, RequestID: id, Operation: req.Operation, ProjectDir: req.ProjectDir, ConfigPath: req.ConfigPath, ConfigSHA256: req.ConfigSHA256, Mode: req.Mode, BinarySHA256: req.BinarySHA256, HostDir: req.HostDir, CleanupCompleted: req.CleanupCompleted}
	}
	binding.EnvironmentSHA256 = hostDigest([]byte(strings.Join(req.Environment, "\x00")))
	binding.Status = "pending"
	if err := saveHostRequest(path, binding); err != nil {
		return fail(err)
	}
	req.RequestID = binding.RequestID
	return &binding, unlock, nil
}

func saveHostRequest(path string, binding hostRequestBinding) error {
	data, err := json.MarshalIndent(binding, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), "host-request.*.tmp")
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
	return os.Rename(f.Name(), path)
}

func hostReceiptMatches(req hostRequest, r hostReceipt) bool {
	if req.ProjectDir != r.ProjectDir {
		return false
	}
	if req.ContextID != "" && (req.ContextID != r.ContextID || req.InstanceID != r.HostInstanceID) {
		return false
	}
	if req.RequestID != "" {
		return req.RequestID == r.RequestID && req.ConfigSHA256 == r.ConfigSHA256 && req.ConfigPath == r.ConfigPath && req.Mode == r.Mode && req.CleanupCompleted == r.CleanupCompleted
	}
	if req.ContextID != "" {
		return req.ContextID == r.ContextID && req.InstanceID == r.HostInstanceID
	}
	return req.ConfigSHA256 != "" && req.ConfigSHA256 == r.ConfigSHA256 && req.ConfigPath == r.ConfigPath && req.Mode == r.Mode
}

func ambiguousHostLookup(req hostRequest, receipts []hostReceipt) bool {
	if req.RequestID != "" || req.ContextID != "" {
		return false
	}
	count := 0
	for _, r := range receipts {
		if hostReceiptMatches(req, r) {
			count++
		}
	}
	return count > 1
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
			if requestBinding != nil && requestBinding.Status == "pending" && response.ContextState == "active" {
				if bound.Current.ContextState != "active" || !hostReceiptMatches(req, response.hostReceipt) ||
					response.Schema != 1 || response.Protocol != hostProtocol || response.HostDir != bound.HostRoot ||
					response.HostPID != bound.Current.HostPID || response.BinaryPath != bound.Current.BinaryPath ||
					response.BinarySHA256 != bound.Current.BinarySHA256 {
					return hostError("unknown_effect", errors.New("active admission receipt differs from the original native owner"))
				}
				requestBinding.Status = "accepted"
				if err := saveHostRequest(req.RequestFile, *requestBinding); err != nil {
					return hostError("unknown_effect", err)
				}
				reference, err := loopruntime.ReadSessionReadbackFile(req.RequestFile, nil)
				if err != nil {
					return hostError("unknown_effect", err)
				}
				bound.Request, bound.Sources["original_request"] = reference, reference
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
