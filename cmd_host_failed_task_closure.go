package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/poteto/noodle/internal/state"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/spf13/cobra"
)

type hostFailedTaskClosure struct {
	SchemaVersion  int                                           `json:"schema_version"`
	Status         string                                        `json:"status"`
	Error          string                                        `json:"error"`
	HostInstanceID string                                        `json:"host_instance_id"`
	ContextID      string                                        `json:"context_id"`
	RequestID      string                                        `json:"request_id"`
	OrderID        string                                        `json:"order_id"`
	SessionID      string                                        `json:"session_id"`
	Reason         string                                        `json:"reason"`
	ClosureReceipt *loopruntime.SessionReadbackFileRef           `json:"closure_receipt"`
	Sources        map[string]loopruntime.SessionReadbackFileRef `json:"sources"`
	Effects        []string                                      `json:"effects"`
}

// Keep this capability optional; other Host runtimes cannot close failed tasks.
type hostFailedTaskClosureOwner interface {
	CloseFailedTask(context.Context, string, string, string, loopruntime.SessionReadbackFileRef) error
}

func newHostFailedTaskClosureCmd(app *App) *cobra.Command {
	var requestFile, orderID, sessionID, reason, diagnostics, expectedSHA string
	cmd := &cobra.Command{Use: "close-failure", Short: "Explicitly close an original failed task with retained diagnostics", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		result, err := callHostFailedTaskClosure(cmd.Context(), app.projectDir, requestFile, orderID, sessionID, reason, diagnostics, expectedSHA)
		if result.SchemaVersion != 0 {
			if outErr := json.NewEncoder(cmd.OutOrStdout()).Encode(result); outErr != nil {
				return errors.Join(err, outErr)
			}
		}
		return err
	}}
	cmd.Flags().StringVar(&requestFile, "request-file", "", "Original admitted Host request binding")
	cmd.Flags().StringVar(&orderID, "order-id", "", "Exact canonical order")
	cmd.Flags().StringVar(&sessionID, "session-id", "", "Exact failed native session")
	cmd.Flags().StringVar(&reason, "reason", "", "Explicit reason for closing the failure")
	cmd.Flags().StringVar(&diagnostics, "diagnostics", "", "Absolute path to retained original failure diagnostics")
	cmd.Flags().StringVar(&expectedSHA, "expected-diagnostics-sha256", "", "Expected SHA256 of the retained diagnostics")
	for _, flag := range []string{"request-file", "order-id", "session-id", "reason", "diagnostics", "expected-diagnostics-sha256"} {
		_ = cmd.MarkFlagRequired(flag)
	}
	return cmd
}

func failedTaskClosureResponse(instance string, req hostRequest) hostFailedTaskClosure {
	return hostFailedTaskClosure{SchemaVersion: 1, Status: "refused", HostInstanceID: instance, ContextID: req.ContextID, RequestID: req.RequestID, OrderID: req.ReadbackOrderID, SessionID: req.ReadbackSessionID, Reason: req.FailureReason, Sources: map[string]loopruntime.SessionReadbackFileRef{}, Effects: []string{}}
}

func validateFailedTaskClosureInput(reason string, diagnostics loopruntime.SessionReadbackFileRef) error {
	if strings.TrimSpace(reason) == "" || len(reason) > 4096 {
		return fmt.Errorf("failure closure reason is empty or exceeds 4096 bytes")
	}
	raw, err := hex.DecodeString(diagnostics.SHA256)
	if !filepath.IsAbs(diagnostics.Path) || err != nil || len(raw) != 32 || strings.ToLower(diagnostics.SHA256) != diagnostics.SHA256 {
		return fmt.Errorf("failure diagnostics absolute path or SHA256 is invalid")
	}
	return loopruntime.VerifySessionReadbackFiles(map[string]loopruntime.SessionReadbackFileRef{"diagnostics": diagnostics})
}

// Read the original Loop's durable receipt, never manufacture one from its ACK.
func readHostFailedTaskClosureReceipt(req hostRequest) (loopruntime.SessionReadbackFileRef, error) {
	var closure struct {
		Order       state.OrderNode                               `json:"order"`
		Reason      string                                        `json:"reason"`
		Diagnostics map[string]loopruntime.SessionReadbackFileRef `json:"diagnostics"`
	}
	path := filepath.Join(req.ProjectDir, ".noodle", "execution-closures", hostDigest([]byte(req.ReadbackOrderID))+".json")
	ref, err := loopruntime.ReadSessionReadbackFile(path, &closure)
	if err != nil {
		return ref, err
	}
	if closure.Order.OrderID != req.ReadbackOrderID || closure.Order.Status != state.OrderFailed || len(closure.Order.Stages) != 1 || closure.Reason != req.FailureReason || len(closure.Diagnostics) != 1 || closure.Diagnostics["original_flow_diagnostics"] != req.FailureDiagnostics {
		return ref, fmt.Errorf("original failure closure receipt binding differs")
	}
	stage := closure.Order.Stages[0]
	if stage.Status != state.StageFailed || stage.Merge != nil || len(stage.Attempts) == 0 {
		return ref, fmt.Errorf("original failure closure stage is unresolved")
	}
	latest := stage.Attempts[len(stage.Attempts)-1]
	if latest.SessionID != req.ReadbackSessionID || (latest.Status != state.AttemptFailed && latest.Status != state.AttemptCancelled) {
		return ref, fmt.Errorf("original failure closure attempt binding differs")
	}
	return ref, nil
}

func (h *noodleHost) failedTaskClosure(ctx context.Context, req hostRequest) hostFailedTaskClosure {
	result := failedTaskClosureResponse(h.instanceID, req)
	fail := func(err error) hostFailedTaskClosure { result.Error = err.Error(); return result }
	if req.Operation != "task_failure_close" || h.shutdown != nil {
		return fail(fmt.Errorf("original Host failure closure unavailable during this operation or shutdown"))
	}
	if err := validateFailedTaskClosureInput(req.FailureReason, req.FailureDiagnostics); err != nil {
		return fail(err)
	}
	owner, sources, err := h.originalTaskContext(req)
	if err != nil {
		return fail(err)
	}
	result.Sources = sources
	result.Sources["diagnostics"] = req.FailureDiagnostics
	closer, ok := owner.runtime.(hostFailedTaskClosureOwner)
	if !ok {
		return fail(fmt.Errorf("original Loop failed task closure capability unavailable"))
	}
	if err := loopruntime.VerifySessionReadbackFiles(result.Sources); err != nil {
		return fail(err)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// An error can follow a durable receipt write or checkpoint mutation.
	// Once called, only the original owner's successful ACK can resolve this.
	result.Status = "unknown"
	result.Effects = []string{"original_owner_call_attempted"}
	if err := closer.CloseFailedTask(ctx, req.ReadbackOrderID, req.ReadbackSessionID, req.FailureReason, req.FailureDiagnostics); err != nil {
		return fail(err)
	}
	ref, err := readHostFailedTaskClosureReceipt(req)
	if err != nil {
		return fail(err)
	}
	result.ClosureReceipt = &ref
	if err := loopruntime.VerifySessionReadbackFiles(result.Sources); err != nil {
		return fail(err)
	}
	result.Status = "closed_failed"
	result.Effects = []string{"original_failure_closure_persisted"}
	return result
}

func callHostFailedTaskClosure(ctx context.Context, project, requestFile, orderID, sessionID, reason, diagnostics, expectedSHA string) (result hostFailedTaskClosure, err error) {
	diagnosticsRef := loopruntime.SessionReadbackFileRef{Path: diagnostics, SHA256: expectedSHA}
	if filepath.IsAbs(diagnostics) {
		actual, readErr := loopruntime.ReadSessionReadbackFile(diagnostics, nil)
		if readErr != nil {
			return result, readErr
		}
		diagnosticsRef.Path = actual.Path
	}
	if err := validateFailedTaskClosureInput(reason, diagnosticsRef); err != nil {
		return result, err
	}
	if orderID == "" || sessionID == "" {
		return result, fmt.Errorf("failure closure order or session is empty")
	}
	bound, err := originalHostTaskClient(project, requestFile, orderID, sessionID, "task_failure_close")
	if err != nil {
		return result, err
	}
	bound.Request.FailureReason, bound.Request.FailureDiagnostics = reason, diagnosticsRef
	req := bound.Request
	result = failedTaskClosureResponse(req.InstanceID, req)
	if err := bound.verify(); err != nil {
		result.Error = err.Error()
		return result, err
	}
	sent, exchangeErr := bound.exchange(ctx, &result)
	unknown := func(cause error) (hostFailedTaskClosure, error) {
		result = failedTaskClosureResponse(req.InstanceID, req)
		result.Status, result.Error = "unknown", cause.Error()
		result.Effects = []string{"original_owner_call_attempted"}
		return result, cause
	}
	if exchangeErr != nil {
		if sent {
			return unknown(exchangeErr)
		}
		result.Error = exchangeErr.Error()
		return result, exchangeErr
	}
	if result.SchemaVersion != 1 || result.HostInstanceID != req.InstanceID || result.ContextID != req.ContextID || result.RequestID != req.RequestID || result.OrderID != orderID || result.SessionID != sessionID || result.Reason != reason || result.Sources == nil || result.Effects == nil {
		return unknown(fmt.Errorf("failure closure response binding differs"))
	}
	if err := bound.verify(); err != nil {
		return unknown(err)
	}
	if err := validateFailedTaskClosureInput(reason, diagnosticsRef); err != nil {
		return unknown(err)
	}
	if result.Status == "refused" && result.Error != "" && len(result.Effects) == 0 && result.ClosureReceipt == nil {
		return result, fmt.Errorf("failure closure refused: %s", result.Error)
	}
	if result.Status != "closed_failed" || result.Error != "" || result.ClosureReceipt == nil || len(result.Effects) != 1 || result.Effects[0] != "original_failure_closure_persisted" {
		return unknown(fmt.Errorf("failure closure acknowledgement unresolved: %s: %s", result.Status, result.Error))
	}
	configPath := req.ConfigPath
	if configPath == "" {
		configPath = filepath.Join(req.ProjectDir, ".noodle.toml")
	}
	configRef, err := loopruntime.ReadSessionReadbackFile(configPath, nil)
	if err != nil {
		return unknown(err)
	}
	if configRef.SHA256 != req.ConfigSHA256 || len(result.Sources) != 3 || result.Sources["original_request"] != bound.RequestRef || result.Sources["config"] != configRef || result.Sources["diagnostics"] != diagnosticsRef {
		return unknown(fmt.Errorf("failure closure response sources differ"))
	}
	ref, err := readHostFailedTaskClosureReceipt(req)
	if err != nil {
		return unknown(err)
	}
	if ref != *result.ClosureReceipt {
		return unknown(fmt.Errorf("failure closure receipt source differs"))
	}
	if err := loopruntime.VerifySessionReadbackFiles(result.Sources); err != nil {
		return unknown(err)
	}
	result.Sources["host_receipts"], result.Sources["client_binary"] = bound.ReceiptRef, bound.BinaryRef
	return result, nil
}
