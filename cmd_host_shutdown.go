package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"

	"github.com/poteto/noodle/internal/admission"
)

// The Host actor owns this state. The single release worker only uses existing
// runtime/resident handles and sends results back; it never changes receipts.
type hostShutdown struct {
	jobs       chan hostShutdownJob
	results    chan hostShutdownResult
	pending    int
	diagnostic string
}

type hostShutdownJob struct {
	contextID string
	runtime   hostRuntime
	openShell bool
	runErr    error
	request   hostRequest
	reply     chan any
}

type hostShutdownResult struct {
	job      hostShutdownJob
	err      error
	resident admission.Resident
}

func (h *noodleHost) beginShutdown(serviceCtx context.Context) {
	if h.shutdown != nil {
		return
	}
	s := &hostShutdown{jobs: make(chan hostShutdownJob, len(h.contexts)+1), results: make(chan hostShutdownResult, len(h.contexts)+1)}
	h.shutdown = s
	go func() {
		for {
			select {
			case <-serviceCtx.Done():
				return
			case job := <-s.jobs:
				result := hostShutdownResult{job: job}
				if job.runtime != nil {
					job.runtime.Shutdown()
					result.err = h.releaseContextResources(job.contextID, job.openShell)
				} else {
					result.resident, result.err = h.residents.Release(job.request.ResidentID, job.contextID)
				}
				select {
				case s.results <- result:
				case <-serviceCtx.Done():
					return
				}
			}
		}
	}()
	// Let each original Loop stop its own sessions before releasing its services.
	for _, c := range h.contexts {
		c.cancel()
	}
}

func (h *noodleHost) completeContext(c *hostContext, runErr error) {
	if h.shutdown == nil {
		h.finish(c, runErr)
		return
	}
	c.done = nil
	c.finalizing = true
	h.shutdown.pending++
	h.shutdown.jobs <- hostShutdownJob{contextID: c.receipt.ContextID, runtime: c.runtime, openShell: c.openShell, runErr: runErr}
}

func (h *noodleHost) applyShutdownResult(result hostShutdownResult) {
	h.shutdown.pending--
	c := h.contexts[result.job.contextID]
	if result.job.runtime != nil {
		h.applyHostFinish(c, result.job.runErr, result.err)
		return
	}
	c.finalizing = false
	c.releaseErr = result.err
	if result.err == nil {
		rows, err := h.residents.Snapshot()
		c.releaseErr = err
		for _, row := range rows {
			if slices.Contains(row.Contexts, c.receipt.ContextID) {
				c.releaseErr = errors.Join(c.releaseErr, fmt.Errorf("original context still owns resident %s", row.ID))
			}
		}
	}
	response := map[string]any{"schema": 1, "owner": "noodle.swap-manager", "host_instance_id": h.instanceID, "context_id": c.receipt.ContextID, "status": result.resident.State, "resident": result.resident, "resources": h.budget.Snapshot(), "next": nil}
	if result.err != nil {
		response["status"], response["error"] = "refused", result.err.Error()
		fmt.Fprintln(os.Stderr, "resident shutdown release failed:", result.err)
	}
	result.job.reply <- response
}

func (h *noodleHost) handleShutdownCall(req hostRequest, reply chan any) bool {
	switch req.Operation {
	case "status", "resources", "view_resources", "resident_inspect", "resident_inspect_bound", "resident_reconcile":
		return false
	case "resident_release_bound":
		c, err := h.shutdownReleaseOwner(req)
		if err != nil {
			reply <- map[string]any{"schema": 1, "owner": "noodle.swap-manager", "status": "refused", "error": err.Error(), "host_instance_id": h.instanceID}
			return true
		}
		c.finalizing = true
		h.shutdown.pending++
		h.shutdown.jobs <- hostShutdownJob{contextID: c.receipt.ContextID, request: req, reply: reply}
	case "task_session_readback":
		reply <- hostSessionReadback{SchemaVersion: 1, Status: "refused", Error: "Host shutdown is draining the original Loop", HostInstanceID: h.instanceID, ContextID: req.ContextID, RequestID: req.RequestID, Effects: []string{}}
	default:
		reply <- hostError("refused", errors.New("Host shutdown in progress; new work and context mutations refused"))
	}
	return true
}

func (h *noodleHost) shutdownReleaseOwner(req hostRequest) (*hostContext, error) {
	c := h.contexts[req.ContextID]
	if h.shutdown == nil || req.Operation != "resident_release_bound" || req.ContextID == "" || req.RequestID == "" || req.ResidentID == "" || req.InstanceID != h.instanceID || req.BinarySHA256 != h.binarySHA || c == nil || !hostReceiptMatches(req, c.receipt) || c.done != nil || c.finalizing || !c.finalized {
		return nil, errors.New("original completed context binding unavailable for shutdown release")
	}
	rows, err := h.residents.Snapshot()
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if row.ID == req.ResidentID && (slices.Contains(row.Contexts, req.ContextID) || slices.Contains(row.Released, req.ContextID)) {
			return c, nil
		}
	}
	return nil, errors.New("resident identity does not belong to the original context")
}

// Failed task outcomes remain failed. Only original resource evidence permits
// the service lifetime to end; no timeout or empty children count substitutes.
func (h *noodleHost) shutdownReady() error {
	if h.shutdown == nil || h.shutdown.pending != 0 {
		return errors.New("Host resource finalization is pending")
	}
	for _, c := range h.contexts {
		if c.done != nil || c.finalizing || !c.finalized {
			return fmt.Errorf("context %s original completion is pending", c.receipt.ContextID)
		}
		if c.releaseErr != nil {
			return fmt.Errorf("context %s resource release unresolved: %w", c.receipt.ContextID, c.releaseErr)
		}
		if !c.receiptSaved {
			return fmt.Errorf("context %s receipt persistence unresolved", c.receipt.ContextID)
		}
	}
	if h.budget == nil || h.residents == nil {
		return errors.New("Host original resource owners unavailable")
	}
	resources := h.budget.Snapshot()
	if resources.Allocations != 0 || resources.UnresolvedResidents != 0 {
		return errors.New("Host original resource allocations remain")
	}
	rows, err := h.residents.Snapshot()
	if err != nil {
		return err
	}
	names := make(map[string]bool)
	for _, row := range rows {
		if !names[row.Name] {
			if err := h.residents.CheckServiceClosed(row.Name); err != nil {
				return fmt.Errorf("resident %s: %w", row.Name, err)
			}
			names[row.Name] = true
		}
	}
	return h.closeIdleOpenShell()
}

func (h *noodleHost) releaseShutdownLocks() {
	for _, c := range h.contexts {
		c.lock.Close()
		h.budget.Unregister(c.receipt.ContextID)
	}
}
