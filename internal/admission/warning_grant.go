package admission

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

const WarningGrantLifetime = 10 * time.Minute

type warningTask struct{ context, order string }

// WarningGrant is a single launch permission in the current Host lifetime.
// It does not reserve RAM or acknowledge worker completion.
type WarningGrant struct {
	ContextID string    `json:"context_id"`
	OrderID   string    `json:"order_id"`
	Status    string    `json:"status"`
	IssuedAt  time.Time `json:"issued_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

func (b *Budget) GrantWarning(id, order string) (WarningGrant, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.contexts[id] || strings.TrimSpace(order) == "" {
		return WarningGrant{}, fmt.Errorf("warning grant task binding unavailable")
	}
	task := warningTask{id, order}
	if grant, ok := b.warningGrants[task]; ok {
		return b.warningGrantStatus(grant), nil
	}
	if !b.warningSafe() {
		return WarningGrant{}, fmt.Errorf("warning grant blocked: pressure=%s stable_warning_samples=%d swapout_known=%t swapout_rate=%g active_allocations=%d unresolved_residents=%d stale=%t", b.pressure, b.warningStable, b.rateValid, b.rate, b.active, b.unresolvedResidents, b.stale())
	}
	if b.warningGrants == nil {
		b.warningGrants = make(map[warningTask]WarningGrant)
	}
	grant := WarningGrant{ContextID: id, OrderID: order, Status: "available", IssuedAt: b.now(), ExpiresAt: b.now().Add(WarningGrantLifetime)}
	b.warningGrants[task] = grant
	return grant, nil
}

func (b *Budget) WarningGrants() []WarningGrant {
	b.mu.Lock()
	defer b.mu.Unlock()
	grants := make([]WarningGrant, 0, len(b.warningGrants))
	for _, grant := range b.warningGrants {
		grants = append(grants, b.warningGrantStatus(grant))
	}
	sort.Slice(grants, func(i, j int) bool {
		if grants[i].ContextID == grants[j].ContextID {
			return grants[i].OrderID < grants[j].OrderID
		}
		return grants[i].ContextID < grants[j].ContextID
	})
	return grants
}

func (b *Budget) warningGrantStatus(grant WarningGrant) WarningGrant {
	if grant.Status == "available" && !b.now().Before(grant.ExpiresAt) {
		grant.Status = "expired"
	}
	return grant
}

func (b *Budget) warningSafe() bool {
	return !b.stale() && !b.unsupported && b.sampleError == nil && b.rateValid && b.rate == 0 &&
		b.pressure == Warning && b.warningStable >= 3 && b.active == 0 && b.unresolvedResidents == 0
}

func (b *Budget) warningAvailable(id, order string) bool {
	grant, ok := b.warningGrants[warningTask{id, order}]
	return ok && b.warningGrantStatus(grant).Status == "available" && b.warningSafe()
}

func (b *Budget) consumeWarningGrant(id, order string) {
	task := warningTask{id, order}
	if grant, ok := b.warningGrants[task]; ok && b.warningGrantStatus(grant).Status == "available" {
		grant.Status = "consumed"
		b.warningGrants[task] = grant
	}
}
