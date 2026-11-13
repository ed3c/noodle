//go:build !darwin || !cgo

package procx

import "fmt"

func ObserveNativeLaunch(original NativeIdentity) (NativeLaunch, error) {
	return NativeLaunch{}, fmt.Errorf("generation-bound kernel launch readback unavailable")
}
