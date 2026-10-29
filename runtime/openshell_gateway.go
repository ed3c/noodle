package runtime

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/url"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"github.com/poteto/noodle/internal/admission"
	"google.golang.org/grpc"
)

// OpenShellGatewaySpec is an original owner's foreground command contract.
// ConfigFingerprint binds the shared deployment and loaded TLS trust. Worker
// routes and payloads are bound by each context's dispatch profile digest.
// This API does not discover or certify private OpenShell installations.
type OpenShellGatewaySpec struct {
	ControlRoot      string
	Resident         admission.ResidentSpec
	Binding          OpenShellCustodyBinding
	TLS              OpenShellTLS
	Version          string
	ComputeDriver    string
	ReadinessTimeout time.Duration
	Retained         []OpenShellRetainedSandbox
}

// OpenShellGateway belongs to a Host, outside any worker Runtime.Shutdown.
// Residents alone spawns, waits, signals and returns the shared allocation.
// Operations refuse immediately when another operation owns mu. Lock order is
// owner -> custody -> Residents -> child (with intermediate locks released
// when not nested). Residents drops its lock before invoking guard;
// Release never holds owner.mu across that callback. Native RPCs are bounded.
type OpenShellGateway struct {
	mu         sync.Mutex
	residents  *admission.Residents
	budget     *admission.Budget
	spec       OpenShellGatewaySpec
	custody    *OpenShellCustody
	guard      *admission.ResidentGuard
	generation *admission.ResidentGeneration
	resident   admission.Resident
	attempted  bool
	ready      bool
	closed     bool
	diagnostic error
}

// NewOpenShellGateway reserves canonical local custody before any service spawn.
// A non-nil owner on error retains diagnostic custody and must be closed through
// this owner. No resident or remote VM is started by construction.
func NewOpenShellGateway(residents *admission.Residents, budget *admission.Budget, spec OpenShellGatewaySpec) (*OpenShellGateway, error) {
	if residents == nil || budget == nil || spec.ReadinessTimeout <= 0 || spec.ReadinessTimeout > 30*time.Second || strings.TrimSpace(spec.Version) == "" || strings.TrimSpace(spec.ComputeDriver) == "" || spec.Resident.Sharing != "shared" || spec.Resident.ForceAfterGrace {
		return nil, fmt.Errorf("openshell gateway owner, shared foreground contract or bounded readiness configuration invalid")
	}
	spec.Resident.Argv = slices.Clone(spec.Resident.Argv)
	spec.Resident.Environment = slices.Clone(spec.Resident.Environment)
	spec.Retained = slices.Clone(spec.Retained)
	if err := admission.ValidateResidentSpec(spec.Resident); err != nil {
		return nil, err
	}
	for _, v := range []string{spec.Binding.StateRoot, spec.Binding.Gateway, spec.Binding.Workspace, spec.Binding.Owner, spec.Binding.ConfigSource, spec.Binding.ConfigFingerprint, spec.Version, spec.ComputeDriver} {
		if strings.TrimSpace(v) == "" || strings.ContainsRune(v, 0) {
			return nil, fmt.Errorf("openshell gateway binding/version/driver missing or invalid")
		}
	}
	u, err := url.Parse(spec.Binding.Gateway)
	if err != nil || u.Scheme != "https" || net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback() {
		return nil, fmt.Errorf("openshell managed gateway is not a loopback HTTPS endpoint")
	}
	spec.TLS, err = freezeOpenShellTLS(spec.TLS)
	if err != nil {
		return nil, err
	}
	conn, err := newOpenShellConnection(spec.Binding.Gateway, spec.TLS)
	if err != nil {
		return nil, err
	}
	c, err := reserveOpenShellCustody(spec.Binding, conn, spec.Retained, spec.ControlRoot)
	if c == nil {
		return nil, err
	}
	c.managed, c.retryReadback = true, true
	spec.Binding = c.state.Binding
	o := &OpenShellGateway{residents: residents, budget: budget, spec: spec, custody: c, diagnostic: err}
	// The native identity also includes the original command via Residents' own
	// executable/config fingerprint. Configuration source is never just a name.
	data, identityErr := json.Marshal(struct {
		Binding                     OpenShellCustodyBinding
		Version, Driver, ServerName string
		Certificates                [][]byte
	}{spec.Binding, spec.Version, spec.ComputeDriver, spec.TLS.ServerName, spec.TLS.Certificate.Certificate})
	if identityErr != nil {
		o.diagnostic = errors.Join(err, identityErr)
		return o, o.diagnostic
	}
	sum := sha256.Sum256(data)
	o.guard, identityErr = admission.NewResidentGuard(fmt.Sprintf("openshell/%x", sum), spec.ReadinessTimeout, o.quiesce)
	if identityErr != nil {
		o.diagnostic = errors.Join(err, identityErr)
		return o, o.diagnostic
	}
	if err == nil && c.state.Blocked != "" {
		err = fmt.Errorf("openshell custody blocked: %s", c.state.Blocked)
		o.diagnostic = err
	}
	return o, err
}

// Freeze the key as well as its certificate buffers; tls.Config.Clone alone
// shares these mutable values with the caller. Unsupported key types refuse.
func freezeOpenShellTLS(s OpenShellTLS) (OpenShellTLS, error) {
	if s.RootCAs == nil || len(s.Certificate.Certificate) == 0 || s.Certificate.PrivateKey == nil {
		return OpenShellTLS{}, fmt.Errorf("openshell gateway CA or mTLS client certificate missing")
	}
	key, err := x509.MarshalPKCS8PrivateKey(s.Certificate.PrivateKey)
	if err != nil {
		return OpenShellTLS{}, fmt.Errorf("snapshot openshell client key: %w", err)
	}
	var certPEM []byte
	for _, der := range s.Certificate.Certificate {
		certPEM = append(certPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})...)
	}
	cert, err := tls.X509KeyPair(certPEM, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}))
	if err != nil {
		return OpenShellTLS{}, fmt.Errorf("snapshot openshell client certificate: %w", err)
	}
	cert.OCSPStaple = slices.Clone(s.Certificate.OCSPStaple)
	cert.SupportedSignatureAlgorithms = slices.Clone(s.Certificate.SupportedSignatureAlgorithms)
	for _, timestamp := range s.Certificate.SignedCertificateTimestamps {
		cert.SignedCertificateTimestamps = append(cert.SignedCertificateTimestamps, slices.Clone(timestamp))
	}
	return OpenShellTLS{RootCAs: s.RootCAs.Clone(), Certificate: cert, ServerName: s.ServerName}, nil
}

// Acquire retains the original lease on readiness failure. Repeated acquire
// never retries readiness or starts a replacement; use explicit Readback.
func (o *OpenShellGateway) Acquire(ctx context.Context, contextID string) (*OpenShellCustody, error) {
	if !o.mu.TryLock() {
		return nil, fmt.Errorf("openshell gateway operation in progress")
	}
	defer o.mu.Unlock()
	if o.closed {
		return nil, fmt.Errorf("openshell gateway owner closed")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !o.attempted {
		if o.diagnostic != nil {
			return nil, o.diagnostic
		}
		if err := o.listenerAbsent(ctx); err != nil {
			return nil, err
		}
	} else if o.generation == nil {
		return nil, errors.Join(o.diagnostic, fmt.Errorf("openshell original generation unavailable; spawn replay refused"))
	}
	if o.attempted {
		if err := o.generation.Running(); err != nil {
			o.disable(err)
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	v, err := o.residents.AcquireGuarded(o.spec.Resident, contextID, o.budget, o.guard)
	if err != nil {
		if v.ID != "" {
			o.attempted = true
			o.resident = v
			o.diagnostic = err
		}
		return nil, err
	}
	o.resident = v
	if !o.attempted {
		o.attempted = true
		o.generation, err = o.residents.OriginalGeneration(v, o.guard)
		if err != nil {
			o.diagnostic = err
			return nil, err
		}
		o.custody.residentRunning = o.generation.Running
		return o.readback(ctx)
	}
	if err := o.generation.Running(); err != nil {
		o.disable(err)
		return nil, err
	}
	if !o.ready {
		return nil, errors.Join(o.diagnostic, fmt.Errorf("openshell gateway requires explicit original generation readback"))
	}
	if !o.custody.mu.TryLock() {
		return nil, fmt.Errorf("openshell custody operation in progress")
	}
	defer o.custody.mu.Unlock()
	if err := o.custody.mutable(); err != nil {
		return nil, err
	}
	return o.custody, nil
}

func (o *OpenShellGateway) listenerAbsent(ctx context.Context) error {
	u, _ := url.Parse(o.spec.Binding.Gateway)
	port := u.Port()
	if port == "" {
		port = "443"
	}
	dialCtx, cancel := context.WithTimeout(ctx, o.spec.ReadinessTimeout)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", net.JoinHostPort(u.Hostname(), port))
	if err == nil {
		_ = conn.Close()
		return fmt.Errorf("openshell gateway listener already present; takeover refused")
	}
	if !errors.Is(err, syscall.ECONNREFUSED) {
		return fmt.Errorf("openshell gateway listener absence unconfirmed: %w", err)
	}
	return nil
}

func (o *OpenShellGateway) disable(err error) { o.ready = false; o.diagnostic = err }

func (o *OpenShellGateway) Readback(ctx context.Context) (*OpenShellCustody, error) {
	if !o.mu.TryLock() {
		return nil, fmt.Errorf("openshell gateway operation in progress")
	}
	defer o.mu.Unlock()
	if o.closed || o.generation == nil {
		return nil, fmt.Errorf("openshell original gateway generation unavailable")
	}
	return o.readback(ctx)
}

func (o *OpenShellGateway) readback(ctx context.Context) (*OpenShellCustody, error) {
	if !o.custody.mu.TryLock() {
		return nil, fmt.Errorf("openshell custody operation in progress")
	}
	defer o.custody.mu.Unlock()
	c := o.custody
	o.ready, c.initialized = false, false
	ctx, cancel := context.WithTimeout(ctx, o.spec.ReadinessTimeout)
	defer cancel()
	err := o.generation.Running()
	if err == nil {
		err = o.nativeReadiness(ctx)
	}
	if err == nil {
		err = c.initialize(ctx)
	}
	if err == nil {
		err = o.generation.Running()
	}
	if err == nil {
		err = ctx.Err()
	}
	o.diagnostic = err
	if err != nil {
		c.initialized = false
		return nil, err
	}
	if c.quiesced {
		o.diagnostic = fmt.Errorf("openshell custody quiesced; reactivation refused")
		return nil, o.diagnostic
	}
	o.ready = true
	return c, nil
}

// Only original-start read-only probes may wait for startup; no VM replay,
// process restart or automatic termination follows any failure.
func (o *OpenShellGateway) nativeReadiness(ctx context.Context) error {
	c := o.custody
	for {
		health, err := c.client.Health(ctx, &pb.HealthRequest{}, grpc.WaitForReady(true))
		if err != nil {
			return fmt.Errorf("openshell native health unavailable: %w", err)
		}
		info, err := c.client.GetGatewayInfo(ctx, &pb.GetGatewayInfoRequest{}, grpc.WaitForReady(true))
		if err != nil {
			return fmt.Errorf("openshell native gateway info unavailable: %w", err)
		}
		drivers := info.GetComputeDrivers()
		if health.GetVersion() != o.spec.Version || info.GetGatewayVersion() != o.spec.Version || len(drivers) != 1 || drivers[0].GetName() != o.spec.ComputeDriver {
			return c.block("gateway native version/compute driver conflicts with owner binding")
		}
		if health.GetStatus() == pb.ServiceStatus_SERVICE_STATUS_HEALTHY && info.GetStatus() == pb.ServiceStatus_SERVICE_STATUS_HEALTHY {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("openshell native gateway unhealthy: %w", ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (o *OpenShellGateway) quiesce(ctx context.Context) error {
	if !o.mu.TryLock() {
		return fmt.Errorf("openshell gateway operation in progress")
	}
	defer o.mu.Unlock()
	if o.closed || o.generation == nil {
		return fmt.Errorf("openshell gateway native initialization incomplete; release refused")
	}
	if err := o.generation.Running(); err != nil {
		o.disable(err)
		return err
	}
	// Custody checks native initialization independently of dispatch readiness.
	// A previous quiesce may have sealed dispatch before its guard timed out.
	if err := o.custody.Quiesce(ctx); err != nil {
		return err
	}
	o.ready = false
	return nil
}

// Release uses the exact original guard, including when Host calls Residents
// directly. It never holds owner.mu while Residents invokes that callback.
func (o *OpenShellGateway) Release(contextID string) (admission.Resident, error) {
	if !o.mu.TryLock() {
		return admission.Resident{}, fmt.Errorf("openshell gateway operation in progress")
	}
	if o.closed || o.generation == nil {
		o.mu.Unlock()
		return admission.Resident{}, fmt.Errorf("openshell original gateway generation unavailable")
	}
	generation := o.generation
	o.mu.Unlock()
	return generation.Release(contextID)
}

// Close releases local custody only after Residents confirms no local child
// obligations remain. It neither signals the service nor certifies VM cleanup.
func (o *OpenShellGateway) Close() error {
	if !o.mu.TryLock() {
		return fmt.Errorf("openshell gateway operation in progress")
	}
	defer o.mu.Unlock()
	if o.closed {
		return nil
	}
	if err := o.residents.CheckServiceClosed(o.spec.Resident.Name); err != nil {
		return err
	}
	o.closed, o.ready = true, false
	return o.custody.closeLocal()
}

// Diagnostic returns the last bounded initialization/readback result.
func (o *OpenShellGateway) Diagnostic() error {
	if !o.mu.TryLock() {
		return fmt.Errorf("openshell gateway operation in progress")
	}
	defer o.mu.Unlock()
	return o.diagnostic
}
