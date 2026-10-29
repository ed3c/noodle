package main

import (
	"fmt"
	"path/filepath"

	loopruntime "github.com/poteto/noodle/runtime"
)

// Original admission and current execution are separate native identities.
// This projection selects existing records; it never transfers their ownership.
type hostOwnerBinding struct {
	Schema          int                                           `json:"schema"`
	HostRoot        string                                        `json:"host_root"`
	GenerationDir   string                                        `json:"generation_dir"`
	Request         loopruntime.SessionReadbackFileRef            `json:"request"`
	AdmissionBinary loopruntime.SessionReadbackFileRef            `json:"admission_binary"`
	ExecutionBinary loopruntime.SessionReadbackFileRef            `json:"execution_binary"`
	Original        hostReceipt                                   `json:"original"`
	Current         hostReceipt                                   `json:"current"`
	Sources         map[string]loopruntime.SessionReadbackFileRef `json:"sources"`
}

func readHostOwnerBinding(requestFile string) (*hostOwnerBinding, error) {
	var b hostRequestBinding
	request, err := loopruntime.ReadSessionReadbackFile(requestFile, &b)
	if err != nil {
		return nil, err
	}
	if b.Schema != 1 || b.Operation != "attach" || b.RequestID == "" || !filepath.IsAbs(b.HostDir) {
		return nil, fmt.Errorf("original native admission binding unavailable")
	}
	poolRef, err := loopruntime.ReadSessionReadbackFile(filepath.Join(b.HostDir, "pool.json"), nil)
	if err != nil {
		return nil, err
	}
	p, err := readHostPool(b.HostDir)
	if err != nil {
		return nil, err
	}
	o, ok := p.Owners[b.RequestID]
	req := hostRequest{ProjectDir: b.ProjectDir, RequestID: b.RequestID, ConfigPath: b.ConfigPath,
		ConfigSHA256: b.ConfigSHA256, Mode: b.Mode, CleanupCompleted: b.CleanupCompleted}
	if !ok || o.MaintenanceHold != "" || !hostReceiptMatches(req, o.Original) ||
		o.Original.HostDir != b.HostDir || o.Original.BinarySHA256 != b.BinarySHA256 || o.EnvironmentSHA != b.EnvironmentSHA256 {
		return nil, fmt.Errorf("original admission differs from pool owner")
	}
	g, ok := p.Generations[o.Current.HostInstanceID]
	if !ok || !hostReceiptMatches(req, o.Current) || o.Current.BinaryPath != g.Binary || o.Current.BinarySHA256 != g.SHA {
		return nil, fmt.Errorf("current execution generation differs from pool owner")
	}
	admission, err := hostReadbackBinaryRef(o.Original.BinaryPath)
	if err != nil || admission.SHA256 != b.BinarySHA256 {
		return nil, fmt.Errorf("original admission executable changed")
	}
	known := false
	for _, generation := range p.Generations {
		known = known || (generation.Binary == admission.Path && generation.SHA == admission.SHA256)
	}
	if !known {
		return nil, fmt.Errorf("admission executable is outside the original pool family")
	}
	execution, err := hostReadbackBinaryRef(g.Binary)
	if err != nil || execution.SHA256 != g.SHA {
		return nil, fmt.Errorf("current execution executable changed")
	}
	var receipts []hostReceipt
	receiptRef, err := loopruntime.ReadSessionReadbackFile(filepath.Join(g.Directory, "receipts.json"), &receipts)
	if err != nil {
		return nil, err
	}
	count := 0
	current := o.Current
	for _, r := range receipts {
		if r.ContextID == o.Current.ContextID && r.HostInstanceID == g.ID && r.HostPID == o.Current.HostPID &&
			r.HostDir == b.HostDir && r.BinarySHA256 == g.SHA && r.BinaryPath == g.Binary && hostReceiptMatches(req, r) {
			count++
			current = r
		}
	}
	if count != 1 || o.Current.ContextID == "" {
		return nil, fmt.Errorf("current execution receipt missing or ambiguous")
	}
	sources := map[string]loopruntime.SessionReadbackFileRef{"original_request": request, "host_pool": poolRef,
		"host_receipts": receiptRef, "admission_binary": admission, "execution_binary": execution}
	if err = loopruntime.VerifySessionReadbackFiles(sources); err != nil {
		return nil, err
	}
	return &hostOwnerBinding{1, b.HostDir, g.Directory, request, admission, execution, o.Original, current, sources}, nil
}
