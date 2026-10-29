package runtime

import (
	"errors"
	"io"
	"sync"
)

// This is an execution-stream adapter only. VM admission stays with the outer
// session until the original stop ACK and a fresh Stopped read both arrive.
type openShellHarnessProcess struct {
	exec           *OpenShellOwnedExec
	stdout, stderr *io.PipeReader
	done           chan struct{}
	problems       chan error
	ioWG           sync.WaitGroup
	stop           func() error
	mu             sync.Mutex
	final          OpenShellExecResult
}

func newOpenShellHarnessProcess(exec *OpenShellOwnedExec, stop func() error) *openShellHarnessProcess {
	out, ow := io.Pipe()
	errout, ew := io.Pipe()
	p := &openShellHarnessProcess{exec: exec, stdout: out, stderr: errout, done: make(chan struct{}), problems: make(chan error, 1), stop: stop}
	go func() {
		var streamErr error
		for output := range exec.Output() {
			writer := ow
			if output.Stream == OpenShellStderr {
				writer = ew
			}
			if _, err := writer.Write(output.Data); err != nil {
				streamErr = errors.Join(streamErr, err)
				_ = exec.Close()
			}
		}
		_ = ow.Close()
		_ = ew.Close()
		result, err := exec.Wait()
		if failure := errors.Join(streamErr, err); failure != nil {
			result.StreamError = errors.Join(errors.New(result.StreamError), failure).Error()
			p.problems <- failure
		}
		p.mu.Lock()
		p.final = result
		p.mu.Unlock()
		close(p.done)
	}()
	return p
}
func (p *openShellHarnessProcess) Stdin() io.WriteCloser { return openShellStdin{p.exec} }
func (p *openShellHarnessProcess) Stdout() io.ReadCloser { return p.stdout }
func (p *openShellHarnessProcess) Stderr() io.ReadCloser { return p.stderr }
func (p *openShellHarnessProcess) Done() <-chan struct{} { return p.done }
func (p *openShellHarnessProcess) ExitCode() (int, bool) {
	r := p.result()
	if !r.ExitObserved {
		return -1, false
	}
	return r.ExitCode, true
}
func (p *openShellHarnessProcess) result() OpenShellExecResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.final
}
func (p *openShellHarnessProcess) Terminate() error             { return p.stop() }
func (p *openShellHarnessProcess) ForceKill() error             { return p.stop() }
func (p *openShellHarnessProcess) IOWaitGroup() *sync.WaitGroup { return &p.ioWG }
func (p *openShellHarnessProcess) WaitProblems() <-chan error   { return p.problems }

type openShellStdin struct{ exec *OpenShellOwnedExec }

func (s openShellStdin) Write(b []byte) (int, error) { return s.exec.Write(b) }
func (s openShellStdin) Close() error                { return s.exec.CloseWrite() }
