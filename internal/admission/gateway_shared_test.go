package admission

import (
	"errors"
	"testing"
)

func TestGatewayWarningCannotBypassSharedGenerationOrUnknownReturns(t *testing.T) {
	for _, condition := range []string{"foreign-generation", "pending-return", "switching", "swap-limit"} {
		t.Run(condition, func(t *testing.T) {
			root := t.TempDir()
			b, _ := sharedTestBudget(t, root, "selected", 4)
			other, _ := sharedTestBudget(t, root, "other", 4)
			switch condition {
			case "foreign-generation":
				permit, err := other.TryAcquire("other")
				if err != nil {
					t.Fatal(err)
				}
				defer permit.Release()
			case "pending-return":
				b.pendingSharedReturns = map[string]bool{"unknown": true}
			case "switching":
				b.HoldForSwitch()
			case "swap-limit":
				b.previous.SwapUsedBytes = b.swapUsedLimit
			}
			if _, err := b.GrantGatewayWarning("selected", "task", "gateway", 0); err == nil {
				t.Fatal("unsafe gateway grant admitted")
			}
			b.credit = 0
			if _, err := b.TryAcquireScopedResident("selected", "gateway"); !errors.Is(err, ErrDeferred) {
				t.Fatalf("unsafe launch: %v", err)
			}
			if len(b.GatewayWarningGrants()) != 0 {
				t.Fatal("refusal minted permission")
			}
		})
	}
}

func TestRollingGatewayUsesOrdinaryQuotaWithoutMintingWarningGrant(t *testing.T) {
	b, now := sharedTestBudget(t, t.TempDir(), "selected", 4)
	if _, err := b.GrantGatewayWarning("selected", "task", "gateway", 0); err == nil || len(b.GatewayWarningGrants()) != 0 {
		t.Fatal("shared owner minted unsupported permission")
	}
	resident, err := b.TryAcquireScopedResident("selected", "gateway")
	if err != nil {
		t.Fatal(err)
	}
	defer resident.Release()
	if _, err = b.TryAcquireTask("selected", "task"); !errors.Is(err, ErrDeferred) {
		t.Fatal("gateway plus worker bypassed shared start window")
	}
	*now = now.Add(PollInterval)
	b.Observe(Sample{At: *now, Pressure: Warning}, nil)
	worker, err := b.TryAcquireTask("selected", "task")
	if err != nil {
		t.Fatal(err)
	}
	worker.Release()
	if got := b.Snapshot(); got.Allocations != 1 || got.Residents != 1 || len(b.GatewayWarningGrants()) != 0 {
		t.Fatalf("ordinary gateway custody changed: %+v", got)
	}
}
