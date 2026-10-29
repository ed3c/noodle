package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"time"

	"github.com/poteto/noodle/internal/state"
	loopruntime "github.com/poteto/noodle/runtime"
	"github.com/spf13/cobra"
)

type hostTaskAcceptance struct {
	SchemaVersion      int                                           `json:"schema_version"`
	Status             string                                        `json:"status"`
	Error              string                                        `json:"error"`
	HostInstanceID     string                                        `json:"host_instance_id"`
	ContextID          string                                        `json:"context_id"`
	RequestID          string                                        `json:"request_id"`
	OrderID            string                                        `json:"order_id"`
	SessionID          string                                        `json:"session_id"`
	ObservationBasis   string                                        `json:"observation_basis"`
	Reclamation        *loopruntime.ExecutionReclamationReceipt      `json:"reclamation"`
	ReclamationReceipt *loopruntime.SessionReadbackFileRef           `json:"reclamation_receipt"`
	ClosureReceipt     *loopruntime.SessionReadbackFileRef           `json:"closure_receipt"`
	Sources            map[string]loopruntime.SessionReadbackFileRef `json:"sources"`
	Effects            []string                                      `json:"effects"`
}

type hostTaskAcceptanceOwner interface {
	AcceptCompletedTask(context.Context, string, string, loopruntime.SessionReadbackFileRef) error
}

func newHostTaskAcceptanceCmd(app *App) *cobra.Command {
	return newHostTaskAcceptanceCommand(app, false)
}

func newHostTaskAcceptanceReadbackCmd(app *App) *cobra.Command {
	return newHostTaskAcceptanceCommand(app, true)
}

func newHostTaskAcceptanceCommand(app *App, readback bool) *cobra.Command {
	var requestFile, orderID, sessionID, acceptance, expectedSHA string
	name, summary := "accept-delivery", "Record outer acceptance of an original completed task delivery"
	if readback {
		name, summary = "acceptance-readback", "Observe retained acceptance through its original Host without replaying it"
	}
	cmd := &cobra.Command{Use: name, Short: summary, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		result, err := callHostTaskAcceptanceOperation(cmd.Context(), app.projectDir, requestFile, orderID, sessionID, acceptance, expectedSHA, readback)
		if result.SchemaVersion != 0 {
			if outErr := json.NewEncoder(cmd.OutOrStdout()).Encode(result); outErr != nil {
				return errors.Join(err, outErr)
			}
		}
		return err
	}}
	cmd.Flags().StringVar(&requestFile, "request-file", "", "Original admitted Host request binding")
	cmd.Flags().StringVar(&orderID, "order-id", "", "Exact canonical order")
	cmd.Flags().StringVar(&sessionID, "session-id", "", "Exact completed native session")
	cmd.Flags().StringVar(&acceptance, "acceptance", "", "Absolute path to outer delivery acceptance with retained evidence")
	cmd.Flags().StringVar(&expectedSHA, "expected-acceptance-sha256", "", "Expected SHA256 of the outer acceptance")
	for _, flag := range []string{"request-file", "order-id", "session-id", "acceptance", "expected-acceptance-sha256"} {
		_ = cmd.MarkFlagRequired(flag)
	}
	return cmd
}

func taskAcceptanceResponse(instance string, req hostRequest) hostTaskAcceptance {
	return hostTaskAcceptance{SchemaVersion: 1, Status: "refused", HostInstanceID: instance, ContextID: req.ContextID, RequestID: req.RequestID, OrderID: req.ReadbackOrderID, SessionID: req.ReadbackSessionID, Sources: map[string]loopruntime.SessionReadbackFileRef{}, Effects: []string{}}
}

func verifyHostTaskAcceptance(req hostRequest) error {
	if req.ReadbackOrderID == "" || req.ReadbackSessionID == "" || !filepath.IsAbs(req.TaskAcceptance.Path) {
		return fmt.Errorf("task acceptance order, session or absolute file path is missing")
	}
	if err := loopruntime.VerifySessionReadbackFiles(map[string]loopruntime.SessionReadbackFileRef{"acceptance": req.TaskAcceptance}); err != nil {
		return err
	}
	// Runtime owns the shared schema, PASS conditions and all evidence checks.
	acceptance, err := loopruntime.ReadTaskDeliveryAcceptance(req.TaskAcceptance)
	if err != nil {
		return err
	}
	if acceptance.OrderID != req.ReadbackOrderID || acceptance.SessionID != req.ReadbackSessionID {
		return fmt.Errorf("outer acceptance original order/session binding differs")
	}
	return nil
}

func readHostTaskAcceptanceReceipt(req hostRequest) (loopruntime.SessionReadbackFileRef, error) {
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
	if closure.Order.OrderID != req.ReadbackOrderID || closure.Order.Status != state.OrderCompleted || len(closure.Order.Stages) != 1 || closure.Reason != "original task owner accepted completed delivery" || len(closure.Diagnostics) != 1 || closure.Diagnostics["owner_acceptance"] != req.TaskAcceptance {
		return ref, fmt.Errorf("original delivery acceptance closure binding differs")
	}
	stage := closure.Order.Stages[0]
	if stage.Status != state.StageCompleted || stage.Merge != nil || len(stage.Attempts) == 0 {
		return ref, fmt.Errorf("original accepted delivery stage is unresolved")
	}
	latest := stage.Attempts[len(stage.Attempts)-1]
	if latest.SessionID != req.ReadbackSessionID || latest.Status != state.AttemptCompleted {
		return ref, fmt.Errorf("original accepted delivery attempt binding differs")
	}
	return ref, nil
}

func (h *noodleHost) taskAcceptance(ctx context.Context, req hostRequest) hostTaskAcceptance {
	result := taskAcceptanceResponse(h.instanceID, req)
	fail := func(err error) hostTaskAcceptance { result.Error = err.Error(); return result }
	if req.Operation != "task_delivery_accept" || h.shutdown != nil {
		return fail(fmt.Errorf("original Host task acceptance unavailable during this operation or shutdown"))
	}
	if err := verifyHostTaskAcceptance(req); err != nil {
		return fail(err)
	}
	owner, sources, err := h.originalTaskContext(req)
	if err != nil {
		return fail(err)
	}
	result.Sources = sources
	result.Sources["acceptance"] = req.TaskAcceptance
	accepter, ok := owner.runtime.(hostTaskAcceptanceOwner)
	if !ok {
		return fail(fmt.Errorf("original Loop completed task acceptance capability unavailable"))
	}
	if err := loopruntime.VerifySessionReadbackFiles(result.Sources); err != nil {
		return fail(err)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// Persistence can precede an error. Never resolve an uncertain owner call
	// by reading a receipt alone or by repeating the acceptance operation.
	result.Status = "unknown"
	result.Effects = []string{"original_owner_call_attempted"}
	if err := accepter.AcceptCompletedTask(ctx, req.ReadbackOrderID, req.ReadbackSessionID, req.TaskAcceptance); err != nil {
		return fail(err)
	}
	if err := verifyHostTaskAcceptance(req); err != nil {
		return fail(err)
	}
	ref, err := readHostTaskAcceptanceReceipt(req)
	if err != nil {
		return fail(err)
	}
	result.ClosureReceipt = &ref
	if err := loopruntime.VerifySessionReadbackFiles(result.Sources); err != nil {
		return fail(err)
	}
	result.Status = "accepted"
	result.Effects = []string{"original_delivery_acceptance_persisted"}
	return result
}

func callHostTaskAcceptance(ctx context.Context, project, requestFile, orderID, sessionID, acceptancePath, expectedSHA string) (result hostTaskAcceptance, err error) {
	return callHostTaskAcceptanceOperation(ctx, project, requestFile, orderID, sessionID, acceptancePath, expectedSHA, false)
}

func callHostTaskAcceptanceReadback(ctx context.Context, project, requestFile, orderID, sessionID, acceptancePath, expectedSHA string) (hostTaskAcceptance, error) {
	return callHostTaskAcceptanceOperation(ctx, project, requestFile, orderID, sessionID, acceptancePath, expectedSHA, true)
}

func callHostTaskAcceptanceOperation(ctx context.Context, project, requestFile, orderID, sessionID, acceptancePath, expectedSHA string, readback bool) (result hostTaskAcceptance, err error) {
	if !filepath.IsAbs(acceptancePath) {
		return result, fmt.Errorf("task acceptance file path is not absolute")
	}
	acceptanceRef, err := loopruntime.ReadSessionReadbackFile(acceptancePath, nil)
	if err != nil {
		return result, err
	}
	acceptanceRef.SHA256 = expectedSHA
	input := hostRequest{ReadbackOrderID: orderID, ReadbackSessionID: sessionID, TaskAcceptance: acceptanceRef}
	if err := verifyHostTaskAcceptance(input); err != nil {
		return result, err
	}
	operation := "task_delivery_accept"
	if readback {
		operation = "task_delivery_acceptance_readback"
	}
	bound, err := originalHostTaskAcceptanceClient(project, requestFile, orderID, sessionID, operation)
	if err != nil {
		return result, err
	}
	bound.Request.TaskAcceptance = acceptanceRef
	req := bound.Request
	result = taskAcceptanceResponse(req.InstanceID, req)
	if err := bound.verifyAcceptance(); err != nil {
		result.Error = err.Error()
		return result, err
	}
	sent, exchangeErr := bound.exchange(ctx, &result)
	unknown := func(cause error) (hostTaskAcceptance, error) {
		result = taskAcceptanceResponse(req.InstanceID, req)
		result.Status, result.Error = "unknown", cause.Error()
		if !readback {
			result.Effects = []string{"original_owner_call_attempted"}
		}
		return result, cause
	}
	if exchangeErr != nil {
		if sent {
			return unknown(exchangeErr)
		}
		result.Error = exchangeErr.Error()
		return result, exchangeErr
	}
	if result.SchemaVersion != 1 || result.HostInstanceID != req.InstanceID || result.ContextID != req.ContextID || result.RequestID != req.RequestID || result.OrderID != orderID || result.SessionID != sessionID || result.Sources == nil || result.Effects == nil {
		return unknown(fmt.Errorf("task acceptance response binding differs"))
	}
	if err := bound.verifyAcceptance(); err != nil {
		return unknown(err)
	}
	if err := verifyHostTaskAcceptance(req); err != nil {
		return unknown(err)
	}
	if result.Status == "refused" && result.Error != "" && len(result.Effects) == 0 && result.ClosureReceipt == nil {
		return result, fmt.Errorf("task acceptance refused: %s", result.Error)
	}
	validResult := result.Status == "accepted" && len(result.Effects) == 1 && result.Effects[0] == "original_delivery_acceptance_persisted"
	if readback {
		validResult = result.Status == "reclaimed" && result.ObservationBasis == "original_owner_reclamation_readback" && len(result.Effects) == 0 && result.ReclamationReceipt != nil && result.Reclamation != nil
	}
	if !validResult || result.Error != "" || result.ClosureReceipt == nil {
		return unknown(fmt.Errorf("task acceptance acknowledgement unresolved: %s: %s", result.Status, result.Error))
	}
	configPath := req.ConfigPath
	if configPath == "" {
		configPath = filepath.Join(req.ProjectDir, ".noodle.toml")
	}
	configRef, err := loopruntime.ReadSessionReadbackFile(configPath, nil)
	if err != nil {
		return unknown(err)
	}
	expectedSources := 3
	if readback {
		expectedSources++
	}
	if configRef.SHA256 != req.ConfigSHA256 || len(result.Sources) != expectedSources || result.Sources["original_request"] != bound.RequestRef || result.Sources["config"] != configRef || result.Sources["acceptance"] != acceptanceRef {
		return unknown(fmt.Errorf("task acceptance response sources differ"))
	}
	ref, err := readHostTaskAcceptanceReceipt(req)
	if err != nil {
		return unknown(err)
	}
	if ref != *result.ClosureReceipt {
		return unknown(fmt.Errorf("task acceptance closure source differs"))
	}
	if readback {
		reclamation, reclamationRef, err := readHostTaskAcceptanceReclamation(req, ref)
		if err != nil {
			return unknown(err)
		}
		if reclamationRef != *result.ReclamationReceipt || !reflect.DeepEqual(reclamation, *result.Reclamation) || result.Sources["reclamation_receipt"] != reclamationRef {
			return unknown(fmt.Errorf("original reclamation response source differs"))
		}
	}
	if err := loopruntime.VerifySessionReadbackFiles(result.Sources); err != nil {
		return unknown(err)
	}
	// receipts.json is a mutable selection index, not an immutable evidence source.
	result.Sources["client_binary"] = bound.BinaryRef
	return result, nil
}
