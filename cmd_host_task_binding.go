package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	loopruntime "github.com/poteto/noodle/runtime"
)

type hostTaskClientBinding struct {
	Request                           hostRequest
	RequestRef, ReceiptRef, BinaryRef loopruntime.SessionReadbackFileRef
	Executable, HostDir               string
}

// Resolve the original owner; only acceptance readback permits completed detachment.
// Never attach, launch, or rewrite a request.
func originalHostTaskClient(project, requestFile, orderID, sessionID, operation string) (bound hostTaskClientBinding, err error) {
	project, err = filepath.EvalSymlinks(project)
	if err != nil {
		return bound, err
	}
	requestFile, err = filepath.Abs(requestFile)
	if err != nil {
		return bound, err
	}
	var binding hostRequestBinding
	requestRef, err := loopruntime.ReadSessionReadbackFile(requestFile, &binding)
	if err != nil {
		return bound, err
	}
	executable, err := os.Executable()
	if err != nil {
		return bound, err
	}
	binaryRef, err := hostReadbackBinaryRef(executable)
	if err != nil {
		return bound, err
	}
	if binding.Schema != 1 || binding.Operation != "attach" || binding.ProjectDir != project || binding.BinarySHA256 != binaryRef.SHA256 || binding.RequestID == "" || !filepath.IsAbs(binding.HostDir) {
		return bound, fmt.Errorf("original Host request project/binary binding differs")
	}
	var receipts []hostReceipt
	receiptRef, err := loopruntime.ReadSessionReadbackFile(filepath.Join(binding.HostDir, "receipts.json"), &receipts)
	if err != nil {
		return bound, err
	}
	req := hostRequest{Operation: operation, ProjectDir: project, RequestID: binding.RequestID, ConfigPath: binding.ConfigPath, ConfigSHA256: binding.ConfigSHA256, Mode: binding.Mode, BinarySHA256: binding.BinarySHA256, CleanupCompleted: binding.CleanupCompleted, ReadbackOrderID: orderID, ReadbackSessionID: sessionID, ReadbackRequest: requestRef}
	count := 0
	var original hostReceipt
	for _, receipt := range receipts {
		stateMatches := receipt.ContextState == "active"
		if operation == "task_delivery_acceptance_readback" {
			stateMatches = validHostTaskAcceptanceReceipt(receipt)
		}
		if hostReceiptMatches(req, receipt) && receipt.RequestID == binding.RequestID && receipt.HostDir == binding.HostDir && receipt.BinarySHA256 == binding.BinarySHA256 && stateMatches {
			original = receipt
			count++
		}
	}
	req.InstanceID, req.ContextID = original.HostInstanceID, original.ContextID
	if count != 1 || req.InstanceID == "" || req.ContextID == "" {
		return bound, fmt.Errorf("original active Host receipt missing or ambiguous")
	}
	return hostTaskClientBinding{Request: req, RequestRef: requestRef, ReceiptRef: receiptRef, BinaryRef: binaryRef, Executable: executable, HostDir: binding.HostDir}, nil
}

func (b hostTaskClientBinding) exchange(ctx context.Context, result any) (sent bool, err error) {
	conn, err := net.DialTimeout("unix", filepath.Join(b.HostDir, "host.sock"), time.Second)
	if err != nil {
		return sent, fmt.Errorf("original Host is unavailable; no launch attempted: %w", err)
	}
	defer conn.Close()
	deadline := time.Now().Add(15 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err = conn.SetDeadline(deadline); err != nil {
		return sent, err
	}
	sent = true
	if err = json.NewEncoder(conn).Encode(b.Request); err != nil {
		return sent, err
	}
	if err = json.NewDecoder(io.LimitReader(conn, 1<<20)).Decode(result); err != nil {
		return sent, err
	}
	return sent, nil
}

func (b hostTaskClientBinding) verify() error {
	if err := loopruntime.VerifySessionReadbackFiles(map[string]loopruntime.SessionReadbackFileRef{"original_request": b.RequestRef, "host_receipts": b.ReceiptRef}); err != nil {
		return err
	}
	currentBinary, err := hostReadbackBinaryRef(b.Executable)
	if err != nil || currentBinary != b.BinaryRef {
		return fmt.Errorf("original Host client binary changed during readback")
	}
	return nil
}

// The Host actor owns completion. This check never consumes the done channel.
func (h *noodleHost) originalTaskContext(req hostRequest) (*hostContext, map[string]loopruntime.SessionReadbackFileRef, error) {
	sources := map[string]loopruntime.SessionReadbackFileRef{}
	if h.blocked || req.InstanceID != h.instanceID || req.BinarySHA256 != h.binarySHA || req.ContextID == "" || req.RequestID == "" || req.ReadbackOrderID == "" || req.ReadbackSessionID == "" {
		return nil, nil, fmt.Errorf("exact original Host session binding unavailable")
	}
	owner := h.contexts[req.ContextID]
	if owner == nil || !owner.openShell || owner.failed || owner.done == nil || owner.receipt.ContextState != "active" || !hostReceiptMatches(req, owner.receipt) {
		return nil, nil, fmt.Errorf("original Host no longer owns the active context")
	}
	// Completion remains owned by the Host event loop; do not drain owner.done.
	var binding hostRequestBinding
	ref, err := loopruntime.ReadSessionReadbackFile(req.ReadbackRequest.Path, &binding)
	if err != nil {
		return nil, nil, err
	}
	if ref != req.ReadbackRequest || binding.Schema != 1 || binding.Operation != "attach" || binding.HostDir != h.dir || binding.ProjectDir != req.ProjectDir || binding.RequestID != req.RequestID || binding.BinarySHA256 != req.BinarySHA256 || binding.ConfigPath != req.ConfigPath || binding.ConfigSHA256 != req.ConfigSHA256 || binding.Mode != req.Mode || binding.CleanupCompleted != req.CleanupCompleted || binding.EnvironmentSHA256 != owner.environmentSHA {
		return nil, nil, fmt.Errorf("original Host request source differs")
	}
	sources["original_request"] = ref
	configPath := req.ConfigPath
	if configPath == "" {
		configPath = filepath.Join(req.ProjectDir, ".noodle.toml")
	}
	ref, err = loopruntime.ReadSessionReadbackFile(configPath, nil)
	if err != nil {
		return nil, nil, err
	}
	if ref.SHA256 != req.ConfigSHA256 {
		return nil, nil, fmt.Errorf("original Host configuration source changed")
	}
	sources["config"] = ref
	return owner, sources, nil
}
