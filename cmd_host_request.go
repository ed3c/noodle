package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/poteto/noodle/internal/lockfile"
)

type hostRequestBinding struct {
	Schema            int    `json:"schema"`
	RequestID         string `json:"request_id"`
	Operation         string `json:"operation"`
	ProjectDir        string `json:"project_dir"`
	ConfigSHA256      string `json:"config_sha256"`
	Mode              string `json:"mode"`
	BinarySHA256      string `json:"binary_sha256"`
	EnvironmentSHA256 string `json:"environment_sha256"`
	Status            string `json:"status"`
	HostDir           string `json:"host_dir"`
}

func bindHostRequest(req *hostRequest) (*hostRequestBinding, func(), error) {
	if req.Operation == "status" || req.Operation == "detach" {
		selected, err := readHostSelector(*req)
		if err != nil {
			return nil, nil, err
		}
		binding, err := readHostRequestFile(req.RequestFile)
		if err != nil {
			return nil, nil, err
		}
		*req = selected
		return &binding, func() {}, nil
	}
	if req.RequestFile == "" {
		return nil, nil, errors.New("attach and once need --request-file for exact response recovery")
	}
	path, err := filepath.Abs(req.RequestFile)
	if err != nil {
		return nil, nil, err
	}
	req.RequestFile = path
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, nil, err
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
		if binding.ProjectDir != req.ProjectDir || binding.BinarySHA256 != req.BinarySHA256 {
			return fail(errors.New("request project or binary binding changed"))
		}
		req.HostDir = binding.HostDir
		if binding.Operation != req.Operation || binding.ConfigSHA256 != req.ConfigSHA256 || binding.Mode != req.Mode {
			return fail(errors.New("original native request binding changed"))
		}
		if binding.Status != "busy" {
			req.Operation = "status"
			req.RequestID = binding.RequestID
			req.Environment = nil
			return &binding, unlock, nil
		}
	} else if !os.IsNotExist(err) {
		return fail(err)
	} else {
		id, err := hostID()
		if err != nil {
			return fail(err)
		}
		binding = hostRequestBinding{Schema: 1, RequestID: id, Operation: req.Operation, ProjectDir: req.ProjectDir, ConfigSHA256: req.ConfigSHA256, Mode: req.Mode, BinarySHA256: req.BinarySHA256, HostDir: req.HostDir}
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
		return req.RequestID == r.RequestID && req.ConfigSHA256 == r.ConfigSHA256 && req.Mode == r.Mode
	}
	if req.ContextID != "" {
		return req.ContextID == r.ContextID && req.InstanceID == r.HostInstanceID
	}
	return req.ConfigSHA256 != "" && req.ConfigSHA256 == r.ConfigSHA256 && (req.Mode == "" || req.Mode == r.Mode)
}

func ambiguousHostLookup(req hostRequest, receipts []hostReceipt) bool {
	count := 0
	for _, r := range receipts {
		if hostReceiptMatches(req, r) {
			count++
		}
	}
	return count > 1
}
