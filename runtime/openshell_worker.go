package runtime

import (
	"context"
	"fmt"
	"strings"
	"sync"
)

// OpenShellSandboxRef is the native boundary identity, not a sandbox name or a
// local launch ID. Adapters must explicitly select this gateway and workspace.
type OpenShellSandboxRef struct {
	Gateway   string
	Workspace string
	SandboxID string
}

// OpenShellIdentity is retained by the original order owner before observing an
// already submitted execution. This contract neither launches nor retries exec.
type OpenShellIdentity struct {
	OrderID      string
	AttemptID    string
	SessionID    string
	Sandbox      OpenShellSandboxRef
	SandboxName  string // diagnostic only; never used to select a boundary
	ConfigSource string // retained source of the explicit gateway/workspace selection
}

type OpenShellBoundaryState string

const (
	OpenShellBoundaryUnknown OpenShellBoundaryState = "unknown"
	OpenShellBoundaryReady   OpenShellBoundaryState = "ready"
	OpenShellBoundaryStopped OpenShellBoundaryState = "stopped"
)

// OpenShellSandboxObservation contains a live native read, not CLI exit status,
// exec Exit, or SandboxStatus.exit_code (which describes canonical main).
type OpenShellSandboxObservation struct {
	Sandbox OpenShellSandboxRef
	Phase   string // native normalized phase: only "Stopped" certifies exit
}

// OpenShellSandboxReader must perform a new native query for each call, return
// the response's native ID, and reject missing resources as errors. Cached state
// and a name resolved to a replacement ID cannot certify this attempt's exit.
// Keeping this interface read-only also constrains recovery to observation.
type OpenShellSandboxReader interface {
	ReadSandbox(context.Context, OpenShellSandboxRef) (OpenShellSandboxObservation, error)
}

// OpenShellSandboxStopper returns nil only for a conclusive native acknowledgement
// of the original control operation's completion, not local submission or queued
// intent. Any error leaves remote effects unknown; VM state cannot settle them.
type OpenShellSandboxStopper interface {
	StopSandbox(context.Context, OpenShellSandboxRef) error
}

// OpenShellExecResult keeps native exit separate from local transport/CLI
// evidence. RequestID is a launch admission fence, not a reattach or exec ID.
// ExitObserved is false after a lost response, even when the local CLI exited.
type OpenShellExecResult struct {
	RequestID      string
	StartAttempted bool // submission is possible; not proof of remote launch
	CLIExited      bool
	CLIExitCode    int
	ExitObserved   bool
	ExitCode       int
	Reason         string
	InputError     string
	StreamError    string // retained even if a native exit was already observed
}

// OpenShellWorkerRecord keeps model, exec and VM evidence independent. Boundary
// is diagnostic on disk; recovery never trusts it as fresh stopping evidence.
type OpenShellWorkerRecord struct {
	Identity     OpenShellIdentity
	ModelOutcome SessionOutcome
	ExecResult   OpenShellExecResult
	Boundary     OpenShellBoundaryState
	StopPending  bool // original native stop has no conclusive acknowledgement
	StopError    string
}

// OpenShellWorker is the evidence gate for one already identified worker, not a
// Runtime or a selectable dispatch route. An owner may retain its admission on
// Done; dispatch uncertainty must not enter DispatcherRuntime's error rollback.
type OpenShellWorker struct {
	mu     sync.Mutex
	record OpenShellWorkerRecord
	read   uint64
	done   chan struct{}
}

func NewOpenShellWorker(identity OpenShellIdentity) (*OpenShellWorker, error) {
	for _, field := range []struct{ name, value string }{
		{"order ID", identity.OrderID},
		{"attempt ID", identity.AttemptID},
		{"session ID", identity.SessionID},
		{"gateway", identity.Sandbox.Gateway},
		{"workspace", identity.Sandbox.Workspace},
		{"native sandbox ID", identity.Sandbox.SandboxID},
		{"configuration source", identity.ConfigSource},
	} {
		if strings.TrimSpace(field.value) == "" {
			return nil, fmt.Errorf("openshell worker %s missing", field.name)
		}
	}
	return &OpenShellWorker{
		record: OpenShellWorkerRecord{Identity: identity, Boundary: OpenShellBoundaryUnknown},
		done:   make(chan struct{}),
	}, nil
}

func (w *OpenShellWorker) Record() OpenShellWorkerRecord {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.record
}

func (w *OpenShellWorker) RecordModelOutcome(outcome SessionOutcome) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.record.ModelOutcome = outcome
}

func (w *OpenShellWorker) RecordExecResult(result OpenShellExecResult) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.record.ExecResult = result
	// A query predating this execution result cannot settle its unknown effects.
	w.invalidateRead()
}

// Done closes exactly once, after a fresh Stopped read of the original native
// ID. CLI/model completion, stop acknowledgement and absence never close it.
// Stopped is final for this attempt: starting another exec requires a new owner.
func (w *OpenShellWorker) Done() <-chan struct{} { return w.done }

func (w *OpenShellWorker) invalidateRead() {
	w.read++
	if w.record.Boundary != OpenShellBoundaryStopped {
		w.record.Boundary = OpenShellBoundaryUnknown
	}
}

func (w *OpenShellWorker) Readback(ctx context.Context, reader OpenShellSandboxReader) error {
	w.mu.Lock()
	if w.record.Boundary == OpenShellBoundaryStopped {
		w.mu.Unlock()
		return nil
	}
	w.invalidateRead()
	read, target := w.read, w.record.Identity.Sandbox
	w.mu.Unlock()
	observation, err := reader.ReadSandbox(ctx, target)
	if err == nil {
		err = ctx.Err()
	}
	if err == nil && observation.Sandbox != target {
		err = fmt.Errorf("native sandbox identity conflicts with original gateway/workspace/ID")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if err != nil {
		return fmt.Errorf("read openshell sandbox %s: %w", target.SandboxID, err)
	}
	if w.record.Boundary == OpenShellBoundaryStopped {
		return nil
	}
	if read != w.read {
		return fmt.Errorf("openshell sandbox %s read superseded in this attempt", target.SandboxID)
	}
	// An unacknowledged native stop could still affect a later continuation on
	// the same ID, even after a local timeout. VM state cannot settle that command.
	if w.record.StopPending {
		return nil
	}
	switch observation.Phase {
	case "Stopped":
		w.record.Boundary = OpenShellBoundaryStopped
		close(w.done)
	case "Ready":
		w.record.Boundary = OpenShellBoundaryReady
	}
	return nil
}

// Stop retains control custody until a conclusive native acknowledgement, then
// independently reads the original ID. Opaque errors stay pending and cannot be
// retried by this owner. Only Done certifies admission release.
func (w *OpenShellWorker) Stop(ctx context.Context, stopper OpenShellSandboxStopper, reader OpenShellSandboxReader) error {
	w.mu.Lock()
	if w.record.Boundary == OpenShellBoundaryStopped {
		w.mu.Unlock()
		return nil
	}
	if w.record.StopPending {
		w.mu.Unlock()
		return fmt.Errorf("openshell stop acknowledgement unresolved in this attempt; original operation reconciliation required")
	}
	w.record.StopPending = true
	w.invalidateRead()
	target := w.record.Identity.Sandbox
	w.mu.Unlock()

	err := stopper.StopSandbox(ctx, target)
	w.mu.Lock()
	if err == nil {
		w.record.StopPending = false
	} else {
		w.record.StopError = err.Error()
	}
	// Local failure does not settle the native command. Invalidate old reads,
	// retain pending custody and its diagnostic, and never automatically replay.
	w.invalidateRead()
	w.mu.Unlock()
	if err != nil {
		return fmt.Errorf("stop openshell sandbox %s: %w", target.SandboxID, err)
	}
	return w.Readback(ctx, reader)
}

// RecoverOpenShellWorker compares the retained identity with the original owner
// binding and performs only a new read. It does not adopt saved Stopped evidence
// or replay an uncertain exec. On query error the non-nil worker still owns an
// unresolved boundary; callers must retain admission rather than roll it back.
func RecoverOpenShellWorker(ctx context.Context, original OpenShellIdentity, retained OpenShellWorkerRecord, reader OpenShellSandboxReader) (*OpenShellWorker, error) {
	if retained.Identity.OrderID != original.OrderID || retained.Identity.AttemptID != original.AttemptID ||
		retained.Identity.SessionID != original.SessionID || retained.Identity.Sandbox != original.Sandbox ||
		retained.Identity.ConfigSource != original.ConfigSource {
		return nil, fmt.Errorf("openshell recovery identity conflicts with original order/attempt/session binding")
	}
	w, err := NewOpenShellWorker(original)
	if err != nil {
		return nil, err
	}
	w.record.ModelOutcome = retained.ModelOutcome
	w.record.ExecResult = retained.ExecResult
	w.record.StopPending = retained.StopPending
	w.record.StopError = retained.StopError
	err = w.Readback(ctx, reader)
	return w, err
}
