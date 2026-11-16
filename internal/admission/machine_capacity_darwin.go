//go:build darwin

package admission

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"time"

	"golang.org/x/sys/unix"
)

func nativeMachineIdentity() (string, error) {
	// XNU's IOPlatformExpertDevice::generatePlatformUUID derives this property
	// from hardware chip IDs (ARM) or EFI system-id (Intel), not a boot ID.
	// Hardware changes refuse an existing pool. ioreg works in CGO_ENABLED=0 releases.
	// https://github.com/apple-oss-distributions/xnu/blob/main/iokit/Kernel/IOPlatformExpert.cpp
	// https://github.com/apple-oss-distributions/IOKitTools/blob/main/ioreg.tproj/ioreg.8
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/usr/sbin/ioreg", "-r", "-d", "1", "-c", "IOPlatformExpertDevice", "-w", "0")
	cmd.Dir = "/"
	cmd.Env = []string{"LC_ALL=C", "LANG=C"}
	cmd.WaitDelay = 100 * time.Millisecond
	output, err := cmd.StdoutPipe()
	if err != nil { return "", fmt.Errorf("machine hardware identity unavailable from ioreg: %w", err) }
	if err = cmd.Start(); err != nil { return "", fmt.Errorf("machine hardware identity unavailable from ioreg: %w", err) }
	data, readErr := io.ReadAll(io.LimitReader(output, platformUUIDOutputLimit+1))
	if readErr != nil || len(data) > platformUUIDOutputLimit { _ = cmd.Process.Kill() }
	waitErr := cmd.Wait()
	if ctx.Err() != nil { return "", fmt.Errorf("machine hardware identity unavailable from ioreg: %w", ctx.Err()) }
	if readErr != nil { return "", fmt.Errorf("machine hardware identity unavailable from ioreg: %w", readErr) }
	if len(data) > platformUUIDOutputLimit { return "", errors.New("IOPlatformUUID observation exceeds output limit") }
	if waitErr != nil { return "", fmt.Errorf("machine hardware identity unavailable from ioreg: %w", waitErr) }
	id, err := parseDarwinPlatformUUID(data)
	if err != nil { return "", err }
	return capacityIdentity(id, os.Getuid())
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
