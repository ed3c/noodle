package runtime

import (
	"context"
	"fmt"
)

// The original context lease owns retirement, just as it owns completion reads.
func (o *OpenShellGateway) Retire(ctx context.Context, contextID string, id OpenShellIdentity, closure SessionReadbackFileRef) error {
	if !o.mu.TryLock() {
		return fmt.Errorf("openshell gateway operation in progress")
	}
	defer o.mu.Unlock()
	custody, generation, err := o.completionOwner(contextID)
	if err != nil {
		return err
	}
	if err := custody.Retire(ctx, id, closure); err != nil {
		return err
	}
	current, currentGeneration, err := o.completionOwner(contextID)
	if err != nil {
		return err
	}
	if current != custody || generation != currentGeneration {
		return fmt.Errorf("openshell retirement owner generation changed")
	}
	return nil
}

func (o *OpenShellGateway) ObserveCleanup(ctx context.Context, contextID string, id OpenShellIdentity, progress bool) (OpenShellCompletedObservation, error) {
	if !o.mu.TryLock() {
		return OpenShellCompletedObservation{}, fmt.Errorf("openshell gateway operation in progress")
	}
	defer o.mu.Unlock()
	custody, _, err := o.completionOwner(contextID)
	if err != nil {
		return OpenShellCompletedObservation{}, err
	}
	for _, record := range custody.Snapshot().Records {
		if record.Worker.Identity == id && record.Retirement != nil {
			return custody.ObserveRetired(ctx, id, record.Retirement.Closure)
		}
	}
	return custody.observeSettled(ctx, id, progress)
}
