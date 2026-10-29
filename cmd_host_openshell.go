package main

import (
	"errors"
	"fmt"
	"slices"

	"github.com/poteto/noodle/internal/admission"
	loopruntime "github.com/poteto/noodle/runtime"
)

func (h *noodleHost) openShellContextsActive() bool {
	for _, c := range h.contexts {
		if c.openShell && c.done != nil {
			return true
		}
	}
	return false
}

// A new generation is possible only after all bound contexts finish and the
// original resident child is conclusively reaped. Unknown effects retain owner.
func (h *noodleHost) closeIdleOpenShell() error {
	if h.openShell == nil || h.openShellContextsActive() {
		return nil
	}
	if err := h.openShell.Close(); err != nil {
		return err
	}
	h.openShell = nil
	h.openShellProfile = nil
	return nil
}
func (h *noodleHost) prepareOpenShell(filename string) (*loopruntime.OpenShellProfile, error) {
	if h.residents == nil || h.budget == nil {
		return nil, fmt.Errorf("OpenShell Host admission/resident owner unavailable")
	}
	p, err := loopruntime.LoadOpenShellProfile(filename)
	if err != nil {
		return nil, err
	}
	return h.bindOpenShellProfile(p)
}
func (h *noodleHost) bindOpenShellProfile(p loopruntime.OpenShellProfile) (*loopruntime.OpenShellProfile, error) {
	p.Gateway.ControlRoot = h.controlRoot()
	if h.openShell != nil && !h.openShellContextsActive() {
		if err := h.openShell.Close(); err != nil {
			return nil, fmt.Errorf("original OpenShell gateway cleanup unresolved: %w", err)
		}
		h.openShell = nil
		h.openShellProfile = nil
	}
	if h.openShell != nil {
		if h.openShellProfile == nil || h.openShellProfile.Gateway.Binding != p.Gateway.Binding {
			return nil, fmt.Errorf("OpenShell profile conflicts with the Host's active gateway binding")
		}
		return &p, nil
	}
	owner, err := loopruntime.NewOpenShellGateway(h.residents, h.budget, p.Gateway)
	if owner != nil {
		h.openShell = owner
		h.openShellProfile = &p
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}
func (h *noodleHost) releaseOpenShell(contextID string) error {
	// A pre-spawn refusal has no resident lease. Do not invent a generation just
	// to release it; on acquisition ambiguity the retained record decides.
	records, err := h.residents.Snapshot()
	if err != nil {
		return err
	}
	for _, r := range records {
		if r.Name == h.openShellProfile.Gateway.Resident.Name && slices.Contains(r.Contexts, contextID) {
			_, err = h.openShell.Release(contextID)
			return err
		}
	}
	return nil
}

// A pressure refusal precedes resident creation. Only fresh original-owner
// evidence of no lease permits releasing this never-started context.
func (h *noodleHost) finishOpenShellDeferral(c *hostContext, cause error) (hostResponse, bool) {
	if !errors.Is(cause, admission.ErrDeferred) {
		return hostResponse{}, false
	}
	records, err := h.residents.Snapshot()
	if err != nil {
		return hostResponse{}, false
	}
	for _, record := range records {
		if slices.Contains(record.Contexts, c.receipt.ContextID) {
			return hostResponse{}, false
		}
	}
	c.receipt.AdmissionRefusal = cause.Error()
	h.finish(c, nil)
	if h.blocked || c.failed {
		return hostResponse{c.receipt, "unknown_effect", "pre-start refusal cleanup or receipt persistence failed"}, true
	}
	return hostReceiptResult(c.receipt), true
}

func hostReceiptResult(receipt hostReceipt) hostResponse {
	if receipt.AdmissionRefusal != "" {
		return hostResponse{receipt, "deferred", receipt.AdmissionRefusal}
	}
	return hostResponse{receipt, "ok", ""}
}
