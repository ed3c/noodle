package runtime

import (
	"fmt"

	"github.com/poteto/noodle/internal/admission"
)

func (owner *OpenShellGateway) GrantWarning(contextID, orderID string, previous uint64) (admission.GatewayWarningGrant, error) {
	if !owner.mu.TryLock() {
		return admission.GatewayWarningGrant{}, fmt.Errorf("openshell gateway operation in progress")
	}
	defer owner.mu.Unlock()
	if owner.closed || owner.diagnostic != nil {
		return admission.GatewayWarningGrant{}, fmt.Errorf("openshell gateway warning owner unavailable: %v", owner.diagnostic)
	}
	if owner.attempted {
		if !owner.ready || owner.generation == nil {
			return admission.GatewayWarningGrant{}, fmt.Errorf("openshell original gateway readiness unavailable")
		}
		if err := owner.generation.Running(); err != nil {
			return admission.GatewayWarningGrant{}, err
		}
	}
	return owner.budget.GrantGatewayWarning(contextID, orderID, owner.spec.Resident.Name, previous)
}
