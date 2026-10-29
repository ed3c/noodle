package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// A failed write or a missing response does not prove a mutation was rejected.
var errNativeRequestPending = errors.New("native request pending; Host response capability unavailable")

var errCodexRPCUnknown = errors.New("Codex app-server effect requires original readback")

const codexRPCOperationTimeout = 30 * time.Second

type codexRPCRefusal struct{ Raw json.RawMessage }

func (e *codexRPCRefusal) Error() string { return fmt.Sprintf("native Codex RPC refused: %s", e.Raw) }

type codexRPCReply struct {
	result json.RawMessage
	err    error
}

type codexRPCTransport interface {
	SetWriteDeadline(time.Time) error
	WriteJSON(any) error
	ReadMessage() (int, []byte, error)
	Close() error
}

// codexRPC preserves request identity across the selected native transport.
type codexRPC struct {
	conn           codexRPCTransport
	writeMu        sync.Mutex
	mu             sync.Mutex
	nextID         uint64
	pending        map[string]chan codexRPCReply
	done           chan struct{}
	err            error
	onEvent        func(json.RawMessage) error
	socketID       string
	draining       bool
	uncertain      bool
	unsubscribed   map[string]bool
	serverRequests map[string]string
}

func dialCodexRPC(ctx context.Context, socket string, onEvent func(json.RawMessage) error) (*codexRPC, error) {
	if !filepath.IsAbs(socket) || filepath.Clean(socket) != socket {
		return nil, fmt.Errorf("original native Unix socket path required")
	}
	socketID, err := nativeSocketIdentity(socket)
	if err != nil {
		return nil, fmt.Errorf("original native endpoint identity: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	dialer := websocket.Dialer{
		HandshakeTimeout: 3 * time.Second,
		NetDialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}
	conn, _, err := dialer.DialContext(ctx, "ws://localhost/", http.Header{})
	if err != nil {
		return nil, fmt.Errorf("connect original Codex app-server: %w", err)
	}
	current, err := nativeSocketIdentity(socket)
	if err != nil || current != socketID {
		conn.Close()
		return nil, fmt.Errorf("original native endpoint changed while connecting")
	}
	c := &codexRPC{conn: conn, pending: make(map[string]chan codexRPCReply), done: make(chan struct{}), onEvent: onEvent, socketID: socketID}
	if err := c.initialize(ctx); err != nil {
		c.close()
		return nil, err
	}
	return c, nil
}

func (c *codexRPC) initialize(ctx context.Context) error {
	go c.read()
	var initialized json.RawMessage
	if err := c.call(ctx, "initialize", map[string]any{
		"clientInfo":   map[string]string{"name": "noodle-native-runtime", "version": "1"},
		"capabilities": map[string]bool{"experimentalApi": true},
	}, &initialized); err != nil {
		return err
	}
	return c.send(map[string]any{"method": "initialized"})
}

func (c *codexRPC) send(value any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.conn.SetWriteDeadline(time.Now().Add(3 * time.Second)); err != nil {
		return err
	}
	return c.conn.WriteJSON(value)
}

func (c *codexRPC) call(ctx context.Context, method string, params any, result any) (callErr error) {
	ctx, cancel := context.WithTimeout(ctx, codexRPCOperationTimeout)
	defer cancel()
	c.mu.Lock()
	if c.err != nil {
		err := c.err
		c.mu.Unlock()
		return err
	}
	if len(c.serverRequests) != 0 && !nativeReadOrInterrupt(method) {
		c.mu.Unlock()
		return errNativeRequestPending
	}
	if c.draining {
		if err := c.allowDrainCall(method, params); err != nil {
			c.mu.Unlock()
			return err
		}
	}
	c.nextID++
	id := strconv.FormatUint(c.nextID, 10)
	reply := make(chan codexRPCReply, 1)
	c.pending[id] = reply
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if errors.Is(callErr, errCodexRPCUnknown) {
			c.uncertain = true
		}
		delete(c.pending, id)
		c.mu.Unlock()
	}()
	if err := c.send(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return fmt.Errorf("%w: %s write: %v", errCodexRPCUnknown, method, err)
	}
	select {
	case response := <-reply:
		if response.err != nil {
			return response.err
		}
		if err := json.Unmarshal(response.result, result); err != nil {
			return fmt.Errorf("%w: %s response: %v", errCodexRPCUnknown, method, err)
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("%w: %s: %v", errCodexRPCUnknown, method, ctx.Err())
	case <-c.done:
		return fmt.Errorf("%w: %s connection closed", errCodexRPCUnknown, method)
	}
}

// Hold admission on this original connection before taking release snapshots.
// A shared socket cannot provide exclusivity over other clients' work.
func (c *codexRPC) beginDrain() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, owned := c.conn.(*codexStdio); !owned {
		return fmt.Errorf("native release has no exclusive foreground transport")
	}
	if len(c.serverRequests) != 0 {
		return errNativeRequestPending
	}
	if c.err != nil || c.uncertain || len(c.pending) != 0 {
		return fmt.Errorf("native release has unresolved RPC work")
	}
	c.draining = true
	return nil
}

// Called with mu held. Unknown unsubscribe effects are never replayed.
func (c *codexRPC) allowDrainCall(method string, params any) error {
	switch method {
	case "thread/read", "thread/turns/list", "thread/loaded/list", "thread/list", "thread/backgroundTerminals/list":
		return nil
	case "turn/interrupt":
		return nil
	case "thread/unsubscribe":
		if c.uncertain || len(c.pending) != 0 {
			return fmt.Errorf("native unsubscribe has unresolved RPC work")
		}
		raw, err := json.Marshal(params)
		if err != nil {
			return err
		}
		var target struct {
			ThreadID string `json:"threadId"`
		}
		if err := json.Unmarshal(raw, &target); err != nil || !codexThreadUUID.MatchString(target.ThreadID) {
			return fmt.Errorf("native unsubscribe thread identity unavailable")
		}
		if c.unsubscribed[target.ThreadID] {
			return fmt.Errorf("native unsubscribe already requested; original readback required")
		}
		if c.unsubscribed == nil {
			c.unsubscribed = make(map[string]bool)
		}
		c.unsubscribed[target.ThreadID] = true
		return nil
	default:
		return fmt.Errorf("native connection is draining; %s is not admitted", method)
	}
}

func (c *codexRPC) read() {
	defer close(c.done)
	for {
		_, raw, err := c.conn.ReadMessage()
		if err != nil {
			c.fail(err)
			return
		}
		var message struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Result json.RawMessage `json:"result"`
			Error  json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(raw, &message); err != nil {
			c.fail(fmt.Errorf("invalid native RPC message: %w", err))
			return
		}
		if message.Method != "" {
			request := len(message.ID) != 0 && string(message.ID) != "null"
			var resolved nativeRequestEnvelope
			if request || message.Method == "serverRequest/resolved" {
				if err := json.Unmarshal(raw, &resolved); err != nil {
					c.fail(err)
					return
				}
				c.mu.Lock()
				if request {
					if c.serverRequests == nil {
						c.serverRequests = make(map[string]string)
					}
					c.serverRequests[string(message.ID)] = resolved.Params.ThreadID
				}
				c.mu.Unlock()
			}
			if c.onEvent != nil {
				if err := c.onEvent(append(json.RawMessage(nil), raw...)); err != nil {
					c.fail(fmt.Errorf("persist native event: %w", err))
					return
				}
			}
			if message.Method == "serverRequest/resolved" {
				c.mu.Lock()
				key := string(resolved.Params.RequestID)
				if thread, ok := c.serverRequests[key]; ok && thread != "" && thread == resolved.Params.ThreadID {
					delete(c.serverRequests, key)
				}
				c.mu.Unlock()
			}
			continue
		}
		var id string
		if err := json.Unmarshal(message.ID, &id); err != nil {
			c.fail(fmt.Errorf("native response ID conflicts with this client"))
			return
		}
		c.mu.Lock()
		reply := c.pending[id]
		c.mu.Unlock()
		if reply == nil {
			continue // A timed-out call must not be replayed by a late response.
		}
		response := codexRPCReply{result: message.Result}
		if len(message.Error) != 0 && string(message.Error) != "null" {
			response.err = &codexRPCRefusal{Raw: append(json.RawMessage(nil), message.Error...)}
		} else if len(message.Result) == 0 {
			response.err = fmt.Errorf("%w: missing native result", errCodexRPCUnknown)
		}
		select {
		case reply <- response:
		default:
		}
	}
}

func (c *codexRPC) fail(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.err = fmt.Errorf("%w: %v", errCodexRPCUnknown, err)
}

func (c *codexRPC) close() {
	_ = c.conn.Close()
	<-c.done
}

func nativeReadOrInterrupt(method string) bool {
	switch method {
	case "thread/read", "thread/turns/list", "thread/loaded/list", "thread/list", "thread/backgroundTerminals/list", "turn/interrupt":
		return true
	default:
		return false
	}
}

type nativeRequestEnvelope struct {
	ID     json.RawMessage
	Method string
	Params struct {
		ThreadID  string
		RequestID json.RawMessage
	}
}
