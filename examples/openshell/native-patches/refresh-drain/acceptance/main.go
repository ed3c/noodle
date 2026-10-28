//go:build unix

// Command acceptance exercises a real patched gateway process with synthetic,
// loopback-only protocol peers. It never starts compute or reads local grants.
package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	dm "github.com/NVIDIA/OpenShell/sdk/go/proto/datamodelv1"
	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const provider = "shutdown-fixture"
const credentialKey = "FIXTURE_ACCESS_TOKEN"

type evidence struct {
	SchemaVersion      int                `json:"schema_version"`
	GatewayPath        string             `json:"gateway_path"`
	SourcePin          sourcePin          `json:"source_pin"`
	SourceTreeSHA256   string             `json:"source_tree_sha256"`
	BuildReceiptPath   string             `json:"build_receipt_path"`
	BuildReceiptSHA256 string             `json:"build_receipt_sha256"`
	GatewaySHA256      string             `json:"gateway_sha256"`
	GatewayVersion     string             `json:"gateway_version"`
	Scenarios          []scenarioEvidence `json:"scenarios"`
	Passed             bool               `json:"passed"`
	Error              string             `json:"error,omitempty"`
}
type scenarioEvidence struct {
	Name                   string        `json:"name"`
	CredentialBackend      string        `json:"credential_backend"`
	SignalScope            string        `json:"signal_scope"`
	GatewayPGIDs           []int         `json:"gateway_pgids"`
	ExitedGroups           []int         `json:"exited_groups"`
	Events                 []string      `json:"events"`
	GatewayPIDs            []int         `json:"gateway_pids"`
	GatewayExits           []processExit `json:"gateway_exits"`
	HeldAfterClosureMillis int64         `json:"held_after_closure_millis"`
	Passed                 bool          `json:"passed"`
}

type processExit struct {
	PID        int    `json:"pid"`
	ExitCode   int    `json:"exit_code"`
	ObservedAt string `json:"observed_at"`
}

type sourcePin struct {
	Upstream      string `json:"upstream"`
	Tag           string `json:"tag"`
	Commit        string `json:"commit"`
	ArchiveSHA256 string `json:"archive_sha256"`
	PatchSHA256   string `json:"patch_sha256"`
	RustToolchain string `json:"rust_toolchain"`
	BuildVersion  string `json:"build_version"`
}

type buildReceipt struct {
	SchemaVersion    int       `json:"schema_version"`
	Action           string    `json:"action"`
	ExitCode         *int      `json:"exit_code"`
	SourceUnchanged  bool      `json:"source_unchanged"`
	SourcePin        sourcePin `json:"source_pin"`
	SourceTreeSHA256 string    `json:"source_tree_sha256"`
	Binary           string    `json:"binary"`
	BinarySHA256     string    `json:"binary_sha256"`
	Version          string    `json:"version"`
}

func main() {
	binary := flag.String("gateway", "", "absolute path to the already-built patched gateway")
	buildPath := flag.String("build-receipt", "", "absolute path to its successful source-pinned build receipt")
	flag.Parse()
	if !filepath.IsAbs(*binary) || !filepath.IsAbs(*buildPath) {
		fmt.Fprintln(os.Stderr, "-gateway and -build-receipt require absolute paths")
		os.Exit(2)
	}
	dir, err := os.MkdirTemp("/tmp", "ndr-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("evidence directory:", dir)
	e := evidence{SchemaVersion: 1}
	err = run(*binary, *buildPath, dir, &e)
	if err != nil {
		e.Error = err.Error()
	}
	e.Passed = err == nil
	b, marshalErr := json.MarshalIndent(e, "", "  ")
	if marshalErr != nil {
		fmt.Fprintln(os.Stderr, "evidence encoding failed:", marshalErr)
		os.Exit(1)
	}
	writeErr := os.WriteFile(filepath.Join(dir, "evidence.json"), append(b, '\n'), 0600)
	if writeErr != nil {
		fmt.Fprintln(os.Stderr, "evidence persistence failed:", writeErr)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if writeErr != nil {
		os.Exit(1)
	}
	fmt.Println("PASS: real gateway drained admitted refreshes before exit; isolated restart confirmed replacement persistence")
}

func isolatedEnv(dir string) []string {
	return []string{"PATH=/usr/bin:/bin", "HOME=" + dir, "XDG_STATE_HOME=" + filepath.Join(dir, "state"), "XDG_CONFIG_HOME=" + filepath.Join(dir, "config"), "XDG_CACHE_HOME=" + filepath.Join(dir, "cache"), "TMPDIR=" + dir, "OPENSHELL_DB_URL=sqlite://" + filepath.Join(dir, "gateway.db") + "?mode=rwc", "RUST_LOG=info", "TOKIO_WORKER_THREADS=2"}
}

func run(binary, buildPath, dir string, e *evidence) error {
	info, err := os.Lstat(buildPath)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > 128*1024 {
		return errors.New("build receipt is not a bounded regular file")
	}
	receiptBytes, err := os.ReadFile(buildPath)
	if err != nil {
		return err
	}
	var receipt buildReceipt
	if err := json.Unmarshal(receiptBytes, &receipt); err != nil {
		return err
	}
	if receipt.SchemaVersion != 1 || receipt.Action != "build" || receipt.ExitCode == nil || *receipt.ExitCode != 0 ||
		!receipt.SourceUnchanged || receipt.Binary != binary || receipt.SourcePin.Upstream != "NVIDIA/OpenShell" ||
		receipt.SourcePin.Tag != "v0.1.5" || receipt.SourcePin.Commit != "bdb956f2cd65a635f1294fccf1d138d263bf650f" ||
		receipt.SourcePin.ArchiveSHA256 != "6299e61bfe41a5eb67985c5da986688b87c495a4602ca90ffcf9b90328afe29f" ||
		receipt.SourcePin.BuildVersion != "0.1.5+noodle.refresh-drain.1" ||
		!digestValid(receipt.SourcePin.PatchSHA256) || !digestValid(receipt.SourceTreeSHA256) {
		return errors.New("build receipt does not bind a successful unchanged pinned refresh-drain build")
	}
	e.GatewayPath = binary
	e.SourcePin = receipt.SourcePin
	e.SourceTreeSHA256 = receipt.SourceTreeSHA256
	e.BuildReceiptPath = buildPath
	receiptHash := sha256.Sum256(receiptBytes)
	e.BuildReceiptSHA256 = hex.EncodeToString(receiptHash[:])
	b, err := os.ReadFile(binary)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(b)
	e.GatewaySHA256 = hex.EncodeToString(sum[:])
	if e.GatewaySHA256 != receipt.BinarySHA256 {
		return errors.New("gateway binary differs from successful build receipt")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	cmd := exec.CommandContext(ctx, binary, "--version")
	cmd.Env = isolatedEnv(dir)
	version, err := cmd.Output()
	cancel()
	if err != nil {
		return fmt.Errorf("gateway version: %w", err)
	}
	e.GatewayVersion = strings.TrimSpace(string(version))
	if e.GatewayVersion != receipt.Version {
		return errors.New("gateway version differs from successful build receipt")
	}
	if !strings.Contains(e.GatewayVersion, "0.1.5+noodle.refresh-drain.1") {
		return fmt.Errorf("gateway version is not the reviewed refresh-drain build: %s", e.GatewayVersion)
	}
	for _, name := range []string{"issuer-pending", "replacement-store-pending", "builtin-db-issuer-pending-group-term"} {
		e.Scenarios = append(e.Scenarios, scenarioEvidence{Name: name})
		if err := scenario(binary, filepath.Join(dir, name), &e.Scenarios[len(e.Scenarios)-1]); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	b, err = os.ReadFile(binary)
	if err != nil {
		return err
	}
	after := sha256.Sum256(b)
	if after != sum {
		return errors.New("gateway binary changed during acceptance")
	}
	receiptBytes, err = os.ReadFile(buildPath)
	if err != nil {
		return err
	}
	if sha256.Sum256(receiptBytes) != receiptHash {
		return errors.New("build receipt changed during acceptance")
	}
	return nil
}

func digestValid(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == sha256.Size
}

type issuer struct {
	gate     *gate
	mu       sync.Mutex
	requests int
	bad      bool
}

func (i *issuer) serve(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	if r.Method != "POST" || r.URL.Path != "/token" || r.ParseForm() != nil {
		http.Error(w, "invalid fixture request", 400)
		return
	}
	i.mu.Lock()
	i.requests++
	count := i.requests
	expected := "fake-old"
	if count > 1 {
		expected = "fake-replacement"
	}
	valid := r.Form.Get("grant_type") == "refresh_token" && r.Form.Get("refresh_token") == expected && r.Form.Get("client_id") == "fixture-client"
	if !valid {
		i.bad = true
	}
	i.mu.Unlock()
	if !valid {
		http.Error(w, "unexpected synthetic grant", 400)
		return
	}
	if err := i.gate.wait(r.Context()); err != nil {
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, `{"access_token":"fake-access","refresh_token":"fake-replacement","expires_in":3600,"token_type":"Bearer"}`)
}

type gatewayProcess struct {
	cmd  *exec.Cmd
	done chan struct{}
	err  error
	pgid int
}

func (p *gatewayProcess) stopCleanup() {
	select {
	case <-p.done:
		return
	default:
	}
	_ = p.cmd.Process.Kill()
	<-p.done
}

func startGateway(binary, dir string, port int, tlsConfig *tls.Config) (*gatewayProcess, *grpc.ClientConn, pb.OpenShellClient, error) {
	args := []string{"--config", filepath.Join(dir, "gateway.toml"), "--name", "shutdown-fixture", "--bind-address", "127.0.0.1", "--port", fmt.Sprint(port), "--compute-driver", "fixture", "--compute-driver-socket", filepath.Join(dir, "compute.sock"), "--tls-cert", filepath.Join(dir, "server.pem"), "--tls-key", filepath.Join(dir, "server-key.pem"), "--tls-client-ca", filepath.Join(dir, "ca.pem"), "--enable-mtls-auth=true", "--enable-operator-auth=true"}
	log, err := os.OpenFile(filepath.Join(dir, "gateway.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, nil, nil, err
	}
	cmd := exec.Command(binary, args...)
	cmd.Env = isolatedEnv(dir)
	cmd.Dir = dir
	cmd.Stdout = log
	cmd.Stderr = log
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		_ = log.Close()
		return nil, nil, nil, err
	}
	p := &gatewayProcess{cmd: cmd, done: make(chan struct{}), pgid: cmd.Process.Pid}
	go func() { p.err = cmd.Wait(); _ = log.Close(); close(p.done) }()
	conn, err := grpc.NewClient(fmt.Sprintf("127.0.0.1:%d", port), grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)))
	if err != nil {
		p.stopCleanup()
		return nil, nil, nil, err
	}
	client := pb.NewOpenShellClient(conn)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-p.done:
			_ = conn.Close()
			return nil, nil, nil, fmt.Errorf("gateway exited during startup; see gateway.log: %v", p.err)
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		_, err = client.Health(ctx, &pb.HealthRequest{})
		cancel()
		if err == nil {
			return p, conn, client, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	p.stopCleanup()
	_ = conn.Close()
	return nil, nil, nil, fmt.Errorf("gateway readiness timed out; see gateway.log: %w", err)
}

// The child is still retained until Wait; no discovered or adopted PID is signalled.
func (p *gatewayProcess) terminate(group bool) error {
	select {
	case <-p.done:
		return errors.New("original gateway already exited")
	default:
	}
	if !group {
		return p.cmd.Process.Signal(syscall.SIGTERM)
	}
	pgid, err := syscall.Getpgid(p.cmd.Process.Pid)
	if err != nil {
		return err
	}
	if p.pgid <= 1 || p.pgid != p.cmd.Process.Pid || pgid != p.pgid {
		return errors.New("original gateway process group identity changed")
	}
	return syscall.Kill(-p.pgid, syscall.SIGTERM)
}

func (p *gatewayProcess) verifyGroupExited(e *scenarioEvidence) error {
	if err := syscall.Kill(-p.pgid, 0); !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("original gateway group remains after child exit: %v", err)
	}
	e.ExitedGroups = append(e.ExitedGroups, p.pgid)
	return nil
}

func gatewayConfig(dir string, builtin bool) string {
	if builtin {
		return "[openshell]\nversion = 2\n[openshell.gateway.credential_storage]\nkey_encryption_key_path = " + fmt.Sprintf("%q", filepath.Join(dir, "credential-encryption-key.bin")) + "\n"
	}
	return "[openshell]\nversion = 2\n[openshell.gateway]\ncredential_drivers = [\"fixture\"]\n[openshell.credential_drivers.fixture]\ntransport = \"uds\"\nsocket_path = " + fmt.Sprintf("%q", filepath.Join(dir, "credentials.sock")) + "\n"
}

func scope() *dm.WorkspaceSelector {
	return &dm.WorkspaceSelector{Selection: &dm.WorkspaceSelector_Workspace{Workspace: "default"}}
}
func configureRequest() *pb.ConfigureProviderRefreshRequest {
	return &pb.ConfigureProviderRefreshRequest{WorkspaceScope: scope(), Provider: provider, CredentialKey: credentialKey, Strategy: pb.ProviderCredentialRefreshStrategy_PROVIDER_CREDENTIAL_REFRESH_STRATEGY_OAUTH2_REFRESH_TOKEN, Material: map[string]string{"client_id": "fixture-client", "refresh_token": "fake-old"}, SecretMaterialKeys: []string{"refresh_token"}, ExpirationTime: timestamppb.New(time.Now().Add(time.Hour))}
}
func rotate(ctx context.Context, c pb.OpenShellClient) error {
	r, err := c.RotateProviderCredential(ctx, &pb.RotateProviderCredentialRequest{WorkspaceScope: scope(), Provider: provider, CredentialKey: credentialKey})
	if err != nil {
		return err
	}
	if r.GetStatus().GetStatus() != "refreshed" {
		return errors.New("refresh did not report refreshed")
	}
	return nil
}

func scenario(binary, dir string, e *scenarioEvidence) error {
	builtin := e.Name == "builtin-db-issuer-pending-group-term"
	e.CredentialBackend, e.SignalScope = "external-fixture", "original-process"
	if builtin {
		e.CredentialBackend, e.SignalScope = "builtin-db-credstore", "original-process-group"
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	fixture := newDriverFixture(dir)
	defer fixture.storeGate.open()
	stop, err := fixture.serve(filepath.Join(dir, "compute.sock"), "openshell.compute.v1.ComputeDriver")
	if err != nil {
		return err
	}
	defer stop()
	if !builtin {
		stop, err = fixture.serve(filepath.Join(dir, "credentials.sock"), "openshell.credentials.v1.CredentialDriver")
		if err != nil {
			return err
		}
		defer stop()
	}
	i := &issuer{gate: newGate()}
	defer i.gate.open()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	server := &http.Server{Handler: http.HandlerFunc(i.serve), ReadHeaderTimeout: 5 * time.Second}
	go server.Serve(listener)
	defer server.Close()
	if e.Name != "replacement-store-pending" {
		fixture.storeGate.open()
	} else {
		i.gate.open()
	}
	config := gatewayConfig(dir, builtin)
	if err := os.WriteFile(filepath.Join(dir, "gateway.toml"), []byte(config), 0600); err != nil {
		return err
	}
	tlsConfig, err := makePKI(dir)
	if err != nil {
		return err
	}
	portListener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	port := portListener.Addr().(*net.TCPAddr).Port
	_ = portListener.Close()
	p, conn, c, err := startGateway(binary, dir, port, tlsConfig)
	if err != nil {
		return err
	}
	defer p.stopCleanup()
	defer conn.Close()
	e.GatewayPIDs = append(e.GatewayPIDs, p.cmd.Process.Pid)
	e.GatewayPGIDs = append(e.GatewayPGIDs, p.pgid)
	profile := &pb.ProviderProfile{Id: "shutdown-fixture", DisplayName: "Shutdown fixture", Credentials: []*pb.ProviderProfileCredential{{Name: "access_token", EnvVars: []string{credentialKey}, Required: true, AuthStyle: "bearer", HeaderName: "authorization", Refresh: &pb.ProviderCredentialRefresh{Strategy: pb.ProviderCredentialRefreshStrategy_PROVIDER_CREDENTIAL_REFRESH_STRATEGY_OAUTH2_REFRESH_TOKEN, TokenUrl: "http://" + listener.Addr().String() + "/token", RefreshBefore: durationpb.New(30 * time.Second), MaxLifetime: durationpb.New(time.Hour), Material: []*pb.ProviderCredentialRefreshMaterial{{Name: "client_id", Required: true}, {Name: "refresh_token", Required: true, Secret: true}}}}}}
	imported, err := c.ImportProviderProfiles(ctx, &pb.ImportProviderProfilesRequest{WorkspaceScope: scope(), Profiles: []*pb.ProviderProfileImportItem{{Profile: profile, Source: "synthetic acceptance"}}})
	if err != nil {
		return fmt.Errorf("import fixture: %w", err)
	}
	if !imported.Imported {
		return fmt.Errorf("fixture profile not imported: %v", imported.Diagnostics)
	}
	_, err = c.CreateProvider(ctx, &pb.CreateProviderRequest{WorkspaceScope: scope(), Provider: &dm.Provider{Metadata: &dm.ObjectMeta{Name: provider}, Type: profile.Id, ProfileWorkspace: "default", Credentials: map[string]string{credentialKey: "fake-initial"}}})
	if err != nil {
		return fmt.Errorf("create fixture: %w", err)
	}
	if _, err = c.ConfigureProviderRefresh(ctx, configureRequest()); err != nil {
		return fmt.Errorf("configure fixture: %w", err)
	}
	refreshDone := make(chan error, 1)
	go func() { refreshDone <- rotate(ctx, c) }()
	g := i.gate
	if e.Name == "replacement-store-pending" {
		g = fixture.storeGate
	}
	select {
	case <-g.entered:
	case err := <-refreshDone:
		return fmt.Errorf("refresh finished before gate: %v", err)
	case <-ctx.Done():
		return errors.New("gate was not reached")
	}
	e.Events = append(e.Events, "native_refresh_reached_gate")
	if err := p.terminate(builtin); err != nil {
		return err
	}
	e.Events = append(e.Events, "sigterm_sent")
	// Invalid config is harmless before the signal handler closes admission. Once
	// closed, the same request must fail at the admission boundary instead.
	closed := false
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		probeCtx, probeCancel := context.WithTimeout(ctx, 300*time.Millisecond)
		_, probeErr := c.ConfigureProviderRefresh(probeCtx, &pb.ConfigureProviderRefreshRequest{})
		probeCancel()
		if admissionClosed(probeErr) {
			closed = true
			break
		}
		select {
		case <-p.done:
			return errors.New("gateway exited before release gate")
		default:
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !closed {
		return errors.New("gateway did not close refresh admission after SIGTERM")
	}
	for _, call := range []func() error{
		func() error { return rotate(ctx, c) },
		func() error { _, err := c.ConfigureProviderRefresh(ctx, configureRequest()); return err },
	} {
		if err := call(); !admissionClosed(err) {
			return fmt.Errorf("mutation after close did not refuse admission: %v", err)
		}
	}
	e.Events = append(e.Events, "manual_and_configuration_rejected_after_close")
	heldFrom := time.Now()
	select {
	case <-p.done:
		return errors.New("gateway exited while admitted refresh was gated")
	case <-time.After(time.Second):
	}
	e.HeldAfterClosureMillis = time.Since(heldFrom).Milliseconds()
	e.Events = append(e.Events, "gateway_alive_with_gate_held")
	g.open()
	e.Events = append(e.Events, "gate_released")
	select {
	case err := <-refreshDone:
		if err != nil {
			return fmt.Errorf("admitted refresh failed: %w", err)
		}
	case <-ctx.Done():
		return errors.New("admitted refresh timed out")
	}
	e.Events = append(e.Events, "admitted_refresh_acknowledged")
	select {
	case <-p.done:
		if p.err != nil {
			return fmt.Errorf("gateway exit: %w", p.err)
		}
	case <-ctx.Done():
		return errors.New("gateway failed to exit after persistence")
	}
	e.Events = append(e.Events, "gateway_exited_successfully")
	e.GatewayExits = append(e.GatewayExits, processExit{PID: p.cmd.Process.Pid, ExitCode: p.cmd.ProcessState.ExitCode(), ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)})
	_ = conn.Close()
	if builtin {
		if err := p.verifyGroupExited(e); err != nil {
			return err
		}
		e.Events = append(e.Events, "original_gateway_group_exited")
	} else {
		if err := fixture.reloadPersisted(); err != nil {
			return err
		}
		e.Events = append(e.Events, "driver_reloaded_replacement_from_synced_storage")
	}
	// Restart only this fixture DB. A second native rotation must resolve the
	// persisted replacement through the driver; the issuer checks it privately.
	p2, conn2, c2, err := startGateway(binary, dir, port, tlsConfig)
	if err != nil {
		return err
	}
	defer p2.stopCleanup()
	defer conn2.Close()
	e.GatewayPIDs = append(e.GatewayPIDs, p2.cmd.Process.Pid)
	e.GatewayPGIDs = append(e.GatewayPGIDs, p2.pgid)
	statusResponse, err := c2.GetProviderRefreshStatus(ctx, &pb.GetProviderRefreshStatusRequest{WorkspaceScope: scope(), Provider: provider, CredentialKey: credentialKey})
	if err != nil {
		return err
	}
	if len(statusResponse.Credentials) != 1 || statusResponse.Credentials[0].Status != "refreshed" {
		return errors.New("committed refresh status missing after restart")
	}
	if err := rotate(ctx, c2); err != nil {
		return fmt.Errorf("replacement replay proof: %w", err)
	}
	i.mu.Lock()
	valid := i.requests == 2 && !i.bad
	i.mu.Unlock()
	if !valid {
		return errors.New("issuer did not observe exactly old then persisted replacement synthetic grants")
	}
	fixture.mu.Lock()
	effects := fixture.computeMutations
	fixture.mu.Unlock()
	if effects != 0 {
		return errors.New("unexpected compute operation attempted")
	}
	e.Events = append(e.Events, "restart_resolved_persisted_replacement_without_compute")
	if err := p2.terminate(builtin); err != nil {
		return err
	}
	select {
	case <-p2.done:
		if p2.err != nil {
			return p2.err
		}
		e.GatewayExits = append(e.GatewayExits, processExit{PID: p2.cmd.Process.Pid, ExitCode: p2.cmd.ProcessState.ExitCode(), ObservedAt: time.Now().UTC().Format(time.RFC3339Nano)})
	case <-ctx.Done():
		return errors.New("verification gateway failed to exit")
	}
	if builtin {
		if err := p2.verifyGroupExited(e); err != nil {
			return err
		}
	}
	e.Passed = true
	return nil
}

func admissionClosed(err error) bool {
	return status.Code(err) == codes.Unavailable && strings.Contains(status.Convert(err).Message(), "refresh admission closed")
}

func makePKI(dir string) (*tls.Config, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "isolated shutdown fixture CA"}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.pem"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	pool.AddCert(parsed)
	var client tls.Certificate
	for index, name := range []string{"server", "client"} {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		cert := &x509.Certificate{SerialNumber: big.NewInt(int64(index + 2)), Subject: pkix.Name{CommonName: "shutdown-fixture", OrganizationalUnit: []string{"admin", "operator"}}, DNSNames: []string{"shutdown-fixture"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour)}
		der, err := x509.CreateCertificate(rand.Reader, cert, ca, &key.PublicKey, caKey)
		if err != nil {
			return nil, err
		}
		keyDER, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return nil, err
		}
		certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
		keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
		if err := os.WriteFile(filepath.Join(dir, name+".pem"), certPEM, 0600); err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(dir, name+"-key.pem"), keyPEM, 0600); err != nil {
			return nil, err
		}
		if name == "client" {
			client, err = tls.X509KeyPair(certPEM, keyPEM)
			if err != nil {
				return nil, err
			}
		}
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool, Certificates: []tls.Certificate{client}, ServerName: "shutdown-fixture"}, nil
}
