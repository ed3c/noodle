package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/poteto/noodle/internal/filex"
)

// History is separate from the original display map so running Hosts continue
// to read exactly the same registry format after an offline CLI update.
type hostViewSuccessor struct {
	SchemaVersion   int             `json:"schema_version"`
	Previous        hostViewBinding `json:"previous"`
	Successor       hostViewBinding `json:"successor"`
	DetachedReceipt hostReceipt     `json:"detached_receipt"`
	ActiveReceipt   hostReceipt     `json:"active_receipt"`
}

func retainHostViewSuccessor(dir string, previous, successor hostViewBinding) error {
	if previous.RunID != successor.RunID || previous.OrderID != successor.OrderID || previous.Request.ProjectDir != successor.Request.ProjectDir || previous.Request.ConfigSHA256 != successor.Request.ConfigSHA256 || previous.Request.HostDir != dir || successor.Request.HostDir != dir || previous.Request.RequestID == successor.Request.RequestID {
		return fmt.Errorf("display successor run, repository, order or config differs")
	}
	data, err := os.ReadFile(filepath.Join(dir, "receipts.json"))
	if err != nil {
		return fmt.Errorf("display successor native receipts unavailable: %w", err)
	}
	var receipts []hostReceipt
	if err := json.Unmarshal(data, &receipts); err != nil {
		return err
	}
	old, err := exactViewReceipt(previous.Request, receipts)
	if err != nil {
		return err
	}
	current, err := exactViewReceipt(successor.Request, receipts)
	if err != nil {
		return err
	}
	if old.ContextState != "detached" || current.ContextState != "active" || old.ContextID == current.ContextID {
		return fmt.Errorf("display successor lacks detached predecessor and active new context")
	}
	history := hostViewSuccessor{SchemaVersion: 1, Previous: previous, Successor: successor, DetachedReceipt: old, ActiveReceipt: current}
	encoded, err := json.Marshal(history)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "loop-view-binding-history", previous.RunID, hostDigest(encoded)+".json")
	retained, err := os.ReadFile(path)
	if err == nil {
		if !bytes.Equal(retained, encoded) {
			return fmt.Errorf("display successor retained history differs")
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return filex.WriteFileAtomicDurable(path, encoded)
}

func exactViewReceipt(request hostRequestBinding, receipts []hostReceipt) (hostReceipt, error) {
	var found hostReceipt
	if request.RequestID == "" || request.ConfigSHA256 == "" || request.BinarySHA256 == "" || !filepath.IsAbs(request.ProjectDir) || (request.Mode != "manual" && request.Mode != "supervised" && request.Mode != "auto") {
		return found, fmt.Errorf("display successor original request is incomplete")
	}
	count := 0
	for _, r := range receipts {
		if r.RequestID != request.RequestID {
			continue
		}
		count++
		if request.Schema != 1 || request.Operation != "attach" || r.Schema != 1 || r.Protocol != hostProtocol || r.ProjectDir != request.ProjectDir || r.ConfigSHA256 != request.ConfigSHA256 || r.BinarySHA256 != request.BinarySHA256 || r.Mode != request.Mode || r.HostDir != request.HostDir || r.ContextID == "" || r.HostInstanceID == "" || r.HostPID <= 1 || !filepath.IsAbs(r.BinaryPath) {
			return found, fmt.Errorf("display successor native receipt binding differs")
		}
		found = r
	}
	if count != 1 {
		return found, fmt.Errorf("display successor native receipt missing or ambiguous")
	}
	return found, nil
}
