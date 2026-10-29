package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

func (h *noodleHost) loadReceipts() error {
	stored, err := os.ReadFile(filepath.Join(h.dir, "receipts.json"))
	if err == nil {
		if err := json.Unmarshal(stored, &h.receipts); err != nil {
			return fmt.Errorf("read host receipts: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	retired, err := verifySuccessorRetirements(h.dir, h.receipts)
	if err != nil {
		return err
	}
	for _, receipt := range h.receipts {
		if receipt.ContextState != "detached" && !retired[receipt.ContextID] {
			h.blocked = true
		}
	}
	return nil
}
