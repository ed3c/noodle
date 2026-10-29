package admission

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Spend the ordinary sample credit so these cases isolate the explicit grant.
func spendOrdinaryCredit(t *testing.T, b *Budget, context string) {
	t.Helper()
	permit, err := b.TryAcquire(context)
	if err != nil {
		t.Fatal(err)
	}
	permit.Release()
}

func warningBudget(t *testing.T) (*Budget, func(Pressure, uint64, error)) {
	t.Helper()
	b, sample := testBudget(4)
	b.Register("selected")
	b.Register("other")
	for range 4 {
		sample(Warning, 100, nil)
	}
	spendOrdinaryCredit(t, b, "other")
	if _, err := b.GrantWarning("selected", "task"); err != nil {
		t.Fatal(err)
	}
	return b, sample
}

func TestWarningGrantOnlyOneSelectedTaskLaunch(t *testing.T) {
	b, _ := warningBudget(t)
	for _, acquire := range []func() (*Permit, error){
		func() (*Permit, error) { return b.TryAcquireTask("other", "task") },
		func() (*Permit, error) { return b.TryAcquireTask("selected", "other-task") },
		func() (*Permit, error) { return b.TryAcquire("selected") },
		func() (*Permit, error) { return b.TryAcquireResident("selected") },
	} {
		if _, err := acquire(); !errors.Is(err, ErrDeferred) {
			t.Fatalf("foreign consumer obtained warning grant: %v", err)
		}
	}
	// The blocked ordinary context at the head of the fair queue cannot consume
	// or indefinitely obstruct an explicitly scoped warning grant.
	p, err := b.TryAcquireTask("selected", "task")
	if err != nil {
		t.Fatal(err)
	}
	if s := b.Snapshot(); s.Active != 1 || s.Limit < 1 || s.Pressure != Warning {
		t.Fatalf("ordinary pressure evidence changed: %+v", s)
	}
	if _, err := p.ReserveReplacement("selected"); !errors.Is(err, ErrDeferred) {
		t.Fatal("warning grant allowed automatic replacement")
	}
	p.Release()
	p.Release()
	grant, err := b.GrantWarning("selected", "task")
	if err != nil || grant.Status != "consumed" {
		t.Fatalf("duplicate grant rearmed: %+v %v", grant, err)
	}
	if _, err := b.TryAcquireTask("selected", "task"); !errors.Is(err, ErrDeferred) {
		t.Fatal("consumed grant launched twice")
	}
}

func TestWarningGrantRetainsPressureAndUnknownGates(t *testing.T) {
	for _, condition := range []string{"critical", "capacity", "stale", "error", "gap", "reset", "resident-unknown", "active", "detached"} {
		t.Run(condition, func(t *testing.T) {
			b, sample := warningBudget(t)
			switch condition {
			case "critical":
				sample(Critical, 100, nil)
			case "capacity":
				now := b.now().Add(PollInterval)
				b.now = func() time.Time { return now }
				b.Observe(Sample{At: now, Pressure: Warning, Swapouts: 100, SwapUsedBytes: DefaultSwapUsedLimit}, nil)
			case "error":
				sample(Warning, 100, errors.New("sample failed"))
			case "reset":
				sample(Warning, 99, nil)
			case "resident-unknown":
				b.SetUnresolvedResidents(1)
			case "detached":
				b.Unregister("selected")
			case "active":
				b.active = 1
			case "stale", "gap":
				now := b.now().Add(MaxSampleAge + time.Second)
				b.now = func() time.Time { return now }
				if condition == "gap" {
					b.Observe(Sample{At: now, Pressure: Warning, Swapouts: 100}, nil)
				}
			}
			if _, err := b.TryAcquireTask("selected", "task"); !errors.Is(err, ErrDeferred) {
				t.Fatalf("%s bypassed ordinary gate: %v", condition, err)
			}
		})
	}
}

func TestWarningGrantExpiryDoesNotRenewAndNormalAdmissionConsumes(t *testing.T) {
	b, _ := warningBudget(t)
	before := b.WarningGrants()[0]
	now := before.ExpiresAt
	b.now = func() time.Time { return now }
	grant, err := b.GrantWarning("selected", "task")
	if err != nil || grant.Status != "expired" || grant.ExpiresAt != before.ExpiresAt {
		t.Fatalf("expired permission renewed: %+v %v", grant, err)
	}
	for i := 3; i >= 0; i-- {
		b.Observe(Sample{At: now.Add(-time.Duration(i) * PollInterval), Pressure: Warning, Swapouts: 100}, nil)
	}
	spendOrdinaryCredit(t, b, "other")
	if _, err := b.TryAcquireTask("selected", "task"); !errors.Is(err, ErrDeferred) {
		t.Fatal("expired grant admitted")
	}

	b, sample := warningBudget(t)
	for range 3 {
		sample(Normal, 100, nil)
	}
	p, err := b.TryAcquireTask("selected", "task")
	if err != nil {
		t.Fatal(err)
	}
	p.Release()
	for range 3 {
		sample(Warning, 100, nil)
	}
	spendOrdinaryCredit(t, b, "selected")
	if _, err := b.TryAcquireTask("selected", "task"); !errors.Is(err, ErrDeferred) {
		t.Fatal("normal launch left a reusable warning grant")
	}
}

func TestWarningGrantConcurrentConsumptionAndStableWindow(t *testing.T) {
	b, _ := warningBudget(t)
	var successes atomic.Int32
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if p, err := b.TryAcquireTask("selected", "task"); err == nil {
				successes.Add(1)
				p.Release()
			}
		}()
	}
	wg.Wait()
	if successes.Load() != 1 {
		t.Fatalf("launch count = %d", successes.Load())
	}
	b, sample := testBudget(4)
	b.Register("selected")
	for range 3 {
		sample(Warning, 100, nil)
		if _, err := b.GrantWarning("selected", "task"); err == nil {
			t.Fatal("grant issued before stable window")
		}
	}
	sample(Warning, 100, nil)
	if _, err := b.GrantWarning("selected", "task"); err != nil {
		t.Fatal(err)
	}
}
