package admission

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func testBudget(max int) (*Budget, func(Pressure, uint64, error)) {
	b := NewBudget(max)
	now := time.Now()
	b.now = func() time.Time { return now }
	return b, func(pressure Pressure, swap uint64, err error) {
		now = now.Add(PollInterval)
		b.Observe(Sample{At: now, Pressure: pressure, Swapouts: swap, CPUs: 10, TotalMemoryBytes: 16 << 30}, err)
	}
}

func TestBudgetObservationsNeverReduceWorkerCapacity(t *testing.T) {
	for _, condition := range []string{"missing", "normal", "critical", "swap", "failed", "unsupported", "stale", "reset"} {
		t.Run(condition, func(t *testing.T) {
			b, sample := testBudget(0)
			b.Register("worker")
			switch condition {
			case "normal":
				sample(Normal, 10, nil)
			case "critical":
				sample(Critical, 10, nil)
			case "swap":
				b.Observe(Sample{At: b.now(), Pressure: Warning, SwapUsedBytes: 99_000_000_000, CPUs: 1, TotalMemoryBytes: 1}, nil)
			case "failed":
				sample(Normal, 10, errors.New("monitor unavailable"))
			case "unsupported":
				b.Observe(Sample{At: b.now(), Unsupported: true}, nil)
			case "stale":
				now := b.now().Add(-MaxSampleAge - time.Second)
				b.Observe(Sample{At: now, Pressure: Normal}, nil)
			case "reset":
				sample(Normal, 20, nil)
				sample(Normal, 1, nil)
			}
			permits := make([]*Permit, 0, DefaultWorkerLimit)
			for range DefaultWorkerLimit {
				permit, err := b.TryAcquire("worker")
				if err != nil {
					t.Fatalf("observation blocked worker %d: %v", len(permits)+1, err)
				}
				permits = append(permits, permit)
			}
			if _, err := b.TryAcquire("worker"); !errors.Is(err, ErrDeferred) {
				t.Fatal("tenth worker exceeded its Host ceiling")
			}
			if got := b.Snapshot(); got.Maximum != 9 || got.Limit != 9 || got.Active != 9 || got.Reason != "worker_limit" {
				t.Fatalf("Host capacity changed: %+v", got)
			}
			for _, permit := range permits {
				permit.Release()
				permit.Release()
			}
			if got := b.Snapshot(); got.Active != 0 || got.Limit != 9 {
				t.Fatalf("duplicate release changed capacity: %+v", got)
			}
		})
	}
}

func TestBudgetConcurrentRequestsUseOnlyOneHostCeiling(t *testing.T) {
	b := NewBudget(0)
	b.Register("worker")
	var mu sync.Mutex
	var permits []*Permit
	var wg sync.WaitGroup
	for range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			permit, err := b.TryAcquire("worker")
			if err == nil {
				mu.Lock()
				permits = append(permits, permit)
				mu.Unlock()
			} else if !errors.Is(err, ErrDeferred) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if len(permits) != 9 {
		t.Fatalf("concurrent requests admitted %d workers", len(permits))
	}
	for _, permit := range permits {
		permit.Release()
	}
	if _, err := b.TryAcquire("foreign"); !errors.Is(err, ErrDeferred) {
		t.Fatal("unregistered context acquired a worker")
	}
}

func TestBudgetReplacementKeepsSlotUntilBothOwnersJoin(t *testing.T) {
	b, sample := testBudget(1)
	b.Register("a")
	b.Register("b")
	for i := 0; i < 4; i++ {
		sample(Normal, 0, nil)
	}
	p, e := b.TryAcquire("a")
	if e != nil {
		t.Fatal(e)
	}
	sample(Normal, 0, nil)
	q, e := p.ReserveReplacement("a")
	if e != nil {
		t.Fatal(e)
	}
	sample(Normal, 0, nil)
	if _, err := p.ReserveReplacement("a"); !errors.Is(err, ErrDeferred) {
		t.Fatal("same worker reserved multiple replacements")
	}
	p.Release()
	if b.Snapshot().Active != 1 {
		t.Fatal("old Done prematurely released replacement slot")
	}
	sample(Normal, 0, nil)
	if _, e = b.TryAcquire("b"); !errors.Is(e, ErrDeferred) {
		t.Fatal("replacement slot stolen")
	}
	q.Release()
	if b.Snapshot().Active != 0 {
		t.Fatal("joined replacement leaked slot")
	}
}

func TestBudgetSnapshotPreservesSampleFailureWithoutBlocking(t *testing.T) {
	b := NewBudget(1)
	if s := b.Snapshot(); s.Reason != "ready" || s.Limit != 1 || s.SampleError != nil || s.Pressure != Unknown {
		t.Fatalf("initial state lost: %+v", s)
	}
	b.Observe(Sample{}, errors.New("vm_stat timed out"))
	if s := b.Snapshot(); s.Reason != "ready" || s.SampleError == nil || *s.SampleError != "vm_stat timed out" || s.Limit != 1 || s.SwapoutRate != nil || s.Pressure != Unknown {
		t.Fatalf("sampler failure hidden or used as admission policy: %+v", s)
	}
}

func TestBudgetCancelledReplacementCanRetryWithoutSharingTwoClaims(t *testing.T) {
	b, sample := testBudget(1)
	b.Register("a")
	for i := 0; i < 4; i++ {
		sample(Normal, 0, nil)
	}
	p, err := b.TryAcquire("a")
	if err != nil {
		t.Fatal(err)
	}
	defer p.Release()
	sample(Normal, 0, nil)
	q, err := p.ReserveReplacement("a")
	if err != nil {
		t.Fatal(err)
	}
	q.Release()
	sample(Normal, 0, nil)
	r, err := p.ReserveReplacement("a")
	if err != nil {
		t.Fatal("cancelled replacement permanently blocked original worker", err)
	}
	r.Release()
	if b.Snapshot().Active != 1 {
		t.Fatal("cancelled replacement freed original worker")
	}
}
