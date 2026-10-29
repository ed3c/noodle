//go:build windows

package dispatcher

import "fmt"

func nativeSocketIdentity(string) (string, error) {
	return "", fmt.Errorf("native Unix app-server control is unavailable on Windows")
}
