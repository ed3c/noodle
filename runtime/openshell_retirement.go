package runtime

import (
	"context"
	"fmt"
	"reflect"
	"strings"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// OpenShellClosure is issued by the original task owner after acceptance or
// explicit failure closure. Failed/timeout status alone supplies no authority.
type OpenShellClosure struct {
	ExecutionBasis string                            `json:"execution_basis"`
	Identity       OpenShellIdentity                 `json:"identity"`
	Outcome        SessionStatus                     `json:"outcome"`
	Reason         string                            `json:"reason"`
	Evidence       map[string]SessionReadbackFileRef `json:"evidence"`
}

// OpenShellRetirement preserves history after native driver-owned deletion.
// A request without its ACK is never replayed, even when Get reports NotFound.
type OpenShellRetirement struct {
	Closure      SessionReadbackFileRef
	Observation  OpenShellCompletedObservation
	RequestID    string
	Acknowledged bool
	Completed    bool
}

func validateRetirement(r OpenShellCustodyRecord) error {
	v := r.Retirement
	if v == nil {
		return nil
	}
	if r.Retained || v.RequestID == "" || v.Closure.Path == "" || len(v.Closure.SHA256) != 64 || (v.Completed && !v.Acknowledged) {
		return fmt.Errorf("retirement intent or completion binding invalid")
	}
	if v.Observation.ExecutionBasis == openShellNotStarted {
		return validateNotStartedRetirement(r)
	}
	if err := validateClosedRecord(r, r.Worker.Identity); err != nil {
		return err
	}
	if v.Observation.Identity != r.Worker.Identity || v.Observation.Phase != "Stopped" || !v.Observation.StopAcknowledged || v.Observation.MainProcessInstanceID != r.MainProcessInstanceID || v.Observation.ExecExitCode != r.Exec.Result.ExitCode || v.Observation.ExecRequestID != r.Exec.Command.RequestID || v.Observation.ExecAttemptID != r.Exec.AttemptID || v.Observation.Task != r.Task || !v.Observation.CreateAcknowledged {
		return fmt.Errorf("retirement original stopped observation differs")
	}
	command, err := OpenShellCommandSHA256(r.Exec.Command)
	if err != nil || command != v.Observation.CommandSHA256 {
		return fmt.Errorf("retirement command observation differs")
	}
	return nil
}

func readOpenShellClosure(ref SessionReadbackFileRef, id OpenShellIdentity) (OpenShellClosure, error) {
	var closure OpenShellClosure
	actual, err := ReadSessionReadbackFile(ref.Path, &closure)
	if err != nil {
		return closure, err
	}
	if actual != ref || closure.Identity != id || strings.TrimSpace(closure.Reason) == "" || len(closure.Evidence) == 0 || (closure.Outcome != StatusCompleted && closure.Outcome != StatusFailed && closure.Outcome != StatusCancelled) {
		return closure, fmt.Errorf("original task closure or retained evidence differs")
	}
	return closure, VerifySessionReadbackFiles(closure.Evidence)
}

// Retire deletes through the original native driver, never local VM paths. The
// public API is name-based; dedicated-owner custody cannot lock external clients.
func (c *OpenShellCustody) Retire(ctx context.Context, id OpenShellIdentity, closureRef SessionReadbackFileRef) error {
	closure, err := readOpenShellClosure(closureRef, id)
	if err != nil {
		return err
	}
	// Fresh original execution, Stop ACK and same-ID Stopped must precede intent.
	validate := validateCompletedRecord
	if closure.Outcome != StatusCompleted {
		validate = validateClosedRecord
	}
	if closure.ExecutionBasis == openShellNotStarted {
		if closure.Outcome != StatusFailed {
			return fmt.Errorf("not-started closure is not a failed outcome")
		}
		validate = validateNotStartedRecord
	}
	observed, err := c.observeStopped(ctx, id, validate)
	if err != nil {
		return err
	}
	if !c.mu.TryLock() {
		return fmt.Errorf("openshell custody operation in progress")
	}
	defer c.mu.Unlock()
	if err := c.mutable(); err != nil {
		return err
	}
	r, err := c.owned(id.Sandbox)
	if err != nil {
		return err
	}
	if r.Worker.Identity != id {
		return fmt.Errorf("retirement task identity differs")
	}
	if err := c.audit(ctx); err != nil {
		return err
	}
	if _, err := c.read(ctx, r); err != nil {
		return err
	}
	if _, err := readOpenShellClosure(closureRef, id); err != nil {
		return err
	}
	if closure.ExecutionBasis == openShellNotStarted {
		if err := validateNotStartedRecord(*r, id); err != nil {
			return err
		}
		if !reflect.DeepEqual(observed, notStartedObservation(*r)) {
			return fmt.Errorf("not-started worker changed before retirement intent")
		}
	}
	requestID, err := openShellUUID()
	if err != nil {
		return err
	}
	r.Retirement = &OpenShellRetirement{Closure: closureRef, Observation: observed, RequestID: requestID}
	if err := c.save(); err != nil {
		return err
	}
	response, err := c.client.DeleteSandbox(ctx, &pb.DeleteSandboxRequest{Name: r.Name, WorkspaceScope: c.scope(), RequestId: requestID})
	if err != nil {
		return fmt.Errorf("native retirement acknowledgement unresolved; replay prohibited: %w", err)
	}
	outcome := response.GetOutcome()
	if response.GetSandboxId() != id.Sandbox.SandboxID || (outcome != pb.DeletionOutcome_DELETION_OUTCOME_COMPLETED && outcome != pb.DeletionOutcome_DELETION_OUTCOME_ACCEPTED) {
		return c.block("native retirement acknowledgement identity or outcome differs")
	}
	r.Retirement.Acknowledged = true
	if err := c.save(); err != nil {
		r.Retirement.Acknowledged = false
		return err
	}
	return c.observeRetirement(ctx, r)
}

// Called under custody mutex. Only an acknowledged original deletion plus fresh
// native absence proves retirement. ACCEPTED alone does not release disk custody.
func (c *OpenShellCustody) observeRetirement(ctx context.Context, r *OpenShellCustodyRecord) error {
	if err := validateRetirement(*r); err != nil {
		return err
	}
	v := r.Retirement
	if v == nil {
		return fmt.Errorf("retirement intent missing")
	}
	if _, err := readOpenShellClosure(v.Closure, r.Worker.Identity); err != nil {
		return err
	}
	if !v.Acknowledged {
		return fmt.Errorf("native retirement acknowledgement unresolved; replay prohibited")
	}
	response, err := c.client.GetSandbox(ctx, &pb.GetSandboxRequest{Name: r.Name, WorkspaceScope: c.scope()})
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if status.Code(err) == codes.NotFound {
		if !v.Completed {
			v.Completed = true
			if err := c.save(); err != nil {
				v.Completed = false
				return err
			}
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("native retirement absence unknown: %w", err)
	}
	if !sameOpenShellSandbox(response.GetSandbox(), r.Name, r.Worker.Identity.Sandbox.SandboxID, c.state.Binding.Workspace) || v.Completed {
		return c.block("retired sandbox identity or name reappeared")
	}
	return fmt.Errorf("native retirement acknowledged but driver cleanup is pending")
}

// ObserveRetired is separate from fresh completion: its Stopped observation is
// explicitly historical, backed by the original ACK and freshly checked absence.
func (c *OpenShellCustody) ObserveRetired(ctx context.Context, id OpenShellIdentity, closureRef SessionReadbackFileRef) (OpenShellCompletedObservation, error) {
	if !c.mu.TryLock() {
		return OpenShellCompletedObservation{}, fmt.Errorf("openshell custody operation in progress")
	}
	defer c.mu.Unlock()
	if err := c.available(); err != nil {
		return OpenShellCompletedObservation{}, err
	}
	for i := range c.state.Records {
		r := &c.state.Records[i]
		if r.Worker.Identity != id {
			continue
		}
		if r.Retirement == nil || !reflect.DeepEqual(r.Retirement.Closure, closureRef) {
			break
		}
		if err := c.observeRetirement(ctx, r); err != nil {
			return OpenShellCompletedObservation{}, err
		}
		return r.Retirement.Observation, nil
	}
	return OpenShellCompletedObservation{}, fmt.Errorf("original retirement identity or closure unavailable")
}
