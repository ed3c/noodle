//go:build darwin

package admission

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
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

func TestSamplerReadLive(t *testing.T) {
	if os.Getenv("NOODLE_TEST_LIVE_SAMPLER") != "1" {
		t.Skip("one read-only native sample requires explicit opt-in")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	sample, err := NewSampler().Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sample.Unsupported || sample.At.IsZero() || sample.TotalMemoryBytes == 0 || sample.CPUs < 1 {
		t.Fatalf("invalid native sample: %+v", sample)
	}
	data, err := json.Marshal(sample)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("native_sample=%s", data)
}
