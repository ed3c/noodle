package main

import (
	"fmt"
	"os"
)

func (h *noodleHost) finish(c *hostContext, runErr error) {
	c.done = nil
	c.runtime.Shutdown()
	if c.openShell && h.openShell != nil {
		if err := h.releaseOpenShell(c.receipt.ContextID); err != nil {
			h.blocked = true
			c.failed = true
			fmt.Fprintln(os.Stderr, "OpenShell context release failed:", err)
		}
	}
	if h.residents != nil {
		if err := h.residents.ReleaseContext(c.receipt.ContextID); err != nil {
			h.blocked = true
			c.failed = true
			fmt.Fprintln(os.Stderr, "resident context release failed:", err)
		}
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
	}
	if !c.failed {
		c.lock.Close()
		delete(h.contexts, c.receipt.ContextID)
		if h.budget != nil {
			h.budget.Unregister(c.receipt.ContextID)
		}
	}
}
