package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/poteto/noodle/dispatcher"
	"github.com/poteto/noodle/loop"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/spf13/cobra"
	"path/filepath"
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
		if requestFile == "" && orderID == "" {
			record, err := dispatcher.ReadReleasedNativeSession(cmd.Context(), filepath.Join(app.projectDir, ".noodle"), sessionID)
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"schema": 1, "session_id": sessionID, "runtime": dispatcher.CodexNativeRuntime, "state": "released", "process_and_group_absent": true, "native": record, "effects": []string{}})
		}
		if requestFile == "" || orderID == "" {
			return fmt.Errorf("OpenShell readback requires original request and order")
		}
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
	for _, name := range []string{"session-id"} {
		_ = cmd.MarkFlagRequired(name)
	}
	return cmd
}

// This client never calls attach/status or rewrites a request file. Persisted
// receipts select an exact owner; only that still-running Host can attest it.
func callHostSessionReadback(ctx context.Context, project, requestFile, orderID, sessionID string) (result hostSessionReadback, err error) {
	bound, err := originalHostTaskClient(project, requestFile, orderID, sessionID, "task_session_readback")
	if err != nil {
		return result, err
	}
	if _, err = bound.exchange(ctx, &result); err != nil {
		return result, err
	}
	if err = bound.verify(); err != nil {
		return hostSessionReadback{}, err
	}
	req := bound.Request
	if result.Status != "observed" {
		return result, fmt.Errorf("session readback refused: %s", result.Error)
	}
	if result.SchemaVersion != 1 || result.Sources == nil || result.HostInstanceID != req.InstanceID || result.ContextID != req.ContextID || result.RequestID != req.RequestID || result.Observation == nil || result.Observation.OrderID != orderID || result.Observation.SessionID != sessionID || result.Observation.Status != "observed" || result.Observation.Runtime != "openshell" || len(result.Observation.Effects) != 0 || len(result.Effects) != 0 {
		return hostSessionReadback{}, fmt.Errorf("session readback response binding differs")
	}
	result.Sources["host_receipts"] = bound.ReceiptRef
	result.Sources["client_binary"] = bound.BinaryRef
	return result, nil
}

func (h *noodleHost) sessionReadback(ctx context.Context, req hostRequest) hostSessionReadback {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	result := hostSessionReadback{SchemaVersion: 1, Status: "refused", HostInstanceID: h.instanceID, ContextID: req.ContextID, RequestID: req.RequestID, Sources: map[string]loopruntime.SessionReadbackFileRef{}, Effects: []string{}}
	refuse := func(err error) hostSessionReadback { result.Error = err.Error(); return result }
	owner, sources, err := h.originalTaskContext(req)
	if err != nil {
		return refuse(err)
	}
	result.Sources = sources
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
	path, err := filepath.EvalSymlinks(path)
	if err != nil {
		return loopruntime.SessionReadbackFileRef{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return loopruntime.SessionReadbackFileRef{}, err
	}
	return loopruntime.SessionReadbackFileRef{Path: path, SHA256: hostDigest(data)}, nil
}
