package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/lockfile"
)

// The foreground switch owns this gate. It has no runtime, budget or workers.
// If it disappears, the persisted maintenance marker keeps new admission closed.
type hostMaintenanceGate struct {
	mu           sync.RWMutex
	closed       bool
	listener     net.Listener
	done         chan struct{}
	root, source string
}

func hostMaintenancePending(root string) (bool, error) {
	raw, err := os.ReadFile(filepath.Join(root, "bootstrap.json"))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var row struct {
		Phase string `json:"phase"`
	}
	if err = json.Unmarshal(raw, &row); err != nil {
		return false, err
	}
	if row.Phase == "" {
		return false, errors.New("Host maintenance phase is missing")
	}
	return row.Phase != "active", nil
}

func openHostMaintenanceGate(root, source string) (*hostMaintenanceGate, error) {
	path := filepath.Join(root, "maintenance.sock")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(path, 0600); err != nil {
		l.Close()
		return nil, err
	}
	g := &hostMaintenanceGate{listener: l, done: make(chan struct{}), root: root, source: source}
	go func() {
		defer close(g.done)
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go g.serve(conn)
		}
	}()
	return g, nil
}

func (g *hostMaintenanceGate) serve(conn net.Conn) {
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(60 * time.Second))
	var req hostRequest
	if json.NewDecoder(io.LimitReader(conn, 4<<20)).Decode(&req) != nil {
		return
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	var result any = hostError("busy", errors.New("initial Host maintenance; retain the original request and wait"))
	if req.Operation == "attach" || req.Operation == "once" {
		if err := g.rememberAdmission(req); err != nil {
			result = hostError("busy", err)
		}
	}
	if !g.closed && (req.Operation == "resources" || req.Operation == "status") {
		raw, err := nativeHostRPC(context.Background(), g.source, req)
		if err != nil {
			result = hostError("unknown_effect", err)
		} else {
			result = raw
		}
	}
	_ = json.NewEncoder(conn).Encode(result)
}

func (g *hostMaintenanceGate) rememberAdmission(req hostRequest) error {
	if !validPoolID(req.RequestID) {
		return errors.New("maintenance admission lacks an original request identity")
	}
	b, err := readHostBootstrap(g.root)
	if err != nil {
		return err
	}
	if req.BinarySHA256 != b.Source.SHA {
		return errors.New("maintenance admission executable is outside the original legacy owner")
	}
	row := hostRequestBinding{Schema: 1, RequestID: req.RequestID, Operation: req.Operation, ProjectDir: req.ProjectDir, ConfigPath: req.ConfigPath, ConfigSHA256: req.ConfigSHA256, Mode: req.Mode, BinarySHA256: req.BinarySHA256, EnvironmentSHA256: hostDigest([]byte(strings.Join(req.Environment, "\x00"))), Status: "busy", HostDir: g.root, CleanupCompleted: req.CleanupCompleted}
	raw, err := json.Marshal(row)
	if err != nil {
		return err
	}
	dir := filepath.Join(g.root, "bootstrap", "waiting")
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	path := filepath.Join(dir, req.RequestID+".json")
	lock, err := lockfile.TryLock(path + ".lock")
	if err != nil {
		return err
	}
	defer lock.Close()
	if old, readErr := os.ReadFile(path); readErr == nil {
		var prior hostRequestBinding
		if err = json.Unmarshal(old, &prior); err != nil {
			return err
		}
		prior.EnvironmentSHA256 = row.EnvironmentSHA256
		if prior != row {
			return errors.New("original waiting request binding changed")
		}
	} else if !os.IsNotExist(readErr) {
		return readErr
	}
	return filex.WriteFileAtomicDurable(path, raw)
}

func waitingPoolAdmission(root string, b hostRequestBinding, p hostPool) (string, bool) {
	if b.Status != "busy" {
		return "", false
	}
	raw, err := os.ReadFile(filepath.Join(root, "bootstrap", "waiting", b.RequestID+".json"))
	if err != nil {
		return "", false
	}
	var original hostRequestBinding
	if json.Unmarshal(raw, &original) != nil {
		return "", false
	}
	original.EnvironmentSHA256 = b.EnvironmentSHA256
	if original != b {
		return "", false
	}
	for _, g := range p.Generations {
		if g.SHA == b.BinarySHA256 {
			return g.Binary, true
		}
	}
	return "", false
}

// Exclude forwarded operations before the final empty-Host observation.
func (g *hostMaintenanceGate) freeze()      { g.mu.Lock(); g.closed = true; g.mu.Unlock() }
func (g *hostMaintenanceGate) Close() error { err := g.listener.Close(); <-g.done; return err }

func publishMaintenanceSocket(root string) error {
	tmp := filepath.Join(root, "maintenance.sock.next")
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Symlink(filepath.Join(root, "maintenance.sock"), tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(root, "host.sock")); err != nil {
		return err
	}
	return filex.SyncDir(root)
}

func maintenanceWaitResponse(root string) *hostResponse {
	pending, err := hostMaintenancePending(root)
	if err != nil {
		r := hostError("unknown_effect", fmt.Errorf("read Host maintenance: %w", err))
		return &r
	}
	if pending {
		r := hostError("busy", errors.New("initial Host maintenance; retain the original request and wait"))
		return &r
	}
	return nil
}
