package main

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/poteto/noodle/loop"
	loopruntime "github.com/poteto/noodle/runtime"
)

type hostTaskAcceptanceClient struct {
	hostTaskClientBinding
	originalReceipt hostReceipt
}

func validHostTaskAcceptanceReceipt(receipt hostReceipt) bool {
	if receipt.AdmissionRefusal != "" {
		return false
	}
	switch receipt.ContextState {
	case "active":
		return receipt.TerminalCause == ""
	case "detached":
		return receipt.TerminalCause == loop.TerminalExplicitCleanup ||
			(receipt.TerminalCause == loop.TerminalAutoCompleted && receipt.CleanupCompleted && receipt.Mode != "manual")
	}
	return false
}

func exactHostTaskAcceptanceReceipt(req hostRequest, hostDir string, receipts []hostReceipt) (hostReceipt, error) {
	var selected hostReceipt
	count := 0
	for _, receipt := range receipts {
		if receipt.RequestID == req.RequestID {
			selected = receipt
			count++
		}
	}
	if count != 1 || !hostReceiptMatches(req, selected) || selected.Schema != 1 || selected.Protocol != hostProtocol || selected.HostDir != hostDir || selected.BinarySHA256 != req.BinarySHA256 || !validHostTaskAcceptanceReceipt(selected) {
		return hostReceipt{}, fmt.Errorf("original acceptance Host receipt missing, ambiguous or unresolved")
	}
	return selected, nil
}

func originalHostTaskAcceptanceClient(project, requestFile, orderID, sessionID, operation string) (hostTaskAcceptanceClient, error) {
	bound, err := originalHostTaskClient(project, requestFile, orderID, sessionID, operation)
	if err != nil {
		return hostTaskAcceptanceClient{}, err
	}
	var receipts []hostReceipt
	ref, err := loopruntime.ReadSessionReadbackFile(bound.ReceiptRef.Path, &receipts)
	if err != nil {
		return hostTaskAcceptanceClient{}, err
	}
	if ref != bound.ReceiptRef {
		return hostTaskAcceptanceClient{}, fmt.Errorf("original acceptance Host receipt changed before selection")
	}
	original, err := exactHostTaskAcceptanceReceipt(bound.Request, bound.HostDir, receipts)
	return hostTaskAcceptanceClient{hostTaskClientBinding: bound, originalReceipt: original}, err
}

func (b hostTaskAcceptanceClient) verifyAcceptance() error {
	if err := loopruntime.VerifySessionReadbackFiles(map[string]loopruntime.SessionReadbackFileRef{"original_request": b.RequestRef}); err != nil {
		return err
	}
	currentBinary, err := hostReadbackBinaryRef(b.Executable)
	if err != nil || currentBinary != b.BinaryRef {
		return fmt.Errorf("original acceptance client binary changed")
	}
	var receipts []hostReceipt
	if _, err := loopruntime.ReadSessionReadbackFile(b.ReceiptRef.Path, &receipts); err != nil {
		return err
	}
	current, err := exactHostTaskAcceptanceReceipt(b.Request, b.HostDir, receipts)
	if err != nil {
		return err
	}
	expected := b.originalReceipt
	if expected.ContextState == "detached" && current.ContextState != "detached" {
		return fmt.Errorf("original acceptance context regressed after completion")
	}
	// Only the original owner's completed cleanup transition may change. Keep
	// PID, instance, context, request, binary, config and all admission fields exact.
	expected.ContextState, expected.TerminalCause = current.ContextState, current.TerminalCause
	if current != expected {
		return fmt.Errorf("original acceptance Host receipt identity changed")
	}
	return nil
}

func (h *noodleHost) taskAcceptanceOperation(ctx context.Context, req hostRequest) (hostTaskAcceptance, bool) {
	switch req.Operation {
	case "task_delivery_accept":
		return h.taskAcceptance(ctx, req), true
	case "task_delivery_acceptance_readback":
		return h.taskAcceptanceReadback(req), true
	}
	return hostTaskAcceptance{}, false
}

func (h *noodleHost) taskAcceptanceReadback(req hostRequest) hostTaskAcceptance {
	result := taskAcceptanceResponse(h.instanceID, req)
	fail := func(err error) hostTaskAcceptance { result.Error = err.Error(); return result }
	if req.Operation != "task_delivery_acceptance_readback" || h.blocked || req.InstanceID != h.instanceID || req.BinarySHA256 != h.binarySHA || req.ContextID == "" || req.RequestID == "" {
		return fail(fmt.Errorf("original acceptance Host instance binding unavailable"))
	}
	if err := verifyHostTaskAcceptance(req); err != nil {
		return fail(err)
	}
	receipt, err := exactHostTaskAcceptanceReceipt(req, h.dir, h.receipts)
	if err != nil {
		return fail(err)
	}
	if receipt.ContextState == "active" {
		_, result.Sources, err = h.originalTaskContext(req)
		if err != nil {
			return fail(err)
		}
	} else {
		var binding hostRequestBinding
		ref, err := loopruntime.ReadSessionReadbackFile(req.ReadbackRequest.Path, &binding)
		if err != nil {
			return fail(err)
		}
		if ref != req.ReadbackRequest || binding.Schema != 1 || binding.Operation != "attach" || binding.HostDir != h.dir || binding.ProjectDir != req.ProjectDir || binding.RequestID != req.RequestID || binding.BinarySHA256 != req.BinarySHA256 || binding.ConfigPath != req.ConfigPath || binding.ConfigSHA256 != req.ConfigSHA256 || binding.Mode != req.Mode || binding.CleanupCompleted != req.CleanupCompleted {
			return fail(fmt.Errorf("original detached acceptance request source differs"))
		}
		original, ok := h.taskReadbacks[req.ContextID]
		if !ok || original.environmentSHA != binding.EnvironmentSHA256 {
			return fail(fmt.Errorf("original detached acceptance execution binding unavailable"))
		}
		result.Sources["original_request"] = ref
		path := req.ConfigPath
		if path == "" {
			path = filepath.Join(req.ProjectDir, ".noodle.toml")
		}
		ref, err = loopruntime.ReadSessionReadbackFile(path, nil)
		if err != nil {
			return fail(err)
		}
		if ref.SHA256 != req.ConfigSHA256 {
			return fail(fmt.Errorf("original detached acceptance configuration changed"))
		}
		result.Sources["config"] = ref
	}
	result.Sources["acceptance"] = req.TaskAcceptance
	ref, err := readHostTaskAcceptanceReceipt(req)
	if err != nil {
		return fail(err)
	}
	reclamation, reclamationRef, err := readHostTaskAcceptanceReclamation(req, ref)
	if err != nil {
		return fail(err)
	}
	original, ok := h.taskReadbacks[req.ContextID]
	if !ok {
		return fail(fmt.Errorf("original worktree readback owner unavailable"))
	}
	for name, head := range reclamation.Worktrees {
		if err := original.worktrees.ObserveSettledExecutionAbsent(name, head); err != nil {
			return fail(err)
		}
	}
	result.Sources["reclamation_receipt"] = reclamationRef
	if err := loopruntime.VerifySessionReadbackFiles(result.Sources); err != nil {
		return fail(err)
	}
	// Native absence was observed by the original owner at cleanup, never freshly
	// queried after detach. This cannot establish receipt of the mutation's ACK.
	result.Status, result.ClosureReceipt = "reclaimed", &ref
	result.ObservationBasis = "original_owner_reclamation_readback"
	result.Reclamation, result.ReclamationReceipt = &reclamation, &reclamationRef
	return result
}
