package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"github.com/poteto/noodle/config"
	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/lockfile"
	"github.com/poteto/noodle/loop"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type hostDeferralGateway struct {
	pb.UnimplementedOpenShellServer
}

func (hostDeferralGateway) Health(context.Context, *pb.HealthRequest) (*pb.HealthResponse, error) {
	return &pb.HealthResponse{Status: pb.ServiceStatus_SERVICE_STATUS_HEALTHY, Version: "fixture"}, nil
}
func (hostDeferralGateway) GetGatewayInfo(context.Context, *pb.GetGatewayInfoRequest) (*pb.GetGatewayInfoResponse, error) {
	return &pb.GetGatewayInfoResponse{Status: pb.ServiceStatus_SERVICE_STATUS_HEALTHY, GatewayVersion: "fixture", ComputeDrivers: []*pb.ComputeDriverInfo{{Name: "vm"}}}, nil
}
func (hostDeferralGateway) ListSandboxes(context.Context, *pb.ListSandboxesRequest) (*pb.ListSandboxesResponse, error) {
	return &pb.ListSandboxesResponse{}, nil
}

func TestHostOpenShellAdmissionChild(t *testing.T) {
	root := os.Getenv("NOODLE_DEFERRAL_GATEWAY_CHILD")
	if root == "" {
		return
	}
	pair, err := tls.LoadX509KeyPair(filepath.Join(root, "server.crt"), filepath.Join(root, "server.key"))
	if err != nil {
		t.Fatal(err)
	}
	ca, err := os.ReadFile(filepath.Join(root, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		t.Fatal("no CA")
	}
	data, err := os.ReadFile(filepath.Join(root, "address"))
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", string(data))
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots})))
	pb.RegisterOpenShellServer(server, hostDeferralGateway{})
	// Bound the helper even if a failed parent assertion loses its normal cleanup.
	timer := time.AfterFunc(20*time.Second, server.Stop)
	defer timer.Stop()
	if err := server.Serve(listener); err != nil {
		t.Fatal(err)
	}
}

func hostDeferralProfile(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write := func(name string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), data, 0700); err != nil {
			t.Fatal(err)
		}
	}
	newKey := func() *ecdsa.PrivateKey {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return key
	}
	caKey := newKey()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "host admission fixture"}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour)}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	write("ca.crt", caPEM)
	for i, kind := range []string{"client", "server"} {
		key := newKey()
		leaf := &x509.Certificate{SerialNumber: big.NewInt(int64(i + 2)), DNSNames: []string{"gateway.test"}, NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}
		der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		write(kind+".crt", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
		raw, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		write(kind+".key", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: raw}))
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	endpoint := "https://" + address
	write("address", []byte(address))
	clientRoot := filepath.Join(root, "client-config")
	client := filepath.Join(clientRoot, "openshell", "gateways", "fixture")
	if err = os.MkdirAll(filepath.Join(client, "mtls"), 0700); err != nil {
		t.Fatal(err)
	}
	for dest, source := range map[string]string{"ca.crt": "ca.crt", "tls.crt": "client.crt", "tls.key": "client.key"} {
		data, err := os.ReadFile(filepath.Join(root, source))
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(client, "mtls", dest), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	metadata, _ := json.Marshal(map[string]string{"name": "fixture", "gateway_endpoint": endpoint, "auth_mode": "mtls"})
	if err = os.WriteFile(filepath.Join(client, "metadata.json"), metadata, 0600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(filepath.Join(root, "payload"), 0700); err != nil {
		t.Fatal(err)
	}
	write("payload/codex", []byte("fixture"))
	write("spec.json", []byte(`{"template":{"image":"fixture"},"providers":["noodle-codex"],"command":["hold"]}`))
	profile := map[string]any{
		"schema_version": 1, "cli": executable, "gateway_name": "fixture", "environment": []string{"XDG_CONFIG_HOME=" + clientRoot}, "payload_directory": "payload", "routes": map[string]any{"codex": map[string]string{"binary": "/sandbox/payload/codex", "spec_file": "spec.json"}},
		"gateway": map[string]any{"resident": admission.ResidentSpec{Name: "host-openshell", Argv: []string{executable, "-test.run=^TestHostOpenShellAdmissionChild$"}, Directory: root, Environment: []string{"NOODLE_DEFERRAL_GATEWAY_CHILD=" + root}, Sharing: "shared", Transport: "network"}, "binding": map[string]string{"state_root": filepath.Join(root, "custody"), "gateway": endpoint, "workspace": "default", "owner": "host-fixture"}, "tls": map[string]string{"ca_file": "ca.crt", "certificate_file": "client.crt", "key_file": "client.key", "server_name": "gateway.test"}, "version": "fixture", "compute_driver": "vm", "readiness_timeout_seconds": 5, "identity_source": "address", "config_files": []string{"address"}},
	}
	data, err := json.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	write("profile.json", data)
	return filepath.Join(root, "profile.json")
}

func TestHostOpenShellWarningRefusalIsRepeatableWithoutPoisoningContext(t *testing.T) {
	root := t.TempDir()
	req := hostTestRequest(t, root)
	profile := hostDeferralProfile(t)
	data := []byte(fmt.Sprintf("mode='supervised'\n[server]\nenabled=false\n[runtime.openshell]\nprofile=%q\n", profile))
	if err := os.WriteFile(filepath.Join(root, config.DefaultConfigPath), data, 0600); err != nil {
		t.Fatal(err)
	}
	req.ConfigSHA256 = hostDigest(data)
	h := &noodleHost{dir: t.TempDir(), instanceID: "instance", binaryPath: "/native/noodle", binarySHA: "test-binary", budget: admission.NewBudget(2), completed: make(chan string, 4)}
	var err error
	h.residents, err = admission.OpenResidents(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	h.residents.BindBudget(h.budget)
	var runtimes []*fakeHostRuntime
	h.newRuntime = func(string, string, config.Config, loop.Dependencies) hostRuntime {
		r := &fakeHostRuntime{ready: make(chan struct{}), released: make(chan struct{})}
		runtimes = append(runtimes, r)
		return r
	}
	t.Cleanup(func() {
		for _, c := range h.contexts {
			if c.done != nil {
				c.cancel()
				h.finish(c, <-c.done)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := h.residents.WaitClosed(ctx); err != nil {
			t.Error(err)
		}
		if h.openShell != nil {
			if err := h.openShell.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	now := time.Now()
	h.budget.Observe(admission.Sample{At: now.Add(-time.Second), Pressure: admission.Warning}, nil)
	h.budget.Observe(admission.Sample{At: now, Pressure: admission.Warning}, nil)
	first := h.handle(context.Background(), req)
	if first.Status != "deferred" || first.ContextState != "detached" || first.AdmissionRefusal == "" || h.blocked || len(h.contexts) != 0 {
		t.Fatalf("known refusal poisoned host: %+v", first)
	}
	records, err := h.residents.Snapshot()
	snapshot := h.budget.Snapshot()
	if err != nil || len(records) != 0 || snapshot.Active != 0 || snapshot.Residents != 0 || snapshot.Allocations != 0 || len(runtimes) != 1 || runtimes[0].cycles != 0 {
		t.Fatalf("refused startup acquired resources: %+v %+v %v", records, snapshot, err)
	}
	select {
	case <-runtimes[0].ready:
		t.Fatal("refused runtime started Run")
	default:
	}
	if _, err := os.Stat(filepath.Join(root, ".noodle", "state.snapshot.json")); !os.IsNotExist(err) {
		t.Fatalf("never-started context unexpectedly has a snapshot: %v", err)
	}
	repeated := h.handle(context.Background(), req)
	if repeated.Status != "deferred" || repeated.ContextID != first.ContextID || repeated.Error != first.AdmissionRefusal || len(runtimes) != 1 {
		t.Fatalf("original refusal replayed or lost reason: %+v", repeated)
	}
	raw, err := os.ReadFile(filepath.Join(h.dir, "receipts.json"))
	if err != nil {
		t.Fatal(err)
	}
	var saved []hostReceipt
	if err = json.Unmarshal(raw, &saved); err != nil || len(saved) != 1 || saved[0].AdmissionRefusal != first.AdmissionRefusal {
		t.Fatal("refusal reason was not retained")
	}
	lookup := req
	lookup.Operation = "status"
	for _, response := range []hostResponse{h.handle(context.Background(), lookup), offlineHostStatus(h.dir, lookup, errors.New("offline"))} {
		if response.Status != "deferred" || response.Error != first.AdmissionRefusal {
			t.Fatalf("readback lost refusal: %+v", response)
		}
	}
	if _, settled := h.finishOpenShellDeferral(nil, errors.New("native outcome unknown")); settled {
		t.Fatal("native uncertainty was treated as a pressure refusal")
	}
	for i := 1; i <= 3; i++ {
		h.budget.Observe(admission.Sample{At: now.Add(time.Duration(i) * time.Millisecond), Pressure: admission.Normal}, nil)
	}
	retry := req
	retry.RequestID = "explicit-new-request"
	second := h.handle(context.Background(), retry)
	if second.Status != "ok" || second.ContextState != "active" || second.ContextID == first.ContextID || len(runtimes) != 2 || h.blocked {
		t.Fatalf("pressure-cleared new request refused: %+v", second)
	}
	records, err = h.residents.Snapshot()
	snapshot = h.budget.Snapshot()
	if err != nil || len(records) != 1 || snapshot.Active != 0 || snapshot.Residents != 1 || snapshot.Allocations != 1 {
		t.Fatalf("new request did not own one gateway: %+v %+v %v", records, snapshot, err)
	}
	// Even an ErrDeferred wrapper cannot certify no effect once a lease exists.
	if _, settled := h.finishOpenShellDeferral(h.contexts[second.ContextID], admission.ErrDeferred); settled {
		t.Fatal("leased context was misclassified as no-effect refusal")
	}
	detach := retry
	detach.Operation = "detach"
	detach.ContextID = second.ContextID
	detach.InstanceID = second.HostInstanceID
	if response := h.handle(context.Background(), detach); response.Status != "ok" || response.ContextState != "detached" {
		t.Fatalf("detach: %+v", response)
	}
}

func TestHostOpenShellDeferredRequestBindingRemainsReadOnly(t *testing.T) {
	dir, err := os.MkdirTemp("", "nh-defer-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	t.Setenv("NOODLE_HOST_DIR", dir)
	listener, err := net.Listen("unix", filepath.Join(dir, "host.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	served := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			served <- err
			return
		}
		defer conn.Close()
		var req hostRequest
		if err = json.NewDecoder(conn).Decode(&req); err != nil {
			served <- err
			return
		}
		served <- json.NewEncoder(conn).Encode(hostResponse{hostReceipt: hostReceipt{ContextState: "detached", RequestID: req.RequestID, AdmissionRefusal: "memory pressure"}, Status: "deferred", Error: "memory pressure"})
	}()
	req := hostTestRequest(t, t.TempDir())
	req.RequestFile = filepath.Join(req.ProjectDir, "request.json")
	if response := callHost(context.Background(), req); response.Status != "deferred" {
		t.Fatalf("response: %+v", response)
	}
	if err = <-served; err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(req.RequestFile)
	if err != nil {
		t.Fatal(err)
	}
	var binding hostRequestBinding
	if err = json.Unmarshal(raw, &binding); err != nil {
		t.Fatal(err)
	}
	if binding.Status != "deferred" {
		t.Fatalf("known refusal left request pending: %+v", binding)
	}
	req.ProjectDir, req.BinarySHA256 = binding.ProjectDir, binding.BinarySHA256
	_, unlock, err := bindHostRequest(&req)
	if err != nil {
		t.Fatal(err)
	}
	unlock()
	if req.Operation != "status" || req.RequestID != binding.RequestID {
		t.Fatalf("refused original request replayed: %+v", req)
	}
}

type hostDeferralShutdownFailure struct {
	fakeHostRuntime
	onShutdown func()
}

func (r *hostDeferralShutdownFailure) Shutdown() { r.onShutdown() }

func TestHostOpenShellDeferralCleanupFailureRetainsContext(t *testing.T) {
	h := &noodleHost{dir: t.TempDir(), contexts: make(map[string]*hostContext)}
	var err error
	h.residents, err = admission.OpenResidents(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	lockPath := filepath.Join(t.TempDir(), "context.lock")
	lock, err := lockfile.TryLock(lockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &hostContext{receipt: hostReceipt{ContextID: "refused-context", ContextState: "active"}, lock: lock, cancel: cancel, runtime: &hostDeferralShutdownFailure{onShutdown: func() { h.blocked = true }}}
	h.contexts[c.receipt.ContextID] = c
	h.receipts = []hostReceipt{c.receipt}
	response, handled := h.finishOpenShellDeferral(c, fmt.Errorf("gateway admission: %w", admission.ErrDeferred))
	if !handled || response.Status != "unknown_effect" || !c.failed || c.receipt.ContextState != "failed" || h.contexts[c.receipt.ContextID] != c {
		t.Fatalf("cleanup uncertainty lost original context: %+v", response)
	}
	if other, err := lockfile.TryLock(lockPath); err == nil {
		other.Close()
		t.Fatal("cleanup uncertainty released project lock")
	}
}
