package runtime

import (
	"context"
	"fmt"
	"reflect"
	"strings"
)

const openShellNotStarted = "not_started"

// No exec record means no submission intent ever passed this original owner.
// An exec record with a missing result is uncertainty, never this capability.
func validateNotStartedRecord(r OpenShellCustodyRecord, id OpenShellIdentity) error {
	for _, v := range []string{id.OrderID, id.AttemptID, id.SessionID, id.Sandbox.Gateway, id.Sandbox.Workspace, id.Sandbox.SandboxID, id.SandboxName, id.ConfigSource} {
		if strings.TrimSpace(v) == "" {
			return fmt.Errorf("not-started identity is incomplete")
		}
	}
	if r.Worker.Identity != id || r.Task != (OpenShellTask{id.OrderID, id.AttemptID, id.SessionID}) || r.Name != id.SandboxName || r.Retained || !r.CreateAcknowledged || r.CreateRequestID == "" {
		return fmt.Errorf("not-started original create acknowledgement or task binding unavailable")
	}
	if r.Exec != nil || r.ProgressStop != nil || r.Worker.ExecResult != (OpenShellExecResult{}) {
		return fmt.Errorf("not-started worker has execution evidence")
	}
	if !r.StopAcknowledged || r.StopRequestID == "" || r.Worker.StopPending || r.Worker.StopError != "" || r.Stopped == nil || r.MainProcessInstanceID == "" {
		return fmt.Errorf("not-started original stop acknowledgement or stopped identity unavailable")
	}
	return nil
}

func notStartedObservation(r OpenShellCustodyRecord) OpenShellCompletedObservation {
	return OpenShellCompletedObservation{ExecutionBasis: openShellNotStarted, Identity: r.Worker.Identity, Task: r.Task, ExecExitCode: -1, MainProcessInstanceID: r.MainProcessInstanceID, Phase: "Stopped", CreateAcknowledged: true, StopAcknowledged: true}
}

// Only the existing context lease may observe diagnostic closure. Retired
// observations remain historical and separately require fresh native absence.
func (o *OpenShellGateway) observeDiagnosticFailure(ctx context.Context, contextID string, id OpenShellIdentity) (OpenShellCompletedObservation, error) {
	if !o.mu.TryLock() {
		return OpenShellCompletedObservation{}, fmt.Errorf("openshell gateway operation in progress")
	}
	defer o.mu.Unlock()
	c, _, err := o.completionOwner(contextID)
	if err != nil {
		return OpenShellCompletedObservation{}, err
	}
	for _, r := range c.Snapshot().Records {
		if r.Worker.Identity == id && r.Retirement != nil {
			return c.ObserveRetired(ctx, id, r.Retirement.Closure)
		}
	}
	return c.observeStopped(ctx, id, validateNotStartedRecord)
}

func validateNotStartedRetirement(r OpenShellCustodyRecord) error {
	if err := validateNotStartedRecord(r, r.Worker.Identity); err != nil {
		return err
	}
	if !reflect.DeepEqual(r.Retirement.Observation, notStartedObservation(r)) {
		return fmt.Errorf("not-started retirement observation differs")
	}
	closure, err := readOpenShellClosure(r.Retirement.Closure, r.Worker.Identity)
	if err != nil {
		return err
	}
	if closure.Outcome != StatusFailed || closure.ExecutionBasis != openShellNotStarted {
		return fmt.Errorf("not-started retirement closure differs")
	}
	return nil
}
