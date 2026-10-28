package admission

import (
	"errors"
	"testing"
	"time"
)

func TestBudgetFiniteSwapUsedLimit(t *testing.T) {
	// Synthetic boundary values test the policy, not machine calibration.
	for _, tc := range []struct {
		name     string
		pressure Pressure
		rate     uint64
		allowed  bool
	}{
		{"below", Normal, 14_999_999_999, true}, {"equal", Normal, 15_000_000_000, false}, {"above", Normal, 15_000_000_001, false},
		{"warning", Warning, 50, true}, {"warning-at-limit", Warning, DefaultSwapUsedLimit, false}, {"critical", Critical, 50, false}, {"unknown", Unknown, 50, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := NewBudgetWithSwapUsedLimit(2, 15_000_000_000)
			now := time.Now()
			b.now = func() time.Time { return now }
			b.Register("a")
			b.Register("b")
			var swap uint64
			for i := 0; i < 4; i++ {
				now = now.Add(PollInterval)
				swap += 500
				b.Observe(Sample{At: now, Pressure: tc.pressure, Swapouts: swap, SwapUsedBytes: tc.rate}, nil)
			}
			if b.Snapshot().SwapUsedLimit != 15_000_000_000 {
				t.Fatal("configured limit missing")
			}
			p, err := b.TryAcquire("a")
			if !tc.allowed {
				if !errors.Is(err, ErrDeferred) {
					t.Fatal("unsafe admission")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = b.TryAcquire("b"); !errors.Is(err, ErrDeferred) {
				t.Fatal("burst admitted")
			}
			now = now.Add(PollInterval)
			swap += 500
			b.Observe(Sample{At: now, Pressure: Normal, Swapouts: swap, SwapUsedBytes: 15_000_000_001}, nil)
			if b.Snapshot().Active != 1 || b.Snapshot().Limit != 0 {
				t.Fatal("threshold must stop starts, retaining active allocation")
			}
			for i := 0; i < 2; i++ {
				now = now.Add(PollInterval)
				b.Observe(Sample{At: now, Pressure: Normal, Swapouts: swap}, nil)
				if b.Snapshot().Credit != 0 {
					t.Fatal("recovered early")
				}
			}
			now = now.Add(PollInterval)
			b.Observe(Sample{At: now, Pressure: Normal, Swapouts: swap}, nil)
			p.Release()
			q, err := b.TryAcquire("b")
			if err != nil {
				t.Fatal(err)
			}
			q.Release()
			now = now.Add(MaxSampleAge + time.Second)
			if _, err = b.TryAcquire("a"); !errors.Is(err, ErrDeferred) {
				t.Fatal("stale sample admitted")
			}
		})
	}
}
