//go:build !darwin && !linux

package procx

import (
	"fmt"
	"net"
)

func PeerPID(conn net.Conn) (int, error) {
	return 0, fmt.Errorf("native Unix peer identity unsupported")
}
