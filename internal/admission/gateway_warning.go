package admission

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

type GatewayWarningGrant struct {
	ContextID      string    `json:"context_id"`
	OrderID        string    `json:"order_id"`
	ResidentID     string    `json:"resident_id"`
	Revision       uint64    `json:"revision"`
	Status         string    `json:"status"`
	GatewayStarted bool      `json:"gateway_started"`
	IssuedAt       time.Time `json:"issued_at"`
	ExpiresAt      time.Time `json:"expires_at"`
}

func (budget *Budget) GrantGatewayWarning(id, order, resident string, previous uint64) (GatewayWarningGrant, error) {
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if budget.shared != nil {
		return GatewayWarningGrant{}, fmt.Errorf("gateway warning permission is unsupported by the shared-quota owner; retain ordinary admission")
	}
	if !budget.contexts[id] || strings.TrimSpace(order) == "" || strings.TrimSpace(resident) == "" {
		return GatewayWarningGrant{}, fmt.Errorf("gateway warning task or resident binding unavailable")
	}
	task := warningTask{id, order}
	if grant, ok := budget.gatewayWarnings[task]; ok {
		if grant.ResidentID != resident {
			return GatewayWarningGrant{}, fmt.Errorf("gateway warning resident binding changed")
		}
		if previous < grant.Revision && grant.Revision-previous == 1 {
			return budget.gatewayWarningStatus(grant), nil
		}
		if previous != grant.Revision || grant.Status == "revoked" || budget.gatewayWarningStatus(grant).Status == "available" {
			return GatewayWarningGrant{}, fmt.Errorf("gateway warning revision or terminal grant unavailable")
		}
	} else if previous != 0 {
		return GatewayWarningGrant{}, fmt.Errorf("previous gateway warning grant not found")
	}
	for other, grant := range budget.gatewayWarnings {
		if other.context == id && other != task && budget.gatewayWarningStatus(grant).Status == "available" {
			return GatewayWarningGrant{}, fmt.Errorf("context already has an unconsumed gateway warning grant")
		}
	}
	if !budget.gatewayWarningSafe(resident) {
		return GatewayWarningGrant{}, fmt.Errorf("gateway warning blocked: pressure=%s active_allocations=%d active_residents=%d unresolved_residents=%d", budget.pressure, budget.active, budget.residents, budget.unresolvedResidents)
	}
	if budget.gatewayWarnings == nil {
		budget.gatewayWarnings = make(map[warningTask]GatewayWarningGrant)
	}
	grant := GatewayWarningGrant{ContextID: id, OrderID: order, ResidentID: resident, Revision: previous + 1,
		Status: "available", GatewayStarted: budget.residentScopes[resident] == 1,
		IssuedAt: budget.now(), ExpiresAt: budget.now().Add(WarningGrantLifetime)}
	budget.gatewayWarnings[task] = grant
	return grant, nil
}

func (budget *Budget) GatewayWarningGrants() []GatewayWarningGrant {
	budget.mu.Lock()
	defer budget.mu.Unlock()
	grants := make([]GatewayWarningGrant, 0, len(budget.gatewayWarnings))
	for _, grant := range budget.gatewayWarnings {
		grants = append(grants, budget.gatewayWarningStatus(grant))
	}
	sort.Slice(grants, func(first, second int) bool {
		if grants[first].ContextID == grants[second].ContextID {
			return grants[first].OrderID < grants[second].OrderID
		}
		return grants[first].ContextID < grants[second].ContextID
	})
	return grants
}

func (budget *Budget) gatewayWarningStatus(grant GatewayWarningGrant) GatewayWarningGrant {
	if grant.Status == "available" && !budget.now().Before(grant.ExpiresAt) {
		grant.Status = "expired"
	}
	return grant
}

func (budget *Budget) gatewayWarningSafe(resident string) bool {
	if budget.switching || len(budget.pendingSharedReturns) != 0 || budget.previous == nil || budget.previous.SwapUsedBytes >= budget.swapUsedLimit {
		return false
	}
	if budget.shared != nil {
		quota := budget.shared.snapshot()
		if quota.Error != "" || quota.Allocations != budget.active {
			return false
		}
	}
	pressureReady := budget.pressure == Normal && budget.reason == "ready"
	pressureReady = pressureReady || budget.pressure == Warning && budget.warningStable >= 3
	return !budget.stale() && !budget.unsupported && budget.sampleError == nil && budget.rateValid && budget.rate == 0 &&
		pressureReady && budget.unresolvedResidents == 0 && budget.active == budget.residents &&
		(budget.residents == 0 || budget.residents == 1 && budget.residentScopes[resident] == 1)
}

func (budget *Budget) gatewayWarningAvailable(id, order string, resident bool, scope string) bool {
	for task, grant := range budget.gatewayWarnings {
		if task.context != id || budget.gatewayWarningStatus(grant).Status != "available" || !budget.gatewayWarningSafe(grant.ResidentID) {
			continue
		}
		if resident && scope != "" && scope == grant.ResidentID && !grant.GatewayStarted && budget.active == 0 {
			return true
		}
		if !resident && order == task.order && grant.GatewayStarted && budget.residentScopes[grant.ResidentID] == 1 {
			return true
		}
	}
	return false
}

func (budget *Budget) markGatewayStarted(id, scope string) {
	for task, grant := range budget.gatewayWarnings {
		if task.context == id && grant.ResidentID == scope && budget.gatewayWarningStatus(grant).Status == "available" {
			grant.GatewayStarted = true
			budget.gatewayWarnings[task] = grant
		}
	}
}
