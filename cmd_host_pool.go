package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/poteto/noodle/internal/filex"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/internal/procx"
	"github.com/poteto/noodle/loop"
)

type hostGeneration struct {
	ID        string              `json:"id"`
	Directory string              `json:"directory"`
	Binary    string              `json:"binary_path"`
	SHA       string              `json:"binary_sha256"`
	State     string              `json:"state"`
	Process   procx.ProcessRecord `json:"process"`
	Error     string              `json:"error,omitempty"`
}

type hostVersionRequest struct {
	MaintenanceState string `json:"maintenance_previous_state,omitempty"`
	ID               string `json:"id"`
	Binary           string `json:"binary_path"`
	SHA              string `json:"binary_sha256"`
	State            string `json:"state"`
	Generation       string `json:"generation,omitempty"`
	Error            string `json:"error,omitempty"`
}

type hostTaskOwner struct {
	MaintenanceHold       string                 `json:"maintenance_hold,omitempty"`
	ExecutionProfile      json.RawMessage        `json:"execution_profile,omitempty"`
	ControllerOnly        bool                   `json:"controller_only,omitempty"`
	ContextInputs         json.RawMessage        `json:"context_inputs,omitempty"`
	RebuildError          string                 `json:"rebuild_error,omitempty"`
	LastAdmission         *hostResponse          `json:"last_admission,omitempty"`
	RoundState            *loop.HostContinuation `json:"round_state,omitempty"`
	AdmissionOnly         bool                   `json:"admission_only,omitempty"`
	Original              hostReceipt            `json:"original"`
	Current               hostReceipt            `json:"current"`
	State                 string                 `json:"state"`
	EnvironmentSHA        string                 `json:"environment_sha256"`
	RuntimeEnvironmentSHA string                 `json:"runtime_environment_sha256,omitempty"`
	Previous              []hostReceipt          `json:"previous,omitempty"`
}

type hostPool struct {
	Schema      int                       `json:"schema"`
	Active      string                    `json:"active"`
	Generations map[string]hostGeneration `json:"generations"`
	Owners      map[string]hostTaskOwner  `json:"owners"`
	Queue       []hostVersionRequest      `json:"queue"`
}

func readHostPool(root string) (hostPool, error) {
	raw, err := os.ReadFile(filepath.Join(root, "pool.json"))
	if err != nil {
		return hostPool{}, err
	}
	var p hostPool
	if err = json.Unmarshal(raw, &p); err != nil {
		return p, err
	}
	if p.Schema != 1 || p.Generations == nil || p.Owners == nil {
		return p, errors.New("invalid native Host pool")
	}
	for id, g := range p.Generations {
		if !validPoolID(id) || g.ID != id || g.Directory != filepath.Join(root, "g", id) {
			return p, errors.New("Host generation custody changed")
		}
	}
	return p, nil
}

func updateHostPool(root string, apply func(*hostPool) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	lock, err := lockfile.Acquire(ctx, filepath.Join(root, "pool.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	p, err := readHostPool(root)
	if os.IsNotExist(err) {
		p = hostPool{Schema: 1, Generations: map[string]hostGeneration{}, Owners: map[string]hostTaskOwner{}}
	} else if err != nil {
		return err
	}
	if err = apply(&p); err != nil {
		return err
	}
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return filex.WriteFileAtomicDurable(filepath.Join(root, "pool.json"), data)
}

func validPoolID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for _, r := range id {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

func nativeHostRPC(ctx context.Context, dir string, req hostRequest) (json.RawMessage, error) {
	conn, err := net.DialTimeout("unix", filepath.Join(dir, "host.sock"), time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	deadline := time.Now().Add(5 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetDeadline(deadline)
	if err = json.NewEncoder(conn).Encode(req); err != nil {
		return nil, err
	}
	var raw json.RawMessage
	err = json.NewDecoder(io.LimitReader(conn, 4<<20)).Decode(&raw)
	return raw, err
}

func publishPoolSocket(root, dir string) error {
	if pending, err := hostMaintenancePending(root); err != nil || pending {
		return err
	}
	f, err := os.CreateTemp(root, "host.sock.*.next")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if err := f.Close(); err != nil {
		return err
	}
	defer os.Remove(tmp)
	if err := os.Remove(tmp); err != nil {
		return err
	}
	if err := os.Symlink(filepath.Join(dir, "host.sock"), tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(root, "host.sock")); err != nil {
		return err
	}
	return filex.SyncDir(root)
}

func generationLive(g hostGeneration) bool { return g.State != "exited" && g.State != "failed" }

// The confirmed maintenance owner launches one generation. Historical version
// queue entries remain metadata; this function never consumes them.
func launchHostGeneration(ctx context.Context, root, binary, digest string) (hostGeneration, error) {
	data, err := os.ReadFile(binary)
	if err != nil || hostDigest(data) != digest {
		return hostGeneration{}, errors.New("selected Host binary changed")
	}
	id, err := hostID()
	if err != nil {
		return hostGeneration{}, err
	}
	g := hostGeneration{ID: id, Directory: filepath.Join(root, "g", id), Binary: binary, SHA: digest, State: "starting"}
	if err = os.MkdirAll(g.Directory, 0700); err != nil {
		return g, err
	}
	if err = updateHostPool(root, func(p *hostPool) error {
		count := 0
		for _, current := range p.Generations {
			if generationLive(current) {
				count++
			}
		}
		if count >= 2 {
			return errors.New("two Host generations already exist")
		}
		p.Generations[id] = g
		return nil
	}); err != nil {
		return g, err
	}
	log, err := os.OpenFile(filepath.Join(g.Directory, "host.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		g.State, g.Error = "failed", err.Error()
		saveErr := updateHostPool(root, func(p *hostPool) error { p.Generations[id] = g; return nil })
		return g, errors.Join(err, saveErr)
	}
	defer log.Close()
	cmd := exec.Command(binary, "host", "serve")
	cmd.Dir = g.Directory
	cmd.Env = hostBaselineEnvironment(g.Directory)
	cmd.Env = append(cmd.Env, "NOODLE_HOST_POOL_ROOT="+root, "NOODLE_HOST_GENERATION="+id)
	cmd.Stdout = log
	cmd.Stderr = log
	detachHostProcess(cmd)
	if err = cmd.Start(); err != nil {
		g.State = "failed"
		g.Error = err.Error()
		saveErr := updateHostPool(root, func(p *hostPool) error { p.Generations[id] = g; return nil })
		return g, errors.Join(err, saveErr)
	}
	g.Process = procx.ProcessRecord{PID: cmd.Process.Pid, SessionID: id, StartedAt: time.Now().UTC()}
	if err = updateHostPool(root, func(p *hostPool) error { p.Generations[id] = g; return nil }); err != nil {
		return g, err
	}
	go func() {
		if exitErr := cmd.Wait(); exitErr != nil {
			if saveErr := updateHostPool(root, func(p *hostPool) error {
				current := p.Generations[id]
				current.Error = exitErr.Error()
				p.Generations[id] = current
				return nil
			}); saveErr != nil {
				fmt.Fprintln(os.Stderr, "Host exit readback persistence:", errors.Join(exitErr, saveErr))
			}
		}
	}()
	ready, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	for {
		raw, err := nativeHostRPC(ready, g.Directory, hostRequest{Operation: "resources"})
		if err == nil {
			var response struct {
				Instance string `json:"host_instance_id"`
				Binary   string `json:"binary_sha256"`
				Rolling  int    `json:"rolling_host_version"`
			}
			if json.Unmarshal(raw, &response) == nil && response.Instance == id && response.Binary == digest && response.Rolling == 1 {
				return g, nil
			}
		}
		select {
		case <-ready.Done():
			return g, fmt.Errorf("new Host readiness unknown; no replay: %w", ready.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}
