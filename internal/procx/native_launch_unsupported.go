//go:build !darwin || !cgo

package procx

import "fmt"

func ObserveNativeImage(original NativeIdentity) (string, error) {
	return "", fmt.Errorf("generation-bound kernel image readback unavailable")
}

func ObserveNativeLaunch(original NativeIdentity) (NativeLaunch, error) {
	return NativeLaunch{}, fmt.Errorf("generation-bound kernel launch readback unavailable")
}
