package runtime

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"github.com/poteto/noodle/internal/admission"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

type managedGatewayServer struct {
	*openShellNativeServer
	root string
}

func (s *managedGatewayServer) event(kind string) {
	f, err := os.OpenFile(filepath.Join(s.root, "events"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err == nil {
		_, _ = fmt.Fprintln(f, kind)
		_ = f.Close()
	}
}
func (s *managedGatewayServer) Health(ctx context.Context, _ *pb.HealthRequest) (*pb.HealthResponse, error) {
	s.event("health")
	if _, err := os.Stat(filepath.Join(s.root, "stall")); err == nil {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	version := "test-version"
	if _, err := os.Stat(filepath.Join(s.root, "wrong-version")); err == nil {
		version = "wrong"
	}
	return &pb.HealthResponse{Status: pb.ServiceStatus_SERVICE_STATUS_HEALTHY, Version: version}, nil
}
func (s *managedGatewayServer) GetGatewayInfo(context.Context, *pb.GetGatewayInfoRequest) (*pb.GetGatewayInfoResponse, error) {
	s.event("info")
	driver := "test-driver"
	if _, err := os.Stat(filepath.Join(s.root, "wrong-driver")); err == nil {
		driver = "wrong"
	}
	return &pb.GetGatewayInfoResponse{Status: pb.ServiceStatus_SERVICE_STATUS_HEALTHY, GatewayVersion: "test-version", ComputeDrivers: []*pb.ComputeDriverInfo{{Name: driver}}}, nil
}

type managedChildConfig struct {
	Address    string
	ServerCert [][]byte
	ServerKey  []byte
	ClientCA   []byte
}

// The sole managed child itself binds TCP and serves verified mTLS gRPC. No
// independent prestarted server may stand in for its process ownership.
func TestManagedOpenShellGatewayChild(t *testing.T) {
	root := os.Getenv("NOODLE_MANAGED_GATEWAY_CHILD")
	if root == "" {
		return
	}
	data, err := os.ReadFile(filepath.Join(root, "child.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg managedChildConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	key, err := x509.ParsePKCS8PrivateKey(cfg.ServerKey)
	if err != nil {
		t.Fatal(err)
	}
	ca := x509.NewCertPool()
	client, err := x509.ParseCertificate(cfg.ClientCA)
	if err != nil {
		t.Fatal(err)
	}
	ca.AddCert(client)
	// Read the original durable record; the parent may already have published
	// running by the time this executable gets scheduled.
	rows, err := os.ReadFile(filepath.Join(root, "residents.json"))
	if err != nil {
		t.Fatal(err)
	}
	var records map[string]admission.Resident
	if err := json.Unmarshal(rows, &records); err != nil || (records["gateway"].State != "starting" && (records["gateway"].State != "running" || records["gateway"].PID != os.Getpid())) {
		t.Fatalf("starting before spawn missing: %s %v", rows, err)
	}
	server := &managedGatewayServer{root: root}
	server.event("spawn " + strconv.Itoa(os.Getpid()))
	deadline := time.Now().Add(20 * time.Second)
	exiting := func() bool {
		_, err := os.Stat(filepath.Join(root, "exit"))
		return err == nil || time.Now().After(deadline)
	}
	for {
		if exiting() {
			return
		}
		if _, err := os.Stat(filepath.Join(root, "serve")); err == nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	listener, err := net.Listen("tcp", cfg.Address)
	if err != nil {
		t.Fatal(err)
	}
	g, native := custodyServer(t)
	if _, err := os.Stat(filepath.Join(root, "completion-observation")); err == nil {
		g.afterCreate = func() {
			for _, sandbox := range g.sandboxes {
				sandbox.Status.MainProcessInstanceId = "original-main"
			}
		}
	}
	if _, err := os.Stat(filepath.Join(root, "provider-ready")); err == nil {
		g.afterCreate = func() {
			for _, sandbox := range g.sandboxes {
				sandbox.Status.MainProcessInstanceId = "11111111-1111-4111-8111-111111111111"
			}
		}
	}
	nativeList, nativeGet := native.list, native.get
	native.list = func(ctx context.Context, req *pb.ListSandboxesRequest) (*pb.ListSandboxesResponse, error) {
		server.event("list")
		if _, err := os.Stat(filepath.Join(root, "inventory-unavailable")); err == nil {
			return nil, status.Error(codes.Unavailable, "temporary inventory outage")
		}
		if _, err := os.Stat(filepath.Join(root, "extra-vm")); err == nil {
			return &pb.ListSandboxesResponse{Sandboxes: []*pb.Sandbox{custodySandbox("external", "external", pb.SandboxPhase_SANDBOX_PHASE_STOPPED)}}, nil
		}
		return nativeList(ctx, req)
	}
	native.get = func(ctx context.Context, req *pb.GetSandboxRequest) (*pb.SandboxResponse, error) {
		server.event("get")
		return nativeGet(ctx, req)
	}
	if _, err := os.Stat(filepath.Join(root, "retained")); err == nil {
		g.sandboxes["retained"] = custodySandbox("retained", "retained-id", pb.SandboxPhase_SANDBOX_PHASE_STOPPED)
	}
	if _, err := os.Stat(filepath.Join(root, "runtime-dispatch")); err == nil {
		native.exec = func(stream pb.OpenShell_ExecSandboxInteractiveServer) error {
			start, err := openShellReceiveStart(stream)
			if err != nil {
				return err
			}
			isStop := len(start.GetCommand()) > 1 && start.GetCommand()[1] == "stop"
			if _, err := os.Stat(filepath.Join(root, "progress-await-stop")); err == nil {
				name := "progress-supervisor"
				if isStop {
					name = "progress-control"
				}
				if err := os.WriteFile(filepath.Join(root, name+"-started"), nil, 0600); err != nil {
					return err
				}
				for {
					if _, err := os.Stat(filepath.Join(root, name+"-release")); err == nil {
						break
					}
					select {
					case <-stream.Context().Done():
						return stream.Context().Err()
					case <-time.After(5 * time.Millisecond):
					}
				}
				if isStop {
					if _, err := os.Stat(filepath.Join(root, "progress-control-unknown")); err == nil {
						return status.Error(codes.Unavailable, "control exit acknowledgement lost")
					}
					return stream.Send(openShellExit(0))
				}
			}
			for {
				_, err := stream.Recv()
				if err == io.EOF {
					break
				}
				if err != nil {
					return err
				}
			}
			data := []byte("{\"type\":\"thread.started\",\"thread_id\":\"12345678-1234-4234-8234-123456789012\"}\n{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"done\"}}\n{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}\n")
			if err := stream.Send(&pb.ExecSandboxEvent{Payload: &pb.ExecSandboxEvent_Stdout{Stdout: &pb.ExecSandboxStdout{Data: data}}}); err != nil {
				return err
			}
			return stream.Send(openShellExit(0))
		}
	}
	server.openShellNativeServer = native
	grpcServer := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: cfg.ServerCert, PrivateKey: key}}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: ca})))
	pb.RegisterOpenShellServer(grpcServer, server)
	done := make(chan error, 1)
	go func() { done <- grpcServer.Serve(listener) }()
	defer grpcServer.Stop()
	if err := os.WriteFile(filepath.Join(root, "listening"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	for !exiting() {
		select {
		case err := <-done:
			t.Fatal(err)
		case <-time.After(5 * time.Millisecond):
		}
	}
}

type managedFixture struct {
	root      string
	spec      OpenShellGatewaySpec
	residents *admission.Residents
	budget    *admission.Budget
	owner     *OpenShellGateway
}

func managedMarker(t *testing.T, root, name string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), nil, 0600); err != nil {
		t.Fatal(err)
	}
}
func managedAwait(t *testing.T, root, name string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(filepath.Join(root, name)); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("managed child did not produce %s", name)
}
func newManagedFixture(t *testing.T, serve bool) *managedFixture {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Residents graceful release unsupported on Windows")
	}
	root := t.TempDir()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("managed child TCP/mTLS acceptance unobserved: %v", err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	security, serverTLS := openShellTestTLS(t)
	key, err := x509.MarshalPKCS8PrivateKey(serverTLS.Certificates[0].PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	cfg := managedChildConfig{Address: address, ServerCert: serverTLS.Certificates[0].Certificate, ServerKey: key, ClientCA: security.Certificate.Certificate[len(security.Certificate.Certificate)-1]}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "child.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	r, err := admission.OpenResidents(root)
	if err != nil {
		t.Fatal(err)
	}
	b := admission.NewBudget(3)
	b.Register("a")
	b.Register("b")
	b.Observe(admission.Sample{At: time.Now(), Unsupported: true}, nil)
	f := &managedFixture{root: root, residents: r, budget: b, spec: OpenShellGatewaySpec{
		Resident: admission.ResidentSpec{Name: "gateway", Argv: []string{exe, "-test.run=^TestManagedOpenShellGatewayChild$"}, Directory: root, Environment: []string{"NOODLE_MANAGED_GATEWAY_CHILD=" + root}, Sharing: "shared", Transport: "network"},
		Binding:  custodyBinding(t, "https://"+address), TLS: security, Version: "test-version", ComputeDriver: "test-driver", ReadinessTimeout: 3 * time.Second,
	}}
	if serve {
		managedMarker(t, root, "serve")
	}
	t.Cleanup(func() {
		managedMarker(t, root, "exit")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := r.WaitClosed(ctx); err != nil {
			t.Error(err)
		}
		if f.owner != nil {
			if err := f.owner.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	return f
}
func (f *managedFixture) open(t *testing.T) *OpenShellGateway {
	t.Helper()
	o, err := NewOpenShellGateway(f.residents, f.budget, f.spec)
	f.owner = o
	if err != nil {
		t.Fatal(err)
	}
	return o
}
func (f *managedFixture) spawns(t *testing.T, intended int) {
	t.Helper()
	data, _ := os.ReadFile(filepath.Join(f.root, "events"))
	if got := strings.Count(string(data), "spawn "); got != intended {
		t.Fatalf("spawns=%d want=%d: %s", got, intended, data)
	}
}
func (f *managedFixture) retained(t *testing.T, expected admission.Resident) {
	t.Helper()
	rows, err := f.residents.Snapshot()
	if err != nil || len(rows) != 1 || rows[0].PID != expected.PID || rows[0].State != "running" || len(rows[0].Contexts) == 0 || f.budget.Snapshot().Allocations != 1 || rows[0].StopRequested != "" {
		t.Fatalf("original obligations lost: %+v %v %+v", rows, err, f.budget.Snapshot())
	}
}

func TestManagedOpenShellGatewaySharingAndRelease(t *testing.T) {
	f := newManagedFixture(t, true)
	o := f.open(t)
	c, err := o.Acquire(t.Context(), "a")
	if err != nil {
		t.Fatal(err)
	}
	first := o.resident
	for _, id := range []string{"a", "b"} {
		other, err := o.Acquire(t.Context(), id)
		if err != nil || other != c || o.resident.PID != first.PID {
			t.Fatalf("shared child lost: %v", err)
		}
	}
	f.spawns(t, 1)
	if err := c.Close(); err == nil {
		t.Fatal("borrowed custody dropped gateway lock")
	}
	if err := o.Close(); err == nil {
		t.Fatal("live gateway close accepted")
	}
	if _, err := NewOpenShellGateway(f.residents, f.budget, f.spec); err == nil {
		t.Fatal("exclusive lock bypassed")
	}
	if _, err := o.Release("b"); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Acquire(t.Context(), "b"); err == nil {
		t.Fatal("released context replayed")
	}
	record := custodyCreate(t, c, "busy")
	if _, err := o.Release("a"); err == nil || !strings.Contains(err.Error(), "READY") {
		t.Fatalf("busy release accepted: %v", err)
	}
	f.retained(t, first)
	if err := c.StopSandbox(t.Context(), record.Worker.Identity.Sandbox); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Release("a"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := f.residents.WaitClosed(ctx); err != nil {
		t.Fatal(err)
	}
	if f.budget.Snapshot().Allocations != 0 {
		t.Fatal("quota retained after native exit")
	}
	if _, err := o.Acquire(t.Context(), "a"); err == nil {
		t.Fatal("exited child restarted")
	}
	if _, err := o.Readback(t.Context()); err == nil {
		t.Fatal("late readback reactivated quiesced owner")
	}
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
	f.spawns(t, 1)
}

func TestManagedOpenShellGatewayDelayedOriginalReadback(t *testing.T) {
	f := newManagedFixture(t, false)
	o := f.open(t)
	firstCtx, firstCancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer firstCancel()
	if c, err := o.Acquire(firstCtx, "a"); c != nil || err == nil {
		t.Fatal("unstarted gateway ready")
	}
	managedAwait(t, f.root, "events")
	first := o.resident
	f.retained(t, first)
	if _, err := o.Release("a"); err == nil {
		t.Fatal("uninitialized release accepted")
	}
	if err := o.Close(); err == nil {
		t.Fatal("uninitialized close discarded original lock")
	}
	if _, err := o.custody.Create(t.Context(), OpenShellTask{"x", "x", "x"}, &pb.SandboxSpec{}); err == nil {
		t.Fatal("uninitialized VM mutation accepted")
	}
	managedMarker(t, f.root, "serve")
	managedAwait(t, f.root, "listening")
	// Repeated acquire is not an implicit readiness retry.
	if _, err := o.Acquire(t.Context(), "a"); err == nil {
		t.Fatal("acquire retried readiness")
	}
	if _, err := o.Readback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if o.resident.PID != first.PID {
		t.Fatal("readback selected replacement")
	}
	f.spawns(t, 1)
	if _, err := o.Release("a"); err != nil {
		t.Fatal(err)
	}
}

func TestManagedOpenShellGatewayNativeFailures(t *testing.T) {
	for _, mode := range []string{"wrong-version", "wrong-driver", "extra-vm", "inventory-unavailable", "stall"} {
		t.Run(mode, func(t *testing.T) {
			f := newManagedFixture(t, true)
			managedMarker(t, f.root, mode)

			o := f.open(t)
			if c, err := o.Acquire(t.Context(), "a"); c != nil || err == nil {
				t.Fatal("native failure reported ready")
			}
			first := o.resident
			f.retained(t, first)
			if o.Diagnostic() == nil {
				t.Fatal("diagnostic lost")
			}
			if _, err := o.Release("a"); err == nil {
				t.Fatal("failed native proof allowed release")
			}
			if err := os.Remove(filepath.Join(f.root, mode)); err != nil {
				t.Fatal(err)
			}
			c, err := o.Readback(t.Context())
			transient := mode == "inventory-unavailable" || mode == "stall"
			if (err == nil) != transient {
				t.Fatalf("readback transient/conflict distinction lost: %v", err)
			}
			if transient {
				if c == nil {
					t.Fatal("successful readback lost custody")
				}
				if _, err := o.Release("a"); err != nil {
					t.Fatal(err)
				}
			}
			f.spawns(t, 1)
		})
	}
}

func TestManagedOpenShellGatewayExternalListenerAndUnknownHost(t *testing.T) {
	f := newManagedFixture(t, true)
	o := f.open(t)
	// A listener exists before the first acquire: native health is irrelevant.
	address := strings.TrimPrefix(f.spec.Binding.Gateway, "https://")
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.Acquire(t.Context(), "a"); err == nil {
		t.Fatal("external listener adopted")
	}
	f.spawns(t, 0)
	_ = listener.Close()
	if _, err := o.Acquire(t.Context(), "a"); err != nil {
		t.Fatal(err)
	}
	old, err := admission.OpenResidents(f.root)
	if err != nil {
		t.Fatal(err)
	}
	// Emulate restart using a different root only to avoid stealing the live lock.
	spec := f.spec
	spec.Binding.StateRoot = t.TempDir()
	unknown, err := NewOpenShellGateway(old, f.budget, spec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unknown.Acquire(t.Context(), "b"); err == nil {
		t.Fatal("unknown Host child adopted")
	}
	if _, err := unknown.Readback(t.Context()); err == nil {
		t.Fatal("unknown generation read back")
	}
	if err := unknown.Close(); err == nil {
		t.Fatal("unknown obligations discarded")
	}
	f.spawns(t, 1)
	// Close only once the original owner has native terminal evidence and the
	// reopened owner has explicitly reconciled absence. No signal via old PID.
	managedMarker(t, f.root, "exit")
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := f.residents.WaitClosed(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	if err := unknown.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestManagedOpenShellGatewayEarlyExitAndCancellation(t *testing.T) {
	for _, kind := range []string{"exit", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			f := newManagedFixture(t, false)
			o := f.open(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := make(chan error, 1)
			go func() { _, err := o.Acquire(ctx, "a"); result <- err }()
			managedAwait(t, f.root, "events")
			if kind == "exit" {
				managedMarker(t, f.root, "exit")
			} else {
				cancel()
			}
			select {
			case err := <-result:
				if err == nil {
					t.Fatal("incomplete startup accepted")
				}
			case <-time.After(5 * time.Second):
				t.Fatal("startup cancellation unbounded")
			}
			if kind == "cancel" {
				f.retained(t, o.resident)
			} else {
				waitCtx, stop := context.WithTimeout(t.Context(), 5*time.Second)
				defer stop()
				if err := f.residents.WaitClosed(waitCtx); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := o.Readback(t.Context()); err == nil {
				t.Fatal("failed generation reported ready")
			}
			f.spawns(t, 1)
		})
	}
}

func TestManagedOpenShellGatewayFrozenInputsAndConfigurationConflict(t *testing.T) {
	f := newManagedFixture(t, true)
	original := f.spec
	o := f.open(t)
	f.spec.Resident.Argv[1] = "-test.run=NoSuchTest"
	f.spec.Resident.Environment[0] = "BROKEN=1"
	f.spec.TLS.Certificate.Certificate[0][0] ^= 0xff
	f.spec.TLS.RootCAs.AddCert(&x509.Certificate{RawSubject: []byte("other")})
	c, err := o.Acquire(t.Context(), "a")
	if err != nil {
		t.Fatal("caller mutated accepted input", err)
	}
	// Residents rejects a second source/config identity on the same named child.
	spec := original
	spec.Resident = o.spec.Resident
	guard, err := admission.NewResidentGuard("different-source", time.Second, c.Quiesce)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.residents.AcquireGuarded(spec.Resident, "b", f.budget, guard); err == nil {
		t.Fatal("source conflict borrowed child")
	}
	spec.Resident.Argv = slices.Clone(o.spec.Resident.Argv)
	spec.Resident.Argv = append(spec.Resident.Argv, "-test.v")
	if _, err := f.residents.AcquireGuarded(spec.Resident, "b", f.budget, o.guard); err == nil {
		t.Fatal("command conflict borrowed child")
	}
	f.spawns(t, 1)
}

func TestManagedOpenShellGatewayConcurrentReadbackReleaseClose(t *testing.T) {
	f := newManagedFixture(t, true)
	o := f.open(t)
	if _, err := o.Acquire(t.Context(), "a"); err != nil {
		t.Fatal(err)
	}
	managedMarker(t, f.root, "stall")
	result := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
		defer cancel()
		_, err := o.Readback(ctx)
		result <- err
	}()
	// Observe actual readback serialization before racing the operations.
	deadline := time.Now().Add(time.Second)
	for o.mu.TryLock() {
		o.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("readback never entered")
		}
		time.Sleep(time.Millisecond)
	}
	var wg sync.WaitGroup
	for _, op := range []func() error{func() error { _, err := o.Acquire(t.Context(), "b"); return err }, func() error { _, err := o.Release("a"); return err }, o.Close} {
		wg.Go(func() {
			if err := op(); err == nil {
				t.Error("concurrent operation bypassed readback")
			}
		})
	}
	wg.Wait()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("stalled readback ready")
		}
	case <-time.After(time.Second):
		t.Fatal("readback deadlocked")
	}
	if _, err := o.Release("a"); err == nil {
		t.Fatal("late release used stale readiness")
	}
	if err := os.Remove(filepath.Join(f.root, "stall")); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Readback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := o.Release("a"); err != nil {
		t.Fatal(err)
	}
}

func TestManagedOpenShellGatewayQuotaAndRetainedInitialization(t *testing.T) {
	t.Run("quota", func(t *testing.T) {
		f := newManagedFixture(t, true)
		o := f.open(t)
		var permits []*admission.Permit
		for range 3 {
			p, err := f.budget.TryAcquire("a")
			if err != nil {
				t.Fatal(err)
			}
			permits = append(permits, p)
		}
		defer func() {
			for _, p := range permits {
				p.Release()
			}
		}()
		if _, err := o.Acquire(t.Context(), "a"); err == nil {
			t.Fatal("quota refusal bypassed")
		}
		f.spawns(t, 0)
		rows, err := f.residents.Snapshot()
		if err != nil || len(rows) != 0 {
			t.Fatalf("quota refusal spawned: %+v %v", rows, err)
		}
		for _, p := range permits {
			p.Release()
		}
		if _, err := o.Acquire(t.Context(), "a"); err != nil {
			t.Fatal(err)
		}
		f.spawns(t, 1)
		if _, err := o.Release("a"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("retained", func(t *testing.T) {
		f := newManagedFixture(t, false)
		f.spec.Retained = []OpenShellRetainedSandbox{{"retained", "retained-id", f.spec.Binding.Workspace}}
		managedMarker(t, f.root, "retained")
		o := f.open(t)
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		if _, err := o.Acquire(ctx, "a"); err == nil {
			t.Fatal("unstarted retained gateway ready")
		}
		if got := custodyDisk(t, f.spec.Binding).PendingRetained; len(got) != 1 || got[0].SandboxID != "retained-id" {
			t.Fatalf("startup lost retained source: %+v", got)
		}
		managedMarker(t, f.root, "serve")
		managedAwait(t, f.root, "listening")
		c, err := o.Readback(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		state := c.Snapshot()
		if len(state.Records) != 1 || state.Records[0].Worker.Identity.Sandbox.SandboxID != "retained-id" || state.Records[0].Stopped == nil {
			t.Fatalf("retained evidence missing: %+v", state)
		}
		if _, err := o.Readback(t.Context()); err != nil {
			t.Fatal(err)
		}
		if len(c.Snapshot().Records) != 1 {
			t.Fatal("retained source replayed")
		}
		if _, err := o.Release("a"); err != nil {
			t.Fatal(err)
		}
		f.spawns(t, 1)
	})
	t.Run("persistence", func(t *testing.T) {
		f := newManagedFixture(t, false)
		o := f.open(t)
		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()
		if _, err := o.Acquire(ctx, "a"); err == nil {
			t.Fatal("unstarted gateway ready")
		}
		path := filepath.Join(f.spec.Binding.StateRoot, "owner.json")
		if err := os.Rename(path, path+".backup"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		managedMarker(t, f.root, "wrong-version")
		managedMarker(t, f.root, "serve")
		managedAwait(t, f.root, "listening")
		if _, err := o.Readback(t.Context()); err == nil || !strings.Contains(err.Error(), "persist openshell custody") {
			t.Fatalf("persistence failure accepted: %v", err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(path+".backup", path); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(filepath.Join(f.root, "wrong-version")); err != nil {
			t.Fatal(err)
		}
		if _, err := o.Readback(t.Context()); err == nil {
			t.Fatal("restored persistence erased fail closed state")
		}
		if _, err := o.Release("a"); err == nil {
			t.Fatal("unresolved persistence released child")
		}
		f.spawns(t, 1)
	})
}
