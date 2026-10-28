package main

import (
	"errors"
	"fmt"
	"os"
)

func (h *noodleHost) finish(c *hostContext, runErr error) {
	c.done = nil
	c.runtime.Shutdown()
	h.applyHostFinish(c, runErr, h.releaseContextResources(c.receipt.ContextID, c.openShell))
}

// Called with stable original owner handles; draining runs this outside the actor.
func (h *noodleHost) releaseContextResources(contextID string, openShell bool) (releaseErr error) {
	if openShell && h.openShell != nil {
		if err := h.releaseOpenShell(contextID); err != nil {
			releaseErr = errors.Join(releaseErr, fmt.Errorf("OpenShell context release failed: %w", err))
		}
	}
	if h.residents != nil {
		if err := h.residents.ReleaseContext(contextID); err != nil {
			releaseErr = errors.Join(releaseErr, fmt.Errorf("resident context release failed: %w", err))
		}
	}
	return releaseErr
}

func (h *noodleHost) applyHostFinish(c *hostContext, runErr, releaseErr error) {
	c.finalized, c.finalizing = true, false
	c.releaseErr = releaseErr
	c.receiptSaved = false
	if releaseErr != nil {
		h.blocked, c.failed = true, true
		fmt.Fprintln(os.Stderr, releaseErr)
	}
	c.cancel()
	if c.failed || runErr != nil || (h.blocked && c.receipt.AdmissionRefusal != "") {
		c.receipt.ContextState = "failed"
		c.receipt.TerminalCause = ""
		c.failed = true
	} else {
		c.receipt.ContextState = "detached"
		if !h.blocked {
			c.receipt.TerminalCause = c.runtime.TerminalCause()
		}
	}
	for i := range h.receipts {
		if h.receipts[i].ContextID == c.receipt.ContextID {
			h.receipts[i] = c.receipt
			break
		}
	}
	if err := h.save(); err != nil {
		c.failed = true
		h.blocked = true
		c.receipt.TerminalCause = ""
		for i := range h.receipts {
			if h.receipts[i].ContextID == c.receipt.ContextID {
				h.receipts[i].TerminalCause = ""
				break
			}
		}
		fmt.Fprintln(os.Stderr, "host receipt persistence failed:", err)
	} else {
		c.receiptSaved = true
	}
	if !c.failed && h.shutdown == nil {
		c.lock.Close()
		delete(h.contexts, c.receipt.ContextID)
		if h.budget != nil {
			h.budget.Unregister(c.receipt.ContextID)
		}
	}
}
