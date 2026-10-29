package admission

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func gatewayWarningBudget(t *testing.T) (*Budget, func(Pressure, uint64, error)) {
	t.Helper()
	budget, sample := testBudget(4)
	budget.Register("selected")
	budget.Register("other")
	for range 4 {
		sample(Warning, 100, nil)
	}
	grant, err := budget.GrantGatewayWarning("selected", "task", "gateway", 0)
	if err != nil || grant.Revision != 1 || grant.Status != "available" {
		t.Fatalf("grant: %+v %v", grant, err)
	}
	// Exercise the grant between ordinary start windows under policy v2.
	budget.credit = 0
	return budget, sample
}

func TestGatewayWarningAllowsExactResidentThenOneWorker(t *testing.T) {
	budget, _ := gatewayWarningBudget(t)
	for _, acquire := range []func() (*Permit, error){
		func() (*Permit, error) { return budget.TryAcquireTask("selected", "task") },
		func() (*Permit, error) { return budget.TryAcquireResident("selected") },
		func() (*Permit, error) { return budget.TryAcquireScopedResident("selected", "foreign") },
		func() (*Permit, error) { return budget.TryAcquireScopedResident("other", "gateway") },
	} {
		if _, err := acquire(); !errors.Is(err, ErrDeferred) {
			t.Fatalf("foreign or premature acquire: %v", err)
		}
	}
	resident, err := budget.TryAcquireScopedResident("selected", "gateway")
	if err != nil {
		t.Fatal(err)
	}
	defer resident.Release()
	if _, err := budget.TryAcquireTask("other", "task"); !errors.Is(err, ErrDeferred) {
		t.Fatal("foreign worker admitted")
	}
	worker, err := budget.TryAcquireTask("selected", "task")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot := budget.Snapshot(); snapshot.Active != 1 || snapshot.Residents != 1 || snapshot.Allocations != 2 || snapshot.Limit == 0 || snapshot.AdmissionPolicy != WorkerAdmissionPolicy || snapshot.Pressure != Warning {
		t.Fatalf("pressure or counts changed: %+v", snapshot)
	}
	if _, err := worker.ReserveReplacement("selected"); !errors.Is(err, ErrDeferred) {
		t.Fatal("automatic replacement admitted")
	}
	worker.Release()
	grant, err := budget.GrantGatewayWarning("selected", "task", "gateway", 0)
	if err != nil || grant.Status != "consumed" || grant.Revision != 1 {
		t.Fatalf("duplicate issuance renewed grant: %+v %v", grant, err)
	}
	if _, err := budget.TryAcquireTask("selected", "task"); !errors.Is(err, ErrDeferred) {
		t.Fatal("consumed grant reused")
	}
	grant, err = budget.GrantGatewayWarning("selected", "task", "gateway", 1)
	if err != nil || grant.Revision != 2 || !grant.GatewayStarted {
		t.Fatalf("explicit next permission: %+v %v", grant, err)
	}
	worker, err = budget.TryAcquireTask("selected", "task")
	if err != nil {
		t.Fatal(err)
	}
	worker.Release()
}

func TestGatewayWarningRetainsUnsafePressureAndOwnershipGates(t *testing.T) {
	for _, condition := range []string{"critical", "swap", "stale", "sample-error", "unresolved", "foreign-resident", "worker-active", "revoked", "expired"} {
		t.Run(condition, func(t *testing.T) {
			budget, sample := gatewayWarningBudget(t)
			resident, err := budget.TryAcquireScopedResident("selected", "gateway")
			if err != nil {
				t.Fatal(err)
			}
			defer resident.Release()
			switch condition {
			case "critical":
				sample(Critical, 100, nil)
			case "swap":
				sample(Warning, 101, nil)
			case "sample-error":
				sample(Warning, 100, errors.New("sample failed"))
			case "stale", "expired":
				now := budget.now().Add(MaxSampleAge + time.Second)
				if condition == "expired" {
					now = budget.GatewayWarningGrants()[0].ExpiresAt
				}
				budget.now = func() time.Time { return now }
				if condition == "expired" {
					for offset := 3; offset >= 0; offset-- {
						budget.Observe(Sample{At: now.Add(-time.Duration(offset) * PollInterval), Pressure: Warning, Swapouts: 100}, nil)
					}
				}
			case "unresolved":
				budget.SetUnresolvedResidents(1)
			case "foreign-resident":
				delete(budget.residentScopes, "gateway")
				budget.residentScopes["foreign"] = 1
			case "worker-active":
				budget.active++
			case "revoked":
				budget.Unregister("selected")
			}
			budget.credit = 0
			if _, err := budget.TryAcquireTask("selected", "task"); !errors.Is(err, ErrDeferred) {
				t.Fatalf("unsafe worker admitted: %v", err)
			}
		})
	}
}

func TestGatewayWarningConcurrentWorkerConsumption(t *testing.T) {
	budget, _ := gatewayWarningBudget(t)
	resident, err := budget.TryAcquireScopedResident("selected", "gateway")
	if err != nil {
		t.Fatal(err)
	}
	defer resident.Release()
	var launches atomic.Int32
	var workers sync.WaitGroup
	for range 20 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if permit, err := budget.TryAcquireTask("selected", "task"); err == nil {
				launches.Add(1)
				permit.Release()
			}
		}()
	}
	workers.Wait()
	if launches.Load() != 1 {
		t.Fatalf("worker launches: %d", launches.Load())
	}
}

func TestGatewayWarningNormalLaunchAlsoConsumesPermission(t *testing.T) {
	budget, sample := gatewayWarningBudget(t)
	for range 4 {
		sample(Normal, 100, nil)
	}
	resident, err := budget.TryAcquireScopedResident("selected", "gateway")
	if err != nil {
		t.Fatal(err)
	}
	defer resident.Release()
	sample(Normal, 100, nil)
	worker, err := budget.TryAcquireTask("selected", "task")
	if err != nil {
		t.Fatal(err)
	}
	worker.Release()
	for range 4 {
		sample(Warning, 100, nil)
	}
	if grant := budget.GatewayWarningGrants()[0]; grant.Status != "consumed" {
		t.Fatalf("normal launch did not consume permission: %+v", grant)
	}
	budget.credit = 0
	if _, err := budget.TryAcquireTask("selected", "task"); !errors.Is(err, ErrDeferred) {
		t.Fatal("normal launch retained reusable warning permission")
	}
}
