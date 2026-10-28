//go:build !darwin || !cgo

package procx

import "fmt"

func NativeFixtureControlAvailable() bool { return false }

func ObserveNativeIdentity(pid int) (NativeIdentity, error) {
	return NativeIdentity{}, fmt.Errorf("generation-bound fixture recovery requires supported native platform access")
}

func SignalNativeIdentity(p NativeIdentity, force bool) error {
	return fmt.Errorf("generation-bound fixture recovery unavailable; PID signal fallback forbidden")
}
