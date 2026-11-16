//go:build darwin && cgo

package admission

/*
#include <unistd.h>
#include <sys/time.h>
#include <uuid/uuid.h>
*/
import "C"

import (
	"errors"
	"fmt"
	"math"
	"os"

	"golang.org/x/sys/unix"
)

func nativeMachineIdentity() (string, error) {
	// gethostuuid(2) identifies the machine hardware across processes, users
	// and boots. Hardware identity changes are not adopted into an old pool.
	// https://github.com/apple-oss-distributions/Libc/blob/main/man/gethostuuid.2
	var id C.uuid_t
	wait := C.struct_timespec{tv_sec: 1}
	if result, err := C.gethostuuid(&id[0], &wait); result != 0 {
		return "", fmt.Errorf("machine hardware identity unavailable from gethostuuid: %w", err)
	}
	if C.uuid_is_null(&id[0]) != 0 { return "", errors.New("machine hardware identity unavailable: gethostuuid returned zero UUID") }
	var text C.uuid_string_t
	C.uuid_unparse_lower(&id[0], &text[0])
	return capacityIdentity(C.GoString(&text[0]), os.Getuid())
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
