package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/poteto/noodle/loop"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/spf13/cobra"
)

type hostSessionReadback struct {
	SchemaVersion  int                                           `json:"schema_version"`
	Status         string                                        `json:"status"`
	Error          string                                        `json:"error"`
	HostInstanceID string                                        `json:"host_instance_id"`
	ContextID      string                                        `json:"context_id"`
	RequestID      string                                        `json:"request_id"`
	Observation    *loop.SessionReadback                         `json:"observation"`
	Sources        map[string]loopruntime.SessionReadbackFileRef `json:"sources"`
	Effects        []string                                      `json:"effects"`
}
type hostSessionReader interface {
	SessionReadback(context.Context, string, string) (loop.SessionReadback, error)
}

func newHostSessionReadbackCmd(app *App) *cobra.Command {
	var requestFile, orderID, sessionID string
	cmd := &cobra.Command{Use: "session-readback", Short: "Observe one original OpenShell session without changing task acceptance", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		result, err := callHostSessionReadback(cmd.Context(), app.projectDir, requestFile, orderID, sessionID)
		if result.SchemaVersion != 0 {
			if outErr := json.NewEncoder(cmd.OutOrStdout()).Encode(result); outErr != nil {
				return errors.Join(err, outErr)
			}
		}
		return err
	}}
	cmd.Flags().StringVar(&requestFile, "request-file", "", "Original admitted Host request binding")
	cmd.Flags().StringVar(&orderID, "order-id", "", "Exact canonical order")
	cmd.Flags().StringVar(&sessionID, "session-id", "", "Exact original native session")
	for _, name := range []string{"request-file", "order-id", "session-id"} {
		_ = cmd.MarkFlagRequired(name)
	}
	return cmd
}

// This client never calls attach/status or rewrites a request file. Persisted
// receipts select an exact owner; only that still-running Host can attest it.
func callHostSessionReadback(ctx context.Context, project, requestFile, orderID, sessionID string) (result hostSessionReadback, err error) {
	project, err = filepath.EvalSymlinks(project)
	if err != nil {
		return result, err
	}
	requestFile, err = filepath.Abs(requestFile)
	if err != nil {
		return result, err
	}
	var binding hostRequestBinding
	requestRef, err := loopruntime.ReadSessionReadbackFile(requestFile, &binding)
	if err != nil {
		return result, err
	}
	executable, err := os.Executable()
	if err != nil {
		return result, err
	}
	binaryRef, err := hostReadbackBinaryRef(executable)
	if err != nil {
		return result, err
	}
	if binding.Schema != 1 || binding.Operation != "attach" || binding.ProjectDir != project || binding.BinarySHA256 != binaryRef.SHA256 || binding.RequestID == "" || !filepath.IsAbs(binding.HostDir) {
		return result, fmt.Errorf("original Host request project/binary binding differs")
	}
	var receipts []hostReceipt
	receiptRef, err := loopruntime.ReadSessionReadbackFile(filepath.Join(binding.HostDir, "receipts.json"), &receipts)
	if err != nil {
		return result, err
	}
	req := hostRequest{Operation: "task_session_readback", ProjectDir: project, RequestID: binding.RequestID, ConfigPath: binding.ConfigPath, ConfigSHA256: binding.ConfigSHA256, Mode: binding.Mode, BinarySHA256: binding.BinarySHA256, CleanupCompleted: binding.CleanupCompleted, ReadbackOrderID: orderID, ReadbackSessionID: sessionID, ReadbackRequest: requestRef}
	count := 0
	var original hostReceipt
	for _, receipt := range receipts {
		if hostReceiptMatches(req, receipt) && receipt.RequestID == binding.RequestID && receipt.HostDir == binding.HostDir && receipt.BinarySHA256 == binding.BinarySHA256 && receipt.ContextState == "active" {
			original = receipt
			count++
		}
	}
	req.InstanceID, req.ContextID = original.HostInstanceID, original.ContextID
	if count != 1 || req.InstanceID == "" || req.ContextID == "" {
		return result, fmt.Errorf("original active Host receipt missing or ambiguous")
	}
	conn, err := net.DialTimeout("unix", filepath.Join(binding.HostDir, "host.sock"), time.Second)
	if err != nil {
		return result, fmt.Errorf("original Host is unavailable; no launch attempted: %w", err)
	}
	defer conn.Close()
	deadline := time.Now().Add(15 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err = conn.SetDeadline(deadline); err != nil {
		return result, err
	}
	if err = json.NewEncoder(conn).Encode(req); err != nil {
		return result, err
	}
	if err = json.NewDecoder(io.LimitReader(conn, 1<<20)).Decode(&result); err != nil {
		return result, err
	}
	if err = loopruntime.VerifySessionReadbackFiles(map[string]loopruntime.SessionReadbackFileRef{"original_request": requestRef, "host_receipts": receiptRef}); err != nil {
		return hostSessionReadback{}, err
	}
	currentBinary, err := hostReadbackBinaryRef(executable)
	if err != nil || currentBinary != binaryRef {
		return hostSessionReadback{}, fmt.Errorf("original Host client binary changed during readback")
	}
	if result.Status != "observed" {
		return result, fmt.Errorf("session readback refused: %s", result.Error)
	}
	if result.SchemaVersion != 1 || result.Sources == nil || result.HostInstanceID != req.InstanceID || result.ContextID != req.ContextID || result.RequestID != req.RequestID || result.Observation == nil || result.Observation.OrderID != orderID || result.Observation.SessionID != sessionID || result.Observation.Status != "observed" || result.Observation.Runtime != "openshell" || len(result.Observation.Effects) != 0 || len(result.Effects) != 0 {
		return hostSessionReadback{}, fmt.Errorf("session readback response binding differs")
	}
	result.Sources["host_receipts"] = receiptRef
	result.Sources["client_binary"] = binaryRef
	return result, nil
}

func (h *noodleHost) sessionReadback(ctx context.Context, req hostRequest) hostSessionReadback {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	result := hostSessionReadback{SchemaVersion: 1, Status: "refused", HostInstanceID: h.instanceID, ContextID: req.ContextID, RequestID: req.RequestID, Sources: map[string]loopruntime.SessionReadbackFileRef{}, Effects: []string{}}
	refuse := func(err error) hostSessionReadback { result.Error = err.Error(); return result }
	if h.blocked || req.InstanceID != h.instanceID || req.BinarySHA256 != h.binarySHA || req.ContextID == "" || req.RequestID == "" || req.ReadbackOrderID == "" || req.ReadbackSessionID == "" {
		return refuse(fmt.Errorf("exact original Host session binding unavailable"))
	}
	owner := h.contexts[req.ContextID]
	if owner == nil || !owner.openShell || owner.failed || owner.done == nil || owner.receipt.ContextState != "active" || !hostReceiptMatches(req, owner.receipt) {
		return refuse(fmt.Errorf("original Host no longer owns the active context"))
	}
	// Completion remains owned by the Host event loop; do not drain owner.done.
	var binding hostRequestBinding
	ref, err := loopruntime.ReadSessionReadbackFile(req.ReadbackRequest.Path, &binding)
	if err != nil {
		return refuse(err)
	}
	if ref != req.ReadbackRequest || binding.Schema != 1 || binding.Operation != "attach" || binding.HostDir != h.dir || binding.ProjectDir != req.ProjectDir || binding.RequestID != req.RequestID || binding.BinarySHA256 != req.BinarySHA256 || binding.ConfigPath != req.ConfigPath || binding.ConfigSHA256 != req.ConfigSHA256 || binding.Mode != req.Mode || binding.CleanupCompleted != req.CleanupCompleted || binding.EnvironmentSHA256 != owner.environmentSHA {
		return refuse(fmt.Errorf("original Host request source differs"))
	}
	result.Sources["original_request"] = ref
	configPath := req.ConfigPath
	if configPath == "" {
		configPath = filepath.Join(req.ProjectDir, ".noodle.toml")
	}
	ref, err = loopruntime.ReadSessionReadbackFile(configPath, nil)
	if err != nil {
		return refuse(err)
	}
	if ref.SHA256 != req.ConfigSHA256 {
		return refuse(fmt.Errorf("original Host configuration source changed"))
	}
	result.Sources["config"] = ref
	reader, ok := owner.runtime.(hostSessionReader)
	if !ok {
		return refuse(fmt.Errorf("original Loop session readback unavailable"))
	}
	observation, err := reader.SessionReadback(ctx, req.ReadbackOrderID, req.ReadbackSessionID)
	if err != nil {
		return refuse(err)
	}
	if err = loopruntime.VerifySessionReadbackFiles(result.Sources); err != nil {
		return refuse(err)
	}
	result.Status = "observed"
	result.Observation = &observation
	return result
}

func hostReadbackBinaryRef(path string) (loopruntime.SessionReadbackFileRef, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return loopruntime.SessionReadbackFileRef{}, err
	}
	return loopruntime.SessionReadbackFileRef{Path: path, SHA256: hostDigest(data)}, nil
}
