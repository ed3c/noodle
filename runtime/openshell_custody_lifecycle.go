package runtime

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"google.golang.org/protobuf/proto"
)

// Create retains the original task, name, UUID and exact spec before its only
// native Create call. Any error after intent returns the nonempty reserved name.
// A caller may explicitly keep an order for a new attempt/session only after
// every previous attempt has observed Stopped with no unresolved operations.
func (c *OpenShellCustody) Create(ctx context.Context, task OpenShellTask, spec *pb.SandboxSpec) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if task.OrderID == "" || task.AttemptID == "" || task.SessionID == "" || spec == nil {
		return "", fmt.Errorf("openshell create task/spec missing")
	}
	if err := c.mutable(); err != nil {
		return "", err
	}
	for _, r := range c.state.Records {
		if !r.Retained && (r.Task.AttemptID == task.AttemptID || r.Task.SessionID == task.SessionID) {
			return r.Name, fmt.Errorf("openshell task already reserved; create replay prohibited")
		}
	}
	if err := c.audit(ctx); err != nil {
		return "", err
	}
	for _, r := range c.state.Records {
		if r.Retained || r.Task.OrderID != task.OrderID {
			continue
		}
		pendingExec := r.Exec != nil && !r.Exec.Result.ExitObserved
		pendingExec = pendingExec || (r.ProgressStop != nil && !r.ProgressStop.Result.ExitObserved)
		if !r.CreateAcknowledged || r.Stopped == nil || r.Worker.StopPending || (r.StopRequestID != "" && !r.StopAcknowledged) || pendingExec {
			return r.Name, fmt.Errorf("openshell order %s previous attempt %s not settled; explicit new attempt rejected", task.OrderID, r.Task.AttemptID)
		}
	}
	id, err := openShellUUID()
	if err != nil {
		return "", err
	}
	// 19 lowercase alphanumeric characters, with the entire history checked.
	name := "n" + strings.ReplaceAll(id, "-", "")[:18]
	for _, r := range c.state.Records {
		if r.Name == name {
			return "", fmt.Errorf("openshell generated name already reserved")
		}
	}
	request := &pb.CreateSandboxRequest{Name: name, WorkspaceScope: c.scope(), RequestId: id, Spec: proto.Clone(spec).(*pb.SandboxSpec)}
	intent, err := openShellBytes(request)
	if err != nil {
		return "", err
	}
	r := OpenShellCustodyRecord{Task: task, Name: name, CreateRequestID: id, CreateIntent: intent}
	c.state.Records = append(c.state.Records, r)
	index := len(c.state.Records) - 1
	if err := c.save(); err != nil {
		return name, err
	}
	response, err := c.client.CreateSandbox(ctx, request)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return name, c.block(fmt.Sprintf("create %s acknowledgement unresolved: %v", name, err))
	}
	s := response.GetSandbox()
	meta := s.GetMetadata()
	if !sameOpenShellSandbox(s, name, meta.GetId(), c.state.Binding.Workspace) || s.GetSpec() == nil {
		return name, c.block("create response identity/spec missing or conflicting")
	}
	for _, old := range c.state.Records {
		if old.Name != name && old.Worker.Identity.Sandbox.SandboxID == meta.GetId() {
			return name, c.block("create response reused a historical native ID")
		}
	}
	r.Spec, err = openShellBytes(s.GetSpec())
	if err != nil {
		return name, c.block(err.Error())
	}
	r.CreateAcknowledged = true
	r.MainProcessInstanceID = s.GetStatus().GetMainProcessInstanceId()
	r.Worker = OpenShellWorkerRecord{Identity: OpenShellIdentity{OrderID: task.OrderID, AttemptID: task.AttemptID, SessionID: task.SessionID, Sandbox: c.ref(meta.GetId()), SandboxName: name, ConfigSource: c.state.Binding.ConfigSource}, Boundary: OpenShellBoundaryUnknown}
	c.state.Records[index] = r
	if err := c.save(); err != nil {
		c.state.Records[index].CreateAcknowledged = false
		return name, err
	}
	return name, nil
}

func (c *OpenShellCustody) owned(ref OpenShellSandboxRef) (*OpenShellCustodyRecord, error) {
	if err := c.available(); err != nil {
		return nil, err
	}
	for i := range c.state.Records {
		r := &c.state.Records[i]
		if r.Worker.Identity.Sandbox == ref {
			if r.Retained || !r.CreateAcknowledged || r.Retirement != nil {
				return nil, fmt.Errorf("sandbox is retained read-only or create unresolved")
			}
			return r, nil
		}
	}
	return nil, fmt.Errorf("sandbox not owned by original custody")
}

// ReadSandbox resolves the API name only from the original owner record.
func (c *OpenShellCustody) ReadSandbox(ctx context.Context, ref OpenShellSandboxRef) (OpenShellSandboxObservation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r, err := c.owned(ref)
	if err != nil {
		return OpenShellSandboxObservation{}, err
	}
	s, err := c.read(ctx, r)
	if err != nil {
		return OpenShellSandboxObservation{}, err
	}
	phase := ""
	switch s.GetStatus().GetPhase() {
	case pb.SandboxPhase_SANDBOX_PHASE_READY:
		phase = "Ready"
	case pb.SandboxPhase_SANDBOX_PHASE_STOPPED:
		phase = "Stopped"
	}
	return OpenShellSandboxObservation{Sandbox: ref, Phase: phase}, nil
}

// StopSandbox is an OpenShellWorker adapter. The saved worker record is also a
// recovery input; pending is cleared ONLY by a same-ID native success ACK.
func (c *OpenShellCustody) StopSandbox(ctx context.Context, ref OpenShellSandboxRef) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.mutable(); err != nil {
		return err
	}
	r, err := c.owned(ref)
	if err != nil {
		return err
	}
	if r.StopRequestID != "" || r.Stopped != nil {
		return fmt.Errorf("openshell stop already submitted or sandbox already observed Stopped; replay prohibited")
	}
	if err := c.audit(ctx); err != nil {
		return err
	}
	if _, err := c.read(ctx, r); err != nil {
		return err
	}
	id, err := openShellUUID()
	if err != nil {
		return err
	}
	r.StopRequestID = id
	r.Worker.StopPending = true
	if err := c.save(); err != nil {
		return err
	}
	response, err := c.client.StopSandbox(ctx, &pb.StopSandboxRequest{Name: r.Name, WorkspaceScope: c.scope(), RequestId: id})
	if err == nil {
		err = ctx.Err()
	}
	if err == nil && !sameOpenShellSandbox(response.GetSandbox(), r.Name, ref.SandboxID, ref.Workspace) {
		r.Worker.StopError = "stop acknowledgement native identity conflicts"
		return c.block(r.Worker.StopError)
	}
	if err != nil {
		r.Worker.StopError = err.Error()
		return errors.Join(fmt.Errorf("stop acknowledgement unresolved: %w", err), c.save())
	}
	r.StopAcknowledged = true
	r.Worker.StopPending = false
	if err := c.save(); err != nil {
		r.StopAcknowledged = false
		r.Worker.StopPending = true
		return err
	}
	return nil
}

// RecoverWorker uses the durable pending flag and only fresh native reads.
func (c *OpenShellCustody) RecoverWorker(ctx context.Context, ref OpenShellSandboxRef) (*OpenShellWorker, error) {
	c.mu.Lock()
	r, err := c.owned(ref)
	if err != nil {
		c.mu.Unlock()
		return nil, err
	}
	retained := r.Worker
	c.mu.Unlock()
	return RecoverOpenShellWorker(ctx, retained.Identity, retained, c)
}

// OpenShellOwnedExec persists Wait's native result independently of VM evidence.
// Closing a stream without Wait leaves the saved exec unresolved, safely.
type OpenShellOwnedExec struct {
	*OpenShellExec
	owner        *OpenShellCustody
	name         string
	progressStop bool
}

func (e *OpenShellOwnedExec) Wait() (OpenShellExecResult, error) {
	result, err := e.OpenShellExec.Wait()
	c := e.owner
	c.mu.Lock()
	defer c.mu.Unlock()
	if available := c.available(); available != nil {
		return result, errors.Join(err, available)
	}
	for i := range c.state.Records {
		r := &c.state.Records[i]
		if r.Name == e.name {
			saved := r.Exec
			if e.progressStop {
				saved = r.ProgressStop
			}
			if saved == nil || saved.Command.RequestID != result.RequestID {
				return result, errors.Join(err, fmt.Errorf("owned exec purpose or request binding differs"))
			}
			saved.Result = result
			if !e.progressStop {
				r.Worker.ExecResult = result
			}
			return result, errors.Join(err, c.save())
		}
	}
	return result, errors.Join(err, fmt.Errorf("owned exec record missing"))
}

// StartOwned persists the UUID/attempt/command before calling the existing
// non-TTY transport. Every saved attempt is one-shot, including failed starts.
func (c *OpenShellCustody) StartOwned(ctx context.Context, ref OpenShellSandboxRef, attempt string, command OpenShellCommand) (*OpenShellOwnedExec, error) {
	return c.startOwned(ctx, ref, attempt, command, false)
}

// StartProgressStop records a separate, one-shot native exec for the original
// supervisor's stop-and-export protocol. It cannot replace the model's receipt.
func (c *OpenShellCustody) StartProgressStop(ctx context.Context, ref OpenShellSandboxRef, requestID string) (*OpenShellOwnedExec, error) {
	return c.startOwned(ctx, ref, "", OpenShellCommand{RequestID: requestID}, true)
}

func (c *OpenShellCustody) startOwned(ctx context.Context, ref OpenShellSandboxRef, attempt string, command OpenShellCommand, progressStop bool) (*OpenShellOwnedExec, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.mutable(); err != nil {
		return nil, err
	}
	r, err := c.owned(ref)
	if err != nil {
		return nil, err
	}
	if progressStop {
		if r.Exec == nil || r.ProgressStop != nil {
			return nil, fmt.Errorf("original model exec missing or progress stop already reserved")
		}
		command, err = progressStopCommand(r.Exec.Command, command.RequestID)
		if err != nil {
			return nil, err
		}
		attempt = r.Exec.AttemptID + "/progress-stop"
	} else if r.Exec != nil {
		return nil, fmt.Errorf("openshell exec attempt already reserved; replay prohibited")
	}
	if err := command.validate(); err != nil {
		return nil, err
	}
	if attempt == "" {
		return nil, fmt.Errorf("openshell exec attempt missing")
	}
	if r.StopRequestID != "" {
		return nil, fmt.Errorf("openshell exec attempt already reserved or stop submitted; replay prohibited")
	}
	for _, old := range c.state.Records {
		if strings.EqualFold(old.CreateRequestID, command.RequestID) || strings.EqualFold(old.StopRequestID, command.RequestID) {
			return nil, fmt.Errorf("openshell mutation UUID already reserved")
		}
		if old.Exec != nil && (old.Exec.AttemptID == attempt || strings.EqualFold(old.Exec.Command.RequestID, command.RequestID)) {
			return nil, fmt.Errorf("openshell exec UUID/attempt already reserved")
		}
		if old.ProgressStop != nil && (old.ProgressStop.AttemptID == attempt || strings.EqualFold(old.ProgressStop.Command.RequestID, command.RequestID)) {
			return nil, fmt.Errorf("openshell progress-stop UUID/attempt already reserved")
		}
	}
	if err := c.audit(ctx); err != nil {
		return nil, err
	}
	s, err := c.read(ctx, r)
	if err != nil {
		return nil, err
	}
	if s.GetStatus().GetPhase() != pb.SandboxPhase_SANDBOX_PHASE_READY {
		return nil, fmt.Errorf("openshell owned sandbox not Ready")
	}
	// Clone command data at admission; caller mutations cannot change the record.
	saved := command
	saved.Argv = slices.Clone(command.Argv)
	saved.Env = maps.Clone(command.Env)
	record := &OpenShellCustodyExec{AttemptID: attempt, Command: saved, Result: OpenShellExecResult{RequestID: command.RequestID, StartAttempted: true, Reason: "exec acknowledgement unresolved; replay prohibited"}}
	if progressStop {
		r.ProgressStop = record
	} else {
		r.Exec = record
		r.Worker.ExecResult = record.Result
	}
	if err := c.save(); err != nil {
		return nil, err
	}
	// Use the owner's verified connection. It remains open until custody Close.
	transport := &OpenShellTransport{client: c.client, target: OpenShellTarget{Sandbox: ref, Name: r.Name, ConfigSource: c.state.Binding.ConfigSource}, ctx: c.ctx, observeSandbox: func(s *pb.Sandbox) error { return c.matches(r, s) }}
	handle, err := transport.Start(ctx, saved)
	if handle == nil {
		return nil, errors.Join(err, c.block(fmt.Sprintf("owned exec preflight/stream unresolved: %v", err)))
	}
	return &OpenShellOwnedExec{OpenShellExec: handle, owner: c, name: r.Name, progressStop: progressStop}, err
}
