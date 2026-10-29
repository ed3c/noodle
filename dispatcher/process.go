package dispatcher

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/poteto/noodle/internal/procx"
)

// ProcessHandle wraps an os/exec.Cmd with lifecycle primitives for child
// process management. It replaces what the tmux dispatcher provided: process isolation,
// liveness checking, and graceful shutdown.
type ProcessHandle struct {
	cmd *exec.Cmd

	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	done     chan struct{}
	doneOnce sync.Once
	ioWG     sync.WaitGroup

	mu          sync.Mutex
	exitCode    int
	exited      bool
	retainGroup bool
	waitProblem chan error
}

// processMetadata is written to process.json for crash recovery.
type processMetadata struct {
	PID       int       `json:"pid"`
	SessionID string    `json:"session_id"`
	StartedAt time.Time `json:"started_at"`
}

// StartProcess spawns a child process with process group isolation and
// returns a handle for lifecycle management.
func StartProcess(cmd *exec.Cmd) (*ProcessHandle, error) {
	return startProcess(cmd, false)
}

func startProcess(cmd *exec.Cmd, retainGroup bool) (*ProcessHandle, error) {
	if retainGroup && runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		return nil, fmt.Errorf("owned group exit observation unsupported on %s", runtime.GOOS)
	}
	configureChildProcess(cmd)

	if cmd.Stdout != nil || cmd.Stderr != nil {
		return nil, fmt.Errorf("output streams already configured")
	}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("create stdin pipe: %w", err)
	}
	// Wait owns StdinPipe, but consumers own the output readers. exec.Cmd's
	// StdoutPipe/StderrPipe readers are closed by Wait, which can beat a delayed
	// consumer after a fast exit. Native pipes keep kernel backpressure and let
	// consumers drain through EOF independently of process reaping.
	started := false
	defer func() {
		if !started {
			_ = stdin.Close()
			_ = cmd.Stdin.(io.Closer).Close()
		}
	}()
	stdout, stdoutWriter, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create stdout pipe: %w", err)
	}
	defer stdoutWriter.Close()
	defer func() {
		if !started {
			_ = stdout.Close()
		}
	}()
	stderr, stderrWriter, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("create stderr pipe: %w", err)
	}
	defer stderrWriter.Close()
	defer func() {
		if !started {
			_ = stderr.Close()
		}
	}()
	cmd.Stdout, cmd.Stderr = stdoutWriter, stderrWriter
	if err := cmd.Start(); err != nil {
		return nil, ProcessStartError{Cause: err}
	}
	started = true

	h := &ProcessHandle{
		cmd:         cmd,
		stdin:       stdin,
		stdout:      stdout,
		stderr:      stderr,
		done:        make(chan struct{}),
		retainGroup: retainGroup,
		waitProblem: make(chan error, 1),
	}

	go h.wait()
	return h, nil
}

// Stdin returns the write end of the child's stdin pipe.
func (h *ProcessHandle) Stdin() io.WriteCloser { return h.stdin }

// Stdout returns the child stdout reader. The consumer must close it after draining.
func (h *ProcessHandle) Stdout() io.ReadCloser { return h.stdout }

// Stderr returns the child stderr reader. The consumer must close it after draining.
func (h *ProcessHandle) Stderr() io.ReadCloser { return h.stderr }

// Done closes after Wait. An admitted child reserves its PID until the group drains.
func (h *ProcessHandle) Done() <-chan struct{} { return h.done }

// PID returns the process ID, or 0 if the process hasn't started.
func (h *ProcessHandle) PID() int {
	if h.cmd.Process == nil {
		return 0
	}
	return h.cmd.Process.Pid
}

// ExitCode returns the exit code and whether the process has exited.
func (h *ProcessHandle) ExitCode() (int, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.exitCode, h.exited
}

// Terminate sends SIGTERM to the process group and returns immediately.
func (h *ProcessHandle) Terminate() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.exited {
		return nil
	}
	return terminateProcess(h.cmd, h.retainGroup)
}

// ForceKill sends SIGKILL to the process group and returns immediately.
func (h *ProcessHandle) ForceKill() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.exited {
		return nil
	}
	return forceKillProcess(h.cmd, h.retainGroup)
}

func (h *ProcessHandle) wait() {
	if h.retainGroup {
		if err := procx.WaitChildExit(h.PID()); err != nil {
			h.waitProblem <- err
			return
		}
		// The unreaped original leader reserves this PID/PGID through every
		// group signal. Reap only after all other members have exited.
		warned := false
		stopRequested := false
		for {
			others, err := procx.GroupHasOtherMembers(h.PID())
			if err == nil && !others {
				break
			}
			if err != nil && !warned {
				h.waitProblem <- err
				warned = true
			}
			if err == nil && others && !stopRequested {
				stopRequested = true
				if err := h.Terminate(); err != nil && !warned {
					h.waitProblem <- err
					warned = true
				}
			}
			time.Sleep(100 * time.Millisecond)
		}
		h.mu.Lock()
		defer h.mu.Unlock()
		h.recordExit(h.cmd.Wait())
		return
	}
	err := h.cmd.Wait()
	h.mu.Lock()
	defer h.mu.Unlock()
	h.recordExit(err)
}

func (h *ProcessHandle) recordExit(err error) {
	h.exited = true
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			h.exitCode = exitErr.ExitCode()
		} else {
			h.exitCode = -1
		}
	}
	h.doneOnce.Do(func() { close(h.done) })
}

// WriteProcessMetadata writes process.json to the session directory.
func WriteProcessMetadata(sessionDir, sessionID string, pid int, startedAt time.Time) error {
	sessionDir = strings.TrimSpace(sessionDir)
	if sessionDir == "" {
		return fmt.Errorf("session directory not set")
	}
	payload, err := json.Marshal(processMetadata{
		PID:       pid,
		SessionID: sessionID,
		StartedAt: startedAt.UTC(),
	})
	if err != nil {
		return fmt.Errorf("encode process metadata: %w", err)
	}
	path := filepath.Join(sessionDir, "process.json")
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		return fmt.Errorf("write process metadata: %w", err)
	}
	return nil
}
