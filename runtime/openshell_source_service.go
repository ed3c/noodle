package runtime

import (
	"context"
	"fmt"
	"time"

	"github.com/poteto/noodle/internal/admission"
)

// Native health does not certify operator authorization, credential supply,
// worker custody, full maintenance or any historical business outcome.
type OpenShellSourceServiceObservation struct {
	Resident   admission.Resident
	ObservedAt time.Time
}

func (o *OpenShellGateway) AcquireSourceService(ctx context.Context, contextID string) (OpenShellSourceServiceObservation, error) {
	if !o.mu.TryLock() {
		return OpenShellSourceServiceObservation{}, fmt.Errorf("openshell gateway operation in progress")
	}
	defer o.mu.Unlock()
	if !o.serviceOnly || contextID == "" {
		return OpenShellSourceServiceObservation{}, fmt.Errorf("original Source service owner and maintenance lease holder required")
	}
	if o.attempted {
		return OpenShellSourceServiceObservation{}, fmt.Errorf("Source service start already attempted; original readback only")
	}
	if _, err := o.acquireResidentLocked(ctx, contextID); err != nil {
		return OpenShellSourceServiceObservation{}, err
	}
	return o.sourceServiceReadbackLocked(ctx)
}

func (o *OpenShellGateway) ReadSourceService(ctx context.Context) (OpenShellSourceServiceObservation, error) {
	if !o.mu.TryLock() {
		return OpenShellSourceServiceObservation{}, fmt.Errorf("openshell gateway operation in progress")
	}
	defer o.mu.Unlock()
	return o.sourceServiceReadbackLocked(ctx)
}

func (o *OpenShellGateway) sourceServiceReadbackLocked(ctx context.Context) (OpenShellSourceServiceObservation, error) {
	result := OpenShellSourceServiceObservation{Resident: o.resident, ObservedAt: time.Now().UTC()}
	if !o.serviceOnly || o.closed || o.generation == nil {
		return result, fmt.Errorf("original Source service generation unavailable")
	}
	if !o.custody.mu.TryLock() {
		return result, fmt.Errorf("openshell custody operation in progress")
	}
	defer o.custody.mu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, o.spec.ReadinessTimeout)
	defer cancel()
	err := o.generation.Running()
	if err == nil {
		err = o.probeNativeReadiness(ctx)
	}
	if err == nil {
		err = o.generation.Running()
	}
	if err == nil {
		err = ctx.Err()
	}
	o.diagnostic = err
	return result, err
}
