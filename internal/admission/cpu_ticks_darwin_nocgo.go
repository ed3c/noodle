//go:build darwin && !cgo

package admission

import "errors"

func readDarwinCPUTicks() (*CPUCounters, error) {
	return nil, errors.New("Mach CPU tick observation unavailable without CGO")
}
