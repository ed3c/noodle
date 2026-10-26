package runtime

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/url"
	"path"
	"strings"
	"sync"

	dm "github.com/NVIDIA/OpenShell/sdk/go/proto/datamodelv1"
	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// OpenShellTarget is supplied by the original owner. Name is an API locator;
// SandboxID is evidence to compare with Get, never an atomic exec precondition.
// The owner still needs exclusive name/control custody before routing work.
type OpenShellTarget struct {
	Sandbox      OpenShellSandboxRef
	Name         string
	ConfigSource string
}

// OpenShellTLS contains owner-loaded credentials. This package reads no files.
// ServerName optionally selects the certificate identity for a forwarded gateway.
type OpenShellTLS struct {
	RootCAs     *x509.CertPool
	Certificate tls.Certificate
	ServerName  string
}

// OpenShellTransport owns its connection; Close cancels all its local streams,
// without stopping any VM. It is deliberately neither a Runtime nor a Stopper.
type OpenShellTransport struct {
	conn   *grpc.ClientConn
	client pb.OpenShellClient
	target OpenShellTarget
	ctx    context.Context
	cancel context.CancelFunc
}

func NewOpenShellTransport(target OpenShellTarget, security OpenShellTLS) (*OpenShellTransport, error) {
	for _, field := range []struct{ name, value string }{
		{"gateway", target.Sandbox.Gateway}, {"workspace", target.Sandbox.Workspace},
		{"sandbox name", target.Name}, {"native sandbox ID", target.Sandbox.SandboxID},
		{"configuration source", target.ConfigSource},
	} {
		if strings.TrimSpace(field.value) == "" || strings.ContainsRune(field.value, 0) {
			return nil, fmt.Errorf("openshell transport %s missing or invalid", field.name)
		}
	}
	u, err := url.Parse(target.Sandbox.Gateway)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("openshell gateway is not an HTTPS endpoint")
	}
	if security.RootCAs == nil || len(security.Certificate.Certificate) == 0 || security.Certificate.PrivateKey == nil {
		return nil, fmt.Errorf("openshell gateway CA or mTLS client certificate missing")
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	config := &tls.Config{
		MinVersion:   tls.VersionTLS12,
		RootCAs:      security.RootCAs.Clone(),
		Certificates: []tls.Certificate{security.Certificate},
		ServerName:   security.ServerName,
	}
	conn, err := grpc.NewClient("dns:///"+net.JoinHostPort(u.Hostname(), port),
		grpc.WithTransportCredentials(credentials.NewTLS(config)),
		grpc.WithDisableRetry(), grpc.WithDisableServiceConfig(),
		grpc.WithDefaultCallOptions(grpc.MaxRetryRPCBufferSize(0)))
	if err != nil {
		return nil, fmt.Errorf("create openshell connection: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &OpenShellTransport{conn: conn, client: pb.NewOpenShellClient(conn), target: target, ctx: ctx, cancel: cancel}, nil
}

func (t *OpenShellTransport) Close() error {
	t.cancel()
	return t.conn.Close()
}

type OpenShellCommand struct {
	RequestID string // canonical nonzero UUID retained by the owner before Start
	Argv      []string
	Workdir   string            // absolute guest path; never a host path resolution
	Env       map[string]string // explicit overrides, not a replacement for guest env
}

func (c OpenShellCommand) validate() error {
	id := c.RequestID
	if len(id) != 36 || id[8] != '-' || id[13] != '-' || id[18] != '-' || id[23] != '-' {
		return fmt.Errorf("openshell request ID is not a canonical nonzero UUID")
	}
	bytes, err := hex.DecodeString(strings.ReplaceAll(id, "-", ""))
	nonzero := false
	for _, b := range bytes {
		nonzero = nonzero || b != 0
	}
	if err != nil || len(bytes) != 16 || !nonzero {
		return fmt.Errorf("openshell request ID is not a canonical nonzero UUID")
	}
	if len(c.Argv) == 0 || strings.TrimSpace(c.Argv[0]) == "" {
		return fmt.Errorf("openshell command missing")
	}
	for _, arg := range c.Argv {
		if strings.ContainsRune(arg, 0) {
			return fmt.Errorf("openshell argument contains NUL")
		}
	}
	if !path.IsAbs(c.Workdir) || strings.ContainsRune(c.Workdir, 0) {
		return fmt.Errorf("openshell workdir is not an absolute guest path")
	}
	if c.Env == nil {
		return fmt.Errorf("openshell environment overrides missing")
	}
	for k, v := range c.Env {
		if k == "" || strings.ContainsAny(k, "=\x00") || strings.ContainsRune(v, 0) {
			return fmt.Errorf("openshell environment entry invalid")
		}
	}
	return nil
}

// Start validates everything before any RPC, then checks Get by workspace/name.
// Get followed by exec is NOT an atomic identity lock. No reconnect, launch retry
// or stdin replay is provided. The native UUID fence cannot recover output.
// A non-nil handle with an error retains a possibly submitted start; the owner
// must Wait and retain its admission, rather than treating it as launch rollback.
func (t *OpenShellTransport) Start(ctx context.Context, command OpenShellCommand) (*OpenShellExec, error) {
	if err := command.validate(); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(t.ctx, cancel)
	release := func() { stop(); cancel() }
	owned := false
	defer func() {
		if !owned {
			release()
		}
	}()
	scope := &dm.WorkspaceSelector{Selection: &dm.WorkspaceSelector_Workspace{Workspace: t.target.Sandbox.Workspace}}
	response, err := t.client.GetSandbox(ctx, &pb.GetSandboxRequest{WorkspaceScope: scope, Name: t.target.Name})
	if err != nil {
		return nil, fmt.Errorf("get openshell sandbox: %w", err)
	}
	meta := response.GetSandbox().GetMetadata()
	if meta.GetId() == "" || meta.GetId() != t.target.Sandbox.SandboxID || meta.GetName() != t.target.Name || meta.GetWorkspace() != t.target.Sandbox.Workspace {
		return nil, fmt.Errorf("openshell sandbox identity missing or conflicts with gateway/workspace/name/ID binding")
	}
	stream, err := t.client.ExecSandboxInteractive(ctx)
	if err != nil {
		return nil, fmt.Errorf("open openshell exec stream: %w", err)
	}
	e := &OpenShellExec{ctx: ctx, cancel: release, stream: stream, output: make(chan OpenShellOutput), done: make(chan struct{}), result: OpenShellExecResult{RequestID: command.RequestID, StartAttempted: true, Reason: "native exec exit unobserved; remote effects unknown"}}
	owned = true
	err = stream.Send(&pb.ExecSandboxInput{Payload: &pb.ExecSandboxInput_Start{Start: &pb.ExecSandboxRequest{
		WorkspaceScope: scope, Sandbox: t.target.Name, RequestId: command.RequestID,
		Command: append([]string(nil), command.Argv...), Workdir: command.Workdir,
		Environment: maps.Clone(command.Env), Tty: false, NoLoginShell: true,
	}}})
	if err != nil {
		e.recordInputError(err)
	}
	go e.receive()
	if err != nil {
		return e, fmt.Errorf("send openshell start; remote effects unknown: %w", err)
	}
	return e, nil
}

type OpenShellOutputStream string

const (
	OpenShellStdout OpenShellOutputStream = "stdout"
	OpenShellStderr OpenShellOutputStream = "stderr"
)

type OpenShellOutput struct {
	Stream OpenShellOutputStream
	Data   []byte
}

// OpenShellExec owns one receiver goroutine. Drain Output concurrently with Write
// and Wait: output is lossless with bounded backpressure until cancellation.
// Write and CloseWrite are serialized; Close cancels blocked send/receive/output.
// Done means local stream completion only; it never releases a worker's VM gate.
type OpenShellExec struct {
	ctx         context.Context
	cancel      context.CancelFunc
	stream      pb.OpenShell_ExecSandboxInteractiveClient
	output      chan OpenShellOutput
	done        chan struct{}
	sendMu      sync.Mutex
	inputClosed bool
	mu          sync.Mutex
	result      OpenShellExecResult
	streamErr   error
}

func (e *OpenShellExec) Output() <-chan OpenShellOutput { return e.output }
func (e *OpenShellExec) Done() <-chan struct{}          { return e.done }

func (e *OpenShellExec) Result() OpenShellExecResult {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.result
}

// Wait preserves native exit and the later stream error independently. EOF
// without native exit returns io.ErrUnexpectedEOF, never an inferred exit zero.
func (e *OpenShellExec) Wait() (OpenShellExecResult, error) {
	<-e.done
	e.sendMu.Lock()
	defer e.sendMu.Unlock()
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.result, e.streamErr
}

// Close abandons only the local stream, not the command or VM. Pending output
// may be discarded on cancellation. The owner must retain unknown remote effects.
func (e *OpenShellExec) Close() error {
	e.cancel()
	_, err := e.Wait()
	return err
}

func (e *OpenShellExec) recordInputError(err error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.result.InputError == "" {
		e.result.InputError = err.Error()
	}
}

func (e *OpenShellExec) Write(data []byte) (int, error) {
	e.sendMu.Lock()
	defer e.sendMu.Unlock()
	if e.inputClosed || e.ctx.Err() != nil {
		return 0, io.ErrClosedPipe
	}
	if len(data) == 0 {
		return 0, nil
	}
	err := e.stream.Send(&pb.ExecSandboxInput{Payload: &pb.ExecSandboxInput_Stdin{Stdin: append([]byte(nil), data...)}})
	if err != nil {
		e.recordInputError(err)
		return 0, err
	}
	return len(data), nil // local send only, not remote consumption evidence
}

func (e *OpenShellExec) CloseWrite() error {
	e.sendMu.Lock()
	defer e.sendMu.Unlock()
	if e.inputClosed {
		return nil
	}
	e.inputClosed = true
	err := e.stream.CloseSend()
	if err != nil {
		e.recordInputError(err)
	}
	return err
}

func (e *OpenShellExec) receive() {
	defer close(e.done)
	defer close(e.output)
	defer e.cancel()
	var err error
	for {
		var event *pb.ExecSandboxEvent
		event, err = e.stream.Recv()
		if err != nil {
			break
		}
		if e.Result().ExitObserved {
			err = fmt.Errorf("openshell event received after native exit")
			break
		}
		var output OpenShellOutput
		switch payload := event.GetPayload().(type) {
		case *pb.ExecSandboxEvent_Stdout:
			output = OpenShellOutput{Stream: OpenShellStdout, Data: payload.Stdout.GetData()}
		case *pb.ExecSandboxEvent_Stderr:
			output = OpenShellOutput{Stream: OpenShellStderr, Data: payload.Stderr.GetData()}
		case *pb.ExecSandboxEvent_Exit:
			if payload.Exit == nil {
				err = fmt.Errorf("openshell native exit payload missing")
				break
			}
			e.mu.Lock()
			e.result.ExitObserved, e.result.ExitCode = true, int(payload.Exit.ExitCode)
			e.result.Reason = ""
			e.mu.Unlock()
			continue
		default:
			err = fmt.Errorf("openshell exec event payload missing or unsupported")
		}
		if err != nil {
			break
		}
		select {
		case e.output <- output:
		case <-e.ctx.Done():
			err = e.ctx.Err()
		}
		if err != nil {
			break
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if errors.Is(err, io.EOF) {
		if e.result.ExitObserved {
			err = nil
		} else {
			err = io.ErrUnexpectedEOF
		}
	}
	e.streamErr = err
	if err != nil {
		e.result.StreamError = err.Error()
	}
	if !e.result.ExitObserved {
		e.result.Reason = "native exec exit unobserved; remote effects unknown"
	}
}
