package admission

import (
	"math"
	"math/bits"
)

// CPUCounters contains native aggregate cumulative scheduler ticks.
type CPUCounters struct {
	User, System, Nice, Idle uint64
}

// CPUObservation never controls admission. A nil percentage means unknown.
type CPUObservation struct {
	BusyPercent *float64 `json:"busy_percent"`
	Mode        string   `json:"mode"`
	Reason      string   `json:"reason"`
	Error       string   `json:"error,omitempty"`
}

func cpuObservation(previous, current *Sample) CPUObservation {
	o := CPUObservation{Mode: "observation_only", Reason: "cpu_sample_missing"}
	if current == nil {
		return o
	}
	o.Error = current.CPUObservationError
	if current.Unsupported {
		o.Reason = "cpu_observation_unsupported"
		return o
	}
	if current.CPUBusyPercent != nil {
		percent := *current.CPUBusyPercent
		if math.IsNaN(percent) || percent < 0 || percent > 100 {
			o.Reason = "cpu_interval_percentage_invalid"
			return o
		}
		o.BusyPercent, o.Reason = &percent, ""
		return o
	}
	if current.CPUCounters == nil {
		o.Reason = "cpu_counters_unavailable"
		return o
	}
	if previous == nil {
		o.Reason = "cpu_first_sample"
		return o
	}
	if previous.Unsupported || previous.CPUCounters == nil {
		o.Reason = "cpu_previous_counters_unavailable"
		return o
	}
	if !current.At.After(previous.At) || current.At.Sub(previous.At) > MaxSampleAge {
		o.Reason = "cpu_sample_interval_invalid"
		return o
	}
	old, next := previous.CPUCounters, current.CPUCounters
	before := [...]uint64{old.User, old.System, old.Nice, old.Idle}
	after := [...]uint64{next.User, next.System, next.Nice, next.Idle}
	var busy, total uint64
	for i := range before {
		if after[i] < before[i] {
			o.Reason = "cpu_counter_rollback"
			return o
		}
		delta := after[i] - before[i]
		value, carry := bits.Add64(total, delta, 0)
		if carry != 0 {
			o.Reason = "cpu_counter_delta_overflow"
			return o
		}
		total = value
		if i < 3 {
			busy += delta
		}
	}
	if total == 0 {
		o.Reason = "cpu_zero_tick_delta"
		return o
	}
	percent := 100 * (float64(busy) / float64(total))
	o.BusyPercent, o.Reason = &percent, ""
	return o
}
