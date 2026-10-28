package main

import (
	"fmt"
	"slices"

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
func (h *noodleHost) prepareOpenShell(filename string) error {
	if h.residents == nil || h.budget == nil {
		return fmt.Errorf("OpenShell Host admission/resident owner unavailable")
	}
	p, err := loopruntime.LoadOpenShellProfile(filename)
	if err != nil {
		return err
	}
	if h.openShell != nil && !h.openShellContextsActive() {
		if err = h.openShell.Close(); err != nil {
			return fmt.Errorf("original OpenShell gateway cleanup unresolved: %w", err)
		}
		h.openShell = nil
		h.openShellProfile = nil
	}
	if h.openShell != nil {
		if h.openShellProfile == nil || h.openShellProfile.Gateway.Binding != p.Gateway.Binding {
			return fmt.Errorf("OpenShell profile conflicts with the Host's active gateway binding")
		}
		return nil
	}
	owner, err := loopruntime.NewOpenShellGateway(h.residents, h.budget, p.Gateway)
	if owner != nil {
		h.openShell = owner
		h.openShellProfile = &p
	}
	return err
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
