//go:build !windows

package dispatcher

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func nativeSocketIdentity(path string) (string, error) {
	var stat unix.Stat_t
	if err := unix.Lstat(path, &stat); err != nil {
		return "", err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFSOCK {
		return "", fmt.Errorf("original native endpoint is not a Unix socket")
	}
	return fmt.Sprintf("%d:%d", stat.Dev, stat.Ino), nil
}
