package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/loop"
)

// History is separate from the original display map so running Hosts continue
// to read exactly the same registry format after an offline CLI update.
type hostViewSuccessor struct {
	SchemaVersion    int                  `json:"schema_version"`
	Previous         hostViewBinding      `json:"previous"`
	Successor        hostViewBinding      `json:"successor"`
	PreviousReceipt  *hostReceipt         `json:"previous_receipt"`
	SuccessorReceipt *hostReceipt         `json:"successor_receipt"`
	Gaps             []loop.InspectionGap `json:"gaps"`
}

func retainHostViewSuccessor(dir string, previous, successor hostViewBinding) error {
	if previous.RunID != successor.RunID || previous.OrderID != successor.OrderID || previous.Request.ProjectDir != successor.Request.ProjectDir || previous.Request.HostDir != dir || successor.Request.HostDir != dir || previous.Request.RequestID == successor.Request.RequestID {
		return fmt.Errorf("display successor run, repository, order or native request identity conflicts")
	}
	history := hostViewSuccessor{SchemaVersion: 2, Previous: previous, Successor: successor, Gaps: []loop.InspectionGap{}}
	receipts, err := readHostReceipts(dir)
	if err != nil {
		history.Gaps = append(history.Gaps, loop.InspectionGap{Path: filepath.Join(dir, "receipts.json"), Code: "unavailable", Detail: err.Error()})
	} else {
		for _, target := range []struct {
			request hostRequestBinding
			output  **hostReceipt
		}{{previous.Request, &history.PreviousReceipt}, {successor.Request, &history.SuccessorReceipt}} {
			observed, err := exactViewReceipt(target.request, receipts)
			if err != nil {
				return err
			}
			if observed == nil {
				history.Gaps = append(history.Gaps, loop.InspectionGap{Path: filepath.Join(dir, "receipts.json"), Code: "missing", Detail: "receipt unavailable for request " + target.request.RequestID})
			} else {
				*target.output = observed
			}
		}
	}
	if history.PreviousReceipt != nil && history.SuccessorReceipt != nil && history.PreviousReceipt.ContextID == history.SuccessorReceipt.ContextID {
		return fmt.Errorf("different native requests reuse the same context identity")
	}
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

func exactViewReceipt(request hostRequestBinding, receipts []hostReceipt) (*hostReceipt, error) {
	var found *hostReceipt
	if request.RequestID == "" || request.ConfigSHA256 == "" || request.BinarySHA256 == "" || !filepath.IsAbs(request.ProjectDir) || (request.Mode != "manual" && request.Mode != "supervised" && request.Mode != "auto") {
		return found, fmt.Errorf("display source selector is incomplete")
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
		copy := r
		found = &copy
	}
	if count > 1 {
		return found, fmt.Errorf("display source receipt is ambiguous")
	}
	return found, nil
}
