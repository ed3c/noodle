package runtime

import (
	"bytes"
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
	"math/big"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	dm "github.com/NVIDIA/OpenShell/sdk/go/proto/datamodelv1"
	pb "github.com/NVIDIA/OpenShell/sdk/go/proto/openshellv1"
	"github.com/poteto/noodle/internal/admission"
	"github.com/poteto/noodle/internal/lockfile"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// This is an isolated role guard, not evidence about a deployed OpenShell
// server. Both sides use verified mTLS and the original gRPC client factory.
type sourceAuthTransportGuard struct {
	mu          sync.Mutex
	calls       int
	denied      int
	getter      bool
	fingerprint [sha256.Size]byte
}

func (g *sourceAuthTransportGuard) intercept(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	g.mu.Lock()
	g.calls++
	check := !g.getter || strings.HasSuffix(info.FullMethod, "/GetProviderCredentials")
	p, ok := peer.FromContext(ctx)
	var auth credentials.TLSInfo
	if ok {
		auth, ok = p.AuthInfo.(credentials.TLSInfo)
	}
	allowed := ok && len(auth.State.VerifiedChains) > 0
	if allowed && check {
		leaf := auth.State.VerifiedChains[0][0]
		role := "user"
		if g.getter {
			role = "operator"
		}
		allowed = slices.Contains(leaf.Subject.OrganizationalUnit, role) && sha256.Sum256(leaf.Raw) == g.fingerprint
	}
	if !allowed {
		g.denied++
	}
	g.mu.Unlock()
	if !allowed {
		return nil, status.Error(codes.PermissionDenied, "isolated certificate role guard")
	}
	return handler(ctx, req)
}

func (g *sourceAuthTransportGuard) counts() (int, int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls, g.denied
}

func sourceAuthTransportRPC(t *testing.T, server pb.OpenShellServer, security *tls.Config, guard *sourceAuthTransportGuard) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("isolated mTLS TCP acceptance not observed: %v", err)
	}
	rpc := grpc.NewServer(grpc.Creds(credentials.NewTLS(security)), grpc.UnaryInterceptor(guard.intercept))
	pb.RegisterOpenShellServer(rpc, server)
	done := make(chan struct{})
	go func() { defer close(done); _ = rpc.Serve(listener) }()
	t.Cleanup(func() { rpc.Stop(); <-done })
	return "https://" + listener.Addr().String()
}

func sourceAuthTransportPKI(t *testing.T, root string) (*tls.Config, map[string]tls.Certificate) {
	t.Helper()
	newKey := func() *ecdsa.PrivateKey {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		return key
	}
	serial := int64(200)
	issue := func(cert, parent *x509.Certificate, key, signer *ecdsa.PrivateKey) []byte {
		t.Helper()
		serial++
		cert.SerialNumber = big.NewInt(serial)
		der, err := x509.CreateCertificate(rand.Reader, cert, parent, &key.PublicKey, signer)
		if err != nil {
			t.Fatal(err)
		}
		return der
	}
	caKey := newKey()
	ca := &x509.Certificate{Subject: pkix.Name{CommonName: "isolated Source CA"}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	caDER := issue(ca, ca, caKey, caKey)
	var err error
	ca, err = x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	intermediateKey := newKey()
	intermediate := &x509.Certificate{Subject: pkix.Name{CommonName: "isolated intermediate"}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: ca.NotBefore, NotAfter: ca.NotAfter}
	intermediateDER := issue(intermediate, ca, intermediateKey, caKey)
	intermediate, err = x509.ParseCertificate(intermediateDER)
	if err != nil {
		t.Fatal(err)
	}
	write := func(name string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write("source-ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
	pairs := make(map[string]tls.Certificate)
	for _, name := range []string{"user", "operator", "client", "server", "expired", "wrong-ca"} {
		key := newKey()
		cert := &x509.Certificate{Subject: pkix.Name{OrganizationalUnit: []string{"user"}}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, NotBefore: ca.NotBefore, NotAfter: ca.NotAfter}
		parent, signer := ca, caKey
		chain := [][]byte{caDER}
		if name == "operator" {
			cert.Subject.OrganizationalUnit = []string{"operator"}
			parent, signer, chain = intermediate, intermediateKey, [][]byte{intermediateDER, caDER}
		}
		if name == "expired" || name == "wrong-ca" {
			cert.Subject.OrganizationalUnit = []string{"operator"}
		}
		if name == "expired" {
			cert.NotBefore, cert.NotAfter = time.Now().Add(-2*time.Hour), time.Now().Add(-time.Hour)
		}
		if name == "wrong-ca" {
			otherKey := newKey()
			other := &x509.Certificate{Subject: pkix.Name{CommonName: "foreign CA"}, IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: ca.NotBefore, NotAfter: ca.NotAfter}
			otherDER := issue(other, other, otherKey, otherKey)
			parent, err = x509.ParseCertificate(otherDER)
			if err != nil {
				t.Fatal(err)
			}
			signer, chain = otherKey, [][]byte{otherDER}
		}
		if name == "server" {
			cert.DNSNames = []string{"source.test", "gateway.test"}
			cert.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		}
		der := issue(cert, parent, key, signer)
		pair := tls.Certificate{Certificate: append([][]byte{der}, chain...), PrivateKey: key}
		pairs[name] = pair
		var certificates []byte
		for _, item := range pair.Certificate {
			certificates = append(certificates, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: item})...)
		}
		private, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		write(name+".pem", certificates)
		write(name+".key", pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}))
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	return &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pairs["server"]}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots}, pairs
}

type sourceAuthTransportFixture struct {
	path, template, dispatch, root string
	profile                        OpenShellProfile
	plan                           SourceAuthPlan
	residents                      *admission.Residents
	source, target                 *credentialNative
	sourceGuard, targetGuard       *sourceAuthTransportGuard
	plain                          []byte
}

func newSourceAuthTransportFixture(t *testing.T) *sourceAuthTransportFixture {
	t.Helper()
	path, template, dispatch, file := sourceAuthFixture(t)
	root, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(root, filepath.Base(path))
	template = filepath.Join(root, filepath.Base(template))
	dispatch = filepath.Join(root, filepath.Base(dispatch))
	file.Gateway.Resident.Argv[0] = filepath.Join(root, "gateway")
	file.Gateway.Resident.Directory = root
	file.Environment = []string{"XDG_CONFIG_HOME=" + filepath.Join(root, "client")}
	f := &sourceAuthTransportFixture{path: path, template: template, dispatch: dispatch, root: root}
	security, pairs := sourceAuthTransportPKI(t, f.root)
	f.sourceGuard = &sourceAuthTransportGuard{getter: true, fingerprint: sha256.Sum256(pairs["operator"].Certificate[0])}
	f.targetGuard = &sourceAuthTransportGuard{fingerprint: sha256.Sum256(pairs["client"].Certificate[0])}
	f.source = &credentialNative{provider: &dm.Provider{Metadata: &dm.ObjectMeta{Name: "chosen", Id: "source-provider", Workspace: "source-workspace"}, Type: "original", ProfileWorkspace: "source-workspace"}, profile: &pb.ProviderProfile{Id: "original", Credentials: []*pb.ProviderProfileCredential{{Name: "access", EnvVars: []string{"CODEX_AUTH_ACCESS_TOKEN"}, Required: true}}}, values: map[string]*pb.ProviderCredentialValue{"CODEX_AUTH_ACCESS_TOKEN": {Value: "synthetic-initial-access", ExpirationTime: timestamppb.New(time.Now().Add(time.Hour))}}}
	f.target = &credentialNative{}
	file.CredentialSource.Gateway = sourceAuthTransportRPC(t, f.source, security, f.sourceGuard)
	file.Gateway.Binding.Gateway = sourceAuthTransportRPC(t, f.target, security, f.targetGuard)
	file.Gateway.Binding.Workspace = "owned-workspace"
	file.Gateway.TLS.CAFile = "source-ca.pem"
	clientRoot := filepath.Join(f.root, "client", "openshell", "gateways", "dedicated")
	for from, to := range map[string]string{"source-ca.pem": "ca.crt", "client.pem": "tls.crt", "client.key": "tls.key"} {
		raw, err := os.ReadFile(filepath.Join(f.root, from))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(clientRoot, "mtls", to), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	storageFailureWrite(t, filepath.Join(clientRoot, "metadata.json"), map[string]string{"name": "dedicated", "gateway_endpoint": file.Gateway.Binding.Gateway, "auth_mode": "mtls"})
	writeProfileFixture(t, path, file)
	candidate := file
	origin := *file.CredentialSource
	origin.TLS.CertificateFile, origin.TLS.KeyFile = "operator.pem", "operator.key"
	candidate.CredentialSource = &origin
	writeProfileFixture(t, template, candidate)
	f.profile, err = LoadOpenShellProfile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := OpenShellDispatchSHA256(f.profile)
	if err != nil {
		t.Fatal(err)
	}
	storageFailureWrite(t, dispatch, openShellDispatchReceipt{Version: 1, ContextID: "context", OrderID: "order", AttemptID: "native-attempt", SessionID: "session", Target: filepath.Join(f.root, "worktree"), Phase: "credential_waiting", ProfileSHA256: digest})
	external, _, err := externalOpenShellProfile(f.source.profile)
	if err != nil {
		t.Fatal(err)
	}
	f.target.profile = external
	f.target.provider = &dm.Provider{Metadata: &dm.ObjectMeta{Name: "chosen", Id: "target-provider", Workspace: "owned-workspace", ResourceVersion: 7}, Type: external.Id, ProfileWorkspace: "owned-workspace"}
	state := OpenShellCustodyState{Version: 5, ControlRoot: f.root, Binding: f.profile.Gateway.Binding, Credentials: []OpenShellCredentialDelivery{{Provider: "chosen", SourceBinding: f.profile.CredentialOrigin.BindingSHA256, SourceProviderID: "source-provider", ProfileID: external.Id, ProviderID: "target-provider", SourceReadPending: true, TargetReceipts: [][]byte{[]byte("opaque-original-receipt")}}}}
	ownerPath := filepath.Join(state.Binding.StateRoot, "owner.json")
	storageFailureWrite(t, ownerPath, state)
	f.residents, err = admission.OpenResidents(f.root)
	if err != nil {
		t.Fatal(err)
	}
	spec := f.profile.Gateway
	spec.ControlRoot = f.root
	ordinary, err := NewOpenShellGateway(f.residents, admission.NewBudget(9), spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ordinary.Close() })
	request := &pb.UpdateProviderRequest{WorkspaceScope: &dm.WorkspaceSelector{Selection: &dm.WorkspaceSelector_Workspace{Workspace: "owned-workspace"}}, RequestId: "original-pending-id", Provider: &dm.Provider{Metadata: &dm.ObjectMeta{Name: "chosen", ResourceVersion: 7}, Type: external.Id, Credentials: map[string]string{"CODEX_AUTH_ACCESS_TOKEN": "synthetic-original-pending"}}}
	f.plain, err = openShellBytes(request)
	if err != nil {
		t.Fatal(err)
	}
	row := state.Credentials[0]
	if err := ordinary.saveCredentialIntent(&row, "update", request.RequestId, request); err != nil {
		t.Fatal(err)
	}
	if err := ordinary.Close(); err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if _, err := ReadSessionReadbackFile(ownerPath, &raw); err != nil {
		t.Fatal(err)
	}
	raw["opaque_history"] = json.RawMessage(`{"unknown":"original failure retained"}`)
	storageFailureWrite(t, ownerPath, raw)
	f.plan, err = InspectSourceAuthPlan(path, template, dispatch, "context", f.root)
	if err != nil {
		t.Fatal(err)
	}
	f.register(t)
	storageFailureWrite(t, filepath.Join(f.root, "frozen-input.json"), map[string]string{"request": "original-request", "canonical_attempt": "canonical-attempt", "native_attempt": "native-attempt", "session": "session", "unknown": "retained"})
	storageFailureWrite(t, filepath.Join(f.root, "input.txt"), []byte("original synthetic worker input"))
	return f
}

func (f *sourceAuthTransportFixture) register(t *testing.T) {
	t.Helper()
	candidate, err := LoadOpenShellProfile(f.template)
	if err != nil {
		t.Fatal(err)
	}
	for name := range f.plan.Sources {
		if strings.HasPrefix(name, "template:") {
			delete(f.plan.Sources, name)
		}
	}
	for path := range candidate.sourceFiles {
		ref, err := ReadSessionReadbackFile(path, nil)
		if err != nil {
			t.Fatal(err)
		}
		f.plan.Sources["template:"+path] = ref
	}
	ref := f.plan.Sources["template:"+f.template]
	f.plan.Sources["template_profile"], f.plan.Sources["registered_template"] = ref, ref
	f.plan.TemplatePolicySHA256 = candidate.SourceSHA256
	f.plan.ProposedAccess.BindingSHA256 = candidate.CredentialOrigin.BindingSHA256
	certHash := sha256.Sum256(candidate.CredentialOrigin.TLS.Certificate.Certificate[0])
	f.plan.ProposedAccess.ClientCertificateSHA256 = hex.EncodeToString(certHash[:])
	f.plan.Sources["template_registration"] = storageFailureWrite(t, filepath.Join(f.root, "registration.json"), map[string]any{"schema": 1, "profile": ref, "policy_sha256": candidate.SourceSHA256})
}

func (f *sourceAuthTransportFixture) open(t *testing.T) *OpenShellGateway {
	t.Helper()
	owner, err := NewOpenShellSourceAuthGateway(f.residents, admission.NewBudget(9), f.path, f.plan, f.root)
	if owner != nil {
		t.Cleanup(func() { _ = owner.Close() })
	}
	if err != nil {
		t.Fatal(err)
	}
	return owner
}

func sourceAuthTransportStart(t *testing.T, owner *OpenShellGateway) {
	t.Helper()
	owner.custody.mu.Lock()
	err := owner.startCredentials()
	owner.custody.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
}

func sourceAuthTransportGet(t *testing.T, client pb.OpenShellClient) (*pb.GetProviderCredentialsResponse, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	return client.GetProviderCredentials(ctx, &pb.GetProviderCredentialsRequest{Name: "chosen", WorkspaceScope: &dm.WorkspaceSelector{Selection: &dm.WorkspaceSelector_Workspace{Workspace: "source-workspace"}}, CredentialKeys: []string{"CODEX_AUTH_ACCESS_TOKEN"}})
}

func (f *sourceAuthTransportFixture) bytes(t *testing.T) map[string][]byte {
	t.Helper()
	paths := []string{f.path, f.template, f.dispatch, filepath.Join(f.profile.Gateway.Binding.StateRoot, "owner.json"), filepath.Join(f.root, "registration.json"), filepath.Join(f.root, "frozen-input.json"), filepath.Join(f.root, "input.txt")}
	for path := range f.profile.sourceFiles {
		paths = append(paths, path)
	}
	for _, ref := range f.plan.Sources {
		if ref.Path != "" {
			paths = append(paths, ref.Path)
		}
	}
	result := make(map[string][]byte)
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		result[path] = raw
	}
	return result
}

func sourceAuthTransportUnchanged(t *testing.T, before map[string][]byte) {
	t.Helper()
	for path, expected := range before {
		current, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(current, expected) {
			t.Fatalf("original bytes changed at %s: %v", path, err)
		}
	}
}

func TestSourceAuthTransportOriginalConsumerAndRoleBoundary(t *testing.T) {
	f := newSourceAuthTransportFixture(t)
	before := f.bytes(t)
	spec := f.profile.Gateway
	spec.ControlRoot = f.root
	ordinary, err := NewOpenShellGateway(f.residents, admission.NewBudget(9), spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ordinary.Close() })
	sourceAuthTransportStart(t, ordinary)
	if response, err := sourceAuthTransportGet(t, ordinary.credentialClient); status.Code(err) != codes.PermissionDenied || len(response.GetCredentials()) != 0 {
		t.Fatal("ordinary Source getter bypassed isolated operator guard", err)
	}
	if err := ordinary.Close(); err != nil {
		t.Fatal(err)
	}
	owner := f.open(t)
	if owner.credentialClient != nil || owner.credentialConnection != nil {
		t.Fatal("factory started the credential channel")
	}
	if calls, denied := f.sourceGuard.counts(); calls != 1 || denied != 1 {
		t.Fatal("factory contacted Source")
	}
	if calls, _ := f.targetGuard.counts(); calls != 0 {
		t.Fatal("construction contacted Target")
	}
	if owner.spec.Binding != f.profile.Gateway.Binding || owner.spec.CredentialSource.BindingSHA256 != f.profile.CredentialOrigin.BindingSHA256 || owner.spec.CredentialSource.TLS.ServerName != f.profile.CredentialOrigin.TLS.ServerName || !reflect.DeepEqual(owner.spec.TLS.Certificate.Certificate, f.profile.Gateway.TLS.Certificate.Certificate) || !reflect.DeepEqual(owner.spec.CredentialSource.TLS.Certificate.Certificate, f.profile.CredentialOrigin.TLS.Certificate.Certificate) {
		t.Fatal("effective auth replaced a logical binding or ordinary TLS")
	}
	oldKey, err := x509.MarshalPKCS8PrivateKey(f.profile.Gateway.TLS.Certificate.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	key, err := x509.MarshalPKCS8PrivateKey(owner.spec.TLS.Certificate.PrivateKey)
	if err != nil || !bytes.Equal(key, oldKey) {
		t.Fatal("Target replay key changed")
	}
	row := owner.custody.Snapshot().Credentials[0]
	aead, err := owner.credentialCipher()
	if err != nil {
		t.Fatal(err)
	}
	sealed := row.Pending.SealedPayload
	plain, err := aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], owner.credentialAAD(&row, row.Pending))
	if err != nil || !bytes.Equal(plain, f.plain) || !row.SourceReadPending || row.Pending.RequestID != "original-pending-id" {
		t.Fatal("original valid ciphertext/AAD or pending identity changed", err)
	}
	var pending pb.UpdateProviderRequest
	if proto.Unmarshal(plain, &pending) != nil || pending.RequestId != row.Pending.RequestID || pending.Provider.GetMetadata().GetResourceVersion() != 7 {
		t.Fatal("original pending protobuf or CAS changed")
	}
	sourceAuthTransportStart(t, owner)
	response, err := sourceAuthTransportGet(t, owner.credentialClient)
	if err != nil || response.GetCredentials()["CODEX_AUTH_ACCESS_TOKEN"].GetValue() != "synthetic-initial-access" {
		t.Fatal("factory TLS did not reach original credential client", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := owner.custody.client.GetProvider(ctx, &pb.GetProviderRequest{Name: "chosen"}); err != nil {
		t.Fatal("ordinary Target transport changed", err)
	}
	operator, err := newOpenShellConnection(owner.spec.Binding.Gateway, *owner.credentialTLS)
	if err != nil {
		t.Fatal(err)
	}
	defer operator.Close()
	if _, err := pb.NewOpenShellClient(operator).GetProvider(ctx, &pb.GetProviderRequest{Name: "chosen"}); status.Code(err) != codes.PermissionDenied {
		t.Fatal("isolated Target guard accepted Source operator", err)
	}
	targetAtSource, err := newOpenShellConnection(owner.spec.CredentialSource.Gateway, owner.spec.TLS)
	if err != nil {
		t.Fatal(err)
	}
	defer targetAtSource.Close()
	if response, err := sourceAuthTransportGet(t, pb.NewOpenShellClient(targetAtSource)); status.Code(err) != codes.PermissionDenied || len(response.GetCredentials()) != 0 {
		t.Fatal("ordinary Target gained Source operator authority", err)
	}
	if calls, denied := f.sourceGuard.counts(); calls != 3 || denied != 2 {
		t.Fatal("unexpected Source RPCs or credential disclosure")
	}
	if calls, denied := f.targetGuard.counts(); calls != 2 || denied != 1 {
		t.Fatal("unexpected Target authority")
	}
	sourceAuthTransportUnchanged(t, before)
	if f.plan.Status != "unknown" || f.plan.Executable || len(f.plan.Effects) != 0 {
		t.Fatal("transport construction rewrote proposal authority")
	}
	resident, err := json.Marshal(owner.spec.Resident)
	if err != nil {
		t.Fatal(err)
	}
	route, err := openShellBytes(f.profile.Routes["codex"].Spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, material := range []string{"operator.pem", "operator.key"} {
		secret, err := os.ReadFile(filepath.Join(f.root, material))
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(resident, secret) || bytes.Contains(route, secret) {
			t.Fatal("operator material reached resident or worker payload")
		}
		err = filepath.WalkDir(f.root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || path == filepath.Join(f.root, material) {
				return nil
			}
			raw, err := os.ReadFile(path)
			if err == nil && bytes.Contains(raw, secret) {
				t.Errorf("operator material copied outside its original source at %s", path)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestSourceAuthTransportRejectsDriftBeforeReservation(t *testing.T) {
	for _, fault := range []string{"invented", "schema", "executable", "effects", "status", "current-endpoint", "proposed-endpoint", "workspace", "servername", "trust", "ordinary-cert", "operator-cert", "logical-source", "operator-binding", "frozen", "context", "order", "native-attempt", "session", "worktree", "candidate-ref", "registration-ref", "registration-bytes", "dependency-ref", "dispatch-ref", "custody-ref", "missing-dependency", "extra-dependency", "ordinary-bytes", "candidate-bytes", "profile-path", "control-root", "custody-binding", "custody-source", "native-task", "dispatch-tuple"} {
		t.Run(fault, func(t *testing.T) {
			f := newSourceAuthTransportFixture(t)
			path, control := f.path, f.root
			switch fault {
			case "invented":
				f.plan = SourceAuthPlan{Schema: 1, Status: "unknown"}
			case "schema":
				f.plan.Schema++
			case "executable":
				f.plan.Executable = true
			case "effects":
				f.plan.Effects = []string{"invented-authority"}
			case "status":
				f.plan.Status = "qualified"
			case "current-endpoint":
				f.plan.CurrentAccess.Gateway = "https://127.0.0.1:1"
			case "proposed-endpoint":
				f.plan.ProposedAccess.Gateway = "https://127.0.0.1:1"
			case "workspace":
				f.plan.ProposedAccess.Workspace = "foreign"
			case "servername":
				f.plan.ProposedAccess.ServerName = "foreign.test"
			case "trust":
				f.plan.ProposedAccess.TrustSHA256 = strings.Repeat("b", 64)
			case "ordinary-cert":
				f.plan.CurrentAccess.ClientCertificateSHA256 = strings.Repeat("b", 64)
			case "operator-cert":
				f.plan.ProposedAccess.ClientCertificateSHA256 = strings.Repeat("b", 64)
			case "logical-source":
				f.plan.CurrentAccess.BindingSHA256 = strings.Repeat("b", 64)
			case "operator-binding":
				f.plan.ProposedAccess.BindingSHA256 = strings.Repeat("b", 64)
			case "frozen":
				f.plan.FrozenProfileSHA256 = strings.Repeat("b", 64)
			case "context":
				f.plan.ContextID = "foreign"
			case "order":
				f.plan.OrderID = "foreign"
			case "native-attempt":
				f.plan.AttemptID = "foreign"
			case "session":
				f.plan.SessionID = "foreign"
			case "worktree":
				f.plan.WorktreePath = filepath.Join(f.root, "foreign")
			case "candidate-ref", "registration-ref", "dependency-ref":
				name := "registered_template"
				if fault == "registration-ref" {
					name = "template_registration"
				} else if fault == "dependency-ref" {
					name = "original:" + f.path
				}
				ref := f.plan.Sources[name]
				ref.SHA256 = strings.Repeat("b", 64)
				f.plan.Sources[name] = ref
			case "registration-bytes":
				storageFailureWrite(t, f.plan.Sources["template_registration"].Path, map[string]any{"schema": 1, "profile": f.plan.Sources["template_profile"], "policy_sha256": "foreign"})
			case "dispatch-ref", "custody-ref":
				name := strings.TrimSuffix(fault, "-ref")
				ref := f.plan.Sources[name]
				ref.SHA256 = ""
				f.plan.Sources[name] = ref
			case "missing-dependency":
				delete(f.plan.Sources, "original:"+f.path)
			case "extra-dependency":
				f.plan.Sources["original:foreign"] = f.plan.Sources["template_profile"]
			case "ordinary-bytes", "candidate-bytes":
				name := "user.pem"
				if fault == "candidate-bytes" {
					name = "operator.pem"
				}
				file := filepath.Join(f.root, name)
				raw, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				storageFailureWrite(t, file, append(raw, '\n'))
			case "profile-path":
				path, _, _, _ = sourceAuthFixture(t)
			case "control-root":
				control = t.TempDir()
			case "custody-binding", "custody-source", "native-task":
				var state OpenShellCustodyState
				name := filepath.Join(f.profile.Gateway.Binding.StateRoot, "owner.json")
				if _, err := ReadSessionReadbackFile(name, &state); err != nil {
					t.Fatal(err)
				}
				if fault == "custody-binding" {
					state.Binding.Owner = "foreign"
				} else if fault == "custody-source" {
					state.Credentials[0].SourceBinding = strings.Repeat("b", 64)
				} else {
					state.Records = []OpenShellCustodyRecord{{Name: "original", Task: OpenShellTask{OrderID: "order", AttemptID: "foreign", SessionID: "session"}}}
				}
				storageFailureWrite(t, name, state)
			case "dispatch-tuple":
				var receipt openShellDispatchReceipt
				if _, err := ReadSessionReadbackFile(f.dispatch, &receipt); err != nil {
					t.Fatal(err)
				}
				receipt.AttemptID = "foreign"
				storageFailureWrite(t, f.dispatch, receipt)
			}
			before := f.bytes(t)
			owner, err := NewOpenShellSourceAuthGateway(f.residents, admission.NewBudget(9), path, f.plan, control)
			if owner != nil {
				_ = owner.Close()
			}
			if err == nil || owner != nil {
				t.Fatal("drift reached custody reservation", fault, err)
			}
			sourceAuthTransportUnchanged(t, before)
			if calls, _ := f.sourceGuard.counts(); calls != 0 {
				t.Fatal("refusal contacted Source")
			}
			if calls, _ := f.targetGuard.counts(); calls != 0 {
				t.Fatal("refusal contacted Target")
			}
			lock, err := lockfile.TryLock(filepath.Join(f.profile.Gateway.Binding.StateRoot, "owner.lock"))
			if err != nil {
				t.Fatal("refusal retained custody lock", err)
			}
			if err := lock.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSourceAuthTransportRevalidatesRegisteredPrincipal(t *testing.T) {
	for _, fault := range []string{"nonoperator", "expired", "wrong-ca", "missing-chain", "wrong-key", "endpoint", "workspace", "servername", "trust"} {
		t.Run(fault, func(t *testing.T) {
			f := newSourceAuthTransportFixture(t)
			if fault == "nonoperator" || fault == "expired" || fault == "wrong-ca" || fault == "wrong-key" {
				from := fault
				if fault == "nonoperator" || fault == "wrong-key" {
					from = "user"
				}
				parts := []string{"pem", "key"}
				if fault == "wrong-key" {
					parts = []string{"key"}
				}
				for _, suffix := range parts {
					raw, err := os.ReadFile(filepath.Join(f.root, from+"."+suffix))
					if err != nil {
						t.Fatal(err)
					}
					storageFailureWrite(t, filepath.Join(f.root, "operator."+suffix), raw)
				}
			} else if fault == "missing-chain" {
				raw, err := os.ReadFile(filepath.Join(f.root, "operator.pem"))
				if err != nil {
					t.Fatal(err)
				}
				leaf, _ := pem.Decode(raw)
				storageFailureWrite(t, filepath.Join(f.root, "operator.pem"), pem.EncodeToMemory(leaf))
			} else {
				var file openShellProfileFile
				if _, err := ReadSessionReadbackFile(f.template, &file); err != nil {
					t.Fatal(err)
				}
				switch fault {
				case "endpoint":
					file.CredentialSource.Gateway = "https://127.0.0.1:1"
				case "workspace":
					file.CredentialSource.Workspace = "foreign"
				case "servername":
					file.CredentialSource.TLS.ServerName = "foreign.test"
				case "trust":
					file.CredentialSource.TLS.CAFile = "ca.pem"
				}
				writeProfileFixture(t, f.template, file)
			}
			if fault != "wrong-key" {
				// Refresh the candidate refs so role/chain rejection cannot be
				// satisfied merely by the old candidate file's digest mismatch.
				f.register(t)
			} else {
				keyPath := filepath.Join(f.root, "operator.key")
				ref, err := ReadSessionReadbackFile(keyPath, nil)
				if err != nil {
					t.Fatal(err)
				}
				f.plan.Sources["template:"+keyPath] = ref
			}
			before := f.bytes(t)
			owner, err := NewOpenShellSourceAuthGateway(f.residents, admission.NewBudget(9), f.path, f.plan, f.root)
			if owner != nil {
				_ = owner.Close()
			}
			if err == nil || owner != nil {
				t.Fatal("invalid registered Source principal reached reservation", fault, err)
			}
			sourceAuthTransportUnchanged(t, before)
			if calls, _ := f.sourceGuard.counts(); calls != 0 {
				t.Fatal("invalid principal contacted Source")
			}
			if calls, _ := f.targetGuard.counts(); calls != 0 {
				t.Fatal("invalid principal contacted Target")
			}
		})
	}
}

func TestSourceAuthTransportHistoricalRefsDoNotFreezeProgression(t *testing.T) {
	f := newSourceAuthTransportFixture(t)
	originalPlan, err := json.Marshal(f.plan)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{f.dispatch, filepath.Join(f.profile.Gateway.Binding.StateRoot, "owner.json")} {
		var raw map[string]json.RawMessage
		if _, err := ReadSessionReadbackFile(path, &raw); err != nil {
			t.Fatal(err)
		}
		raw["later_unknown_observation"] = json.RawMessage(`{"original":"preserved"}`)
		storageFailureWrite(t, path, raw)
	}
	before := f.bytes(t)
	owner := f.open(t)
	if owner.custody.Snapshot().Credentials[0].Pending == nil || !owner.custody.Snapshot().Credentials[0].SourceReadPending {
		t.Fatal("new observation erased pending effects")
	}
	sourceAuthTransportUnchanged(t, before)
	after, err := json.Marshal(f.plan)
	if err != nil || !bytes.Equal(originalPlan, after) {
		t.Fatal("factory rebased historical proposal")
	}
	if calls, _ := f.sourceGuard.counts(); calls != 0 {
		t.Fatal("construction continued credential progression")
	}
	if calls, _ := f.targetGuard.counts(); calls != 0 {
		t.Fatal("construction replayed pending mutation")
	}
}

func TestSourceAuthTransportFreezesSelectionAndKeepsExclusiveCustody(t *testing.T) {
	f := newSourceAuthTransportFixture(t)
	owner := f.open(t)
	before := f.bytes(t)
	second, err := NewOpenShellSourceAuthGateway(f.residents, admission.NewBudget(9), f.path, f.plan, f.root)
	if second != nil {
		_ = second.Close()
	}
	if err == nil || second != nil {
		t.Fatal("auth factory bypassed original exclusive custody")
	}
	sourceAuthTransportUnchanged(t, before)
	candidate, err := LoadOpenShellProfile(f.template)
	if err != nil {
		t.Fatal(err)
	}
	candidate.CredentialOrigin.TLS.Certificate.Certificate[0][0] ^= 0xff
	candidate.CredentialOrigin.TLS.Certificate.PrivateKey.(*ecdsa.PrivateKey).D.SetInt64(1)
	candidate.CredentialOrigin.TLS.RootCAs.AddCert(&x509.Certificate{RawSubject: []byte("foreign")})
	f.profile.Gateway.TLS.Certificate.PrivateKey.(*ecdsa.PrivateKey).D.SetInt64(1)
	f.plan.ProposedAccess.Gateway = "https://127.0.0.1:1"
	clear(f.plan.Sources)
	storageFailureWrite(t, filepath.Join(f.root, "operator.key"), []byte("changed after selection"))
	sourceAuthTransportStart(t, owner)
	response, err := sourceAuthTransportGet(t, owner.credentialClient)
	if err != nil || response.GetCredentials()["CODEX_AUTH_ACCESS_TOKEN"].GetValue() != "synthetic-initial-access" {
		t.Fatal("frozen Source TLS followed caller buffers, proposal or source file", err)
	}
	if err := owner.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := sourceAuthTransportGet(t, owner.credentialClient); err == nil {
		t.Fatal("closed owner retained a usable Source channel")
	}
}

func TestSourceAuthTransportRegisteredSourceTemplateKeepsTarget(t *testing.T) {
	f := newSourceAuthTransportFixture(t)
	raw, err := os.ReadFile(f.path)
	if err != nil {
		t.Fatal(err)
	}
	file, err := sourceOperatorProfile(raw, f.path)
	if err != nil {
		t.Fatal(err)
	}
	file.Gateway.Binding.Gateway = file.CredentialSource.Gateway
	file.Gateway.Binding.Workspace = file.CredentialSource.Workspace
	file.Gateway.TLS = file.CredentialSource.TLS
	file.Gateway.TLS.CertificateFile = filepath.Join(f.root, "operator.pem")
	file.Gateway.TLS.KeyFile = filepath.Join(f.root, "operator.key")
	file.CredentialSource = nil
	client := filepath.Join(f.root, "candidate-client")
	file.Environment = []string{"XDG_CONFIG_HOME=" + client}
	client = filepath.Join(client, "openshell", "gateways", file.GatewayName)
	storageFailureWrite(t, filepath.Join(client, "metadata.json"), map[string]string{"name": file.GatewayName, "gateway_endpoint": file.Gateway.Binding.Gateway, "auth_mode": "mtls"})
	for from, to := range map[string]string{"source-ca.pem": "ca.crt", "operator.pem": "tls.crt", "operator.key": "tls.key"} {
		raw, err := os.ReadFile(filepath.Join(f.root, from))
		if err != nil {
			t.Fatal(err)
		}
		storageFailureWrite(t, filepath.Join(client, "mtls", to), raw)
	}
	writeProfileFixture(t, f.template, file)
	f.plan, err = InspectSourceAuthPlan(f.path, f.template, f.dispatch, "context", f.root)
	if err != nil {
		t.Fatal(err)
	}
	f.register(t)
	before := f.bytes(t)
	owner := f.open(t)
	if owner.spec.Binding != f.profile.Gateway.Binding || owner.spec.Binding.Gateway == file.Gateway.Binding.Gateway || !reflect.DeepEqual(owner.spec.Resident, f.profile.Gateway.Resident) || !reflect.DeepEqual(owner.spec.TLS.Certificate.Certificate, f.profile.Gateway.TLS.Certificate.Certificate) {
		t.Fatal("Source template replaced the original Target deployment")
	}
	sourceAuthTransportStart(t, owner)
	response, err := sourceAuthTransportGet(t, owner.credentialClient)
	if err != nil || response.GetCredentials()["CODEX_AUTH_ACCESS_TOKEN"].GetValue() != "synthetic-initial-access" {
		t.Fatal("registered Source template did not reach credential consumer", err)
	}
	sourceAuthTransportUnchanged(t, before)
}

func TestSourceAuthTransportKeepsOriginalProviderIdentityGuards(t *testing.T) {
	for _, fault := range []string{"source-binding", "provider-id", "provider-workspace", "profile"} {
		t.Run(fault, func(t *testing.T) {
			f := newSourceAuthTransportFixture(t)
			owner := f.open(t)
			before := f.bytes(t)
			original := owner.custody.Snapshot().Credentials[0]
			row := &OpenShellCredentialDelivery{Provider: original.Provider, SourceBinding: original.SourceBinding, SourceProviderID: original.SourceProviderID, ProfileID: original.ProfileID}
			f.source.mu.Lock()
			switch fault {
			case "source-binding":
				row.SourceBinding = strings.Repeat("b", 64)
			case "provider-id":
				f.source.provider.Metadata.Id = "replacement-provider"
			case "provider-workspace":
				f.source.provider.Metadata.Workspace = "foreign"
			case "profile":
				f.source.profile.Credentials[0].EnvVars = []string{"FOREIGN"}
			}
			f.source.mu.Unlock()
			sourceAuthTransportStart(t, owner)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if err := owner.deliverCredentialRow(ctx, row); err == nil {
				t.Fatal("effective auth bypassed original credential identity guard")
			}
			f.source.mu.Lock()
			retrievals := f.source.retrievals
			f.source.mu.Unlock()
			if retrievals != 0 {
				t.Fatal("identity drift reached credential getter")
			}
			if calls, _ := f.targetGuard.counts(); calls != 0 {
				t.Fatal("identity drift reached Target")
			}
			sourceAuthTransportUnchanged(t, before)
		})
	}
}
