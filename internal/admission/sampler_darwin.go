//go:build darwin

package admission

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const vmStatOutputLimit = 16 * 1024

type darwinSampler struct{}

func NewSampler() Sampler { return darwinSampler{} }

func (darwinSampler) Read(ctx context.Context) (Sample, error) {
	if err := ctx.Err(); err != nil {
		return Sample{}, err
	}
	memory, err := unix.SysctlUint64("hw.memsize")
	if err != nil {
		return Sample{}, fmt.Errorf("read physical memory: %w", err)
	}
	cpus, err := unix.SysctlUint32("hw.logicalcpu")
	if err != nil {
		return Sample{}, fmt.Errorf("read logical CPUs: %w", err)
	}
	if memory == 0 || cpus == 0 || uint64(cpus) > uint64(^uint(0)>>1) {
		return Sample{}, fmt.Errorf("invalid hardware capacity: memory=%d CPUs=%d", memory, cpus)
	}
	out, err := readVMStat(ctx, exec.CommandContext(ctx, "/usr/bin/vm_stat"))
	if err != nil {
		return Sample{}, err
	}
	swapouts, err := parseVMStat(out)
	if err != nil {
		return Sample{}, err
	}
	level, err := unix.SysctlUint32("kern.memorystatus_vm_pressure_level")
	if err != nil {
		return Sample{}, fmt.Errorf("read memory pressure: %w", err)
	}
	pressure, err := darwinPressure(level)
	if err != nil {
		return Sample{}, err
	}
	if err := ctx.Err(); err != nil {
		return Sample{}, err
	}
	return Sample{At: time.Now(), Pressure: pressure, Swapouts: swapouts, TotalMemoryBytes: memory, CPUs: int(cpus)}, nil
}

func darwinPressure(level uint32) (Pressure, error) {
	// XNU kern_memorystatus_notify.c exports dispatch levels from
	// bsd/sys/event_private.h, not the internal kVMPressure enum.
	switch level {
	case 1:
		return Normal, nil
	case 2:
		return Warning, nil
	case 4:
		return Critical, nil
	default:
		return Unknown, fmt.Errorf("unknown Darwin memory pressure level: %d", level)
	}
}

type vmStatBuffer struct{ buffer bytes.Buffer }

func (b *vmStatBuffer) Write(p []byte) (int, error) {
	if len(p) > vmStatOutputLimit-b.buffer.Len() {
		return 0, fmt.Errorf("vm_stat output exceeds %d bytes", vmStatOutputLimit)
	}
	return b.buffer.Write(p)
}

func readVMStat(ctx context.Context, cmd *exec.Cmd) ([]byte, error) {
	var output vmStatBuffer
	cmd.Dir = "/"
	cmd.Env = []string{"LC_ALL=C", "LANG=C"}
	cmd.Stdout = &output
	cmd.Stderr = &output
	cmd.WaitDelay = 100 * time.Millisecond
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("read vm_stat: %w", ctx.Err())
		}
		return nil, fmt.Errorf("read vm_stat: %w", err)
	}
	return output.buffer.Bytes(), nil
}

var vmStatHeader = regexp.MustCompile(`^Mach Virtual Memory Statistics: \(page size of ([0-9]+) bytes\)$`)

// vm_stat reports cumulative pages. The shared Sample counter is bytes.
func parseVMStat(data []byte) (uint64, error) {
	if len(data) > vmStatOutputLimit {
		return 0, fmt.Errorf("vm_stat output exceeds %d bytes", vmStatOutputLimit)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	header := vmStatHeader.FindStringSubmatch(strings.TrimSpace(lines[0]))
	if header == nil {
		return 0, fmt.Errorf("vm_stat page size header missing or malformed")
	}
	pageSize, err := strconv.ParseUint(header[1], 10, 64)
	if err != nil || pageSize == 0 || pageSize&(pageSize-1) != 0 {
		return 0, fmt.Errorf("vm_stat page size invalid: %q", header[1])
	}
	var pages uint64
	found := false
	for _, line := range lines[1:] {
		name, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok || name != "Swapouts" {
			continue
		}
		if found {
			return 0, fmt.Errorf("vm_stat Swapouts counter duplicated")
		}
		value = strings.TrimSpace(value)
		if !strings.HasSuffix(value, ".") {
			return 0, fmt.Errorf("vm_stat Swapouts counter malformed")
		}
		value = strings.TrimSuffix(value, ".")
		if value == "" || strings.IndexFunc(value, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
			return 0, fmt.Errorf("vm_stat Swapouts counter malformed")
		}
		pages, err = strconv.ParseUint(value, 10, 64)
		if err != nil || pages > math.MaxUint64/pageSize {
			return 0, fmt.Errorf("vm_stat Swapouts byte counter overflows uint64")
		}
		found = true
	}
	if !found {
		return 0, fmt.Errorf("vm_stat Swapouts counter missing")
	}
	return pages * pageSize, nil
}
