//go:build darwin

package admission

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
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
	out, err := readVMStat(ctx, exec.CommandContext(ctx, "/usr/bin/vm_stat"))
	if err != nil {
		return Sample{}, err
	}
	swapouts, err := parseVMStat(out)
	if err != nil {
		return Sample{}, err
	}
	rawSwap, err := unix.SysctlRaw("vm.swapusage")
	if err != nil {
		return Sample{}, fmt.Errorf("read swap usage: %w", err)
	}
	used, err := parseSwapUsage(rawSwap)
	if err != nil {
		return Sample{}, err
	}
	sample := Sample{At: time.Now(), Swapouts: swapouts, SwapUsedBytes: used}
	observeDarwinPressure(&sample, func() (uint32, error) {
		return unix.SysctlUint32("kern.memorystatus_vm_pressure_level")
	})
	observeDarwinMemory(&sample, func() (uint64, error) {
		return unix.SysctlUint64("hw.memsize")
	})
	observeDarwinCPU(&sample, func() (uint32, error) {
		return unix.SysctlUint32("hw.logicalcpu")
	}, readDarwinCPUTicks)
	if err := ctx.Err(); err != nil {
		return Sample{}, err
	}
	// Optional CPU readback must never discard an already valid Swap sample.
	observeDarwinCPUInterval(ctx, &sample, readDarwinCPUInterval)
	return sample, nil
}

func observeDarwinPressure(sample *Sample, readLevel func() (uint32, error)) {
	level, err := readLevel()
	if err != nil {
		sample.Pressure = Unknown
		sample.PressureObservationError = fmt.Sprintf("read memory pressure: %v", err)
		return
	}
	sample.Pressure, err = darwinPressure(level)
	if err != nil {
		sample.PressureObservationError = err.Error()
	}
}

func observeDarwinMemory(sample *Sample, readTotal func() (uint64, error)) {
	total, err := readTotal()
	switch {
	case err != nil:
		sample.MemoryObservationError = fmt.Sprintf("read physical memory: %v", err)
	case total == 0:
		sample.MemoryObservationError = "invalid physical memory capacity: 0"
	default:
		sample.TotalMemoryBytes = total
	}
}

func observeDarwinCPU(sample *Sample, readCount func() (uint32, error), readTicks func() (*CPUCounters, error)) {
	count, countErr := readCount()
	if countErr == nil && (count == 0 || uint64(count) > uint64(^uint(0)>>1)) {
		countErr = fmt.Errorf("invalid logical CPU count: %d", count)
	}
	if countErr == nil {
		sample.CPUs = int(count)
	} else {
		sample.CPUs = 0
		countErr = fmt.Errorf("logical CPU count unavailable: %w", countErr)
	}
	counters, ticksErr := readTicks()
	if ticksErr != nil {
		counters = nil
	}
	sample.CPUCounters = counters
	if err := errors.Join(countErr, ticksErr); err != nil {
		sample.CPUObservationError = err.Error()
	} else {
		sample.CPUObservationError = ""
	}
}

func observeDarwinCPUInterval(ctx context.Context, sample *Sample, read func(context.Context) (float64, error)) {
	if sample.CPUCounters != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, SampleTimeout)
	defer cancel()
	percent, err := read(ctx)
	if err != nil {
		sample.CPUObservationError = err.Error()
		return
	}
	sample.CPUBusyPercent = &percent
	sample.CPUObservationError = ""
}

func readDarwinCPUInterval(ctx context.Context) (float64, error) {
	cmd := exec.CommandContext(ctx, "/usr/bin/top", "-l", "2", "-s", "0", "-n", "0", "-R", "-F", "-stats", "pid")
	cmd.Dir = "/"
	cmd.Env = []string{"LC_ALL=C", "LANG=C"}
	cmd.WaitDelay = 100 * time.Millisecond
	output, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("read interval CPU usage: %w", err)
	}
	return parseDarwinCPUInterval(string(output))
}

func parseDarwinCPUInterval(output string) (float64, error) {
	const prefix = "CPU usage:"
	var samples []string
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, prefix) {
			samples = append(samples, strings.TrimPrefix(line, prefix))
		}
	}
	// top's first report has no interval. Only the second reports the delta.
	if len(samples) != 2 {
		return 0, errors.New("expected two native CPU usage reports")
	}
	fields := strings.Fields(samples[1])
	if len(fields) != 6 || fields[1] != "user," || fields[3] != "sys," || fields[5] != "idle" {
		return 0, errors.New("invalid native CPU usage report")
	}
	idleText, ok := strings.CutSuffix(fields[4], "%")
	idle, err := strconv.ParseFloat(idleText, 64)
	if !ok || err != nil || math.IsNaN(idle) || idle < 0 || idle > 100 {
		return 0, errors.New("invalid native idle percentage")
	}
	return 100 - idle, nil
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

// Darwin sys/sysctl.h xsw_usage: total, available, used (uint64),
// page size and encrypted (uint32), all in native byte order.
func parseSwapUsage(raw []byte) (uint64, error) {
	if len(raw) != 32 {
		return 0, fmt.Errorf("invalid vm.swapusage size: %d", len(raw))
	}
	total := binary.NativeEndian.Uint64(raw[0:8])
	available := binary.NativeEndian.Uint64(raw[8:16])
	used := binary.NativeEndian.Uint64(raw[16:24])
	if used > total || available > total || used != total-available {
		return 0, fmt.Errorf("inconsistent vm.swapusage counters")
	}
	return used, nil
}
