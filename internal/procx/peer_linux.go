//go:build linux

package procx

import (
	"fmt"
	"net"

	"golang.org/x/sys/unix"
)

func PeerPID(conn net.Conn) (int, error) {
	c, ok := conn.(*net.UnixConn)
	if !ok {
		return 0, fmt.Errorf("native Unix peer required")
	}
	raw, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var pid int
	var readErr error
	err = raw.Control(func(fd uintptr) {
		cred, e := unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
		readErr = e
		if e == nil {
			pid = int(cred.Pid)
		}
	})
	if err != nil {
		return 0, err
	}
	if readErr != nil {
		return 0, readErr
	}
	if pid <= 1 {
		return 0, fmt.Errorf("native peer PID unavailable")
	}
	return pid, nil
}
