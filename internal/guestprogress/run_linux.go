//go:build linux

package guestprogress

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type controlCall struct {
	request Request
	reply   chan Receipt
}

// Run is a dedicated process entry point, not an embeddable daemon component:
// subreaper ancestry and the single waiter belong exclusively to this process.
func Run(cfg Config) (int, error) {
	return runWithOperations(cfg, supervisorOperations{writeJSON: writeJSON, signalChildren: signalOwnedChildren})
}

type supervisorOperations struct {
	writeJSON      func(string, string, any) error
	signalChildren func(unix.Signal) error
}

func runWithOperations(cfg Config, ops supervisorOperations) (int, error) {
	if !cfg.Identity.valid() || len(cfg.Token) != 64 || digest([]byte(cfg.Token)) != cfg.Identity.TokenSHA256 || len(cfg.Argv) == 0 || !filepath.IsAbs(cfg.Argv[0]) || cfg.Grace <= 0 || cfg.Grace > time.Minute {
		return -1, fmt.Errorf("progress launch binding invalid")
	}
	if err := os.MkdirAll(cfg.SessionDirectory, 0700); err != nil {
		return -1, err
	}
	for _, root := range []string{cfg.Repository, cfg.SessionDirectory} {
		if !filepath.IsAbs(root) {
			return -1, fmt.Errorf("progress source path is not absolute")
		}
		actual, err := filepath.EvalSymlinks(root)
		if err != nil {
			return -1, err
		}
		if actual != filepath.Clean(root) {
			return -1, fmt.Errorf("progress source path has a symlink alias")
		}
		if relative, err := filepath.Rel(root, cfg.Directory); err != nil || relative == "." || (!strings.HasPrefix(relative, ".."+string(filepath.Separator)) && relative != "..") {
			return -1, fmt.Errorf("progress output overlaps its input")
		}
	}
	if err := os.Mkdir(cfg.Directory, 0700); err != nil {
		return -1, err
	}
	if err := privateDirectory(cfg.Directory); err != nil {
		return -1, err
	}
	if err := ops.writeJSON(cfg.Directory, "identity.json", cfg.Identity); err != nil {
		return -1, err
	}
	listener, err := net.Listen("unix", filepath.Join(cfg.Directory, "control.sock"))
	if err != nil {
		return -1, err
	}
	defer listener.Close()
	if err = os.Chmod(filepath.Join(cfg.Directory, "control.sock"), 0600); err != nil {
		return -1, err
	}
	calls := make(chan controlCall)
	done := make(chan struct{})
	defer close(done)
	go acceptControls(listener, calls, done)
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err = unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return -1, fmt.Errorf("enable owned descendant reaping: %w", err)
	}
	// No other code in this process calls Wait. Unreaped children cannot have
	// their PIDs reused between the ancestry read and signal delivery.
	cmd := exec.Command(cfg.Argv[0], cfg.Argv[1:]...)
	cmd.Dir = cfg.Repository
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = withoutControlToken(os.Environ())
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err = cmd.Start(); err != nil {
		return -1, err
	}
	defer cmd.Process.Release()
	leader := cmd.Process.Pid
	result := Receipt{Schema: 1, Identity: cfg.Identity, Status: "draining", ExitCode: -1, Archives: []Archive{}}
	waiters := []chan Receipt{}
	var stopping time.Time
	var failure error
	// A post-launch failure never releases the subreaper. Keep its control
	// socket and sole waiter until every owned descendant has been reaped.
	recordFailure := func(cause error) {
		if failure != nil {
			return
		}
		failure = cause
		result.Error = cause.Error()
		result.Status = "unknown"
		stopping = time.Now()
		failure = errors.Join(failure, ops.writeJSON(cfg.Directory, "failure.json", result))
		fmt.Fprintln(os.Stderr, failure)
	}
	supervisorStart, err := processStart(os.Getpid())
	if err != nil {
		recordFailure(err)
	}
	harnessStart, err := processStart(leader)
	if err != nil {
		recordFailure(err)
	}
	if failure == nil {
		if err = ops.writeJSON(cfg.Directory, "process.json", processBinding{Identity: cfg.Identity, SupervisorPID: os.Getpid(), SupervisorStart: supervisorStart, HarnessPID: leader, HarnessStart: harnessStart}); err != nil {
			recordFailure(err)
		}
	}
	leaderExited := false
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		allReaped := false
		for {
			var status unix.WaitStatus
			pid, e := unix.Wait4(-1, &status, unix.WNOHANG, nil)
			if errors.Is(e, unix.EINTR) {
				continue
			}
			if errors.Is(e, unix.ECHILD) {
				allReaped = true
				break
			}
			if e != nil {
				recordFailure(e)
				break
			}
			if pid == 0 {
				break
			}
			if pid == leader {
				leaderExited = true
				if status.Exited() {
					result.ExitCode = status.ExitStatus()
				} else if status.Signaled() {
					result.ExitCode = 128 + int(status.Signal())
				}
				if stopping.IsZero() {
					stopping = time.Now()
				}
			}
		}
		if allReaped {
			if !leaderExited {
				recordFailure(fmt.Errorf("original harness exit was not observed"))
			}
			result.ChildrenReaped = true
			break
		}
		if !stopping.IsZero() {
			signal := unix.SIGTERM
			if time.Since(stopping) >= cfg.Grace {
				signal = unix.SIGKILL
			}
			if err = ops.signalChildren(signal); err != nil {
				recordFailure(err)
			}
		}
		select {
		case call := <-calls:
			if err = call.request.validate(cfg.Identity); err == nil && result.RequestID != "" && result.RequestID != call.request.RequestID {
				err = fmt.Errorf("different stop request already owns progress drain")
			}
			if err != nil {
				refusal := result
				refusal.RequestID = call.request.RequestID
				refusal.Status = "refused"
				refusal.Error = err.Error()
				call.reply <- refusal
				continue
			}
			if result.RequestID == "" {
				result.RequestID = call.request.RequestID
				// Token never appears in durable/public receipts.
				if err = ops.writeJSON(cfg.Directory, "stop.json", result); err != nil {
					recordFailure(err)
				}
				stopping = time.Now()
			}
			waiters = append(waiters, call.reply)
		case <-tick.C:
		}
	}
	if failure != nil {
		failure = errors.Join(failure, ops.writeJSON(cfg.Directory, "failure.json", result))
		for _, reply := range waiters {
			reply <- result
		}
		return -1, failure
	}
	for _, spec := range []struct {
		root, name string
		excluded   map[string]bool
	}{{cfg.Repository, "noodle-result.tar", nil}, {cfg.SessionDirectory, "noodle-session.tar", map[string]bool{"codex/auth.json": true}}} {
		archive, e := archiveTree(spec.root, cfg.Directory, spec.name, spec.excluded)
		if e != nil {
			result.Status = "unknown"
			result.Error = e.Error()
			e = errors.Join(e, ops.writeJSON(cfg.Directory, "failure.json", result))
			for _, reply := range waiters {
				reply <- result
			}
			return -1, e
		}
		result.Archives = append(result.Archives, archive)
	}
	result.Status = "archived"
	if err = ops.writeJSON(cfg.Directory, "result.json", result); err != nil {
		return -1, err
	}
	for _, reply := range waiters {
		reply <- result
	}
	return result.ExitCode, nil
}

func withoutControlToken(environment []string) []string {
	result := make([]string, 0, len(environment))
	for _, value := range environment {
		if !strings.HasPrefix(value, "NOODLE_PROGRESS_TOKEN=") {
			result = append(result, value)
		}
	}
	return result
}

func processStart(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", err
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return "", fmt.Errorf("process start identity missing")
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) < 20 {
		return "", fmt.Errorf("process start identity incomplete")
	}
	if _, err = strconv.ParseUint(fields[19], 10, 64); err != nil {
		return "", err
	}
	return fields[19], nil
}

func verifyControlPeer(conn net.Conn, directory string, identity Identity) error {
	var binding processBinding
	if err := readJSON(filepath.Join(directory, "process.json"), &binding); err != nil {
		return err
	}
	if binding.Identity != identity || binding.SupervisorPID <= 1 || binding.SupervisorStart == "" {
		return fmt.Errorf("progress supervisor identity missing")
	}
	unixConn, ok := conn.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("progress control transport differs")
	}
	raw, err := unixConn.SyscallConn()
	if err != nil {
		return err
	}
	var credentials *unix.Ucred
	var readErr error
	if err = raw.Control(func(fd uintptr) {
		credentials, readErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return err
	}
	if readErr != nil {
		return readErr
	}
	if int(credentials.Pid) != binding.SupervisorPID || int(credentials.Uid) != os.Geteuid() {
		return fmt.Errorf("progress control socket peer differs")
	}
	start, err := processStart(binding.SupervisorPID)
	if err != nil || start != binding.SupervisorStart {
		return fmt.Errorf("progress control process generation differs")
	}
	return nil
}

// /proc task children lists are kernel-maintained direct-child ancestry, not
// a scan for similarly named orphan processes. Adoption is confined to this
// subreaper's descendants. The only waiter is the caller, so no child PID can
// be reaped/reused until this function finishes. OpenShell additionally brokers
// positive-PID signals through retained pidfds; its child policy forbids direct
// pidfd and process-group operations.
func signalOwnedChildren(signal unix.Signal) error {
	tasks, err := os.ReadDir("/proc/self/task")
	if err != nil {
		return err
	}
	seen := map[int]bool{}
	for _, task := range tasks {
		data, e := os.ReadFile(filepath.Join("/proc/self/task", task.Name(), "children"))
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			return e
		}
		for _, text := range strings.Fields(string(data)) {
			pid, e := strconv.Atoi(text)
			if e != nil || pid <= 1 {
				return fmt.Errorf("owned child identity invalid")
			}
			if seen[pid] {
				continue
			}
			seen[pid] = true
			e = unix.Kill(pid, signal)
			if e != nil && !errors.Is(e, unix.ESRCH) {
				return fmt.Errorf("signal owned child: %w", e)
			}
		}
	}
	return nil
}

func acceptControls(listener net.Listener, calls chan<- controlCall, done <-chan struct{}) {
	for {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		go func() {
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(3 * time.Minute))
			var request Request
			decoder := json.NewDecoder(io.LimitReader(conn, 4096))
			decoder.DisallowUnknownFields()
			if decoder.Decode(&request) != nil {
				return
			}
			call := controlCall{request: request, reply: make(chan Receipt, 1)}
			select {
			case calls <- call:
			case <-done:
				return
			}
			select {
			case result := <-call.reply:
				_ = json.NewEncoder(conn).Encode(result)
			case <-done:
				// Prefer a final receipt already queued before process shutdown.
				select {
				case result := <-call.reply:
					_ = json.NewEncoder(conn).Encode(result)
				default:
				}
			}
		}()
	}
}
