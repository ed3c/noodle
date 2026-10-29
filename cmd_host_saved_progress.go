package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/poteto/noodle/loop"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/spf13/cobra"
)

type hostSavedProgress struct {
	SchemaVersion  int                                           `json:"schema_version"`
	Status         string                                        `json:"status"`
	Error          string                                        `json:"error"`
	HostInstanceID string                                        `json:"host_instance_id"`
	ContextID      string                                        `json:"context_id"`
	RequestID      string                                        `json:"request_id"`
	OrderID        string                                        `json:"order_id"`
	SessionID      string                                        `json:"session_id"`
	Inspection     *loop.SavedProgressInspection                 `json:"inspection"`
	Queued         *loop.SavedProgressQueued                     `json:"queued"`
	Sources        map[string]loopruntime.SessionReadbackFileRef `json:"sources"`
	Effects        []string                                      `json:"effects"`
}

type hostSavedProgressOwner interface {
	InspectSavedProgress(context.Context, string, string) (loop.SavedProgressInspection, error)
	QueueSavedProgressAttempt(context.Context, string, string, string) (loop.SavedProgressQueued, error)
}

func newHostSavedProgressCmd(app *App, queue bool) *cobra.Command {
	var requestFile, orderID, sessionID, version string
	name, summary := "interruption-inspect", "Inspect original stopped progress without resuming a native session"
	if queue {
		name, summary = "new-attempt", "Queue an explicit new attempt from original saved progress"
	}
	cmd := &cobra.Command{Use: name, Short: summary, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		result, err := callHostSavedProgress(cmd.Context(), app.projectDir, requestFile, orderID, sessionID, version, queue)
		if result.SchemaVersion != 0 {
			if outErr := json.NewEncoder(cmd.OutOrStdout()).Encode(result); outErr != nil {
				return errors.Join(err, outErr)
			}
		}
		return err
	}}
	cmd.Flags().StringVar(&requestFile, "request-file", "", "Original admitted Host request binding")
	cmd.Flags().StringVar(&orderID, "order-id", "", "Exact canonical order")
	cmd.Flags().StringVar(&sessionID, "session-id", "", "Exact stopped native session")
	for _, flag := range []string{"request-file", "order-id", "session-id"} {
		_ = cmd.MarkFlagRequired(flag)
	}
	if queue {
		cmd.Flags().StringVar(&version, "expected-version", "", "Original interruption inspection consent version")
		_ = cmd.MarkFlagRequired("expected-version")
	}
	return cmd
}

func validSavedProgressVersion(version string) bool {
	raw, err := hex.DecodeString(version)
	return err == nil && len(raw) == 32 && strings.ToLower(version) == version
}

func savedProgressResponse(instance string, req hostRequest) hostSavedProgress {
	return hostSavedProgress{SchemaVersion: 1, Status: "refused", HostInstanceID: instance, ContextID: req.ContextID, RequestID: req.RequestID, OrderID: req.ReadbackOrderID, SessionID: req.ReadbackSessionID, Sources: map[string]loopruntime.SessionReadbackFileRef{}, Effects: []string{}}
}

func (h *noodleHost) savedProgress(ctx context.Context, req hostRequest) hostSavedProgress {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	result := savedProgressResponse(h.instanceID, req)
	refuse := func(err error) hostSavedProgress { result.Error = err.Error(); return result }
	queue := req.Operation == "task_new_attempt"
	if !queue && req.Operation != "task_interruption_inspect" {
		return refuse(fmt.Errorf("saved progress operation unavailable"))
	}
	if queue && !validSavedProgressVersion(req.ExpectedVersion) {
		return refuse(fmt.Errorf("saved progress consent version invalid"))
	}
	owner, sources, err := h.originalTaskContext(req)
	if err != nil {
		return refuse(err)
	}
	result.Sources = sources
	reader, ok := owner.runtime.(hostSavedProgressOwner)
	if !ok {
		return refuse(fmt.Errorf("original Loop saved progress capability unavailable"))
	}
	if queue {
		queued, queueErr := reader.QueueSavedProgressAttempt(ctx, req.ReadbackOrderID, req.ReadbackSessionID, req.ExpectedVersion)
		result.Queued = &queued
		result.Effects = append(result.Effects, queued.Effects...)
		if queueErr != nil {
			if len(queued.Effects) > 0 {
				result.Status = "unknown"
			}
			return refuse(queueErr)
		}
		result.Status = "queued"
	} else {
		inspected, readErr := reader.InspectSavedProgress(ctx, req.ReadbackOrderID, req.ReadbackSessionID)
		if readErr != nil {
			return refuse(readErr)
		}
		result.Inspection = &inspected
		result.Status = "observed"
	}
	// Only original Host binding sources are rechecked here. The Loop owns the
	// canonical consent version and revalidates it when consuming the control.
	if err = loopruntime.VerifySessionReadbackFiles(result.Sources); err != nil {
		result.Status = "refused"
		if queue {
			result.Status = "unknown"
		}
		return refuse(err)
	}
	return result
}

func callHostSavedProgress(ctx context.Context, project, requestFile, orderID, sessionID, version string, queue bool) (result hostSavedProgress, err error) {
	operation := "task_interruption_inspect"
	if queue {
		if !validSavedProgressVersion(version) {
			return result, fmt.Errorf("saved progress consent version invalid")
		}
		operation = "task_new_attempt"
	}
	bound, err := originalHostTaskClient(project, requestFile, orderID, sessionID, operation)
	if err != nil {
		return result, err
	}
	bound.Request.ExpectedVersion = version
	sent, exchangeErr := bound.exchange(ctx, &result)
	unknown := func(cause error) (hostSavedProgress, error) {
		// A lost acknowledgement cannot be retried as a known no-effect refusal.
		result = savedProgressResponse(bound.Request.InstanceID, bound.Request)
		result.Status = "unknown"
		result.Error = cause.Error()
		if queue && sent {
			result.Effects = []string{"original_owner_call_attempted"}
		}
		return result, cause
	}
	if exchangeErr != nil {
		if sent {
			return unknown(exchangeErr)
		}
		return result, exchangeErr
	}
	req := bound.Request
	if result.SchemaVersion != 1 || result.HostInstanceID != req.InstanceID || result.ContextID != req.ContextID || result.RequestID != req.RequestID || result.OrderID != orderID || result.SessionID != sessionID || result.Sources == nil || result.Effects == nil {
		return unknown(fmt.Errorf("saved progress response binding differs"))
	}
	if err = bound.verify(); err != nil {
		// Preserve the original acknowledged command even if subsequent source
		// readback fails; it may already be consumed by the original Loop.
		result.Status = "unknown"
		result.Error = err.Error()
		return result, err
	}
	result.Sources["host_receipts"] = bound.ReceiptRef
	result.Sources["client_binary"] = bound.BinaryRef
	if result.Status == "refused" || result.Status == "unknown" {
		return result, fmt.Errorf("saved progress %s: %s", result.Status, result.Error)
	}
	if queue {
		q := result.Queued
		if result.Status != "queued" || result.Inspection != nil || q == nil || q.Status != "queued" || q.CommandID != "saved-progress-"+version || q.Version != version || len(q.Effects) != 1 || q.Effects[0] != "original_control_queued" || len(result.Effects) != 1 || result.Effects[0] != "original_control_queued" {
			return unknown(fmt.Errorf("saved progress queue acknowledgement differs"))
		}
	} else {
		i := result.Inspection
		if result.Status != "observed" || result.Queued != nil || i == nil || i.SchemaVersion != 1 || i.Status != "retryable" || i.OrderID != orderID || i.SessionID != sessionID || i.Runtime != "openshell" || i.NativeResumeSupported || !validSavedProgressVersion(i.Version) || len(i.Effects) != 0 || len(result.Effects) != 0 {
			return unknown(fmt.Errorf("saved progress inspection binding differs"))
		}
	}
	return result, nil
}
