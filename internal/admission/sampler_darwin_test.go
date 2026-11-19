//go:build darwin

package admission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseVMStatBytes(t *testing.T) {
	for _, pageSize := range []uint64{4096, 16384, 65536} {
		for _, pages := range []uint64{0, 31, 1 << 40, (1 << 63) / pageSize} {
			input := fmt.Sprintf("Mach Virtual Memory Statistics: (page size of %d bytes)\nSwapins: 999.\nSwapouts: %d.\n", pageSize, pages)
			got, err := parseVMStat([]byte(input))
			if err != nil || got != pages*pageSize {
				t.Fatalf("pages=%d size=%d: got %d, %v", pages, pageSize, got, err)
			}
		}
	}
}

func TestParseVMStatRejectsInvalid(t *testing.T) {
	header := "Mach Virtual Memory Statistics: (page size of 16384 bytes)\n"
	for name, input := range map[string]string{
		"empty": "", "missing counter": header + "Swapins: 1.\n",
		"missing header": "Swapouts: 1.", "zero page": strings.Replace(header, "16384", "0", 1) + "Swapouts: 1.",
		"invalid page": strings.Replace(header, "16384", "123", 1) + "Swapouts: 1.",
		"negative":     header + "Swapouts: -1.", "signed": header + "Swapouts: +1.",
		"fraction": header + "Swapouts: 1.2.", "missing dot": header + "Swapouts: 1",
		"overflow":         header + "Swapouts: 18446744073709551615.",
		"counter overflow": header + "Swapouts: 18446744073709551616.",
		"page overflow":    strings.Replace(header, "16384", "18446744073709551616", 1) + "Swapouts: 1.",
		"duplicate":        header + "Swapouts: 1.\nSwapouts: 2.",
		"oversized":        header + "Swapouts: 1.\n" + strings.Repeat(" ", vmStatOutputLimit),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseVMStat([]byte(input)); err == nil {
				t.Fatal("invalid sample accepted")
			}
		})
	}
}

func TestDarwinPressureLevels(t *testing.T) {
	for level, want := range map[uint32]Pressure{1: Normal, 2: Warning, 4: Critical} {
		if got, err := darwinPressure(level); err != nil || got != want {
			t.Fatalf("%d: %s, %v", level, got, err)
		}
	}
	for _, level := range []uint32{0, 3, 5, 0xffffffff} {
		if got, err := darwinPressure(level); err == nil || got != Unknown {
			t.Fatalf("unknown %d accepted: %s, %v", level, got, err)
		}
	}
}

func TestReadVMStatFailureBoundaries(t *testing.T) {
	t.Run("deadline", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		_, err := readVMStat(ctx, exec.CommandContext(ctx, "/bin/sleep", "5"))
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("exit error", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if _, err := readVMStat(ctx, exec.CommandContext(ctx, "/usr/bin/false")); err == nil {
			t.Fatal("failed command accepted")
		}
	})
	t.Run("oversized output", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, err := readVMStat(ctx, exec.CommandContext(ctx, "/usr/bin/printf", "%s", strings.Repeat("x", vmStatOutputLimit+1)))
		if err == nil || !strings.Contains(err.Error(), "exceeds") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("fixed environment", func(t *testing.T) {
		t.Setenv("GH_TOKEN", "not-in-sampler-child")
		t.Setenv("LC_ALL", "invalid-locale")
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		out, err := readVMStat(ctx, exec.CommandContext(ctx, "/usr/bin/env"))
		if err != nil || string(out) != "LC_ALL=C\nLANG=C\n" {
			t.Fatalf("unexpected child environment: %q, %v", out, err)
		}
	})
}

func TestSamplerReadCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NewSampler().Read(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestDarwinPressureObservationPreservesCPUAndSwap(t *testing.T) {
	for _, tc := range []struct {
		name       string
		level      uint32
		err        error
		pressure   Pressure
		diagnostic string
	}{
		{name: "critical", level: 4, pressure: Critical},
		{name: "unknown level", level: 3, pressure: Unknown, diagnostic: "unknown Darwin memory pressure level: 3"},
		{name: "unavailable", err: errors.New("sysctl failed"), pressure: Unknown, diagnostic: "read memory pressure: sysctl failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sample := Sample{At: time.Unix(123, 0), Swapouts: 31, SwapUsedBytes: 1024, CPUs: 8, CPUCounters: &CPUCounters{User: 10, Idle: 20}}
			want := sample
			want.Pressure, want.PressureObservationError = tc.pressure, tc.diagnostic
			observeDarwinPressure(&sample, func() (uint32, error) { return tc.level, tc.err })
			if !reflect.DeepEqual(sample, want) {
				t.Fatalf("pressure observation changed unrelated data: got %+v, want %+v", sample, want)
			}
		})
	}
}

func TestDarwinMemoryObservationPreservesPressureAndSwap(t *testing.T) {
	for _, tc := range []struct {
		name       string
		total      uint64
		err        error
		wantTotal  uint64
		diagnostic string
	}{
		{name: "available", total: 16 << 30, wantTotal: 16 << 30},
		{name: "zero", diagnostic: "invalid physical memory capacity: 0"},
		{name: "unavailable", err: errors.New("sysctl failed"), diagnostic: "read physical memory: sysctl failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sample := Sample{At: time.Unix(123, 0), Pressure: Warning, Swapouts: 31, SwapUsedBytes: 1024}
			want := sample
			want.TotalMemoryBytes, want.MemoryObservationError = tc.wantTotal, tc.diagnostic
			observeDarwinMemory(&sample, func() (uint64, error) { return tc.total, tc.err })
			if !reflect.DeepEqual(sample, want) {
				t.Fatalf("memory observation changed unrelated data: got %+v, want %+v", sample, want)
			}
		})
	}
}

func TestDarwinCPUObservationPreservesMemoryAndSwap(t *testing.T) {
	ticks := &CPUCounters{User: 10, System: 20, Nice: 3, Idle: 40}
	for _, tc := range []struct {
		name               string
		count              uint32
		countErr, ticksErr error
		wantCount          int
		wantTicks          *CPUCounters
		wantDiagnostics    []string
	}{
		{name: "available", count: 8, wantCount: 8, wantTicks: ticks},
		{name: "count unavailable", countErr: errors.New("sysctl failed"), wantTicks: ticks, wantDiagnostics: []string{"logical CPU count unavailable", "sysctl failed"}},
		{name: "count zero", wantTicks: ticks, wantDiagnostics: []string{"invalid logical CPU count: 0"}},
		{name: "ticks unavailable", count: 8, ticksErr: errors.New("Mach read failed"), wantCount: 8, wantDiagnostics: []string{"Mach read failed"}},
		{name: "both unavailable", countErr: errors.New("sysctl failed"), ticksErr: errors.New("Mach read failed"), wantDiagnostics: []string{"sysctl failed", "Mach read failed"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			original := Sample{At: time.Unix(123, 0), Pressure: Normal, Swapouts: 31, SwapUsedBytes: 1024, TotalMemoryBytes: 16 << 30}
			sample := original
			observeDarwinCPU(&sample, func() (uint32, error) { return tc.count, tc.countErr }, func() (*CPUCounters, error) { return ticks, tc.ticksErr })
			if sample.CPUs != tc.wantCount || !reflect.DeepEqual(sample.CPUCounters, tc.wantTicks) {
				t.Fatalf("CPU observation differs: %+v", sample)
			}
			for _, diagnostic := range tc.wantDiagnostics {
				if !strings.Contains(sample.CPUObservationError, diagnostic) {
					t.Fatalf("missing CPU diagnostic %q: %q", diagnostic, sample.CPUObservationError)
				}
			}
			if len(tc.wantDiagnostics) == 0 && sample.CPUObservationError != "" {
				t.Fatalf("valid CPU observation has error: %q", sample.CPUObservationError)
			}
			sample.CPUs, sample.CPUCounters, sample.CPUObservationError = 0, nil, ""
			if !reflect.DeepEqual(sample, original) {
				t.Fatalf("CPU failure changed original memory/swap observation: %+v", sample)
			}
		})
	}
}

func TestParseDarwinCPUInterval(t *testing.T) {
	const initial = "CPU usage: 5% user, 5% sys, 90% idle\n"
	for _, tc := range []struct {
		name, second string
		want         float64
		invalid      bool
	}{
		{name: "second report only", second: "CPU usage: 50% user, 20% sys, 30% idle", want: 70},
		{name: "zero is observed", second: "CPU usage: 0% user, 0% sys, 100% idle"},
		{name: "all busy", second: "CPU usage: 70% user, 30% sys, 0% idle", want: 100},
		{name: "no interval", invalid: true},
		{name: "unrecognized report", second: "CPU usage: 0% user, 0% sys, 50 idle", invalid: true},
		{name: "nonfinite interval", second: "CPU usage: 0% user, 0% sys, NaN% idle", invalid: true},
		{name: "out of range", second: "CPU usage: 0% user, 0% sys, 101% idle", invalid: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDarwinCPUInterval(initial + tc.second)
			if (err != nil) != tc.invalid || got != tc.want {
				t.Fatalf("readback=%v, error=%v, want %v", got, err, tc.want)
			}
		})
	}
}

func TestDarwinCPUIntervalFallbackClearlySeparatesSwap(t *testing.T) {
	original := Sample{At: time.Now(), Pressure: Critical, Swapouts: 42, SwapUsedBytes: 30, CPUObservationError: "ticks unavailable"}
	sample := original
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	observeDarwinCPUInterval(ctx, &sample, func(ctx context.Context) (float64, error) {
		return 0, ctx.Err()
	})
	if sample.CPUBusyPercent != nil || sample.CPUObservationError != "context canceled" {
		t.Fatalf("cancelled CPU interval became an observation: %+v", sample)
	}
	sample.CPUObservationError = original.CPUObservationError
	if !reflect.DeepEqual(sample, original) {
		t.Fatalf("CPU timeout discarded valid Swap or pressure: %+v", sample)
	}
	observeDarwinCPUInterval(context.Background(), &sample, func(ctx context.Context) (float64, error) {
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > SampleTimeout {
			t.Fatal("optional CPU command has no bounded deadline")
		}
		return 12.5, nil
	})
	observation := cpuObservation(nil, &sample)
	if observation.BusyPercent == nil || *observation.BusyPercent != 12.5 || sample.CPUCounters != nil || observation.Error != "" {
		t.Fatalf("actual interval was lost or converted to fake counters: %+v", observation)
	}
}

func TestSamplerReadLive(t *testing.T) {
	if os.Getenv("NOODLE_TEST_LIVE_SAMPLER") != "1" {
		t.Skip("two read-only native samples require explicit opt-in")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var previous *Sample
	observed := false
	for i := 0; i < 2; i++ {
		if i != 0 {
			select {
			case <-time.After(250 * time.Millisecond):
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
		}
		sample, err := NewSampler().Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if sample.Unsupported || sample.At.IsZero() || sample.TotalMemoryBytes == 0 || sample.CPUs < 0 {
			t.Fatalf("invalid native sample: %+v", sample)
		}
		if sample.CPUCounters == nil && sample.CPUBusyPercent == nil && sample.CPUObservationError == "" {
			t.Fatalf("missing CPU sources have no unknown diagnostic: %+v", sample)
		}
		data, err := json.Marshal(sample)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("native_sample_%d=%s", i+1, data)
		observation := cpuObservation(previous, &sample)
		if observation.BusyPercent != nil {
			observed = true
			t.Logf("native_busy_percent=%.2f", *observation.BusyPercent)
		}
		previous = &sample
	}
	if !observed {
		t.Fatal("neither native sampling path observed an interval CPU percentage")
	}
}
