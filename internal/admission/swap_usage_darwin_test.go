//go:build darwin

package admission

import (
	"encoding/binary"
	"testing"
)

func TestSwapUsageNativeLayout(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		total, available, used uint64
		size                   int
		ok                     bool
	}{
		{"zero", 0, 0, 0, 32, true}, {"used", 20_000_000_000, 5_000_000_000, 15_000_000_000, 32, true},
		{"short", 0, 0, 0, 24, false}, {"overflow", 10, 0, 11, 32, false}, {"inconsistent", 10, 5, 4, 32, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := make([]byte, tc.size)
			binary.NativeEndian.PutUint64(raw[:8], tc.total)
			binary.NativeEndian.PutUint64(raw[8:16], tc.available)
			binary.NativeEndian.PutUint64(raw[16:24], tc.used)
			got, err := parseSwapUsage(raw)
			if (err == nil) != tc.ok || (tc.ok && got != tc.used) {
				t.Fatalf("used=%d err=%v", got, err)
			}
		})
	}
}
