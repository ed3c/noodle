//go:build darwin

package admission

import (
	"errors"
	"fmt"
	"math"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

func nativeMachineIdentity() (string, error) {
	id, err := unix.Sysctl("kern.hostuuid")
	if err != nil { return "", fmt.Errorf("machine identity unavailable: %w", err) }
	return capacityIdentity(strings.TrimSpace(id), os.Getuid())
}

func capacityFileIdentity(path string, directory bool) error {
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil { return err }
	kind := uint16(unix.S_IFREG)
	if directory { kind = unix.S_IFDIR }
	if st.Mode & unix.S_IFMT != kind || st.Uid != uint32(os.Getuid()) || st.Mode & 0022 != 0 || (!directory && st.Nlink != 1) {
		return errors.New("machine capacity file is foreign, aliased or writable by another user")
	}
	return nil
}

func nativeDiskCapacity(path string) (diskCapacity, error) {
	path, err := capacityProbePath(path)
	if err != nil { return diskCapacity{}, err }
	var st unix.Stat_t
	var fs unix.Statfs_t
	if err = unix.Stat(path, &st); err != nil { return diskCapacity{}, err }
	if err = unix.Statfs(path, &fs); err != nil { return diskCapacity{}, err }
	if fs.Bsize == 0 || fs.Bavail > math.MaxUint64/uint64(fs.Bsize) { return diskCapacity{}, errors.New("disk available capacity invalid") }
	return diskCapacity{Volume: fmt.Sprintf("dev:%d", st.Dev), Available: fs.Bavail*uint64(fs.Bsize)}, nil
}
