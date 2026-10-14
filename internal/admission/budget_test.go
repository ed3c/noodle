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

func TestBudgetPressureRecoveryCreditAndCeiling(t *testing.T) {
	b, sample := testBudget(2)
	b.Register("a")
	b.Register("b")
	sample(Normal, 50, nil)
	if b.Snapshot().SwapoutRate != nil {
		t.Fatal("baseline must not claim zero swapout rate")
	}
	for i := 0; i < 2; i++ {
		sample(Normal, 50, nil)
		if _, err := b.TryAcquire("a"); !errors.Is(err, ErrDeferred) {
			t.Fatal("resumed before stable window")
		}
	}
	sample(Normal, 50, nil)
	p, err := b.TryAcquire("a")
	if err != nil {
		t.Fatal(err)
	}
	if b.Snapshot().Limit != 1 {
		t.Fatal("recovery did not begin at one")
	}
	if _, err = b.TryAcquire("b"); !errors.Is(err, ErrDeferred) {
		t.Fatal("sample credit allowed a burst")
	}
	sample(Normal, 50, nil)
	q, err := b.TryAcquire("b")
	if err != nil {
		t.Fatal(err)
	}
	sample(Normal, 50, nil)
	if _, err = b.TryAcquire("a"); !errors.Is(err, ErrDeferred) {
		t.Fatal("exceeded ceiling")
	}
	sample(Warning, 50, nil)
	p.Release()
	p.Release()
	if _, err = b.TryAcquire("a"); !errors.Is(err, ErrDeferred) {
		t.Fatal("warning admitted worker")
	}
	if b.Snapshot().Active != 1 {
		t.Fatal("release was not exactly once")
	}
	q.Release()
}

func TestBudgetUnknownResetGapAndPositiveRateBlock(t *testing.T) {
	for _, kind := range []string{"error", "reset", "gap", "stale", "positive", "critical"} {
		t.Run(kind, func(t *testing.T) {
			b, sample := testBudget(4)
			b.Register("a")
			for i := 0; i < 4; i++ {
				sample(Normal, 100, nil)
			}
			switch kind {
			case "error":
				sample(Normal, 100, errors.New("unavailable"))
			case "reset":
				sample(Normal, 1, nil)
			case "gap":
				now := b.now().Add(MaxSampleAge + time.Second)
				b.now = func() time.Time { return now }
				b.Observe(Sample{At: now, Pressure: Normal, Swapouts: 100}, nil)
			case "stale":
				now := b.now().Add(MaxSampleAge + time.Second)
				b.now = func() time.Time { return now }
			case "positive":
				sample(Normal, 101, nil)
			case "critical":
				sample(Critical, 100, nil)
			}
			if _, err := b.TryAcquire("a"); !errors.Is(err, ErrDeferred) {
				t.Fatalf("%s admitted worker", kind)
			}
			if b.Snapshot().Limit != 0 {
				t.Fatal("blocked admission reports nonzero effective capacity")
			}
			if kind != "positive" && kind != "critical" && b.Snapshot().SwapoutRate != nil {
				t.Fatal("unknown rate represented as observed zero")
			}
		})
	}
}

func TestBudgetConcurrentRequestsAndFairness(t *testing.T) {
	b, sample := testBudget(4)
	for _, id := range []string{"a", "b"} {
		b.Register(id)
	}
	for i := 0; i < 4; i++ {
		sample(Normal, 0, nil)
	}
	var mu sync.Mutex
	var permits []*Permit
	var winner string
	var wg sync.WaitGroup
	for _, id := range []string{"a", "b"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			p, e := b.TryAcquire(id)
			if e == nil {
				mu.Lock()
				permits = append(permits, p)
				winner = id
				mu.Unlock()
			}
		}(id)
	}
	wg.Wait()
	if len(permits) != 1 {
		t.Fatalf("same sample admitted %d", len(permits))
	}
	permits[0].Release()
	sample(Normal, 0, nil)
	if _, err := b.TryAcquire(winner); !errors.Is(err, ErrDeferred) {
		t.Fatal("winner jumped a waiting context")
	}
	loser := "a"
	if winner == "a" {
		loser = "b"
	}
	p, e := b.TryAcquire(loser)
	if e != nil {
		t.Fatal(e)
	}
	p.Release()
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

func TestBudgetHardwareAndUnsupportedAreFinite(t *testing.T) {
	if HardwareLimit(16<<30, 10) != 4 {
		t.Fatal("wrong 16 GiB ceiling")
	}
	b := NewBudget(0)
	b.Register("a")
	b.Observe(Sample{At: time.Now(), Unsupported: true, CPUs: 10}, nil)
	p, e := b.TryAcquire("a")
	if e != nil {
		t.Fatal(e)
	}
	defer p.Release()
	if b.Snapshot().Maximum != 1 || b.Snapshot().Pressure != Unknown || !b.Snapshot().Unsupported || b.Snapshot().SwapoutRate != nil {
		t.Fatal("unsupported platform claimed memory health")
	}
	if _, e = b.TryAcquire("a"); !errors.Is(e, ErrDeferred) {
		t.Fatal("unsupported platform is unbounded")
	}
}

func TestBudgetSnapshotPreservesSampleFailureDiagnosis(t *testing.T) {
	b := NewBudget(1)
	if s := b.Snapshot(); s.Reason != "awaiting_sample" || s.Limit != 0 || s.SampleError != nil {
		t.Fatalf("initial state lost: %+v", s)
	}
	b.Observe(Sample{}, errors.New("vm_stat timed out"))
	if s := b.Snapshot(); s.Reason != "sample_failed" || s.SampleError == nil || *s.SampleError != "vm_stat timed out" || s.Limit != 0 || s.SwapoutRate != nil {
		t.Fatalf("sampler failure hidden: %+v", s)
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
