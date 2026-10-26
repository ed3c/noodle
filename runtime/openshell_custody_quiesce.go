package runtime

import (
	"context"
	"errors"
	"fmt"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
)

// Quiesce is the gateway owner's last-lease barrier. Each invocation performs
// full inventory and same-ID native Get, including retained history. Success
// seals only this instance's mutations; it is not an external name-based lock
// and is never reused as a cached PASS. A busy VM leaves normal Stop available.
func (c *OpenShellCustody) Quiesce(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.audit(ctx); err != nil {
		return err
	}
	var unsettled error
	for i := range c.state.Records {
		r := &c.state.Records[i]
		s, err := c.read(ctx, r)
		if err != nil {
			return err
		}
		if s.GetStatus().GetPhase() != pb.SandboxPhase_SANDBOX_PHASE_STOPPED {
			unsettled = errors.Join(unsettled, fmt.Errorf("sandbox %s native phase %s; gateway release refused", r.Name, s.GetStatus().GetPhase()))
		}
		if (!r.Retained && !r.CreateAcknowledged) || r.Worker.StopPending || (r.StopRequestID != "" && !r.StopAcknowledged) || (r.Exec != nil && !r.Exec.Result.ExitObserved) {
			unsettled = errors.Join(unsettled, fmt.Errorf("sandbox %s operation acknowledgement unresolved; gateway release refused", r.Name))
		}
	}
	if err := errors.Join(unsettled, ctx.Err()); err != nil {
		return err
	}
	c.quiesced = true
	return nil
}

// Called only under the original custody mutex. Reads and diagnostics continue
// to use available, so sealing mutations never hides native readback.
func (c *OpenShellCustody) mutable() error {
	if err := c.available(); err != nil {
		return err
	}
	if c.quiesced {
		return fmt.Errorf("openshell custody quiesced; new effects refused")
	}
	return nil
}
