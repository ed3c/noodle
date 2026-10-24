package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/poteto/noodle/internal/execution"
	"github.com/poteto/noodle/loop"
	"github.com/poteto/noodle/worktree"
)

type hostContextSelection struct {
	ProjectDir   string   `json:"project_dir"`
	ContextID    string   `json:"context_id"`
	InstanceID   string   `json:"instance_id"`
	ReadbackArgv []string `json:"readback_argv"`
}

type hostWorkspaceInspection struct {
	ObservationScope string                     `json:"observation_scope"`
	LiveVerified     bool                       `json:"live_verified"`
	SchemaVersion    int                        `json:"schema_version"`
	Status           string                     `json:"status"`
	ProjectDir       string                     `json:"project_dir"`
	Selections       []hostContextSelection     `json:"selections"`
	Contexts         []hostReceipt              `json:"contexts"`
	Workspaces       []loop.WorkspaceInspection `json:"workspaces"`
	Gaps             []loop.InspectionGap       `json:"gaps"`
}

// hostReadDirectory resolves the one configured endpoint without creating it.
func hostReadDirectory() (string, error) {
	dir := os.Getenv("NOODLE_HOST_DIR")
	if dir == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return "", err
		}
		dir = filepath.Join(cache, "noodle", "host")
	}
	return filepath.Abs(dir)
}

// readHostSelector treats historical request files as data selectors, not caller credentials.
func readHostSelector(req hostRequest) (hostRequest, error) {
	project, err := filepath.Abs(req.ProjectDir)
	if err != nil {
		return req, err
	}
	project, err = filepath.EvalSymlinks(project)
	if err != nil {
		return req, err
	}
	req.ProjectDir = project
	if (req.ContextID == "") != (req.InstanceID == "") {
		return req, fmt.Errorf("context-id and instance-id are required together")
	}
	if req.HostDir == "" {
		req.HostDir, err = hostReadDirectory()
		if err != nil {
			return req, err
		}
	}
	if req.RequestFile == "" {
		return req, nil
	}
	binding, err := readHostRequestFile(req.RequestFile)
	if err != nil {
		return req, err
	}
	bindingProject, err := filepath.EvalSymlinks(binding.ProjectDir)
	if err != nil {
		return req, err
	}
	if bindingProject != project || (req.ConfigSHA256 != "" && req.ConfigSHA256 != binding.ConfigSHA256) || (req.Mode != "" && req.Mode != binding.Mode) {
		return req, fmt.Errorf("request selector differs from project, config or mode")
	}
	req.HostDir, req.RequestID, req.ConfigSHA256, req.Mode = binding.HostDir, binding.RequestID, binding.ConfigSHA256, binding.Mode
	return req, nil
}

func readHostRequestFile(path string) (hostRequestBinding, error) {
	var binding hostRequestBinding
	data, err := os.ReadFile(path)
	if err != nil {
		return binding, err
	}
	if err = json.Unmarshal(data, &binding); err != nil {
		return binding, err
	}
	if binding.Schema != 1 || binding.RequestID == "" || !filepath.IsAbs(binding.HostDir) || (binding.Operation != "attach" && binding.Operation != "once") {
		return binding, fmt.Errorf("invalid native request selector")
	}
	return binding, nil
}

func readHostReceipts(dir string) ([]hostReceipt, error) {
	data, err := os.ReadFile(filepath.Join(dir, "receipts.json"))
	if err != nil {
		return nil, err
	}
	var receipts []hostReceipt
	if err = json.Unmarshal(data, &receipts); err != nil {
		return nil, err
	}
	return receipts, nil
}

func selectHostReceipt(req hostRequest, receipts []hostReceipt) (hostReceipt, error) {
	if req.ContextID == "" && req.RequestID == "" {
		return hostReceipt{}, fmt.Errorf("select a context-id and instance-id or request-file from host status")
	}
	var matches []hostReceipt
	for _, r := range receipts {
		if hostReceiptMatches(req, r) {
			matches = append(matches, r)
		}
	}
	if len(matches) != 1 {
		return hostReceipt{}, fmt.Errorf("native selector matched %d receipts; inspect host status and select one exact context", len(matches))
	}
	return matches[0], nil
}

func inspectHostWorkspace(req hostRequest) (hostWorkspaceInspection, error) {
	req, err := readHostSelector(req)
	if err != nil {
		return hostWorkspaceInspection{}, err
	}
	r := hostWorkspaceInspection{ObservationScope: "persisted_only", LiveVerified: false, SchemaVersion: 1, Status: "observed", ProjectDir: req.ProjectDir, Contexts: []hostReceipt{}, Selections: []hostContextSelection{}, Workspaces: []loop.WorkspaceInspection{}, Gaps: []loop.InspectionGap{}}
	paths := map[string]bool{req.ProjectDir: true}
	app, gitErr := worktree.NewAppWithExecution(execution.Capture(req.ProjectDir))
	if gitErr != nil {
		r.Gaps = append(r.Gaps, loop.InspectionGap{Path: req.ProjectDir, Code: "worktree_inventory", Detail: gitErr.Error()})
	} else {
		// Include the primary checkout too when the supplied workspace is a linked checkout.
		entries, err := app.WorktreeEntries()
		if err != nil {
			r.Gaps = append(r.Gaps, loop.InspectionGap{Path: req.ProjectDir, Code: "worktree_inventory", Detail: err.Error()})
		} else {
			for _, entry := range entries {
				paths[entry.Path] = true
			}
		}
	}
	names := make([]string, 0, len(paths))
	for path := range paths {
		names = append(names, path)
	}
	sort.Strings(names)
	for _, path := range names {
		r.Workspaces = append(r.Workspaces, loop.ReadWorkspaceInspection(path))
	}
	receipts, err := readHostReceipts(req.HostDir)
	if err != nil {
		code := "unreadable"
		if os.IsNotExist(err) {
			code = "missing"
		}
		r.Gaps = append(r.Gaps, loop.InspectionGap{Path: filepath.Join(req.HostDir, "receipts.json"), Code: code, Detail: err.Error()})
	} else {
		for _, receipt := range receipts {
			if req.ContextID != "" || req.RequestID != "" || req.ConfigSHA256 != "" {
				if hostReceiptMatches(req, receipt) {
					r.Contexts = append(r.Contexts, receipt)
				}
			} else if paths[receipt.ProjectDir] {
				r.Contexts = append(r.Contexts, receipt)
			}
		}
	}
	for _, receipt := range r.Contexts {
		r.Selections = append(r.Selections, hostContextSelection{ProjectDir: receipt.ProjectDir, ContextID: receipt.ContextID, InstanceID: receipt.HostInstanceID, ReadbackArgv: []string{os.Args[0], "--project-dir", receipt.ProjectDir, "host", "status", "--context-id", receipt.ContextID, "--instance-id", receipt.HostInstanceID}})
	}
	return r, nil
}

func readSelectedHostReceipt(req hostRequest) (hostRequest, hostReceipt, error) {
	req, err := readHostSelector(req)
	if err != nil {
		return req, hostReceipt{}, err
	}
	receipts, err := readHostReceipts(req.HostDir)
	if err != nil {
		return req, hostReceipt{}, err
	}
	receipt, err := selectHostReceipt(req, receipts)
	if err != nil {
		return req, hostReceipt{}, err
	}
	if receipt.Schema != 1 || receipt.Protocol != hostProtocol || receipt.RequestID == "" || receipt.ContextID == "" || receipt.HostInstanceID == "" || receipt.ConfigSHA256 == "" || receipt.BinarySHA256 == "" || (receipt.Mode != "manual" && receipt.Mode != "supervised" && receipt.Mode != "auto") {
		return req, hostReceipt{}, fmt.Errorf("selected native receipt is incomplete or unsupported")
	}
	if receipt.HostDir != req.HostDir {
		return req, hostReceipt{}, fmt.Errorf("selected receipt endpoint differs")
	}
	if req.RequestFile != "" {
		data, err := os.ReadFile(req.RequestFile)
		if err != nil {
			return req, hostReceipt{}, err
		}
		var binding hostRequestBinding
		if err = json.Unmarshal(data, &binding); err != nil {
			return req, hostReceipt{}, err
		}
		if binding.BinarySHA256 != receipt.BinarySHA256 {
			return req, hostReceipt{}, fmt.Errorf("request and receipt binary records differ")
		}
	}
	return req, receipt, nil
}

type hostSelectedStatus struct {
	hostResponse
	ObservationScope     string                  `json:"observation_scope"`
	LiveVerified         bool                    `json:"live_verified"`
	WorkspaceObservation hostWorkspaceInspection `json:"workspace_observation"`
}

func selectedHostStatus(inventory hostWorkspaceInspection) hostSelectedStatus {
	result := hostSelectedStatus{ObservationScope: "persisted_only", LiveVerified: false, WorkspaceObservation: inventory}
	switch len(inventory.Contexts) {
	case 0:
		result.hostResponse = hostError("not_found", fmt.Errorf("selected context is not present in retained receipts; inspect the reported source gaps"))
	case 1:
		result.hostResponse = hostResponse{hostReceipt: inventory.Contexts[0], Status: "observed"}
		result.WorkspaceObservation.Gaps = append(result.WorkspaceObservation.Gaps, loop.InspectionGap{Path: filepath.Join(inventory.Contexts[0].HostDir, "host.sock"), Code: "live_unverified", Detail: "persisted receipt does not establish Host or process liveness"})
	default:
		result.hostResponse = hostError("ambiguous", fmt.Errorf("selected identity matches multiple retained receipts; all candidates remain in workspace_observation"))
	}
	return result
}

// Selected status may observe a live Host, but a refusal never erases retained facts.
func observeSelectedHostStatus(ctx context.Context, req hostRequest, inventory hostWorkspaceInspection, persistedOnly bool) hostSelectedStatus {
	result := selectedHostStatus(inventory)
	if persistedOnly || len(inventory.Contexts) != 1 {
		return result
	}
	gap := func(err error) hostSelectedStatus {
		result.WorkspaceObservation.Gaps = append(result.WorkspaceObservation.Gaps, loop.InspectionGap{Path: filepath.Join(inventory.Contexts[0].HostDir, "host.sock"), Code: "live_unavailable", Detail: err.Error()})
		return result
	}
	receipt := inventory.Contexts[0]
	if receipt.ContextID == "" || receipt.HostInstanceID == "" || receipt.RequestID == "" || receipt.HostDir == "" {
		return gap(fmt.Errorf("retained context identity is incomplete"))
	}
	binaryPath, err := os.Executable()
	if err != nil {
		return gap(err)
	}
	binary, err := os.ReadFile(binaryPath)
	if err != nil {
		return gap(err)
	}
	request := hostRequest{Operation: "status", ProjectDir: receipt.ProjectDir, ContextID: receipt.ContextID, InstanceID: receipt.HostInstanceID, RequestID: receipt.RequestID, ConfigSHA256: receipt.ConfigSHA256, Mode: receipt.Mode, BinarySHA256: hostDigest(binary)}
	dialer := net.Dialer{Timeout: time.Second}
	conn, err := dialer.DialContext(ctx, "unix", filepath.Join(receipt.HostDir, "host.sock"))
	if err != nil {
		return gap(err)
	}
	defer conn.Close()
	if err = conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return gap(err)
	}
	if err = json.NewEncoder(conn).Encode(request); err != nil {
		return gap(err)
	}
	var live hostResponse
	if err = json.NewDecoder(io.LimitReader(conn, 4<<20)).Decode(&live); err != nil {
		return gap(err)
	}
	if live.Status != "ok" || live.Error != "" {
		return gap(fmt.Errorf("Host status %s: %s", live.Status, live.Error))
	}
	if !hostReceiptMatches(request, live.hostReceipt) || live.HostDir != receipt.HostDir || live.BinarySHA256 != receipt.BinarySHA256 {
		return gap(fmt.Errorf("Host response differs from selected native context"))
	}
	result.hostResponse, result.LiveVerified, result.ObservationScope = live, true, "live_host_reply"
	result.WorkspaceObservation.Gaps = slices.DeleteFunc(result.WorkspaceObservation.Gaps, func(g loop.InspectionGap) bool { return g.Code == "live_unverified" })
	return result
}
