package runtime

import (
	"context"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"github.com/poteto/noodle/internal/admission"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// These tests cover only reserve/initialization/protocol boundaries. They do
// not prove managed-child ownership or substitute for the TCP child suite.
func gatewayReservationFixture(t *testing.T) (*custodyGateway, *managedGatewayServer, *grpc.ClientConn, OpenShellCustodyBinding) {
	t.Helper()
	g, native := custodyServer(t)
	server := &managedGatewayServer{openShellNativeServer: native, root: t.TempDir()}
	security, serverTLS := openShellTestTLS(t)
	listener := bufconn.Listen(1024 * 1024)
	rpc := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverTLS)))
	pb.RegisterOpenShellServer(rpc, server)
	done := make(chan struct{})
	go func() { defer close(done); _ = rpc.Serve(listener) }()
	t.Cleanup(func() { rpc.Stop(); _ = listener.Close(); <-done })
	conn, err := grpc.NewClient("passthrough:///gateway.test", grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, RootCAs: security.RootCAs, Certificates: []tls.Certificate{security.Certificate}, ServerName: security.ServerName})), grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) }), grpc.WithDisableRetry(), grpc.WithDisableServiceConfig())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return g, server, conn, custodyBinding(t, "https://127.0.0.1:1")
}

func TestOpenShellGatewayReserveRetainedInProcess(t *testing.T) {
	g, _, conn, binding := gatewayReservationFixture(t)
	refs := []OpenShellRetainedSandbox{{Name: "retained", SandboxID: "retained-id", Workspace: binding.Workspace}}
	c, err := reserveOpenShellCustody(binding, conn, refs)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	g.mutate(func() {
		if g.lists != 0 || g.gets != 0 {
			t.Fatal("reserve contacted native gateway")
		}
	})
	refs[0].SandboxID = "caller replacement"
	if got := custodyDisk(t, binding).PendingRetained; len(got) != 1 || got[0].SandboxID != "retained-id" {
		t.Fatalf("retained source lost: %+v", got)
	}
	if _, err := c.Create(t.Context(), OpenShellTask{"x", "x", "x"}, &pb.SandboxSpec{}); err == nil {
		t.Fatal("reserved custody allowed mutation")
	}
	if err := c.Quiesce(t.Context()); err == nil {
		t.Fatal("reserved custody allowed release")
	}
	// A failed read is recoverable only by explicit readback. Its original source
	// is still on disk and no partially initialized empty ledger can become ready.
	c.retryReadback = true
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := c.initialize(ctx); err == nil {
		t.Fatal("canceled initialization accepted")
	}
	if got := custodyDisk(t, binding).PendingRetained; len(got) != 1 {
		t.Fatalf("failure erased source: %+v", got)
	}
	if c.Snapshot().Blocked != "" {
		t.Fatal("transient read became permanent drift")
	}
	g.mutate(func() {
		g.sandboxes["retained"] = custodySandbox("retained", "retained-id", pb.SandboxPhase_SANDBOX_PHASE_STOPPED)
	})
	if err := c.initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	state := custodyDisk(t, binding)
	if len(state.PendingRetained) != 0 || len(state.Records) != 1 || !state.Records[0].Retained || state.Records[0].Stopped == nil {
		t.Fatalf("retained stopped evidence missing: %+v", state)
	}
	if err := c.initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	if len(c.Snapshot().Records) != 1 {
		t.Fatal("retained initialization repeated")
	}
}

func TestOpenShellGatewayNativeProtocolInProcess(t *testing.T) {
	for _, mode := range []string{"healthy", "wrong-version", "wrong-driver", "stall"} {
		t.Run(mode, func(t *testing.T) {
			_, server, conn, binding := gatewayReservationFixture(t)
			c, err := reserveOpenShellCustody(binding, conn, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			o := &OpenShellGateway{custody: c, spec: OpenShellGatewaySpec{Version: "test-version", ComputeDriver: "test-driver"}}
			if mode != "healthy" {
				managedMarker(t, server.root, mode)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
			defer cancel()
			err = o.nativeReadiness(ctx)
			if (err == nil) != (mode == "healthy") {
				t.Fatalf("native protocol outcome %s: %v", mode, err)
			}
			conflict := mode == "wrong-version" || mode == "wrong-driver"
			if (c.Snapshot().Blocked != "") != conflict {
				t.Fatalf("conflict/transient evidence lost: %+v", c.Snapshot())
			}
		})
	}
}

func TestOpenShellGatewayInputValidationAndLock(t *testing.T) {
	security, _ := openShellTestTLS(t)
	root := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	residents, err := admission.OpenResidents(root)
	if err != nil {
		t.Fatal(err)
	}
	spec := OpenShellGatewaySpec{Resident: admission.ResidentSpec{Name: "gateway", Argv: []string{exe}, Directory: root, Environment: []string{}, Sharing: "shared", Transport: "network"}, Binding: custodyBinding(t, "https://127.0.0.1:1"), TLS: security, Version: "v", ComputeDriver: "d", ReadinessTimeout: time.Second}
	for _, mutate := range []func(*OpenShellGatewaySpec){
		func(s *OpenShellGatewaySpec) { s.Binding.Gateway = "https://example.com:443" },
		func(s *OpenShellGatewaySpec) { s.Binding.Gateway = "http://127.0.0.1:1" },
		func(s *OpenShellGatewaySpec) { s.Binding.Gateway = "https://127.0.0.1:1/path" },
		func(s *OpenShellGatewaySpec) { s.TLS.RootCAs = nil },
		func(s *OpenShellGatewaySpec) { s.Version = "" },
		func(s *OpenShellGatewaySpec) { s.ComputeDriver = "" },
		func(s *OpenShellGatewaySpec) { s.ReadinessTimeout = 31 * time.Second },
		func(s *OpenShellGatewaySpec) { s.Resident.Argv = []string{exe, "\x00"} },
		func(s *OpenShellGatewaySpec) { s.Resident.ForceAfterGrace = true },
		func(s *OpenShellGatewaySpec) { s.Resident.Environment = []string{"A=1", "A=2"} },
	} {
		invalid := spec
		mutate(&invalid)
		if o, err := NewOpenShellGateway(residents, admission.NewBudget(1), invalid); err == nil || o != nil {
			t.Fatal("invalid input acquired owner")
		}
	}
	if _, err := os.Stat(filepath.Join(spec.Binding.StateRoot, "owner.json")); !os.IsNotExist(err) {
		t.Fatal("invalid input persisted custody")
	}
	o, err := NewOpenShellGateway(residents, admission.NewBudget(1), spec)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = o.Close() }()
	alias := spec
	alias.Binding.StateRoot = filepath.Join(root, "alias")
	if err := os.Symlink(spec.Binding.StateRoot, alias.Binding.StateRoot); err != nil {
		t.Fatal(err)
	}
	if second, err := NewOpenShellGateway(residents, admission.NewBudget(1), alias); err == nil || second != nil {
		t.Fatal("canonical root lock bypassed")
	}
	if err := o.Close(); err != nil {
		t.Fatal(err)
	}
	changed := spec
	changed.Binding.ConfigSource = "other-source"
	blocked, err := NewOpenShellGateway(residents, admission.NewBudget(1), changed)
	if blocked == nil || err == nil {
		t.Fatal("existing binding conflict ignored")
	}
	defer func() { _ = blocked.Close() }()
	if _, err := blocked.Acquire(t.Context(), "a"); err == nil {
		t.Fatal("binding conflict spawned")
	}
	rows, err := residents.Snapshot()
	if err != nil || len(rows) != 0 {
		t.Fatalf("invalid input spawned: %+v %v", rows, err)
	}
}

func TestOpenShellGatewayFreezesTLS(t *testing.T) {
	original, _ := openShellTestTLS(t)
	frozen, err := freezeOpenShellTLS(original)
	if err != nil {
		t.Fatal(err)
	}
	expected := append([]byte(nil), frozen.Certificate.Certificate[0]...)
	original.Certificate.Certificate[0][0] ^= 0xff
	original.Certificate.PrivateKey.(*ecdsa.PrivateKey).D.Set(big.NewInt(1))
	original.RootCAs.AddCert(&x509.Certificate{RawSubject: []byte("replacement")})
	if !reflect.DeepEqual(expected, frozen.Certificate.Certificate[0]) || frozen.Certificate.PrivateKey.(*ecdsa.PrivateKey).D.Cmp(big.NewInt(1)) == 0 || original.RootCAs.Equal(frozen.RootCAs) {
		t.Fatal("TLS snapshot aliases caller")
	}
}

func TestOpenShellGatewayPendingSourceReopenInProcess(t *testing.T) {
	_, _, conn, binding := gatewayReservationFixture(t)
	refs := []OpenShellRetainedSandbox{{"retained", "retained-id", binding.Workspace}}
	c, err := reserveOpenShellCustody(binding, conn, refs)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	// A restarted owner must retain the original pending source even when the
	// caller supplies no retained entries. No connection or VM effect is needed.
	connection, err := newOpenShellConnection(binding.Gateway, OpenShellTLS{})
	if err == nil || connection != nil {
		t.Fatal("missing mTLS accepted")
	}
	security, _ := openShellTestTLS(t)
	connection, err = newOpenShellConnection(binding.Gateway, security)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := reserveOpenShellCustody(binding, connection, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	if got := reopened.Snapshot().PendingRetained; !reflect.DeepEqual(got, refs) {
		t.Fatalf("reopen lost retained source: %+v", got)
	}
	state := reopened.Snapshot()
	state.PendingRetained[0].SandboxID = "detached"
	if strings.Contains(reopened.Snapshot().PendingRetained[0].SandboxID, "detached") {
		t.Fatal("snapshot aliases retained source")
	}
}

func TestOpenShellGatewayCorruptLedgerBeforeSpawnInProcess(t *testing.T) {
	_, _, conn, binding := gatewayReservationFixture(t)
	c, err := reserveOpenShellCustody(binding, conn, nil)
	if err != nil {
		t.Fatal(err)
	}
	c.state.Records = []OpenShellCustodyRecord{{Name: "lost", CreateAcknowledged: true}}
	if err := c.save(); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	security, _ := openShellTestTLS(t)
	next, err := newOpenShellConnection(binding.Gateway, security)
	if err != nil {
		t.Fatal(err)
	}
	blocked, err := reserveOpenShellCustody(binding, next, nil)
	if blocked == nil || err == nil {
		t.Fatal("corrupt ledger accepted reservation")
	}
	defer func() { _ = blocked.Close() }()
	if blocked.Snapshot().Blocked == "" {
		t.Fatal("invalid ledger not durably blocked")
	}
}

func TestOpenShellGatewayReadFailureRevokesReadinessInProcess(t *testing.T) {
	_, _, conn, binding := gatewayReservationFixture(t)
	c, err := reserveOpenShellCustody(binding, conn, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	c.retryReadback = true
	if err := c.initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := c.readFailure("inventory unavailable", status.Error(codes.Unavailable, "outage")); err == nil {
		t.Fatal("outage accepted")
	}
	if c.initialized || c.Snapshot().Blocked != "" {
		t.Fatal("outage retained readiness or became permanent drift")
	}
	if _, err := c.Create(t.Context(), OpenShellTask{"x", "x", "x"}, &pb.SandboxSpec{}); err == nil {
		t.Fatal("outage permitted VM effects")
	}
	if err := c.initialize(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := c.readFailure("read bound sandbox", status.Error(codes.NotFound, "missing")); err == nil {
		t.Fatal("missing native identity accepted")
	}
	if err := c.initialize(t.Context()); err == nil || c.Snapshot().Blocked == "" {
		t.Fatal("confirmed missing identity recovered as transient")
	}
}
