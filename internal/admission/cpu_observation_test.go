package admission

import (
	"errors"
	"math"
	"testing"
	"time"
)

func TestCPUObservationUsesTickDeltaAndKeepsUnknownSeparateFromZero(t *testing.T) {
	at := time.Unix(9000, 0)
	base := Sample{At: at, CPUCounters: &CPUCounters{User: 100, System: 100, Nice: 100, Idle: 100}}
	for _, tc := range []struct {
		name     string
		previous *Sample
		current  Sample
		want     *float64
		reason   string
	}{
		{"busy", &base, Sample{At: at.Add(PollInterval), CPUCounters: &CPUCounters{User: 110, System: 110, Nice: 110, Idle: 170}}, floatPointer(30), ""},
		{"zero-busy", &base, Sample{At: at.Add(PollInterval), CPUCounters: &CPUCounters{User: 100, System: 100, Nice: 100, Idle: 200}}, floatPointer(0), ""},
		{"all-busy", &base, Sample{At: at.Add(PollInterval), CPUCounters: &CPUCounters{User: 150, System: 120, Nice: 130, Idle: 100}}, floatPointer(100), ""},
		{"first", nil, base, nil, "cpu_first_sample"},
		{"missing", &base, Sample{At: at.Add(PollInterval)}, nil, "cpu_counters_unavailable"},
		{"rollback", &base, Sample{At: at.Add(PollInterval), CPUCounters: &CPUCounters{User: 99, System: 110, Nice: 110, Idle: 170}}, nil, "cpu_counter_rollback"},
		{"zero-delta", &base, Sample{At: at.Add(PollInterval), CPUCounters: base.CPUCounters}, nil, "cpu_zero_tick_delta"},
		{"gap", &base, Sample{At: at.Add(MaxSampleAge + time.Second), CPUCounters: base.CPUCounters}, nil, "cpu_sample_interval_invalid"},
		{"backward-time", &base, Sample{At: at.Add(-time.Second), CPUCounters: base.CPUCounters}, nil, "cpu_sample_interval_invalid"},
		{"overflow", &Sample{At: at, CPUCounters: &CPUCounters{}}, Sample{At: at.Add(PollInterval), CPUCounters: &CPUCounters{User: math.MaxUint64, Idle: 1}}, nil, "cpu_counter_delta_overflow"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := cpuObservation(tc.previous, &tc.current)
			if got.Mode != "observation_only" || got.Reason != tc.reason || (got.BusyPercent == nil) != (tc.want == nil) {
				t.Fatal(got)
			}
			if tc.want != nil && *got.BusyPercent != *tc.want {
				t.Fatal(*got.BusyPercent, *tc.want)
			}
		})
	}
}
func floatPointer(v float64) *float64 { return &v }

func TestBudgetCPUObservationIsIndependentOfCPUCountAndSampleFreshness(t *testing.T) {
	b := NewBudget(9)
	now := time.Unix(9000, 0)
	b.now = func() time.Time { return now }
	b.Observe(Sample{At: now, Pressure: Normal, CPUCounters: &CPUCounters{Idle: 100}}, nil)
	if got := b.Snapshot().CPUObservation; got.BusyPercent != nil || got.Reason != "cpu_first_sample" {
		t.Fatal(got)
	}
	now = now.Add(PollInterval)
	b.Observe(Sample{At: now, Pressure: Normal, CPUs: 0, CPUObservationError: "logical CPU count unavailable", CPUCounters: &CPUCounters{Idle: 200}}, nil)
	if got := b.Snapshot().CPUObservation; got.BusyPercent == nil || *got.BusyPercent != 0 || got.Error == "" {
		t.Fatal(got)
	}
	now = now.Add(MaxSampleAge + time.Second)
	if got := b.Snapshot().CPUObservation; got.BusyPercent != nil || got.Reason != "cpu_sample_stale" {
		t.Fatal(got)
	}
	b.Observe(Sample{}, errors.New("sampler failed"))
	if got := b.Snapshot().CPUObservation; got.BusyPercent != nil || got.Error != "sampler failed" {
		t.Fatal(got)
	}
}
