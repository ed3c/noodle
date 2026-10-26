package runtime

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dm "github.com/NVIDIA/OpenShell/sdk/go/proto/datamodelv1"
	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

const openShellRequestID = "e63f82eb-c577-49ba-b145-394483dbdc08"

type openShellNativeServer struct {
	pb.UnimplementedOpenShellServer
	get        func(context.Context, *pb.GetSandboxRequest) (*pb.SandboxResponse, error)
	exec       func(pb.OpenShell_ExecSandboxInteractiveServer) error
	gets       atomic.Int32
	execs      atomic.Int32
	disconnect func()
}

func (s *openShellNativeServer) GetSandbox(ctx context.Context, req *pb.GetSandboxRequest) (*pb.SandboxResponse, error) {
	s.gets.Add(1)
	if s.get != nil {
		return s.get(ctx, req)
	}
	return &pb.SandboxResponse{Sandbox: &pb.Sandbox{Metadata: &dm.ObjectMeta{Id: "native-vm", Name: "owned-name", Workspace: "owned-workspace"}}}, nil
}

func (s *openShellNativeServer) ExecSandboxInteractive(stream pb.OpenShell_ExecSandboxInteractiveServer) error {
	s.execs.Add(1)
	return s.exec(stream)
}

// These credentials are generated entirely in memory. No user credential or
// gateway/VM is used; the real local gRPC server enforces certificate verification.
func openShellTestTLS(t *testing.T) (OpenShellTLS, *tls.Config) {
	t.Helper()
	key := func() *ecdsa.PrivateKey {
		k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	caKey := key()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	cert := func(serial int64, usage x509.ExtKeyUsage) tls.Certificate {
		k := key()
		c := &x509.Certificate{SerialNumber: big.NewInt(serial), DNSNames: []string{"gateway.test"}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
		der, err := x509.CreateCertificate(rand.Reader, c, ca, &k.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: k}
	}
	return OpenShellTLS{RootCAs: roots, Certificate: cert(2, x509.ExtKeyUsageClientAuth), ServerName: "gateway.test"}, &tls.Config{
		MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert(3, x509.ExtKeyUsageServerAuth)},
		ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots,
	}
}

func openShellTestGateway(t *testing.T, server *openShellNativeServer) (OpenShellTarget, OpenShellTLS) {
	t.Helper()
	security, serverTLS := openShellTestTLS(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("local gRPC listener unavailable (acceptance not observed): %v", err)
	}
	s := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)))
	server.disconnect = s.Stop
	pb.RegisterOpenShellServer(s, server)
	returned := make(chan struct{})
	go func() { defer close(returned); _ = s.Serve(listener) }()
	t.Cleanup(func() { s.Stop(); <-returned })
	return OpenShellTarget{Sandbox: OpenShellSandboxRef{Gateway: "https://" + listener.Addr().String(), Workspace: "owned-workspace", SandboxID: "native-vm"}, Name: "owned-name", ConfigSource: "test-owner"}, security
}

func openShellTestTransport(t *testing.T, server *openShellNativeServer) (*OpenShellTransport, OpenShellTarget) {
	t.Helper()
	target, security := openShellTestGateway(t, server)
	transport, err := NewOpenShellTransport(target, security)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = transport.Close() })
	return transport, target
}

func openShellTestCommand() OpenShellCommand {
	return OpenShellCommand{RequestID: openShellRequestID, Argv: []string{"program", "--rpc", "literal ' $ argument", ""}, Workdir: "/work/task", Env: map[string]string{"MODE": "rpc", "EMPTY": ""}}
}

func openShellTestStart(t *testing.T, transport *OpenShellTransport) *OpenShellExec {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	exec, err := transport.Start(ctx, openShellTestCommand())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exec.Close() })
	return exec
}

func openShellReceiveStart(stream pb.OpenShell_ExecSandboxInteractiveServer) (*pb.ExecSandboxRequest, error) {
	input, err := stream.Recv()
	if err != nil {
		return nil, err
	}
	if input.GetStart() == nil {
		return nil, status.Error(codes.InvalidArgument, "start missing")
	}
	return input.GetStart(), nil
}

func openShellExit(code int32) *pb.ExecSandboxEvent {
	return &pb.ExecSandboxEvent{Payload: &pb.ExecSandboxEvent_Exit{Exit: &pb.ExecSandboxExit{ExitCode: code}}}
}

func TestOpenShellTransportInteractiveHalfCloseAndExit(t *testing.T) {
	for _, code := range []int32{0, 7} {
		t.Run(strconv.Itoa(int(code)), func(t *testing.T) {
			firstInput := make(chan struct{})
			server := &openShellNativeServer{}
			server.get = func(ctx context.Context, req *pb.GetSandboxRequest) (*pb.SandboxResponse, error) {
				if req.Name != "owned-name" || req.WorkspaceScope.GetWorkspace() != "owned-workspace" {
					t.Error("Get changed locator")
				}
				p, ok := peer.FromContext(ctx)
				if !ok {
					t.Error("TLS peer missing")
				} else if info, ok := p.AuthInfo.(credentials.TLSInfo); !ok || len(info.State.VerifiedChains) == 0 {
					t.Error("client certificate not verified")
				}
				return &pb.SandboxResponse{Sandbox: &pb.Sandbox{Metadata: &dm.ObjectMeta{Id: "native-vm", Name: "owned-name", Workspace: "owned-workspace"}}}, nil
			}
			server.exec = func(stream pb.OpenShell_ExecSandboxInteractiveServer) error {
				start, err := openShellReceiveStart(stream)
				if err != nil {
					return err
				}
				command := openShellTestCommand()
				if start.Tty || !start.NoLoginShell || start.RequestId != command.RequestID || start.Sandbox != "owned-name" || start.WorkspaceScope.GetWorkspace() != "owned-workspace" || start.Workdir != command.Workdir || !reflect.DeepEqual(start.Command, command.Argv) || !reflect.DeepEqual(start.Environment, command.Env) || len(start.Stdin) != 0 {
					t.Errorf("incorrect native start metadata: %v", start)
				}
				for i, want := range []string{"first\n", "second\n"} {
					input, err := stream.Recv()
					if err != nil {
						return err
					}
					if string(input.GetStdin()) != want {
						t.Error("stdin content or ordering changed")
					}
					if i == 0 {
						close(firstInput)
					}
				}
				if _, err := stream.Recv(); err != io.EOF {
					return status.Error(codes.InvalidArgument, "stdin half-close missing")
				}
				for _, output := range []*pb.ExecSandboxEvent{
					{Payload: &pb.ExecSandboxEvent_Stdout{Stdout: &pb.ExecSandboxStdout{Data: []byte("{\"ok\":true}\n")}}},
					{Payload: &pb.ExecSandboxEvent_Stderr{Stderr: &pb.ExecSandboxStderr{Data: []byte("diagnostic\n")}}}, openShellExit(code),
				} {
					if err := stream.Send(output); err != nil {
						return err
					}
				}
				return nil
			}
			transport, target := openShellTestTransport(t, server)
			exec := openShellTestStart(t, transport)
			if n, err := exec.Write([]byte("first\n")); n != 6 || err != nil {
				t.Fatalf("first stdin: %d %v", n, err)
			}
			select {
			case <-firstInput:
			case <-exec.ctx.Done():
				t.Fatal("first stdin not received while input remained open")
			}
			if n, err := exec.Write([]byte("second\n")); n != 7 || err != nil {
				t.Fatalf("second stdin: %d %v", n, err)
			}
			if err := exec.CloseWrite(); err != nil {
				t.Fatal(err)
			}
			if err := exec.CloseWrite(); err != nil {
				t.Fatal(err)
			}
			if _, err := exec.Write([]byte("late")); !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("write after half-close: %v", err)
			}
			var output []OpenShellOutput
			for chunk := range exec.Output() {
				output = append(output, chunk)
			}
			want := []OpenShellOutput{{Stream: OpenShellStdout, Data: []byte("{\"ok\":true}\n")}, {Stream: OpenShellStderr, Data: []byte("diagnostic\n")}}
			if !reflect.DeepEqual(output, want) {
				t.Fatalf("output after half-close: %#v", output)
			}
			result, err := exec.Wait()
			if err != nil || !result.ExitObserved || result.ExitCode != int(code) || result.CLIExited || !result.StartAttempted || result.RequestID != openShellRequestID || result.StreamError != "" || result.InputError != "" {
				t.Fatalf("native exit lost: %+v, %v", result, err)
			}
			identity := openShellTestIdentity()
			identity.Sandbox, identity.SandboxName, identity.ConfigSource = target.Sandbox, target.Name, target.ConfigSource
			worker, err := NewOpenShellWorker(identity)
			if err != nil {
				t.Fatal(err)
			}
			worker.RecordExecResult(result)
			assertOpenShellBoundary(t, worker, OpenShellBoundaryUnknown)
			if server.execs.Load() != 1 {
				t.Fatal("exec replayed")
			}
		})
	}
}

func TestOpenShellTransportGetMismatchNeverExecutes(t *testing.T) {
	for _, tc := range []struct {
		name     string
		response *pb.SandboxResponse
		err      error
	}{
		{"not found", nil, status.Error(codes.NotFound, "missing")},
		{"unavailable", nil, status.Error(codes.Unavailable, "gateway lost")},
		{"no sandbox", &pb.SandboxResponse{}, nil},
		{"no metadata", &pb.SandboxResponse{Sandbox: &pb.Sandbox{}}, nil},
		{"empty ID", &pb.SandboxResponse{Sandbox: &pb.Sandbox{Metadata: &dm.ObjectMeta{Name: "owned-name", Workspace: "owned-workspace"}}}, nil},
		{"replacement ID", &pb.SandboxResponse{Sandbox: &pb.Sandbox{Metadata: &dm.ObjectMeta{Id: "replacement", Name: "owned-name", Workspace: "owned-workspace"}}}, nil},
		{"wrong name", &pb.SandboxResponse{Sandbox: &pb.Sandbox{Metadata: &dm.ObjectMeta{Id: "native-vm", Name: "other", Workspace: "owned-workspace"}}}, nil},
		{"wrong workspace", &pb.SandboxResponse{Sandbox: &pb.Sandbox{Metadata: &dm.ObjectMeta{Id: "native-vm", Name: "owned-name", Workspace: "other"}}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := &openShellNativeServer{get: func(context.Context, *pb.GetSandboxRequest) (*pb.SandboxResponse, error) { return tc.response, tc.err }}
			transport, _ := openShellTestTransport(t, server)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			exec, err := transport.Start(ctx, openShellTestCommand())
			if err == nil || exec != nil || server.execs.Load() != 0 || server.gets.Load() != 1 {
				t.Fatalf("Get failure caused an execution: handle=%v error=%v execs=%d", exec, err, server.execs.Load())
			}
		})
	}
}

func TestOpenShellTransportValidatesCommandBeforeRPC(t *testing.T) {
	server := &openShellNativeServer{}
	transport, _ := openShellTestTransport(t, server)
	for name, invalidate := range map[string]func(*OpenShellCommand){
		"missing UUID":     func(c *OpenShellCommand) { c.RequestID = "" },
		"zero UUID":        func(c *OpenShellCommand) { c.RequestID = "00000000-0000-0000-0000-000000000000" },
		"invalid UUID":     func(c *OpenShellCommand) { c.RequestID = "g63f82eb-c577-49ba-b145-394483dbdc08" },
		"short UUID":       func(c *OpenShellCommand) { c.RequestID = "e63f82ebc57749bab145394483dbdc08" },
		"missing argv":     func(c *OpenShellCommand) { c.Argv = nil },
		"empty program":    func(c *OpenShellCommand) { c.Argv[0] = "" },
		"NUL argument":     func(c *OpenShellCommand) { c.Argv[1] = "\x00" },
		"relative workdir": func(c *OpenShellCommand) { c.Workdir = "work" },
		"NUL workdir":      func(c *OpenShellCommand) { c.Workdir = "/work\x00" },
		"missing env":      func(c *OpenShellCommand) { c.Env = nil },
		"empty env key":    func(c *OpenShellCommand) { c.Env[""] = "value" },
		"equals env key":   func(c *OpenShellCommand) { c.Env["A=B"] = "value" },
		"NUL env key":      func(c *OpenShellCommand) { c.Env["A\x00"] = "value" },
		"NUL env value":    func(c *OpenShellCommand) { c.Env["A"] = "value\x00" },
	} {
		t.Run(name, func(t *testing.T) {
			command := openShellTestCommand()
			invalidate(&command)
			if exec, err := transport.Start(t.Context(), command); err == nil || exec != nil {
				t.Fatalf("invalid command accepted: %v", err)
			}
		})
	}
	if server.gets.Load() != 0 || server.execs.Load() != 0 {
		t.Fatal("invalid input reached native API")
	}
}

func TestOpenShellTransportTerminalEvidence(t *testing.T) {
	for _, tc := range []struct {
		name      string
		exit      *pb.ExecSandboxEvent
		streamErr error
		wantExit  bool
		wantCode  int
	}{
		{"EOF without exit", nil, nil, false, 0},
		{"disconnect without exit", nil, status.Error(codes.Unavailable, "relay disconnected"), false, 0},
		{"exit then error", openShellExit(19), status.Error(codes.DataLoss, "trailing stream error"), true, 19},
		{"zero exit then error", openShellExit(0), status.Error(codes.Unavailable, "trailing stream error"), true, 0},
		{"invalid event", &pb.ExecSandboxEvent{}, nil, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := &openShellNativeServer{exec: func(stream pb.OpenShell_ExecSandboxInteractiveServer) error {
				if _, err := openShellReceiveStart(stream); err != nil {
					return err
				}
				if tc.exit != nil {
					if err := stream.Send(tc.exit); err != nil {
						return err
					}
				}
				return tc.streamErr
			}}
			transport, _ := openShellTestTransport(t, server)
			exec := openShellTestStart(t, transport)
			for range exec.Output() {
			}
			result, err := exec.Wait()
			if err == nil || result.StreamError == "" || result.ExitObserved != tc.wantExit || result.ExitCode != tc.wantCode || result.CLIExited || (!tc.wantExit && !strings.Contains(result.Reason, "remote effects unknown")) {
				t.Fatalf("evidence conflated: %+v %v", result, err)
			}
			if tc.name == "EOF without exit" && !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("missing exit error = %v", err)
			}
			if tc.streamErr != nil && status.Code(err) != status.Code(tc.streamErr) {
				t.Fatalf("native status lost: %v", err)
			}
			worker := newOpenShellTestWorker(t)
			worker.RecordExecResult(result)
			assertOpenShellBoundary(t, worker, OpenShellBoundaryUnknown)
			if server.execs.Load() != 1 {
				t.Fatal("stream failure retried execution")
			}
		})
	}
}

func TestOpenShellTransportDuplicateDoesNotReplay(t *testing.T) {
	var launches, inputs atomic.Int32
	var mu sync.Mutex
	seen := make(map[string]bool)
	server := &openShellNativeServer{exec: func(stream pb.OpenShell_ExecSandboxInteractiveServer) error {
		start, err := openShellReceiveStart(stream)
		if err != nil {
			return err
		}
		mu.Lock()
		duplicate := seen[start.RequestId]
		seen[start.RequestId] = true
		mu.Unlock()
		if duplicate {
			return status.Error(codes.AlreadyExists, "execution terminated; output not stored; not launched again")
		}
		launches.Add(1)
		for {
			input, err := stream.Recv()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			if string(input.GetStdin()) != "once" {
				return status.Error(codes.InvalidArgument, "stdin changed")
			}
			inputs.Add(1)
		}
		return stream.Send(openShellExit(3))
	}}
	transport, _ := openShellTestTransport(t, server)
	first := openShellTestStart(t, transport)
	if _, err := first.Write([]byte("once")); err != nil {
		t.Fatal(err)
	}
	if err := first.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	for range first.Output() {
	}
	if result, err := first.Wait(); err != nil || !result.ExitObserved || result.ExitCode != 3 {
		t.Fatalf("first result: %+v %v", result, err)
	}
	second := openShellTestStart(t, transport) // explicit owner duplicate, never internal replay
	var chunks int
	for range second.Output() {
		chunks++
	}
	result, err := second.Wait()
	if status.Code(err) != codes.AlreadyExists || result.ExitObserved || chunks != 0 || launches.Load() != 1 || inputs.Load() != 1 || server.execs.Load() != 2 {
		t.Fatalf("duplicate recovered/replayed evidence: %+v %v launches=%d inputs=%d", result, err, launches.Load(), inputs.Load())
	}
}

func TestOpenShellTransportTLSFailsClosed(t *testing.T) {
	server := &openShellNativeServer{}
	target, security := openShellTestGateway(t, server)
	for name, change := range map[string]func(*OpenShellTLS){
		"untrusted CA":          func(c *OpenShellTLS) { c.RootCAs = x509.NewCertPool() },
		"wrong server identity": func(c *OpenShellTLS) { c.ServerName = "other.test" },
		"untrusted client":      func(c *OpenShellTLS) { other, _ := openShellTestTLS(t); c.Certificate = other.Certificate },
	} {
		t.Run(name, func(t *testing.T) {
			c := security
			change(&c)
			transport, err := NewOpenShellTransport(target, c)
			if err != nil {
				t.Fatal(err)
			}
			defer transport.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			if exec, err := transport.Start(ctx, openShellTestCommand()); err == nil || exec != nil {
				t.Fatalf("TLS rejection accepted: %v", err)
			}
		})
	}
	if server.gets.Load() != 0 || server.execs.Load() != 0 {
		t.Fatal("TLS failure reached native API")
	}
}

func TestOpenShellTransportRequiresExplicitBindingAndMTLS(t *testing.T) {
	security, _ := openShellTestTLS(t)
	original := OpenShellTarget{Sandbox: OpenShellSandboxRef{Gateway: "https://gateway.test:443", Workspace: "owned-workspace", SandboxID: "native-vm"}, Name: "owned-name", ConfigSource: "owner"}
	for name, change := range map[string]func(*OpenShellTarget, *OpenShellTLS){
		"gateway":        func(c *OpenShellTarget, _ *OpenShellTLS) { c.Sandbox.Gateway = "" },
		"workspace":      func(c *OpenShellTarget, _ *OpenShellTLS) { c.Sandbox.Workspace = " " },
		"ID":             func(c *OpenShellTarget, _ *OpenShellTLS) { c.Sandbox.SandboxID = "" },
		"name":           func(c *OpenShellTarget, _ *OpenShellTLS) { c.Name = "" },
		"source":         func(c *OpenShellTarget, _ *OpenShellTLS) { c.ConfigSource = "" },
		"plaintext":      func(c *OpenShellTarget, _ *OpenShellTLS) { c.Sandbox.Gateway = "http://gateway.test" },
		"userinfo":       func(c *OpenShellTarget, _ *OpenShellTLS) { c.Sandbox.Gateway = "https://user:secret@gateway.test" },
		"query":          func(c *OpenShellTarget, _ *OpenShellTLS) { c.Sandbox.Gateway = "https://gateway.test?token=secret" },
		"path":           func(c *OpenShellTarget, _ *OpenShellTLS) { c.Sandbox.Gateway = "https://gateway.test/api" },
		"no CA":          func(_ *OpenShellTarget, c *OpenShellTLS) { c.RootCAs = nil },
		"no client cert": func(_ *OpenShellTarget, c *OpenShellTLS) { c.Certificate.Certificate = nil },
		"no client key":  func(_ *OpenShellTarget, c *OpenShellTLS) { c.Certificate.PrivateKey = nil },
	} {
		t.Run(name, func(t *testing.T) {
			target, tlsConfig := original, security
			change(&target, &tlsConfig)
			transport, err := NewOpenShellTransport(target, tlsConfig)
			if transport != nil {
				defer transport.Close()
			}
			if err == nil || transport != nil || strings.Contains(err.Error(), "secret") {
				t.Fatalf("invalid binding accepted or secret disclosed: %v", err)
			}
		})
	}
}

type openShellWriteResult struct {
	n   int
	err error
}

// A successful Send only means local acceptance. Keep sending bounded chunks
// until a specific Write stays pending while the server never Recvs stdin.
// Its completion must later report an error, so cancellation between two writes
// (or while reporting progress) cannot masquerade as unblocking a Send.
func openShellBlockStdin(t *testing.T, exec *OpenShellExec) (<-chan openShellWriteResult, <-chan struct{}) {
	t.Helper()
	const maxWrites = 128
	payload := bytes.Repeat([]byte("x"), 64*1024)
	attempts := make(chan struct{})
	completed := make(chan openShellWriteResult, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range maxWrites {
			select {
			case attempts <- struct{}{}:
			case <-exec.ctx.Done():
				return
			}
			n, err := exec.Write(payload)
			completed <- openShellWriteResult{n: n, err: err}
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		exec.cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("stdin sender leaked during cleanup")
		}
	})
	for sent := 0; sent < maxWrites; sent++ {
		select {
		case <-attempts:
		case <-exec.ctx.Done():
			t.Fatal("stream cancelled before stdin backpressure was established")
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case result := <-completed:
			timer.Stop()
			if result.err != nil || result.n != len(payload) {
				t.Fatalf("stdin failed before backpressure: %d bytes, %v", result.n, result.err)
			}
		case <-timer.C:
			if sent == 0 {
				t.Fatal("no stdin write completed before the pending send")
			}
			t.Logf("%d stdin writes of %d bytes completed; next Write pending", sent, len(payload))
			return completed, done
		case <-exec.ctx.Done():
			timer.Stop()
			t.Fatal("stream cancelled while establishing stdin backpressure")
		}
	}
	t.Fatalf("stdin accepted all %d chunks; backpressure was not established within the byte limit", maxWrites)
	return nil, nil
}

func TestOpenShellTransportCancellationUnblocksIO(t *testing.T) {
	for _, mode := range []string{"receive", "output backpressure", "input backpressure", "transport close", "transport close with output backpressure", "transport close with input backpressure", "gateway disconnect", "native exit then cancel"} {
		t.Run(mode, func(t *testing.T) {
			outputBlocked := strings.Contains(mode, "output backpressure")
			inputBlocked := strings.Contains(mode, "input backpressure")
			entered, returned := make(chan struct{}), make(chan struct{})
			server := &openShellNativeServer{exec: func(stream pb.OpenShell_ExecSandboxInteractiveServer) error {
				defer close(returned)
				if _, err := openShellReceiveStart(stream); err != nil {
					return err
				}
				close(entered)
				if mode == "native exit then cancel" {
					if err := stream.Send(openShellExit(23)); err != nil {
						return err
					}
				}
				if outputBlocked {
					for {
						if err := stream.Send(&pb.ExecSandboxEvent{Payload: &pb.ExecSandboxEvent_Stdout{Stdout: &pb.ExecSandboxStdout{Data: bytes.Repeat([]byte("x"), 64*1024)}}}); err != nil {
							return err
						}
					}
				}
				<-stream.Context().Done()
				return stream.Context().Err()
			}}
			transport, _ := openShellTestTransport(t, server)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			exec, err := transport.Start(ctx, openShellTestCommand())
			if err != nil {
				t.Fatal(err)
			}
			defer exec.Close()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("server did not receive start")
			}
			if mode == "native exit then cancel" {
				for !exec.Result().ExitObserved {
					select {
					case <-ctx.Done():
						t.Fatal("native exit not observed")
					case <-time.After(time.Millisecond):
					}
				}
			}
			var written <-chan openShellWriteResult
			var senderDone <-chan struct{}
			if inputBlocked {
				written, senderDone = openShellBlockStdin(t, exec)
			}
			if outputBlocked {
				// Observe a real frame first, then stop draining while the server
				// continues producing output beyond the bounded receive window.
				select {
				case chunk := <-exec.Output():
					if chunk.Stream != OpenShellStdout || len(chunk.Data) != 64*1024 {
						t.Fatal("output frame missing")
					}
				case <-ctx.Done():
					t.Fatal("output not received")
				}
				select {
				case <-exec.Done():
					t.Fatal("undrained output completed")
				case <-time.After(100 * time.Millisecond):
				}
			}
			if mode == "gateway disconnect" {
				server.disconnect()
			} else if strings.HasPrefix(mode, "transport close") {
				if err := transport.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				cancel()
			}
			select {
			case <-exec.Done():
			case <-time.After(3 * time.Second):
				t.Fatal("local receiver leaked under cancellation")
			}
			if inputBlocked {
				select {
				case result := <-written:
					if result.err == nil || result.n != 0 {
						t.Fatalf("pending Send completed without cancellation error: %d bytes, %v", result.n, result.err)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("pending stdin Send did not unblock")
				}
				select {
				case <-senderDone:
				case <-time.After(3 * time.Second):
					t.Fatal("stdin sender did not exit after its Send unblocked")
				}
			}
			select {
			case <-returned:
			case <-time.After(3 * time.Second):
				t.Fatal("native stream not cancelled")
			}
			result, err := exec.Wait()
			if err == nil || result.StreamError == "" || result.ExitObserved != (mode == "native exit then cancel") || (result.ExitObserved && result.ExitCode != 23) {
				t.Fatalf("cancellation fabricated/lost exit: %+v %v", result, err)
			}
			worker := newOpenShellTestWorker(t)
			worker.RecordExecResult(result)
			assertOpenShellBoundary(t, worker, OpenShellBoundaryUnknown)
			if server.execs.Load() != 1 {
				t.Fatal("lost or cancelled stream relaunched execution")
			}
		})
	}
}
