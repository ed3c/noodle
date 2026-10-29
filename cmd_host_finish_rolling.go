package main

import (
	"errors"
	"fmt"
	"github.com/poteto/noodle/loop"
	"os"
)

func (h *noodleHost) finishRollingContext(c *hostContext) {
	if h.rolling != nil && !c.failed {
		if err := updateHostPool(h.rolling.root, func(p *hostPool) error {
			owner, ok := p.Owners[c.receipt.RequestID]
			if !ok {
				return nil
			}
			if owner.Current.HostInstanceID != h.instanceID {
				return errors.New("completed context no longer owns its native route")
			}
			owner.Current = c.receipt
			owner.State = "terminal"
			p.Owners[c.receipt.RequestID] = owner
			return nil
		}); err != nil {
			h.blocked = true
			c.failed = true
			h.rolling.failure = err.Error()
			c.receipt.ContextState = "failed"
			c.receipt.TerminalCause = ""
			if saveErr := h.persistHostContextReceipt(c); saveErr != nil {
				fmt.Fprintln(os.Stderr, "rolling failure receipt persistence failed:", saveErr)
			}
		}
	}
	if h.rolling != nil && !c.failed && h.rolling.retiring && c.receipt.TerminalCause == loop.TerminalMigrationDetach {
		if err := h.recordRollingBoundary(c); err != nil {
			h.rolling.failure = err.Error()
		}
	}
}
