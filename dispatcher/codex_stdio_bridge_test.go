//go:build !windows

package dispatcher

import (
	"context"
	"encoding/json"
	"net"
	"os"

	"github.com/gorilla/websocket"
	"testing"
)

// The child bridges the deterministic protocol fixture to the actual production
// stdio transport. Native behavior is covered separately with the pinned binary.
func TestNativeStdioBridge(t *testing.T) {
	socket := os.Getenv("NOODLE_TEST_BRIDGE_SOCKET")
	if socket == "" {
		return
	}
	dialer := websocket.Dialer{NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}
	conn, _, err := dialer.Dial("ws://localhost/", nil)
	if err != nil {
		os.Exit(19)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if _, err := os.Stdout.Write(append(raw, '\n')); err != nil {
				return
			}
		}
	}()
	go func() {
		decoder := json.NewDecoder(os.Stdin)
		for {
			var raw json.RawMessage
			if decoder.Decode(&raw) != nil {
				conn.Close()
				return
			}
			if conn.WriteMessage(websocket.TextMessage, raw) != nil {
				return
			}
		}
	}()
	<-done
	conn.Close()
	os.Exit(0)
}
