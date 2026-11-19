//go:build darwin && !cgo

package admission

import (
	"strings"
	"testing"
)

func TestDarwinCPUTicksUnavailableWithoutCGO(t *testing.T) {
	counters, err := readDarwinCPUTicks()
	if counters != nil || err == nil || !strings.Contains(err.Error(), "without CGO") {
		t.Fatalf("missing native CPU source did not remain unknown: %+v, %v", counters, err)
	}
}
