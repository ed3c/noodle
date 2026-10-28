package runtime

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"github.com/poteto/noodle/internal/admission"
)

const openShellObservationTimeout = 5 * time.Second

// OpenShellCompletedObservation contains native custody evidence only. Neither
// Task.AttemptID nor ExecAttemptID is a canonical scheduling-attempt identity.
// The caller must separately bind its canonical attempt and published artifacts.
type OpenShellCompletedObservation struct {
	Identity              OpenShellIdentity
	Task                  OpenShellTask
	ExecAttemptID         string
	ExecRequestID         string
	ExecExitCode          int
	MainProcessInstanceID string
	Phase                 string
	CreateAcknowledged    bool
	StopAcknowledged      bool
}

// ObserveCompleted performs one fresh native Get against an already completed,
// originally owned worker. It never initializes, reconciles, saves, blocks or
// adopts evidence. Read errors leave both memory and the durable ledger intact.
func (c *OpenShellCustody) ObserveCompleted(ctx context.Context, expected OpenShellIdentity) (OpenShellCompletedObservation, error) {
	if !c.mu.TryLock() {
		return OpenShellCompletedObservation{}, fmt.Errorf("openshell custody operation in progress")
	}
	defer c.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, openShellObservationTimeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return OpenShellCompletedObservation{}, err
	}
	if err := c.available(); err != nil {
		return OpenShellCompletedObservation{}, err
	}
	if c.ctx == nil || c.client == nil {
		return OpenShellCompletedObservation{}, fmt.Errorf("openshell original custody connection unavailable")
	}
	if err := c.ctx.Err(); err != nil {
		return OpenShellCompletedObservation{}, err
	}
	stop := context.AfterFunc(c.ctx, cancel)
	defer stop()
	if expected.Sandbox.Gateway != c.state.Binding.Gateway || expected.Sandbox.Workspace != c.state.Binding.Workspace || expected.ConfigSource != c.state.Binding.ConfigSource {
		return OpenShellCompletedObservation{}, fmt.Errorf("openshell completion binding differs from original custody")
	}
	var record OpenShellCustodyRecord
	count := 0
	for _, candidate := range c.state.Records {
		if candidate.Worker.Identity == expected {
			record = candidate
			count++
		}
	}
	if count != 1 {
		return OpenShellCompletedObservation{}, fmt.Errorf("openshell original completion identity is missing or ambiguous")
	}
	if err := validateCompletedRecord(record, expected); err != nil {
		return OpenShellCompletedObservation{}, err
	}
	response, err := c.client.GetSandbox(ctx, &pb.GetSandboxRequest{Name: record.Name, WorkspaceScope: c.scope()})
	if err != nil {
		return OpenShellCompletedObservation{}, fmt.Errorf("observe original openshell completion: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return OpenShellCompletedObservation{}, err
	}
	if err := c.ctx.Err(); err != nil {
		return OpenShellCompletedObservation{}, err
	}
	if err := c.available(); err != nil {
		return OpenShellCompletedObservation{}, err
	}
	native := response.GetSandbox()
	if !sameOpenShellSandbox(native, record.Name, expected.Sandbox.SandboxID, expected.Sandbox.Workspace) {
		return OpenShellCompletedObservation{}, fmt.Errorf("openshell completion native identity changed or is missing")
	}
	spec, err := openShellBytes(native.GetSpec())
	if err != nil || native.GetSpec() == nil || !bytes.Equal(spec, record.Spec) {
		return OpenShellCompletedObservation{}, fmt.Errorf("openshell completion specification differs from original binding")
	}
	status := native.GetStatus()
	if status.GetMainProcessInstanceId() != record.MainProcessInstanceID {
		return OpenShellCompletedObservation{}, fmt.Errorf("openshell completion main process instance changed or is missing")
	}
	if status.GetPhase() != pb.SandboxPhase_SANDBOX_PHASE_STOPPED {
		return OpenShellCompletedObservation{}, fmt.Errorf("openshell original completion is not Stopped: %s", status.GetPhase())
	}
	if record.Stopped.ComputeInstanceID != "" && status.GetAgentPod() != "" && status.GetAgentPod() != record.Stopped.ComputeInstanceID {
		return OpenShellCompletedObservation{}, fmt.Errorf("openshell completion compute instance differs from stopped history")
	}
	return OpenShellCompletedObservation{Identity: record.Worker.Identity, Task: record.Task, ExecAttemptID: record.Exec.AttemptID, ExecRequestID: record.Exec.Command.RequestID, ExecExitCode: record.Exec.Result.ExitCode, MainProcessInstanceID: record.MainProcessInstanceID, Phase: "Stopped", CreateAcknowledged: true, StopAcknowledged: true}, nil
}

func validateCompletedRecord(record OpenShellCustodyRecord, expected OpenShellIdentity) error {
	for _, value := range []string{expected.OrderID, expected.AttemptID, expected.SessionID, expected.Sandbox.Gateway, expected.Sandbox.Workspace, expected.Sandbox.SandboxID, expected.SandboxName, expected.ConfigSource} {
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("openshell completion identity is incomplete")
		}
	}
	if record.Task != (OpenShellTask{OrderID: expected.OrderID, AttemptID: expected.AttemptID, SessionID: expected.SessionID}) || record.Name != expected.SandboxName {
		return fmt.Errorf("openshell completion task differs from original worker")
	}
	if record.Retained || !record.CreateAcknowledged || record.CreateRequestID == "" {
		return fmt.Errorf("openshell completion create acknowledgement is unavailable")
	}
	if record.Exec == nil || record.Exec.AttemptID == "" || record.Exec.Command.RequestID == "" {
		return fmt.Errorf("openshell completion original exec evidence is unavailable")
	}
	exec := record.Exec.Result
	if !exec.StartAttempted || !exec.ExitObserved || exec.ExitCode != 0 || exec.RequestID != record.Exec.Command.RequestID || exec.InputError != "" || exec.StreamError != "" {
		return fmt.Errorf("openshell completion native exec result is unresolved or unsuccessful")
	}
	if !record.StopAcknowledged || record.StopRequestID == "" || record.Worker.StopPending || record.Worker.StopError != "" {
		return fmt.Errorf("openshell completion stop acknowledgement is unresolved")
	}
	if record.Stopped == nil || record.MainProcessInstanceID == "" {
		return fmt.Errorf("openshell completion stopped history or main process identity is unavailable")
	}
	return nil
}

// ObserveCompleted borrows only the existing ready owner and current context
// lease. It releases the gateway mutex across the bounded native read, then
// rechecks that original generation and lease without acquiring or renewing it.
func (o *OpenShellGateway) ObserveCompleted(ctx context.Context, contextID string, expected OpenShellIdentity) (OpenShellCompletedObservation, error) {
	if err := ctx.Err(); err != nil {
		return OpenShellCompletedObservation{}, err
	}
	if !o.mu.TryLock() {
		return OpenShellCompletedObservation{}, fmt.Errorf("openshell gateway operation in progress")
	}
	custody, generation, err := o.completionOwner(contextID)
	o.mu.Unlock()
	if err != nil {
		return OpenShellCompletedObservation{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, openShellObservationTimeout)
	defer cancel()
	observed, err := custody.ObserveCompleted(ctx, expected)
	if err != nil {
		return OpenShellCompletedObservation{}, err
	}
	if !o.mu.TryLock() {
		return OpenShellCompletedObservation{}, fmt.Errorf("openshell gateway changed during completion observation")
	}
	defer o.mu.Unlock()
	current, currentGeneration, err := o.completionOwner(contextID)
	if err != nil {
		return OpenShellCompletedObservation{}, err
	}
	if current != custody || currentGeneration != generation {
		return OpenShellCompletedObservation{}, fmt.Errorf("openshell completion owner generation changed")
	}
	if err := ctx.Err(); err != nil {
		return OpenShellCompletedObservation{}, err
	}
	return observed, nil
}

// Called only under gateway.mu. Residents.Snapshot and generation.Running are
// read-only; the cached gateway resident's Contexts may be stale after Release.
func (o *OpenShellGateway) completionOwner(contextID string) (*OpenShellCustody, *admission.ResidentGeneration, error) {
	if contextID == "" || o.closed || !o.attempted || !o.ready || o.diagnostic != nil || o.custody == nil || o.generation == nil || o.residents == nil {
		return nil, nil, fmt.Errorf("openshell original ready completion owner is unavailable")
	}
	if err := o.generation.Running(); err != nil {
		return nil, nil, err
	}
	rows, err := o.residents.Snapshot()
	if err != nil {
		return nil, nil, err
	}
	for _, row := range rows {
		if row.ID != o.resident.ID {
			continue
		}
		if row.PID != o.resident.PID || row.Started != o.resident.Started || row.Fingerprint != o.resident.Fingerprint || row.GuardID != o.resident.GuardID || row.State != "running" || !slices.Contains(row.Contexts, contextID) {
			return nil, nil, fmt.Errorf("openshell completion context lease or original resident identity is unavailable")
		}
		return o.custody, o.generation, nil
	}
	return nil, nil, fmt.Errorf("openshell completion original resident is missing")
}
