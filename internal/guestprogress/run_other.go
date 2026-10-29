//go:build !linux

package guestprogress

import (
	"fmt"
	"net"
)

func Run(Config) (int, error) { return -1, fmt.Errorf("guest progress supervision requires Linux") }

func verifyControlPeer(net.Conn, string, Identity) error {
	return fmt.Errorf("guest progress peer verification requires Linux")
}
